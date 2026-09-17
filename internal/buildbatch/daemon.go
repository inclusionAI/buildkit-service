package buildbatch

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	defaultDaemonSocket     = "/tmp/buildctl-batch.sock"
	daemonResultDB          = "/tmp/result.lmdb"
	daemonLogsFile          = "/tmp/logs.jsonl"
	daemonImageDir          = "/tmp/buildctl-batch-images"
	daemonMaxUploadBytes    = int64(512 << 20)
	daemonMaxExtractedBytes = int64(4 << 30)
	daemonMaxArchiveFiles   = 100000
)

var errDaemonArchiveLimit = errors.New("archive exceeds configured limit")

type daemonState struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type daemonServer struct {
	defaultAddrs string
	resultDB     string
	logsFile     string

	mu          sync.RWMutex
	build       daemonState
	buildCancel context.CancelFunc
	buildDone   chan struct{}
	oneshot     bool

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// triggerShutdown signals the daemon to begin a graceful shutdown. It is safe
// to call multiple times; only the first call has an effect.
func (s *daemonServer) triggerShutdown() {
	s.shutdownOnce.Do(func() {
		close(s.shutdownCh)
	})
}

func writeDockerAuth(b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode --auth: %w", err)
	}
	dir := "/root/.docker"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		return err
	}
	logInfo("Wrote docker config to %s (%d bytes)", p, len(raw))
	return nil
}

// RunDaemon starts the batch HTTP daemon.
func RunDaemon(ctx context.Context, cfg DaemonConfig) error {
	if cfg.Auth != "" {
		if err := writeDockerAuth(cfg.Auth); err != nil {
			return err
		}
	}
	return runDaemon(ctx, cfg.Socket, cfg.Addrs)
}

