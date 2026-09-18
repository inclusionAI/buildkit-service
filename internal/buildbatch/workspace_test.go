package buildbatch

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
	sourceDockerfile, err := os.ReadFile(filepath.Join(imageDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sourceDockerfile), "Episode Processing Project") || !strings.Contains(string(sourceDockerfile), "RUN cat >") {
		t.Fatalf("source Dockerfile was modified: %s", sourceDockerfile)
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
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	prepared, _, err := prepareBuildImageDirs(root, nil)
	if err == nil || !strings.Contains(err.Error(), "line 2:") || !strings.Contains(err.Error(), "COPY") || prepared != "" {
		t.Fatalf("expected preparation to fail before build: prepared=%q err=%v", prepared, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("source context modified on rejection: %v", err)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed preparation leaked workspace: %v", entries)
	}
}

func TestValidateSourceImageDirErrors(t *testing.T) {
	t.Run("missing Dockerfile", func(t *testing.T) {
		dir := t.TempDir()
		err := validateSourceImageDir(dir)
		want := dir + ": missing required file Dockerfile"
		if err == nil || err.Error() != want {
			t.Fatalf("validation error changed: got %v, want %q", err, want)
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateSourceImageDir(dir)
		want := dir + ": missing required file metadata.json"
		if err == nil || err.Error() != want {
			t.Fatalf("validation error changed: got %v, want %q", err, want)
		}
	})

	t.Run("Dockerfile is not regular", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "Dockerfile"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := validateSourceImageDir(dir)
		want := dir + ": required file Dockerfile must be a regular file"
		if err == nil || err.Error() != want {
			t.Fatalf("validation error changed: got %v, want %q", err, want)
		}
	})

	t.Run("metadata is not regular", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "metadata.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := validateSourceImageDir(dir)
		want := dir + ": required file metadata.json must be a regular file"
		if err == nil || err.Error() != want {
			t.Fatalf("validation error changed: got %v, want %q", err, want)
		}
	})
}

func TestPrepareBuildImageDirsHashesBeforeTransformations(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image-a")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM $BUILDCTL_BATCH_BASE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "metadata.json"), []byte(`{"target":"$BUILDCTL_BATCH_TARGET"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wantHash, err := sourceContextContentHash(imageDir)
	if err != nil {
		t.Fatal(err)
	}
	preparedRoot, scheduleKeys, err := prepareBuildImageDirs(root, map[string]string{
		"BUILDCTL_BATCH_BASE":   "scratch",
		"BUILDCTL_BATCH_TARGET": "example.com/team/image:v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(preparedRoot)
	if scheduleKeys["image-a"] != wantHash {
		t.Fatalf("schedule hash changed after transformation: got %q, want %q", scheduleKeys["image-a"], wantHash)
	}
	sourceHash, err := sourceContextContentHash(imageDir)
	if err != nil {
		t.Fatal(err)
	}
	if sourceHash != wantHash {
		t.Fatalf("source workspace changed during preparation: got hash %q, want %q", sourceHash, wantHash)
	}
}

func TestLoadBuildSpecsCleanupRemovesOwnedWorkspaceOnly(t *testing.T) {
	sourceRoot := t.TempDir()
	writeBuildImageDir(t, sourceRoot, "image-a", "example.com/team/image:a")

	specs, cleanup, err := loadBuildSpecs(sourceRoot, "", []bool{true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil || len(specs) != 1 {
		t.Fatalf("loadBuildSpecs returned %d specs and cleanup=%v", len(specs), cleanup != nil)
	}
	preparedRoot := filepath.Dir(specs[0].dir)
	if _, err := os.Stat(preparedRoot); err != nil {
		t.Fatalf("prepared workspace missing before cleanup: %v", err)
	}

	cleanup()
	cleanup()
	if _, err := os.Stat(preparedRoot); !os.IsNotExist(err) {
		t.Fatalf("prepared workspace still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "image-a", "Dockerfile")); err != nil {
		t.Fatalf("cleanup changed source workspace: %v", err)
	}
}
