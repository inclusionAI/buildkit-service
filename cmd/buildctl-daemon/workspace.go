package main

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const zipMaxExtractedFileMode = 0o755

type imageMetadata struct {
	Target string `json:"target"`
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

var errArchiveLimit = errors.New("archive exceeds configured limit")

func saveUploadedFile(upload *multipart.FileHeader, dst string, maxBytes int64) error {
	if upload.Size > maxBytes {
		return fmt.Errorf("%w: compressed upload exceeds %d bytes", errArchiveLimit, maxBytes)
	}
	src, err := upload.Open()
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create upload file: %w", err)
	}
	defer out.Close()
	_, exceeded, err := copyWithLimit(out, src, maxBytes)
	if err != nil {
		return fmt.Errorf("save upload: %w", err)
	}
	if exceeded {
		return fmt.Errorf("%w: compressed upload exceeds %d bytes", errArchiveLimit, maxBytes)
	}
	return nil
}

func extractZip(src, dst string, maxExtractedBytes int64, maxArchiveFiles int) error {
	reader, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()
	if len(reader.File) > maxArchiveFiles {
		return fmt.Errorf("%w: archive has %d entries, maximum is %d", errArchiveLimit, len(reader.File), maxArchiveFiles)
	}

	var extractedBytes int64
	for _, file := range reader.File {
		if err := validateBuildArchivePath(file.Name); err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(file.Name))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		remaining := maxExtractedBytes - extractedBytes
		if remaining < 0 || file.UncompressedSize64 > uint64(remaining) {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errArchiveLimit, maxExtractedBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		mode := file.FileInfo().Mode().Perm()
		if mode == 0 || mode > zipMaxExtractedFileMode {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			in.Close()
			return err
		}
		written, exceeded, copyErr := copyWithLimit(out, in, remaining)
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return copyErr
		}
		if exceeded {
			_ = os.Remove(target)
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errArchiveLimit, maxExtractedBytes)
		}
		extractedBytes += written
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, bool, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return written, false, err
	}
	var extra [1]byte
	n, err := src.Read(extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return written, false, err
	}
	return written, n > 0, nil
}

type limitedLogWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedLogWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if w.remaining <= 0 || originalLength == 0 {
		return originalLength, nil
	}
	if int64(len(data)) > w.remaining {
		data = data[:w.remaining]
	}
	written, err := w.writer.Write(data)
	w.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != len(data) {
		return written, io.ErrShortWrite
	}
	return originalLength, nil
}

func validateBuildArchivePath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return fmt.Errorf("invalid zip path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid zip path %q", name)
	}
	return nil
}

func findBuildContext(root string) (string, error) {
	if fileExists(filepath.Join(root, "Dockerfile")) {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if fileExists(filepath.Join(candidate, "Dockerfile")) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("zip must contain Dockerfile at root or in one top-level directory")
	}
	return candidates[0], nil
}

func resolveBuildImage(contextDir, imageOverride string) (string, error) {
	image := strings.TrimSpace(imageOverride)
	if image != "" {
		return image, nil
	}

	metadata, err := readImageMetadata(contextDir)
	if err != nil {
		return "", err
	}
	image = strings.TrimSpace(metadata.Target)
	if image == "" {
		return "", fmt.Errorf("image is required when metadata.json target is empty")
	}
	return image, nil
}

func readImageMetadata(contextDir string) (imageMetadata, error) {
	path := filepath.Join(contextDir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return imageMetadata{}, fmt.Errorf("image is required when metadata.json is missing")
		}
		return imageMetadata{}, fmt.Errorf("read metadata.json: %w", err)
	}
	var metadata imageMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return imageMetadata{}, fmt.Errorf("parse metadata.json: %w", err)
	}
	return metadata, nil
}

func sourceContextContentHash(dir string) (string, error) {
	type contextFile struct {
		path string
		rel  string
	}
	var files []contextFile
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel source context path: %w", err)
		}
		files = append(files, contextFile{path: path, rel: filepath.ToSlash(rel)})
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk source context for scheduling hash: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	for _, file := range files {
		_, _ = h.Write([]byte(file.rel))
		_, _ = h.Write([]byte{0})
		f, err := os.Open(file.path)
		if err != nil {
			return "", fmt.Errorf("open source context file %s: %w", file.rel, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash source context file %s: %w", file.rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close source context file %s: %w", file.rel, closeErr)
		}
		_, _ = h.Write([]byte{0})
	}

	return fmt.Sprintf("source:%x", h.Sum(nil)), nil
}

func shortScheduleKey(key string) string {
	if len(key) <= len("source:")+12 || !strings.HasPrefix(key, "source:") {
		return key
	}
	return key[:len("source:")+12]
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func tailFile(path string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func newTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
