package buildbatch

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	defaultDaemonSocket = "/tmp/buildctl-batch.sock"
	daemonResultDB      = "/tmp/result.lmdb"
	daemonLogsFile      = "/tmp/logs.jsonl"
)

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

	e := srv.routes()

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

	return srv.completionError()
}

// completionError reflects a failed oneshot build in the process exit code so
// the enclosing Job is marked Failed.
func (s *daemonServer) completionError() error {
	s.mu.RLock()
	oneshot := s.oneshot
	status := s.build.Status
	buildErr := s.build.Error
	s.mu.RUnlock()
	if oneshot && status == "failed" {
		return fmt.Errorf("oneshot build failed: %s", buildErr)
	}
	return nil
}

// routes assembles the daemon HTTP API.
func (s *daemonServer) routes() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/api/v1/health", s.handleHealth)
	e.POST("/api/v1/build", s.handleBuildPost)
	e.POST("/api/v1/build/cancel", s.handleBuildCancel)
	e.GET("/api/v1/build/status", s.handleBuildStatus)
	e.POST("/api/v1/export", s.handleExport)
	e.POST("/api/v1/preheat", s.handlePreheat)
	e.POST("/api/v1/shutdown", s.handleShutdown)
	return e
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
