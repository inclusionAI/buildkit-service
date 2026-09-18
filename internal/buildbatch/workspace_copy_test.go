package buildbatch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDirectoryPreservesContentPermissionsAndSymlinks(t *testing.T) {
	source := t.TempDir()
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "context.txt")
	if err := os.WriteFile(file, []byte("context\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("context.txt", filepath.Join(nested, "context.link")); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyDirectory(source, destination); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "nested", "context.txt"))
	if err != nil || string(content) != "context\n" {
		t.Fatalf("copied content changed: content=%q err=%v", content, err)
	}
	for _, pair := range [][2]string{{nested, filepath.Join(destination, "nested")}, {file, filepath.Join(destination, "nested", "context.txt")}} {
		sourceInfo, err := os.Stat(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		destinationInfo, err := os.Stat(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if sourceInfo.Mode().Perm() != destinationInfo.Mode().Perm() {
			t.Fatalf("mode changed for %s: got %o, want %o", pair[1], destinationInfo.Mode().Perm(), sourceInfo.Mode().Perm())
		}
	}
	linkTarget, err := os.Readlink(filepath.Join(destination, "nested", "context.link"))
	if err != nil || linkTarget != "context.txt" {
		t.Fatalf("symlink changed: target=%q err=%v", linkTarget, err)
	}
}
