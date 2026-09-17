package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBuildImageDir(t *testing.T, root, name, target string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"target":"`+target+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}
