package builddaemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPreservesValidationAndWorkDirOrder(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "created-before-address-validation")
	cfg := DefaultConfig()
	cfg.WorkDir = workDir
	cfg.BuildkitdAddrs = ""
	if err := Run(context.Background(), cfg); err == nil || err.Error() != "--buildkitd-addr is required" {
		t.Fatalf("unexpected address validation error: %v", err)
	}
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		t.Fatalf("work dir was not created before address validation: info=%v err=%v", info, err)
	}

	workDir = filepath.Join(t.TempDir(), "not-created-before-config-validation")
	cfg = DefaultConfig()
	cfg.WorkDir = workDir
	cfg.KeepTTL = -time.Second
	if err := Run(context.Background(), cfg); err == nil || err.Error() != "--keep-ttl must not be negative" {
		t.Fatalf("unexpected config validation error: %v", err)
	}
	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work dir was created before config validation: %v", err)
	}
}

func TestRunShutsDownOnContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Listen = address
	cfg.WorkDir = t.TempDir()
	cfg.BuildkitdAddrs = "tcp://127.0.0.1:1234"
	cfg.ModesJSON = `{"default_mode":{"concurrency":1}}`
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, requestErr := client.Get("http://" + address + "/healthz")
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				cancel()
				t.Fatalf("unexpected health status: %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("daemon did not become ready: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestServeHTTPShutsDownAfterContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, server, func() error { return server.Serve(listener) })
	}()

	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		cancel()
		t.Fatalf("unexpected status: %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP returned after shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveHTTP did not return after context cancellation")
	}
}

func TestServeHTTPPreservesServerErrorHandling(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "server closed", err: http.ErrServerClosed},
		{name: "listen failure", err: errors.New("listen failed"), want: errors.New("listen failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			server := &http.Server{}
			got := serveHTTP(ctx, server, func() error { return test.err })
			cancel()
			if test.want == nil && got != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if test.want != nil && (got == nil || got.Error() != test.want.Error()) {
				t.Fatalf("unexpected error: got %v, want %v", got, test.want)
			}
		})
	}
}

func TestPprofHandlersRemainRegistered(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	_, pattern := http.DefaultServeMux.Handler(req)
	if !strings.Contains(pattern, "/debug/pprof/") {
		t.Fatalf("pprof handler is not registered: %q", pattern)
	}
}
