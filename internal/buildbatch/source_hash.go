package buildbatch

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

func sourceContextContentHash(dir string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if !entry.IsDir() && !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk source context for scheduling hash: %w", err)
	}
	sort.Strings(paths)

	h := sha256.New()
	for _, path := range paths {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return "", fmt.Errorf("rel source context path: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("stat source context path %s: %w", rel, err)
		}
		rel = filepath.ToSlash(rel)
		mode := info.Mode()
		if mode.IsDir() {
			fmt.Fprintf(h, "dir:%s\x00%o\x00", rel, mode.Perm())
			continue
		}
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("read source context symlink %s: %w", rel, err)
			}
			fmt.Fprintf(h, "symlink:%s\x00%o\x00%s\x00", rel, mode.Perm(), target)
			continue
		}
		fmt.Fprintf(h, "file:%s\x00%o\x00", rel, mode.Perm())
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open source context file %s: %w", rel, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash source context file %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close source context file %s: %w", rel, closeErr)
		}
		_, _ = h.Write([]byte{0})
	}

	return fmt.Sprintf("source:%x", h.Sum(nil)), nil
}
