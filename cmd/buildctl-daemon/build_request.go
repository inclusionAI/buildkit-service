package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"
	"github.com/labstack/echo/v4"
)

func (s *buildServer) handleCreateBuild(c echo.Context) error {
	s.admissionMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			s.admissionMu.Unlock()
		}
	}()
	maxRetainedTasks := s.cfg.MaxRetainedTasks
	if maxRetainedTasks <= 0 {
		maxRetainedTasks = defaultMaxRetainedTasks
	}
	if s.store.len() >= maxRetainedTasks {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "maximum retained task count reached"})
	}
	maxWorkDirBytes := s.cfg.MaxWorkDirBytes
	if maxWorkDirBytes <= 0 {
		maxWorkDirBytes = defaultMaxWorkDirBytes
	}
	workDirBytes, err := directorySize(s.cfg.WorkDir)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	remainingWorkDirBytes := maxWorkDirBytes - workDirBytes
	if remainingWorkDirBytes <= 0 {
		return c.JSON(http.StatusInsufficientStorage, map[string]string{"error": "work directory byte budget exhausted"})
	}
	maxRequestBytes := s.cfg.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = defaultMaxRequestBytes
	}
	if remainingWorkDirBytes < maxRequestBytes {
		maxRequestBytes = remainingWorkDirBytes
	}
	uploadReadTimeout := s.cfg.UploadReadTimeout
	if uploadReadTimeout <= 0 {
		uploadReadTimeout = defaultUploadReadTimeout
	}
	controller := http.NewResponseController(c.Response().Writer)
	deadlineSet := controller.SetReadDeadline(time.Now().Add(uploadReadTimeout)) == nil
	c.Request().Body = http.MaxBytesReader(c.Response().Writer, c.Request().Body, maxRequestBytes)
	parseErr := c.Request().ParseMultipartForm(32 << 20)
	if deadlineSet {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if c.Request().MultipartForm != nil {
		defer c.Request().MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(parseErr, &maxBytesErr) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "build request exceeds max-request-bytes"})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid multipart build request"})
	}
	imageType, err := parseImageType(firstNonEmpty(c.FormValue("image_type"), c.FormValue("compression")))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	mode, modeCfg, err := s.modes.resolve(c.FormValue("mode"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	upload, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is required"})
	}

	id, err := newTaskID()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	taskDir := filepath.Join(s.cfg.WorkDir, id)
	contextParentDir := filepath.Join(taskDir, "context")
	if err := os.MkdirAll(contextParentDir, 0o755); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	zipPath := filepath.Join(taskDir, "source.zip")
	if err := saveUploadedFile(upload, zipPath, maxRequestBytes); err != nil {
		_ = os.RemoveAll(taskDir)
		if errors.Is(err, errArchiveLimit) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	zipInfo, err := os.Stat(zipPath)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	remainingWorkDirBytes -= zipInfo.Size()
	if remainingWorkDirBytes <= 0 {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusInsufficientStorage, map[string]string{"error": "work directory byte budget exhausted"})
	}
	maxExtractedBytes := s.cfg.MaxExtractedBytes
	if maxExtractedBytes <= 0 {
		maxExtractedBytes = defaultMaxExtractedBytes
	}
	if remainingWorkDirBytes < maxExtractedBytes {
		maxExtractedBytes = remainingWorkDirBytes
	}
	maxArchiveFiles := s.cfg.MaxArchiveFiles
	if maxArchiveFiles <= 0 {
		maxArchiveFiles = defaultMaxArchiveFiles
	}
	if err := extractZip(zipPath, contextParentDir, maxExtractedBytes, maxArchiveFiles); err != nil {
		_ = os.RemoveAll(taskDir)
		if errors.Is(err, errArchiveLimit) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	contextDir, err := findBuildContext(contextParentDir)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	hashStartedAt := time.Now()
	fmt.Fprintf(os.Stderr, "buildctl-daemon: hashing source context for task %s at %s\n", id, contextDir)
	scheduleKey, err := sourceContextContentHash(contextDir)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	fmt.Fprintf(os.Stderr, "buildctl-daemon: source context hash for task %s done in %s key=%s\n", id, time.Since(hashStartedAt).Round(100*time.Millisecond), shortScheduleKey(scheduleKey))
	if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(contextDir, "Dockerfile")); err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	image, err := resolveBuildImage(contextDir, c.FormValue("image"))
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	routedTarget := image
	routingDigest := strings.TrimPrefix(scheduleKey, "source:")
	if modeCfg.RoutingEnabled {
		if s.router == nil {
			_ = os.RemoveAll(taskDir)
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "registry routing is not configured"})
		}
		routedTarget, err = s.router.Route(image, routingDigest)
		if err != nil {
			_ = os.RemoveAll(taskDir)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	syncBuild := queryBool(firstNonEmpty(c.FormValue("sync"), c.QueryParam("sync")))
	buildParentCtx := context.Background()
	if syncBuild {
		buildParentCtx = c.Request().Context()
	}
	buildCtx, cancel := context.WithCancel(buildParentCtx)
	if timeout, err := parseOptionalPositiveInt(c.FormValue("timeout_seconds")); err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	} else if timeout > 0 {
		cancel()
		buildCtx, cancel = context.WithTimeout(buildParentCtx, time.Duration(timeout)*time.Second)
	}
	retry, err := parseOptionalNonNegativeInt(firstNonEmpty(c.FormValue("retry"), c.QueryParam("retry")), "retry", defaultBuildRetry)
	if err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	retryIntervalSeconds, err := parseOptionalNonNegativeInt(firstNonEmpty(c.FormValue("retry-interval"), c.QueryParam("retry-interval")), "retry-interval", int(defaultRetryInterval/time.Second))
	if err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	task := &buildTask{
		ID:            id,
		Status:        buildStatusQueued,
		Image:         image,
		Mode:          mode,
		RoutedTarget:  routedTarget,
		ImageType:     imageType,
		Target:        strings.TrimSpace(c.FormValue("target")),
		CreatedAt:     time.Now(),
		LogPath:       filepath.Join(taskDir, "build.log"),
		WorkDir:       taskDir,
		contextDir:    contextDir,
		scheduleKey:   scheduleKey,
		routingDigest: routingDigest,
		noCache:       queryBool(c.FormValue("no_cache")),
		retry:         retry,
		retryInterval: time.Duration(retryIntervalSeconds) * time.Second,
		buildArgs:     parseBuildArgs(c.Request()),
		ctx:           buildCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	s.store.add(task)
	done := task.done
	if s.scheduler == nil {
		go s.runBuildTask(task.ID)
	} else if err := s.scheduler.enqueue(task.Mode, task.ID); err != nil {
		task.cancel()
		close(task.done)
		s.cleanupTask(task.ID)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	s.admissionMu.Unlock()
	admissionLocked = false
	if syncBuild {
		return s.handleSyncBuild(c, task.ID, done)
	}

	snapshot, _ := s.store.get(task.ID)
	return c.JSON(http.StatusAccepted, viewTask(snapshot))
}

func parseBuildArgs(req *http.Request) map[string]string {
	if req.MultipartForm == nil {
		return nil
	}
	args := make(map[string]string)
	for key, values := range req.MultipartForm.Value {
		if len(values) == 0 {
			continue
		}
		var name string
		switch {
		case strings.HasPrefix(key, "build_arg."):
			name = strings.TrimPrefix(key, "build_arg.")
		case strings.HasPrefix(key, "build_arg:"):
			name = strings.TrimPrefix(key, "build_arg:")
		default:
			continue
		}
		name = strings.TrimSpace(name)
		if name != "" {
			args[name] = values[0]
		}
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

func queryBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func parseOptionalPositiveInt(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("timeout_seconds must be a positive integer")
	}
	return parsed, nil
}

func parseOptionalNonNegativeInt(raw, name string, def int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
