package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileTailTruncatesLargeLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	logs, err := readFileTail(path, 4)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !strings.Contains(logs, "truncated") {
		t.Fatalf("expected truncation marker, got %q", logs)
	}
	if !strings.HasSuffix(logs, "6789") {
		t.Fatalf("expected tail bytes, got %q", logs)
	}
}

func TestBuildOutputCaptureBoundsAndKeepsTail(t *testing.T) {
	capture, err := newBuildOutputCapture()
	if err != nil {
		t.Fatal(err)
	}
	prefix := bytes.Repeat([]byte("a"), int(maxBuildLogBytes))
	tail := []byte("expected-tail")
	if _, err := capture.Write(append(prefix, tail...)); err != nil {
		t.Fatal(err)
	}
	if len(capture.buffer) != int(maxBuildLogBytes) {
		t.Fatalf("capture allocated %d bytes, want %d", len(capture.buffer), maxBuildLogBytes)
	}
	logs, err := capture.closeAndReadTail(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "truncated") || !strings.HasSuffix(logs, string(tail)) {
		t.Fatalf("expected bounded tail with marker, got suffix %q", logs[len(logs)-64:])
	}
}

func TestBuildCommandArgsUsesSpecCompression(t *testing.T) {
	addr := &buildkitAddr{addr: "tcp://127.0.0.1:1234"}

	nydusArgs := strings.Join(buildCommandArgs(buildSpec{target: "example.com/ns/repo:tag_nydus_v3"}, addr), " ")
	if !strings.Contains(nydusArgs, "compression=nydus") {
		t.Fatalf("expected nydus compression, got %q", nydusArgs)
	}
	if strings.Contains(nydusArgs, "compression=gzip") {
		t.Fatalf("did not expect gzip compression in nydus args: %q", nydusArgs)
	}

	ociArgs := strings.Join(buildCommandArgs(buildSpec{target: "example.com/ns/repo:tag", oci: true}, addr), " ")
	if !strings.Contains(ociArgs, "compression=gzip") {
		t.Fatalf("expected gzip compression, got %q", ociArgs)
	}
	if strings.Contains(ociArgs, "compression=nydus") {
		t.Fatalf("did not expect nydus compression in oci args: %q", ociArgs)
	}
}
