package buildbatch

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	daemonMaxExtractedBytes = int64(4 << 30)
	daemonMaxArchiveFiles   = 100000
)

var errDaemonArchiveLimit = errors.New("archive exceeds configured limit")

type buildArchiveTopLevel struct {
	name     string
	children map[string]struct{}
	rootFile bool
}

func validateBuildArchive(zipPath string, maxExtractedBytes int64, maxArchiveFiles int) (int, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	if err := validateDaemonArchiveLimits(r.File, maxExtractedBytes, maxArchiveFiles); err != nil {
		return 0, err
	}

	topLevels := make(map[string]*buildArchiveTopLevel)
	for _, file := range r.File {
		cleaned, err := validateBuildArchivePath(file.Name)
		if err != nil {
			return 0, err
		}
		if cleaned == "" {
			continue
		}

		parts := strings.Split(cleaned, "/")
		top := parts[0]
		entry, ok := topLevels[top]
		if !ok {
			entry = &buildArchiveTopLevel{
				name:     top,
				children: make(map[string]struct{}),
			}
			topLevels[top] = entry
		}

		if len(parts) == 1 {
			if !file.FileInfo().IsDir() {
				entry.rootFile = true
			}
			continue
		}

		entry.children[parts[1]] = struct{}{}
	}

	if len(topLevels) == 0 {
		return 0, fmt.Errorf("zip archive is empty or does not contain any image directories")
	}

	var validCount int
	var rootFiles []string
	var invalidDirs []string
	for _, name := range sortedBuildArchiveKeys(topLevels) {
		entry := topLevels[name]
		if entry.rootFile {
			rootFiles = append(rootFiles, entry.name)
			continue
		}
		_, hasDockerfile := entry.children["Dockerfile"]
		_, hasMetadata := entry.children["metadata.json"]
		if hasDockerfile && hasMetadata {
			validCount++
			continue
		}

		missing := make([]string, 0, 2)
		if !hasDockerfile {
			missing = append(missing, "Dockerfile")
		}
		if !hasMetadata {
			missing = append(missing, "metadata.json")
		}

		childPreview := sortedBuildArchiveChildren(entry.children)
		if len(childPreview) > 3 {
			childPreview = childPreview[:3]
		}
		invalidDirs = append(invalidDirs, fmt.Sprintf("%s (missing %s", entry.name, strings.Join(missing, " and ")))
		if len(childPreview) > 0 {
			invalidDirs[len(invalidDirs)-1] += fmt.Sprintf(", contains %s", strings.Join(childPreview, ", "))
		}
		invalidDirs[len(invalidDirs)-1] += ")"
	}

	if len(rootFiles) > 0 {
		return 0, fmt.Errorf("zip root must contain only image directories, found root file entries: %s", strings.Join(limitBuildArchiveList(rootFiles, 5), ", "))
	}
	if len(invalidDirs) > 0 {
		return 0, fmt.Errorf("zip root must directly contain image directories with Dockerfile and metadata.json; invalid top-level directories: %s", strings.Join(limitBuildArchiveList(invalidDirs, 5), "; "))
	}
	if validCount == 0 {
		return 0, fmt.Errorf("zip archive does not contain any valid image directories")
	}

	return validCount, nil
}

func validateBuildArchivePath(name string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	if normalized == "" {
		return "", nil
	}
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("zip contains absolute path %q", name)
	}
	cleaned := path.Clean(normalized)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("zip contains invalid path %q", name)
	}
	return cleaned, nil
}

func sortedBuildArchiveKeys(entries map[string]*buildArchiveTopLevel) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBuildArchiveChildren(children map[string]struct{}) []string {
	result := make([]string, 0, len(children))
	for child := range children {
		result = append(result, child)
	}
	sort.Strings(result)
	return result
}

func limitBuildArchiveList(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	trimmed := append([]string{}, values[:limit]...)
	trimmed = append(trimmed, fmt.Sprintf("... and %d more", len(values)-limit))
	return trimmed
}

func extractZip(zipPath, dest string, maxExtractedBytes int64, maxArchiveFiles int) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := validateDaemonArchiveLimits(r.File, maxExtractedBytes, maxArchiveFiles); err != nil {
		return err
	}

	destPrefix := filepath.Clean(dest) + string(os.PathSeparator)
	var extractedBytes int64
	for _, f := range r.File {
		cleaned, err := validateBuildArchivePath(f.Name)
		if err != nil {
			return err
		}
		if cleaned == "" {
			continue
		}
		target := filepath.Clean(filepath.Join(dest, filepath.FromSlash(cleaned)))
		if !strings.HasPrefix(target, destPrefix) {
			return fmt.Errorf("zip contains invalid path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		remaining := maxExtractedBytes - extractedBytes
		if remaining < 0 || f.UncompressedSize64 > uint64(remaining) {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		written, exceeded, copyErr := daemonCopyWithLimit(out, rc, remaining)
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
		if exceeded {
			_ = os.Remove(target)
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		extractedBytes += written
	}
	return nil
}

func validateDaemonArchiveLimits(files []*zip.File, maxExtractedBytes int64, maxArchiveFiles int) error {
	if len(files) > maxArchiveFiles {
		return fmt.Errorf("%w: archive has %d entries, maximum is %d", errDaemonArchiveLimit, len(files), maxArchiveFiles)
	}
	var total uint64
	for _, file := range files {
		if file.UncompressedSize64 > uint64(maxExtractedBytes)-total {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errDaemonArchiveLimit, maxExtractedBytes)
		}
		total += file.UncompressedSize64
	}
	return nil
}

func daemonCopyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, bool, error) {
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
