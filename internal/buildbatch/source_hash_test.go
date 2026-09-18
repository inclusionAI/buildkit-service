package buildbatch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceContextContentHashInputs(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeHashFixture(t, first, false)
	writeHashFixture(t, second, true)

	firstHash, err := sourceContextContentHash(first)
	if err != nil {
		t.Fatal(err)
	}
	stableHash, err := sourceContextContentHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := sourceContextContentHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != stableHash || firstHash != secondHash {
		t.Fatalf("equivalent contexts produced different hashes: %q %q %q", firstHash, stableHash, secondHash)
	}

	if err := os.WriteFile(filepath.Join(second, "nested", "data.txt"), []byte("changed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	assertSourceHashChanged(t, firstHash, second, "file content")
	if err := os.WriteFile(filepath.Join(second, "nested", "data.txt"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(second, "nested", "data.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSourceHashChanged(t, firstHash, second, "file permissions")
	if err := os.Chmod(filepath.Join(second, "nested", "data.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(second, "data.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/other.txt", filepath.Join(second, "data.link")); err != nil {
		t.Fatal(err)
	}
	assertSourceHashChanged(t, firstHash, second, "symlink target")
}

func writeHashFixture(t *testing.T, root string, reverse bool) {
	t.Helper()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	files := []string{"Dockerfile", "metadata.json", filepath.Join("nested", "data.txt")}
	if reverse {
		files[0], files[2] = files[2], files[0]
	}
	contents := map[string]string{
		"Dockerfile":                        "FROM scratch\n",
		"metadata.json":                     `{"target":"example.com/team/image:v1"}`,
		filepath.Join("nested", "data.txt"): "payload\n",
	}
	for _, name := range files {
		mode := os.FileMode(0o644)
		if name == filepath.Join("nested", "data.txt") {
			mode = 0o640
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents[name]), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("nested/data.txt", filepath.Join(root, "data.link")); err != nil {
		t.Fatal(err)
	}
}

func assertSourceHashChanged(t *testing.T, original, dir, reason string) {
	t.Helper()
	got, err := sourceContextContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == original {
		t.Fatalf("source hash did not change after %s", reason)
	}
}
