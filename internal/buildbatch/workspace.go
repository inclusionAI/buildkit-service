package buildbatch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"
)

var buildctlBatchVarPattern = regexp.MustCompile(`\$\{?(BUILDCTL_BATCH_[A-Za-z0-9_]+)\}?`)

type imageMetadata struct {
	Target string `json:"target"`
}

func shortScheduleKey(key string) string {
	if len(key) <= len("source:")+12 || !strings.HasPrefix(key, "source:") {
		return key
	}
	return key[:len("source:")+12]
}

func prepareBuildImageDirs(imageDirs string, buildVars map[string]string) (string, map[string]string, error) {
	entries, err := os.ReadDir(imageDirs)
	if err != nil {
		return "", nil, fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("image-dirs %s does not contain any image directories", imageDirs)
	}

	preparedRoot, err := os.MkdirTemp("", "buildctl-batch-image-dirs-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp image-dirs: %w", err)
	}
	scheduleKeys := make(map[string]string, len(entries))

	cleanup := func() {
		_ = os.RemoveAll(preparedRoot)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			cleanup()
			return "", nil, fmt.Errorf("image-dirs %s must contain only directories, found %s", imageDirs, entry.Name())
		}

		srcDir := filepath.Join(imageDirs, entry.Name())
		if err := validateSourceImageDir(srcDir); err != nil {
			cleanup()
			return "", nil, err
		}

		dstDir := filepath.Join(preparedRoot, entry.Name())
		if err := copyDirectory(srcDir, dstDir); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy %s: %w", srcDir, err)
		}

		scheduleKey, err := sourceContextContentHash(dstDir)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		scheduleKeys[entry.Name()] = scheduleKey

		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "Dockerfile"), buildVars); err != nil {
			cleanup()
			return "", nil, err
		}
		if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(dstDir, "Dockerfile")); err != nil {
			cleanup()
			return "", nil, err
		}
		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "metadata.json"), buildVars); err != nil {
			cleanup()
			return "", nil, err
		}
	}

	return preparedRoot, scheduleKeys, nil
}

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

func validateSourceImageDir(dir string) error {
	if err := requireRegularFile(filepath.Join(dir, "Dockerfile")); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if err := requireRegularFile(filepath.Join(dir, "metadata.json")); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing required file %s", filepath.Base(path))
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("required file %s must be a regular file", filepath.Base(path))
	}
	return nil
}

func replaceBuildVariablesInFile(path string, buildVars map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	replaced, err := replaceBuildVariables(raw, path, buildVars)
	if err != nil {
		return err
	}
	if bytes.Equal(raw, replaced) {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, replaced, info.Mode())
}

func replaceBuildVariables(raw []byte, path string, buildVars map[string]string) ([]byte, error) {
	if len(buildVars) == 0 {
		return raw, nil
	}

	content := string(raw)
	for key, value := range buildVars {
		dollarToken := "$" + key
		braceToken := "${" + key + "}"
		content = strings.ReplaceAll(content, braceToken, value)
		content = strings.ReplaceAll(content, dollarToken, value)
	}

	unresolved := findUnresolvedBuildVariables(content)
	if len(unresolved) > 0 {
		missing := make([]string, 0, len(unresolved))
		for _, token := range unresolved {
			if _, ok := buildVars[token]; !ok {
				missing = append(missing, token)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("%s contains unresolved build variable(s): %s", path, strings.Join(uniqueStrings(missing), ", "))
		}
	}

	return []byte(content), nil
}

func parseBuildVariables(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	buildVars := make(map[string]string, len(items))
	for _, item := range items {
		key, value, found := strings.Cut(item, "=")
		if !found {
			return nil, fmt.Errorf("invalid --var %q, expected KEY=value", item)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("invalid --var %q, key must not be empty", item)
		}
		if !strings.HasPrefix(key, "BUILDCTL_BATCH_") {
			return nil, fmt.Errorf("invalid --var %q, key must start with BUILDCTL_BATCH_", item)
		}
		buildVars[key] = value
	}
	return buildVars, nil
}

func findUnresolvedBuildVariables(content string) []string {
	matches := buildctlBatchVarPattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			result = append(result, match[1])
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	last := ""
	for _, value := range values {
		if value == last {
			continue
		}
		result = append(result, value)
		last = value
	}
	return result
}

func loadImageMetadata(metaPath string) (imageMetadata, error) {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("read %s: %w", metaPath, err)
	}
	var meta imageMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return imageMetadata{}, fmt.Errorf("invalid %s: %w", metaPath, err)
	}
	meta.Target = strings.TrimSpace(meta.Target)
	if meta.Target == "" {
		return imageMetadata{}, fmt.Errorf("%s has empty target", metaPath)
	}
	return meta, nil
}

func copyDirectory(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, relPath)

		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}
		if d.Type()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, targetPath)
		}
		return copyFile(path, targetPath, info.Mode())
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
