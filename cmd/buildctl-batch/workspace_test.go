package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareBuildImageDirsPreprocessesLegacyDockerfileHeredoc(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image-a")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte(`FROM ubuntu:22.04
ARG NAME
RUN cat > /workspace/README.md << 'READMEEOF'
# ${BUILDCTL_BATCH_TITLE}
READMEEOF
`), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "metadata.json"), []byte(`{"target":"example.com/ns/repo:tag"}`), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	preparedRoot, _, err := prepareBuildImageDirs(root, map[string]string{"BUILDCTL_BATCH_TITLE": "Episode Processing Project"})
	if err != nil {
		t.Fatalf("prepare image dirs: %v", err)
	}
	defer os.RemoveAll(preparedRoot)

	rewritten, err := os.ReadFile(filepath.Join(preparedRoot, "image-a", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(rewritten)
	if !strings.Contains(content, "COPY <<'READMEEOF' /workspace/README.md") {
		t.Fatalf("expected heredoc COPY rewrite, got:\n%s", content)
	}
	if !strings.Contains(content, "# Episode Processing Project") {
		t.Fatalf("expected build variable replacement before rewrite, got:\n%s", content)
	}
	if strings.Contains(content, "RUN cat >") || strings.Contains(content, "BUILDCTL_BATCH_TITLE") {
		t.Fatalf("unexpected legacy command or unresolved variable remains:\n%s", content)
	}
}

func TestPrepareBuildImageDirsRejectsChainedHeredoc(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image-a")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("FROM alpine\nRUN true && cat >/f << 'EOF'\nlisten_port=5140\nEOF\n")
	path := filepath.Join(imageDir, "Dockerfile")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "metadata.json"), []byte(`{"target":"example.com/ns/repo:tag"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, _, err := prepareBuildImageDirs(root, nil)
	if err == nil || !strings.Contains(err.Error(), "line 2:") || !strings.Contains(err.Error(), "COPY") || prepared != "" {
		t.Fatalf("expected preparation to fail before build: prepared=%q err=%v", prepared, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("source context modified on rejection: %v", err)
	}
}

func TestBuildVariableParsingAndReplacement(t *testing.T) {
	vars, err := parseBuildVariables([]string{
		"BUILDCTL_BATCH_REGISTRY=registry.example.com",
		"BUILDCTL_BATCH_TAG=v1=debug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if vars["BUILDCTL_BATCH_REGISTRY"] != "registry.example.com" || vars["BUILDCTL_BATCH_TAG"] != "v1=debug" {
		t.Fatalf("unexpected parsed variables: %#v", vars)
	}

	got, err := replaceBuildVariables(
		[]byte("FROM $BUILDCTL_BATCH_REGISTRY/ns/repo:${BUILDCTL_BATCH_TAG}\n"),
		"Dockerfile",
		vars,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM registry.example.com/ns/repo:v1=debug\n"; string(got) != want {
		t.Fatalf("replacement = %q, want %q", got, want)
	}

	if _, err := replaceBuildVariables([]byte("FROM $BUILDCTL_BATCH_MISSING/repo\n"), "Dockerfile", vars); err == nil || err.Error() != "Dockerfile contains unresolved build variable(s): BUILDCTL_BATCH_MISSING" {
		t.Fatalf("unexpected unresolved variable error: %v", err)
	}
	if _, err := parseBuildVariables([]string{"REGISTRY=example.com"}); err == nil || err.Error() != `invalid --var "REGISTRY=example.com", key must start with BUILDCTL_BATCH_` {
		t.Fatalf("unexpected invalid variable error: %v", err)
	}
}