func runDaemon(ctx context.Context, socketPath, defaultAddrs string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	srv := &daemonServer{
		defaultAddrs: defaultAddrs,
		resultDB:     daemonResultDB,
		logsFile:     daemonLogsFile,
		build:        daemonState{Status: "idle"},
		shutdownCh:   make(chan struct{}),
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/api/v1/health", srv.handleHealth)
	e.POST("/api/v1/build", srv.handleBuildPost)
	e.POST("/api/v1/build/cancel", srv.handleBuildCancel)
	e.GET("/api/v1/build/status", srv.handleBuildStatus)
	e.POST("/api/v1/export", srv.handleExport)
	e.POST("/api/v1/preheat", srv.handlePreheat)
	e.POST("/api/v1/shutdown", srv.handleShutdown)

	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	defer ln.Close()

	httpSrv := &http.Server{Handler: e}

	go func() {
		select {
		case <-ctx.Done():
		case <-srv.shutdownCh:
			logInfo("Shutdown requested, shutting down daemon")
		}
		srv.gracefulShutdown(httpSrv)
	}()

	logInfo("Daemon listening on %s", socketPath)
	if err := httpSrv.Serve(ln); err != http.ErrServerClosed {
		return err
	}

	// In oneshot mode the daemon exits after a single build. Reflect a failed
	// build in the exit code so the enclosing Job is marked Failed.
	srv.mu.RLock()
	oneshot := srv.oneshot
	status := srv.build.Status
	buildErr := srv.build.Error
	srv.mu.RUnlock()
	if oneshot && status == "failed" {
		return fmt.Errorf("oneshot build failed: %s", buildErr)
	}
	return nil
}

// gracefulShutdown waits for any in-flight build to finish (bounded) before
// shutting down the HTTP server.
func (s *daemonServer) gracefulShutdown(httpSrv *http.Server) {
	s.mu.RLock()
	done := s.buildDone
	s.mu.RUnlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			logError("Build did not finish within 30s, forcing shutdown")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}

func (s *daemonServer) handleShutdown(c echo.Context) error {
	logInfo("Shutdown requested from %s", c.RealIP())
	s.triggerShutdown()
	return c.JSON(http.StatusAccepted, daemonState{Status: "shutting-down"})
}

func (s *daemonServer) handleHealth(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

func (s *daemonServer) handleBuildStatus(c echo.Context) error {
	return c.JSON(http.StatusOK, s.getBuildState())
}

func (s *daemonServer) handleBuildCancel(c echo.Context) error {
	s.mu.RLock()
	state := s.build
	cancel := s.buildCancel
	s.mu.RUnlock()

	if state.Status != "running" || cancel == nil {
		return c.JSON(http.StatusOK, state)
	}

	logInfo("Cancelling current build on request from %s", c.RealIP())
	cancel()
	return c.JSON(http.StatusAccepted, daemonState{Status: "running"})
}

// ---------------- /api/v1/build ----------------

func (s *daemonServer) handleBuildPost(c echo.Context) error {
	resetCommandStartTime(time.Now())

	s.mu.Lock()
	if s.build.Status == "running" {
		s.mu.Unlock()
		logError("Rejected build request: another build is already running")
		return c.JSON(http.StatusConflict, daemonState{Status: "error", Error: "a build is already running"})
	}
	s.build = daemonState{Status: "running"}
	done := make(chan struct{})
	buildCtx, buildCancel := context.WithCancel(context.Background())
	s.buildCancel = buildCancel
	s.buildDone = done
	s.mu.Unlock()

	logInfo("Accepted build request from %s", c.RealIP())

	// Receive zip body → temp file
	tmpZip, err := os.CreateTemp("", "buildctl-batch-upload-*.zip")
	if err != nil {
		logError("Build request: create temp zip failed: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		return c.JSON(http.StatusInternalServerError, s.getBuildState())
	}
	bytesWritten, exceeded, err := daemonCopyWithLimit(tmpZip, c.Request().Body, daemonMaxUploadBytes)
	if err != nil {
		tmpZip.Close()
		os.Remove(tmpZip.Name())
		logError("Build request: receive zip failed: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: "receive zip: " + err.Error()})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		return c.JSON(http.StatusInternalServerError, s.getBuildState())
	}
	if exceeded {
		tmpZip.Close()
		os.Remove(tmpZip.Name())
		s.setBuildState(daemonState{Status: "failed", Error: "compressed upload exceeds 512 MiB"})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		return c.JSON(http.StatusRequestEntityTooLarge, s.getBuildState())
	}
	tmpZip.Close()
	logInfo("Build request body stored at %s (%d bytes)", tmpZip.Name(), bytesWritten)

	q := c.QueryParams()
	if _, err := buildOptionsFromQuery(q, s.defaultAddrs, s.resultDB, s.logsFile); err != nil {
		logError("Build request: invalid query parameters: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		os.Remove(tmpZip.Name())
		return c.JSON(http.StatusBadRequest, s.getBuildState())
	}
	imageDirCount, err := validateBuildArchive(tmpZip.Name(), daemonMaxExtractedBytes, daemonMaxArchiveFiles)
	if err != nil {
		logError("Build request: invalid zip layout in %s: %v", tmpZip.Name(), err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		os.Remove(tmpZip.Name())
		status := http.StatusBadRequest
		if errors.Is(err, errDaemonArchiveLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		return c.JSON(status, s.getBuildState())
	}
	logInfo("Build request zip layout validated: %d image directorie(s)", imageDirCount)

	oneshot, err := queryBoolStrict(q, "oneshot", false)
	if err != nil {
		logError("Build request: invalid oneshot flag: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		buildCancel()
		close(done)
		s.clearBuildExecution(done)
		os.Remove(tmpZip.Name())
		return c.JSON(http.StatusBadRequest, s.getBuildState())
	}
	if oneshot {
		s.mu.Lock()
		s.oneshot = true
		s.mu.Unlock()
	}

	logInfo("Build request accepted for async processing: zip=%s oneshot=%t", tmpZip.Name(), oneshot)
	go s.runBuildAsync(buildCtx, tmpZip.Name(), q, done, oneshot)
	return c.JSON(http.StatusAccepted, daemonState{Status: "running"})
}

func (s *daemonServer) runBuildAsync(buildCtx context.Context, zipPath string, q url.Values, done chan struct{}, oneshot bool) {
	if oneshot {
		// Trigger shutdown after the build reaches a terminal state (this defer
		// runs last, after done is closed) so the daemon process exits and the
		// enclosing Job completes.
		defer s.triggerShutdown()
	}
	defer close(done)
	defer s.clearBuildExecution(done)
	defer os.Remove(zipPath)
	defer func() {
		_ = os.RemoveAll(daemonImageDir)
	}()
	logInfo("Async build started: zip=%s", zipPath)

	opts, err := buildOptionsFromQuery(q, s.defaultAddrs, s.resultDB, s.logsFile)
	if err != nil {
		logError("Async build: parse options failed: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		return
	}
	logInfo("Async build options resolved: addrs=%d concurrency=%d timeout=%ds retry=%d verbose=%t oci=%t target=%q skip-fail=%t",
		len(opts.addrs), opts.concurrency, opts.timeout, opts.retry, opts.verbose, opts.oci, opts.target, opts.skipFail)
	opts.ctx = buildCtx

	_ = os.RemoveAll(daemonImageDir)
	if err := extractZip(zipPath, daemonImageDir, daemonMaxExtractedBytes, daemonMaxArchiveFiles); err != nil {
		logError("Async build: extract zip %s to %s failed: %v", zipPath, daemonImageDir, err)
		s.setBuildState(daemonState{Status: "failed", Error: "extract zip: " + err.Error()})
		return
	}
	logInfo("Async build extracted zip to %s", daemonImageDir)
	opts.imageDirs = daemonImageDir

	logInfo("Async build entering runBuildCommand")
	if err := runBuildCommand(opts); err != nil {
		logError("Async build failed: %v", err)
		s.setBuildState(daemonState{Status: "failed", Error: err.Error()})
		return
	}

	logInfo("Async build completed successfully")
	s.setBuildState(daemonState{Status: "completed"})
}

// ---------------- /api/v1/export ----------------

func (s *daemonServer) handleExport(c echo.Context) error {
	q := c.QueryParams()
	opts, err := exportOptionsFromQuery(q, s.resultDB)
	if err != nil {
		return c.JSON(http.StatusBadRequest, daemonState{Status: "error", Error: err.Error()})
	}

	tmpOut, err := os.CreateTemp("", "buildctl-batch-export-*.jsonl")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, daemonState{Status: "error", Error: err.Error()})
	}
	tmpPath := tmpOut.Name()
	tmpOut.Close()
	defer os.Remove(tmpPath)

	opts.resultPath = tmpPath

	if err := runExport(opts); err != nil {
		return c.JSON(http.StatusInternalServerError, daemonState{Status: "error", Error: err.Error()})
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, daemonState{Status: "error", Error: err.Error()})
	}
	defer f.Close()

	return c.Stream(http.StatusOK, "application/x-ndjson", f)
}

// ---------------- /api/v1/preheat ----------------

func (s *daemonServer) handlePreheat(c echo.Context) error {
	q := c.QueryParams()
	opts, err := preheatOptionsFromQuery(q, s.resultDB)
	if err != nil {
		return c.JSON(http.StatusBadRequest, daemonState{Status: "error", Error: err.Error()})
	}

	if err := runPreheat(opts); err != nil {
		return c.JSON(http.StatusInternalServerError, daemonState{Status: "error", Error: err.Error()})
	}

	return c.JSON(http.StatusOK, daemonState{Status: "completed"})
}

// ---------------- helpers ----------------

func (s *daemonServer) setBuildState(st daemonState) {
	s.mu.Lock()
	s.build = st
	s.mu.Unlock()
	if st.Error != "" {
		logError("Build state changed to %s: %s", st.Status, st.Error)
		return
	}
	logInfo("Build state changed to %s", st.Status)
}

func (s *daemonServer) clearBuildExecution(done chan struct{}) {
	s.mu.Lock()
	if s.buildDone == done {
		s.buildDone = nil
		s.buildCancel = nil
	}
	s.mu.Unlock()
}

func (s *daemonServer) getBuildState() daemonState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.build
}

func validateQueryKeys(q url.Values, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key, values := range q {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("unsupported query parameter %q", key)
		}
		if key == "var" {
			continue
		}
		if len(values) > 1 {
			return fmt.Errorf("query parameter %q must be specified at most once", key)
		}
	}
	return nil
}

func queryValue(q url.Values, key string) (string, bool, error) {
	values, ok := q[key]
	if !ok || len(values) == 0 {
		return "", false, nil
	}
	if len(values) > 1 {
		return "", false, fmt.Errorf("query parameter %q must be specified at most once", key)
	}
	return values[0], true, nil
}

func queryBoolStrict(q url.Values, key string, def bool) (bool, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("query parameter %q must be a boolean", key)
	}
}

func queryIntStrict(q url.Values, key string, def int, min int) (int, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return 0, err
	}
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be an integer", key)
	}
	if n < min {
		return 0, fmt.Errorf("query parameter %q must be greater than or equal to %d", key, min)
	}
	return n, nil
}

func queryDurationStrict(q url.Values, key string, def time.Duration, min time.Duration) (time.Duration, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return 0, err
	}
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be a duration", key)
	}
	if d < min {
		return 0, fmt.Errorf("query parameter %q must be greater than or equal to %s", key, min)
	}
	return d, nil
}

