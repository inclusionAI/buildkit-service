package buildbatch

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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

func TestValidateBuildArchivePath(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		wantError string
	}{
		{name: "relative", input: "image/context/file", want: "image/context/file"},
		{name: "windows separators", input: `image\context\file`, want: "image/context/file"},
		{name: "empty", input: " ", want: ""},
		{name: "root", input: "/image/Dockerfile", wantError: `zip contains absolute path "/image/Dockerfile"`},
		{name: "traversal", input: "../outside", wantError: `zip contains invalid path "../outside"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateBuildArchivePath(test.input)
			if test.wantError != "" {
				if err == nil || err.Error() != test.wantError {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("got path %q, error %v; want %q", got, err, test.want)
			}
		})
	}
}
