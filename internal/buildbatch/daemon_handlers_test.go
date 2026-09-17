package buildbatch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestDaemonRoutes(t *testing.T) {
	srv := newTestDaemonServer()
	var got []string
	for _, route := range srv.routes().Routes() {
		got = append(got, route.Method+" "+route.Path)
	}
	sort.Strings(got)
	want := []string{
		"GET /api/v1/build/status",
		"GET /api/v1/health",
		"POST /api/v1/build",
		"POST /api/v1/build/cancel",
		"POST /api/v1/export",
		"POST /api/v1/preheat",
		"POST /api/v1/shutdown",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon routes changed:\n got: %v\nwant: %v", got, want)
	}
}

func TestDaemonHealthAndBuildStatusResponses(t *testing.T) {
	srv := newTestDaemonServer()

	status, body := invokeDaemonHandler(t, srv.handleHealth, http.MethodGet, "/api/v1/health", nil)
	if status != http.StatusOK || body != "{\"status\":\"ok\"}\n" {
		t.Fatalf("health response changed: status=%d body=%q", status, body)
	}

	status, body = invokeDaemonHandler(t, srv.handleBuildStatus, http.MethodGet, "/api/v1/build/status", nil)
	if status != http.StatusOK || body != "{\"status\":\"idle\"}\n" {
		t.Fatalf("build status response changed: status=%d body=%q", status, body)
	}
}

func TestBuildPostReturnsConflictWhileBuildIsRunning(t *testing.T) {
	srv := newTestDaemonServer()
	srv.build = daemonState{Status: "running"}

	status, body := invokeDaemonHandler(t, srv.handleBuildPost, http.MethodPost, "/api/v1/build", strings.NewReader("unused"))
	if status != http.StatusConflict || body != "{\"status\":\"error\",\"error\":\"a build is already running\"}\n" {
		t.Fatalf("conflict response changed: status=%d body=%q", status, body)
	}
}

func TestBuildCancelResponses(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		srv := newTestDaemonServer()
		status, body := invokeDaemonHandler(t, srv.handleBuildCancel, http.MethodPost, "/api/v1/build/cancel", nil)
		if status != http.StatusOK || body != "{\"status\":\"idle\"}\n" {
			t.Fatalf("idle cancel response changed: status=%d body=%q", status, body)
		}
	})

	t.Run("running", func(t *testing.T) {
		srv := newTestDaemonServer()
		var cancelled atomic.Bool
		srv.build = daemonState{Status: "running"}
		srv.buildCancel = func() {
			cancelled.Store(true)
		}

		status, body := invokeDaemonHandler(t, srv.handleBuildCancel, http.MethodPost, "/api/v1/build/cancel", nil)
		if status != http.StatusAccepted || body != "{\"status\":\"running\"}\n" {
			t.Fatalf("running cancel response changed: status=%d body=%q", status, body)
		}
		if !cancelled.Load() {
			t.Fatal("running build cancel function was not called")
		}
	})
}

func TestShutdownHandlerSignalsDaemon(t *testing.T) {
	srv := newTestDaemonServer()
	status, body := invokeDaemonHandler(t, srv.handleShutdown, http.MethodPost, "/api/v1/shutdown", nil)
	if status != http.StatusAccepted || body != "{\"status\":\"shutting-down\"}\n" {
		t.Fatalf("shutdown response changed: status=%d body=%q", status, body)
	}
	select {
	case <-srv.shutdownCh:
	default:
		t.Fatal("shutdown handler did not signal the daemon")
	}
}

func TestBuildPostRemovesUploadAfterQueryValidationFailure(t *testing.T) {
	uploadDir := t.TempDir()
	t.Setenv("TMPDIR", uploadDir)
	srv := newTestDaemonServer()

	status, _ := invokeDaemonHandler(t, srv.handleBuildPost, http.MethodPost, "/api/v1/build?bogus=1", strings.NewReader("payload"))
	if status != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", status)
	}
	assertDirectoryEmpty(t, uploadDir)
}

func TestExportHandlerRemovesTemporaryOutput(t *testing.T) {
	temporaryDir := t.TempDir()
	t.Setenv("TMPDIR", temporaryDir)
	srv := newTestDaemonServer()
	srv.resultDB = filepath.Join(t.TempDir(), "result.lmdb")

	status, _ := invokeDaemonHandler(t, srv.handleExport, http.MethodPost, "/api/v1/export", nil)
	if status != http.StatusOK && status != http.StatusInternalServerError {
		t.Fatalf("unexpected export status: %d", status)
	}
	assertDirectoryEmpty(t, temporaryDir)
}

func newTestDaemonServer() *daemonServer {
	return &daemonServer{
		defaultAddrs: "tcp://127.0.0.1:9094",
		resultDB:     "/tmp/result.lmdb",
		logsFile:     "/tmp/logs.jsonl",
		build:        daemonState{Status: "idle"},
		shutdownCh:   make(chan struct{}),
	}
}

func invokeDaemonHandler(t *testing.T, handler echo.HandlerFunc, method, target string, body io.Reader) (int, string) {
	t.Helper()
	e := echo.New()
	request := httptest.NewRequest(method, target, body)
	recorder := httptest.NewRecorder()
	if err := handler(e.NewContext(request, recorder)); err != nil {
		t.Fatalf("invoke handler: %v", err)
	}
	return recorder.Code, recorder.Body.String()
}

func assertDirectoryEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read temporary directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files were not removed: %v", entries)
	}
}