type buildArchiveTopLevel struct {
	name     string
	children map[string]struct{}
	rootFile bool
}

func validateBuildArchive(zipPath string, maxExtractedBytes int64, maxArchiveFiles int) (int, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	if err := validateDaemonArchiveLimits(r.File, maxExtractedBytes, maxArchiveFiles); err != nil {
		return 0, err
	}

	topLevels := make(map[string]*buildArchiveTopLevel)
	for _, file := range r.File {
		cleaned, err := validateBuildArchivePath(file.Name)
		if err != nil {
			return 0, err
		}
		if cleaned == "" {
			continue
		}

		parts := strings.Split(cleaned, "/")
		top := parts[0]
		entry, ok := topLevels[top]
		if !ok {
			entry = &buildArchiveTopLevel{
				name:     top,
				children: make(map[string]struct{}),
			}
			topLevels[top] = entry
		}

		if len(parts) == 1 {
			if !file.FileInfo().IsDir() {
				entry.rootFile = true
			}
			continue
		}

		entry.children[parts[1]] = struct{}{}
	}

	if len(topLevels) == 0 {
		return 0, fmt.Errorf("zip archive is empty or does not contain any image directories")
	}

	var validCount int
	var rootFiles []string
	var invalidDirs []string
	for _, name := range sortedBuildArchiveKeys(topLevels) {
		entry := topLevels[name]
		if entry.rootFile {
			rootFiles = append(rootFiles, entry.name)
			continue
		}
		_, hasDockerfile := entry.children["Dockerfile"]
		_, hasMetadata := entry.children["metadata.json"]
		if hasDockerfile && hasMetadata {
			validCount++
			continue
		}

		missing := make([]string, 0, 2)
		if !hasDockerfile {
			missing = append(missing, "Dockerfile")
		}
		if !hasMetadata {
			missing = append(missing, "metadata.json")
		}

		childPreview := sortedBuildArchiveChildren(entry.children)
		if len(childPreview) > 3 {
			childPreview = childPreview[:3]
		}
		invalidDirs = append(invalidDirs, fmt.Sprintf("%s (missing %s", entry.name, strings.Join(missing, " and ")))
		if len(childPreview) > 0 {
			invalidDirs[len(invalidDirs)-1] += fmt.Sprintf(", contains %s", strings.Join(childPreview, ", "))
		}
		invalidDirs[len(invalidDirs)-1] += ")"
	}

	if len(rootFiles) > 0 {
		return 0, fmt.Errorf("zip root must contain only image directories, found root file entries: %s", strings.Join(limitBuildArchiveList(rootFiles, 5), ", "))
	}
	if len(invalidDirs) > 0 {
		return 0, fmt.Errorf("zip root must directly contain image directories with Dockerfile and metadata.json; invalid top-level directories: %s", strings.Join(limitBuildArchiveList(invalidDirs, 5), "; "))
	}
	if validCount == 0 {
		return 0, fmt.Errorf("zip archive does not contain any valid image directories")
	}

	return validCount, nil
}

