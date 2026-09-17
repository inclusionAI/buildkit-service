package buildbatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTriggerShutdownIsIdempotent(t *testing.T) {
	srv := &daemonServer{shutdownCh: make(chan struct{})}

	srv.triggerShutdown()
	srv.triggerShutdown() // must not panic on a second close

	select {
	case <-srv.shutdownCh:
	default:
		t.Fatal("expected shutdown channel to be closed after triggerShutdown")
	}
}

func TestRunDaemonStopsWhenContextIsCancelled(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("buildctl-batch-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, socketPath, "")
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat daemon socket: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for daemon socket")
		}
		select {
		case err := <-done:
			t.Fatalf("daemon stopped before creating its socket: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run daemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}

func TestRunDaemonPropagatesListenError(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "missing", "buildctl-batch.sock")
	err := runDaemon(context.Background(), socketPath, "")
	if err == nil || !strings.HasPrefix(err.Error(), "listen on "+socketPath+": ") {
		t.Fatalf("listen error changed: %v", err)
	}
}

func TestDaemonCompletionErrorPreservesOneshotExitSemantics(t *testing.T) {
	tests := []struct {
		name    string
		oneshot bool
		state   daemonState
		want    string
	}{
		{name: "regular failure", state: daemonState{Status: "failed", Error: "build failed"}},
		{name: "oneshot success", oneshot: true, state: daemonState{Status: "completed"}},
		{name: "oneshot failure", oneshot: true, state: daemonState{Status: "failed", Error: "build failed"}, want: "oneshot build failed: build failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestDaemonServer()
			srv.oneshot = test.oneshot
			srv.build = test.state
			err := srv.completionError()
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected completion error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("completion error changed: got %v, want %q", err, test.want)
			}
		})
	}
}

func TestClearBuildExecutionOnlyClearsOwnedRun(t *testing.T) {
	srv := newTestDaemonServer()
	owned := make(chan struct{})
	other := make(chan struct{})
	srv.buildDone = owned
	srv.buildCancel = func() {}

	srv.clearBuildExecution(other)
	if srv.buildDone != owned || srv.buildCancel == nil {
		t.Fatal("unrelated build execution cleared active state")
	}

	srv.clearBuildExecution(owned)
	if srv.buildDone != nil || srv.buildCancel != nil {
		t.Fatal("owned build execution state was not cleared")
	}
}
