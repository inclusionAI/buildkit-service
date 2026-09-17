package builddaemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/labstack/echo/v4"
)

type buildServer struct {
	cfg       Config
	store     *taskStore
	pool      *addrPool
	runner    buildRunner
	images    imageChecker
	modes     *buildModes
	scheduler *buildModeScheduler
	router    targetRouter
	metrics   daemonMetrics
	// globalSem caps builds running concurrently across all buildkitd
	// addresses. nil disables the global limit (per-address concurrency
	// still applies). Tasks over the limit stay queued until a slot frees.
	globalSem chan struct{}
	// admissionMu serializes upload/extraction accounting against work-dir.
	admissionMu sync.Mutex
}

// daemonMetrics holds monotonic build outcome counters. The running gauge is
// derived from the task store at scrape time, so it stays accurate even after
// terminal tasks are reaped.
type daemonMetrics struct {
	succeeded int64
	failed    int64
}

func (s *buildServer) routes() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/metrics", s.handleMetrics)

	v1 := e.Group("/v1")
	if s.cfg.AuthToken != "" {
		v1.Use(bearerAuthMiddleware(s.cfg.AuthToken))
	}
	v1.POST("/builds", s.handleCreateBuild)
	v1.GET("/builds", s.handleListBuilds)
	v1.GET("/builds/:id", s.handleGetBuild)
	v1.GET("/builds/:id/logs", s.handleGetBuildLogs)
	v1.DELETE("/builds/:id", s.handleCancelBuild)
	v1.POST("/builds/:id/cancel", s.handleCancelBuild)
	v1.HEAD("/images", s.handleHeadImage)
	return e
}

func bearerAuthMiddleware(token string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			header := c.Request().Header.Get(echo.HeaderAuthorization)
			if header != "Bearer "+token {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			}
			return next(c)
		}
	}
}

func (s *buildServer) handleListBuilds(c echo.Context) error {
	return c.JSON(http.StatusOK, viewTasks(s.store.list()))
}

func (s *buildServer) handleGetBuild(c echo.Context) error {
	task, ok := s.store.get(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusOK, viewTask(task))
}

func (s *buildServer) handleGetBuildLogs(c echo.Context) error {
	task, ok := s.store.get(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	tailBytes := s.cfg.MaxLogBytes
	if raw := strings.TrimSpace(c.QueryParam("tail_bytes")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "tail_bytes must be a positive integer"})
		}
		tailBytes = parsed
	}
	maxLogBytes := s.cfg.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	if tailBytes > maxLogBytes {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "tail_bytes exceeds max-log-bytes"})
	}
	logs, err := tailFile(task.LogPath, tailBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.String(http.StatusOK, logs)
}

func (s *buildServer) handleCancelBuild(c echo.Context) error {
	task, ok := s.store.cancel(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusAccepted, viewTask(task))
}

// handleMetrics exposes Prometheus text metrics. The active gauge comes from
// the global semaphore, while queued/running describe task states.
func (s *buildServer) handleMetrics(c echo.Context) error {
	queued := s.store.countByStatus(buildStatusQueued)
	running := s.store.countByStatus(buildStatusRunning)
	active := running
	if s.globalSem != nil {
		active = len(s.globalSem)
	}
	succeeded := atomic.LoadInt64(&s.metrics.succeeded)
	failed := atomic.LoadInt64(&s.metrics.failed)

	var b strings.Builder
	b.WriteString("# HELP buildctl_daemon_builds_queued Builds waiting for a buildkit worker slot.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_queued gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_queued %d\n", queued)
	b.WriteString("# HELP buildctl_daemon_builds_running Builds currently running.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_running gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_running %d\n", running)
	b.WriteString("# HELP buildctl_daemon_builds_active Builds currently holding a global execution slot.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_active gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_active %d\n", active)
	b.WriteString("# HELP buildctl_daemon_builds_succeeded_total Builds that completed successfully.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_succeeded_total counter\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_succeeded_total %d\n", succeeded)
	b.WriteString("# HELP buildctl_daemon_builds_failed_total Builds that ended in failure.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_failed_total counter\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_failed_total %d\n", failed)
	if s.modes != nil {
		b.WriteString("# HELP buildctl_daemon_mode_builds_queued Builds waiting for a worker slot, partitioned by mode.\n")
		b.WriteString("# TYPE buildctl_daemon_mode_builds_queued gauge\n")
		b.WriteString("# HELP buildctl_daemon_mode_builds_running Builds currently running, partitioned by mode.\n")
		b.WriteString("# TYPE buildctl_daemon_mode_builds_running gauge\n")
		for _, mode := range s.modes.names() {
			fmt.Fprintf(&b, "buildctl_daemon_mode_builds_queued{mode=%q} %d\n", mode, s.store.countByModeAndStatus(mode, buildStatusQueued))
			fmt.Fprintf(&b, "buildctl_daemon_mode_builds_running{mode=%q} %d\n", mode, s.store.countByModeAndStatus(mode, buildStatusRunning))
		}
	}
	return c.Blob(http.StatusOK, "text/plain; version=0.0.4", []byte(b.String()))
}
