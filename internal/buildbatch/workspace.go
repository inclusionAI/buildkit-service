package buildbatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"
)

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