func validateBuildArchivePath(name string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	if normalized == "" {
		return "", nil
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("zip contains absolute path %q", name)
	}
	cleaned := path.Clean(normalized)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("zip contains invalid path %q", name)
	}
	return cleaned, nil
}

func sortedBuildArchiveKeys(entries map[string]*buildArchiveTopLevel) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBuildArchiveChildren(children map[string]struct{}) []string {
	result := make([]string, 0, len(children))
	for child := range children {
		result = append(result, child)
	}
	sort.Strings(result)
	return result
}

func limitBuildArchiveList(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	trimmed := append([]string{}, values[:limit]...)
	trimmed = append(trimmed, fmt.Sprintf("... and %d more", len(values)-limit))
	return trimmed
}

func buildOptionsFromQuery(q url.Values, defaultAddrs string, resultDB string, logsFile string) (options, error) {
	if err := validateQueryKeys(q, "addrs", "concurrency", "fail-fast", "oci", "both-formats", "oom-cooldown", "timeout", "retry", "verbose", "target", "skip-fail", "var", "oneshot"); err != nil {
		return options{}, err
	}
	addrsValue, ok, err := queryValue(q, "addrs")
	if err != nil {
		return options{}, err
	}
	if !ok || strings.TrimSpace(addrsValue) == "" {
		addrsValue = defaultAddrs
	}
	addrs, err := parseBuildkitAddrs(addrsValue)
	if err != nil {
		return options{}, err
	}
	concurrency, err := queryIntStrict(q, "concurrency", 1, 1)
	if err != nil {
		return options{}, err
	}
	timeout, err := queryIntStrict(q, "timeout", 300, 0)
	if err != nil {
		return options{}, err
	}
	retry, err := queryIntStrict(q, "retry", 0, 0)
	if err != nil {
		return options{}, err
	}
	oomCooldown, err := queryDurationStrict(q, "oom-cooldown", defaultBuildkitOOMCooldown, 0)
	if err != nil {
		return options{}, err
	}
	for _, addr := range addrs {
		if addr != nil {
			addr.cooldown = oomCooldown
		}
	}
	failFast, err := queryBoolStrict(q, "fail-fast", false)
	if err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	bothFormats, err := queryBoolStrict(q, "both-formats", false)
	if err != nil {
		return options{}, err
	}
	verbose, err := queryBoolStrict(q, "verbose", false)
	if err != nil {
		return options{}, err
	}
	skipFail, err := queryBoolStrict(q, "skip-fail", false)
	if err != nil {
		return options{}, err
	}
	target, _, err := queryValue(q, "target")
	if err != nil {
		return options{}, err
	}
	buildVars, err := parseBuildVariables(q["var"])
	if err != nil {
		return options{}, err
	}
	return options{
		addrs:       addrs,
		addrsRaw:    addrsValue,
		concurrency: concurrency,
		failfast:    failFast,
		oci:         oci,
		bothFormats: bothFormats,
		oomCooldown: oomCooldown,
		resultPath:  resultDB,
		logsPath:    logsFile,
		vars:        buildVars,
		timeout:     timeout,
		retry:       retry,
		verbose:     verbose,
		target:      strings.TrimSpace(target),
		skipFail:    skipFail,
	}, nil
}

