package buildbatch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const nydusV3TargetSuffix = "_nydus_v3"

type buildSpec struct {
	dir         string
	target      string
	scheduleKey string
	oci         bool
}

type buildJob struct {
	key         string
	scheduleKey string
	specs       []buildSpec
}

func loadBuildSpecs(imageDirs, target string, buildModes []bool, buildVars map[string]string) ([]buildSpec, func(), error) {
	if imageDirs == "" && target == "" {
		return nil, nil, fmt.Errorf("either --image-dirs or a positional [target] is required")
	}
	if len(buildModes) == 0 {
		return nil, nil, fmt.Errorf("at least one build mode is required")
	}

	if imageDirs == "" {
		specs := make([]buildSpec, 0, len(buildModes))
		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(target, oci)
			specs = append(specs, buildSpec{
				target:      normalizedTarget,
				scheduleKey: stripNydusV3Suffix(normalizedTarget),
				oci:         oci,
			})
		}
		sort.Slice(specs, func(i, j int) bool {
			return specs[i].target < specs[j].target
		})
		return specs, nil, nil
	}

	targetFilters := make(map[string]struct{}, len(buildModes))
	if strings.TrimSpace(target) != "" {
		for _, oci := range buildModes {
			targetFilters[normalizeTargetForMode(target, oci)] = struct{}{}
		}
	}
	preparedRoot, scheduleKeys, err := prepareBuildImageDirs(imageDirs, buildVars)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(preparedRoot)
	}

	entries, err := os.ReadDir(preparedRoot)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		cleanup()
		return nil, nil, fmt.Errorf("image-dirs %s does not contain any image directories", imageDirs)
	}

	var specs []buildSpec
	for _, e := range entries {
		if !e.IsDir() {
			cleanup()
			return nil, nil, fmt.Errorf("image-dirs %s must contain only directories, found %s", imageDirs, e.Name())
		}
		dir := filepath.Join(preparedRoot, e.Name())
		metaPath := filepath.Join(dir, "metadata.json")

		meta, err := loadImageMetadata(metaPath)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if meta.Target == "" {
			cleanup()
			return nil, nil, fmt.Errorf("%s has empty target in metadata.json", dir)
		}
		scheduleKey := scheduleKeys[e.Name()]
		if scheduleKey == "" {
			cleanup()
			return nil, nil, fmt.Errorf("missing source context scheduling hash for %s", dir)
		}

		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(meta.Target, oci)

			if len(targetFilters) > 0 {
				if _, ok := targetFilters[normalizedTarget]; !ok {
					continue
				}
			}

			specs = append(specs, buildSpec{
				dir:         dir,
				target:      normalizedTarget,
				scheduleKey: scheduleKey,
				oci:         oci,
			})
		}
	}

	sort.Slice(specs, func(i, j int) bool {
		return specs[i].target < specs[j].target
	})
	return specs, cleanup, nil
}

func groupBuildSpecs(specs []buildSpec) []buildJob {
	if len(specs) == 0 {
		return nil
	}

	grouped := make(map[string][]buildSpec, len(specs))
	for _, spec := range specs {
		key := stripNydusV3Suffix(spec.target)
		grouped[key] = append(grouped[key], spec)
	}

	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	jobs := make([]buildJob, 0, len(keys))
	for _, key := range keys {
		jobSpecs := append([]buildSpec(nil), grouped[key]...)
		sort.SliceStable(jobSpecs, func(i, j int) bool {
			if jobSpecs[i].oci != jobSpecs[j].oci {
				return !jobSpecs[i].oci
			}
			return jobSpecs[i].target < jobSpecs[j].target
		})
		scheduleKey := key
		if len(jobSpecs) > 0 && strings.TrimSpace(jobSpecs[0].scheduleKey) != "" {
			scheduleKey = jobSpecs[0].scheduleKey
		}
		jobs = append(jobs, buildJob{key: key, scheduleKey: scheduleKey, specs: jobSpecs})
	}

	return jobs
}

func filterBuildJobs(jobs []buildJob, results buildResultReader, skipFail bool) ([]buildJob, map[string]buildOutcomeState, int, int, error) {
	existingOutcomes := make(map[string]buildOutcomeState, len(jobs))
	filtered := make([]buildJob, 0, len(jobs))
	var skippedSucceeded, skippedFailed int

	for _, job := range jobs {
		pendingSpecs := make([]buildSpec, 0, len(job.specs))
		allSucceeded := len(job.specs) > 0
		anyFailed := false

		for _, spec := range job.specs {
			entry, found, err := results.Get(spec.target)
			if err != nil {
				return nil, nil, 0, 0, err
			}
			if !found {
				allSucceeded = false
				pendingSpecs = append(pendingSpecs, spec)
				continue
			}
			if entry.Success {
				continue
			}

			allSucceeded = false
			anyFailed = true
			if !skipFail {
				pendingSpecs = append(pendingSpecs, spec)
			}
		}

		if allSucceeded {
			existingOutcomes[job.key] = buildOutcomeSucceeded
			skippedSucceeded++
			continue
		}
		if anyFailed && skipFail {
			existingOutcomes[job.key] = buildOutcomeFailed
			skippedFailed++
			continue
		}

		filtered = append(filtered, buildJob{key: job.key, scheduleKey: job.scheduleKey, specs: pendingSpecs})
	}

	return filtered, existingOutcomes, skippedSucceeded, skippedFailed, nil
}

func hasNydusV3Suffix(target string) bool {
	return strings.HasSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func ensureNydusV3Suffix(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || hasNydusV3Suffix(trimmed) {
		return trimmed
	}
	return trimmed + nydusV3TargetSuffix
}

func stripNydusV3Suffix(target string) string {
	return strings.TrimSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func normalizeTargetForMode(target string, oci bool) string {
	if oci {
		return stripNydusV3Suffix(target)
	}
	return ensureNydusV3Suffix(target)
}

func resolveBuildModes(oci, bothFormats bool) ([]bool, error) {
	if oci && bothFormats {
		return nil, fmt.Errorf("--oci and --both-formats are mutually exclusive")
	}
	if bothFormats {
		return []bool{false, true}, nil
	}
	if oci {
		return []bool{true}, nil
	}
	return []bool{false}, nil
}

func targetMatchesMode(target string, oci bool) bool {
	if oci {
		return !hasNydusV3Suffix(target)
	}
	return hasNydusV3Suffix(target)
}
