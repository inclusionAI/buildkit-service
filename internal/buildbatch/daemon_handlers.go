package buildbatch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	daemonImageDir       = "/tmp/buildctl-batch-images"
	daemonMaxUploadBytes = int64(512 << 20)
)

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