func exportOptionsFromQuery(q url.Values, resultDB string) (options, error) {
	if err := validateQueryKeys(q, "oci", "with-fail"); err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	withFail, err := queryBoolStrict(q, "with-fail", false)
	if err != nil {
		return options{}, err
	}
	return options{
		fromResultPath: resultDB,
		oci:            oci,
		withFail:       withFail,
	}, nil
}

func preheatOptionsFromQuery(q url.Values, resultDB string) (options, error) {
	if err := validateQueryKeys(q, "dragonfly-scheduler-addr", "concurrency", "interval", "timeout", "fail-fast", "oci", "verbose"); err != nil {
		return options{}, err
	}
	schedulerAddr, ok, err := queryValue(q, "dragonfly-scheduler-addr")
	if err != nil {
		return options{}, err
	}
	if !ok || strings.TrimSpace(schedulerAddr) == "" {
		return options{}, fmt.Errorf("dragonfly-scheduler-addr is required")
	}
	concurrency, err := queryIntStrict(q, "concurrency", 1, 1)
	if err != nil {
		return options{}, err
	}
	interval, err := queryIntStrict(q, "interval", 5, 0)
	if err != nil {
		return options{}, err
	}
	timeout, err := queryIntStrict(q, "timeout", 5, 0)
	if err != nil {
		return options{}, err
	}
	failFast, err := queryBoolStrict(q, "fail-fast", false)
	if err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	verbose, err := queryBoolStrict(q, "verbose", false)
	if err != nil {
		return options{}, err
	}
	return options{
		fromResultPath:         resultDB,
		dragonflySchedulerAddr: strings.TrimSpace(schedulerAddr),
		concurrency:            concurrency,
		interval:               interval,
		timeout:                timeout,
		failfast:               failFast,
		oci:                    oci,
		verbose:                verbose,
	}, nil
}

