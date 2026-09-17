package builddaemon

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFindBuildContextRootAndNested(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := findBuildContext(root)
	if err != nil || ctx != root {
		t.Fatalf("root context = %q, %v", ctx, err)
	}

	nestedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(nestedRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedRoot, "src", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err = findBuildContext(nestedRoot)
	if err != nil || ctx != filepath.Join(nestedRoot, "src") {
		t.Fatalf("nested context = %q, %v", ctx, err)
	}
}

func TestResolveBuildImagePrefersFormImage(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "metadata.json"), []byte(`{"target":"example.com/ns/from-metadata:tag"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := resolveBuildImage(contextDir, "example.com/ns/from-form:tag")
	if err != nil {
		t.Fatal(err)
	}
	if image != "example.com/ns/from-form:tag" {
		t.Fatalf("expected form image to win, got %q", image)
	}
}

func TestResolveBuildImageFallsBackToMetadataTarget(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "metadata.json"), []byte(`{"target":"example.com/ns/from-metadata:tag"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := resolveBuildImage(contextDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if image != "example.com/ns/from-metadata:tag" {
		t.Fatalf("expected metadata target, got %q", image)
	}
}

func TestSourceContextContentHashMatchesRFCAlgorithm(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "data.txt"), []byte("alpha\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := sourceContextContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	const want = "source:bf08700ef6b428e939efe2b57613e45067678b99a98a5fce7ef21a1ed9a6b0c9"
	if got != want {
		t.Fatalf("context digest mismatch: got %q, want %q", got, want)
	}

	if err := os.Chmod(filepath.Join(dir, "nested", "data.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotAfterModeChange, err := sourceContextContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfterModeChange != want {
		t.Fatalf("file mode unexpectedly affected RFC context digest: got %q, want %q", gotAfterModeChange, want)
	}
}

func TestExtractZipEnforcesArchiveLimits(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "source.zip")
	var zipBuf bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuf)
	for name, content := range map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"payload.txt": "payload",
	} {
		writer, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zipBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "files"), 1<<20, 1); !errors.Is(err, errArchiveLimit) {
		t.Fatalf("expected file-count limit error, got %v", err)
	}
	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "bytes"), 4, 10); !errors.Is(err, errArchiveLimit) {
		t.Fatalf("expected extracted-size limit error, got %v", err)
	}
	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "ok"), 1<<20, 10); err != nil {
		t.Fatalf("expected archive within limits to extract: %v", err)
	}
}

func TestLimitedLogWriterDiscardsOutputAfterLimit(t *testing.T) {
	var output bytes.Buffer
	writer := &limitedLogWriter{writer: &output, remaining: 5}

	for _, value := range []string{"abc", "def", "ghi"} {
		written, err := writer.Write([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if written != len(value) {
			t.Fatalf("Write returned %d, want %d", written, len(value))
		}
	}
	if got := output.String(); got != "abcde" {
		t.Fatalf("stored log is %q, want %q", got, "abcde")
	}
}
