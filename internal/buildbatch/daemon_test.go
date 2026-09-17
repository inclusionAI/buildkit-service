package buildbatch

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildOptionsFromQueryAcceptsOneshot(t *testing.T) {
	q := url.Values{}
	q.Set("oneshot", "true")

	if _, err := buildOptionsFromQuery(q, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl"); err != nil {
		t.Fatalf("expected oneshot to be an accepted query key, got %v", err)
	}
}

func TestBuildOptionsFromQueryRejectsUnknownKey(t *testing.T) {
	q := url.Values{}
	q.Set("bogus", "1")

	if _, err := buildOptionsFromQuery(q, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl"); err == nil {
		t.Fatal("expected unknown query key to be rejected")
	}
}

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

func TestBatchArchiveLimits(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "source.zip")
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range map[string]string{
		"image/Dockerfile":    "FROM scratch\n",
		"image/metadata.json": `{"target":"example.com/team/image:v1"}`,
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := validateBuildArchive(zipPath, 1<<20, 1); !errors.Is(err, errDaemonArchiveLimit) {
		t.Fatalf("expected file-count limit error, got %v", err)
	}
	if _, err := validateBuildArchive(zipPath, 4, 10); !errors.Is(err, errDaemonArchiveLimit) {
		t.Fatalf("expected extracted-size limit error, got %v", err)
	}
	if _, err := validateBuildArchive(zipPath, 1<<20, 10); err != nil {
		t.Fatalf("expected valid archive within limits: %v", err)
	}
	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "output"), 1<<20, 10); err != nil {
		t.Fatalf("expected archive within limits to extract: %v", err)
	}
}
