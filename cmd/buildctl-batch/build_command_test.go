package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamPreparedBuildJobsStopsBlockedSendOnCancellation(t *testing.T) {
	sourceRoot := t.TempDir()
	writeBuildImageDir(t, sourceRoot, "image-a", "example.com/team/image:a")
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	preparedRoot := t.TempDir()
	out := make(chan buildJob)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- streamPreparedBuildJobs(
			ctx,
			options{imageDirs: sourceRoot},
			[]bool{true},
			nil,
			entries,
			preparedRoot,
			stubBuildResultReader{},
			newBuildOutcomeCounters(1, nil),
			out,
		)
	}()

	preparedMetadata := filepath.Join(preparedRoot, "image-a", "metadata.json")
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(preparedMetadata); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("producer did not reach blocked job send")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("producer returned %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not exit after cancellation")
	}

	sourceDockerfile, err := os.ReadFile(filepath.Join(sourceRoot, "image-a", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceDockerfile) != "FROM scratch\n" {
		t.Fatalf("producer modified source Dockerfile: %q", sourceDockerfile)
	}
}

func TestStreamPreparedBuildJobsPropagatesProducerErrorWithoutClosingOutput(t *testing.T) {
	sourceRoot := t.TempDir()
	writeBuildImageDir(t, sourceRoot, "image-a", "example.com/team/image:a")
	if err := os.WriteFile(filepath.Join(sourceRoot, "image-b.txt"), []byte("invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan buildJob, 1)

	err = streamPreparedBuildJobs(
		context.Background(),
		options{imageDirs: sourceRoot},
		[]bool{true},
		nil,
		entries,
		t.TempDir(),
		stubBuildResultReader{},
		newBuildOutcomeCounters(2, nil),
		out,
	)
	if err == nil || !strings.Contains(err.Error(), "must contain only directories, found image-b.txt") {
		t.Fatalf("unexpected producer error: %v", err)
	}
	select {
	case job := <-out:
		if job.key != "example.com/team/image:a" {
			t.Fatalf("unexpected produced job: %#v", job)
		}
	default:
		t.Fatal("expected valid job before producer error")
	}

	// The orchestration goroutine owns closing this channel, not the producer.
	out <- buildJob{key: "still-open"}
}