func extractZip(zipPath, dest string, maxExtractedBytes int64, maxArchiveFiles int) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := validateDaemonArchiveLimits(r.File, maxExtractedBytes, maxArchiveFiles); err != nil {
		return err
	}

	destPrefix := filepath.Clean(dest) + string(os.PathSeparator)
	var extractedBytes int64
	for _, f := range r.File {
		cleaned, err := validateBuildArchivePath(f.Name)
		if err != nil {
			return err
		}
		if cleaned == "" {
			continue
		}
		target := filepath.Clean(filepath.Join(dest, filepath.FromSlash(cleaned)))
		if !strings.HasPrefix(target, destPrefix) {
			return fmt.Errorf("zip contains invalid path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		remaining := maxExtractedBytes - extractedBytes
		if remaining < 0 || f.UncompressedSize64 > uint64(remaining) {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		written, exceeded, copyErr := daemonCopyWithLimit(out, rc, remaining)
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
		if exceeded {
			_ = os.Remove(target)
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		extractedBytes += written
	}
	return nil
}

func validateDaemonArchiveLimits(files []*zip.File, maxExtractedBytes int64, maxArchiveFiles int) error {
	if len(files) > maxArchiveFiles {
		return fmt.Errorf("%w: archive has %d entries, maximum is %d", errDaemonArchiveLimit, len(files), maxArchiveFiles)
	}
	var total uint64
	for _, file := range files {
		if file.UncompressedSize64 > uint64(maxExtractedBytes)-total {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		total += file.UncompressedSize64
	}
	return nil
}

func daemonCopyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, bool, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return written, false, err
	}
	var extra [1]byte
	n, err := src.Read(extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return written, false, err
	}
	return written, n > 0, nil
}
