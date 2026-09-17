package buildbatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBuildModesRejectsMutuallyExclusiveFlags(t *testing.T) {
	if _, err := resolveBuildModes(true, true); err == nil {
		t.Fatal("expected mutually exclusive flags to fail")
	}
}

func TestGroupBuildSpecsOrdersNydusBeforeOci(t *testing.T) {
	jobs := groupBuildSpecs([]buildSpec{
		{target: "example.com/ns/repo:tag", oci: true},
		{target: "example.com/ns/repo:tag_nydus_v3"},
	})
	if len(jobs) != 1 {
		t.Fatalf("expected 1 grouped job, got %d", len(jobs))
	}
	if jobs[0].key != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected job key %q", jobs[0].key)
	}
	if len(jobs[0].specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(jobs[0].specs))
	}
	if jobs[0].specs[0].oci {
		t.Fatalf("expected nydus spec first, got %#v", jobs[0].specs)
	}
	if !jobs[0].specs[1].oci {
		t.Fatalf("expected OCI spec second, got %#v", jobs[0].specs)
	}
}

func TestFilterBuildJobsResumesIncompleteBothFormatsTask(t *testing.T) {
	jobs := []buildJob{{
		key: "example.com/ns/repo:tag",
		specs: []buildSpec{
			{target: "example.com/ns/repo:tag_nydus_v3"},
			{target: "example.com/ns/repo:tag", oci: true},
		},
	}}

	filtered, existing, skippedSucceeded, skippedFailed, err := filterBuildJobs(jobs, stubBuildResultReader{entries: map[string]resultEntry{
		"example.com/ns/repo:tag_nydus_v3": {Target: "example.com/ns/repo:tag_nydus_v3", Success: true},
	}}, false)
	if err != nil {
		t.Fatalf("filter jobs: %v", err)
	}
	if skippedSucceeded != 0 || skippedFailed != 0 {
		t.Fatalf("unexpected skipped counts: success=%d failed=%d", skippedSucceeded, skippedFailed)
	}
	if len(existing) != 0 {
		t.Fatalf("expected no completed task outcomes, got %#v", existing)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered job, got %d", len(filtered))
	}
	if len(filtered[0].specs) != 1 || !filtered[0].specs[0].oci {
		t.Fatalf("expected only pending OCI spec, got %#v", filtered[0].specs)
	}
}

func TestFilterBuildJobsTreatsBothFormatsAsSingleSuccess(t *testing.T) {
	jobs := []buildJob{{
		key: "example.com/ns/repo:tag",
		specs: []buildSpec{
			{target: "example.com/ns/repo:tag_nydus_v3"},
			{target: "example.com/ns/repo:tag", oci: true},
		},
	}}

	filtered, existing, skippedSucceeded, skippedFailed, err := filterBuildJobs(jobs, stubBuildResultReader{entries: map[string]resultEntry{
		"example.com/ns/repo:tag_nydus_v3": {Target: "example.com/ns/repo:tag_nydus_v3", Success: true},
		"example.com/ns/repo:tag":          {Target: "example.com/ns/repo:tag", Success: true},
	}}, false)
	if err != nil {
		t.Fatalf("filter jobs: %v", err)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected no pending jobs, got %#v", filtered)
	}
	if skippedSucceeded != 1 || skippedFailed != 0 {
		t.Fatalf("unexpected skipped counts: success=%d failed=%d", skippedSucceeded, skippedFailed)
	}
	if existing["example.com/ns/repo:tag"] != buildOutcomeSucceeded {
		t.Fatalf("expected completed task state, got %#v", existing)
	}
}

func TestFilterBuildJobsCombinesSuccessfulFailedAndPendingHistory(t *testing.T) {
	jobs := []buildJob{
		{key: "target-a", specs: []buildSpec{{target: "target-a"}}},
		{key: "target-b", specs: []buildSpec{{target: "target-b"}}},
		{key: "target-c", specs: []buildSpec{{target: "target-c"}}},
	}
	filtered, existing, skippedSucceeded, skippedFailed, err := filterBuildJobs(jobs, stubBuildResultReader{entries: map[string]resultEntry{
		"target-a": {Target: "target-a", Success: true},
		"target-b": {Target: "target-b", Success: false},
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if skippedSucceeded != 1 || skippedFailed != 1 {
		t.Fatalf("skipped success=%d failure=%d", skippedSucceeded, skippedFailed)
	}
	if existing["target-a"] != buildOutcomeSucceeded || existing["target-b"] != buildOutcomeFailed {
		t.Fatalf("existing outcomes = %#v", existing)
	}
	if len(filtered) != 1 || filtered[0].key != "target-c" {
		t.Fatalf("pending jobs = %#v", filtered)
	}
}

func TestLoadBuildSpecsExpandsBothFormatsFromImageDirs(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "image-a")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "metadata.json"), []byte(`{"target":"example.com/ns/repo:tag"}`), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	specs, cleanup, err := loadBuildSpecs(root, "", []bool{false, true}, nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("load specs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected 2 specs, got %d", len(specs))
	}

	jobs := groupBuildSpecs(specs)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 grouped job, got %d", len(jobs))
	}
	if !strings.HasPrefix(jobs[0].scheduleKey, "source:") {
		t.Fatalf("expected source context hash schedule key, got %q", jobs[0].scheduleKey)
	}
	if jobs[0].scheduleKey == jobs[0].key {
		t.Fatalf("expected schedule key to differ from target key, got %q", jobs[0].scheduleKey)
	}
	if len(jobs[0].specs) != 2 || jobs[0].specs[0].oci || !jobs[0].specs[1].oci {
		t.Fatalf("expected grouped nydus->oci ordering, got %#v", jobs[0].specs)
	}

	got := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if spec.dir == "" {
			t.Fatal("expected prepared build directory")
		}
		got[spec.target] = spec.oci
	}

	if oci, ok := got["example.com/ns/repo:tag"]; !ok || !oci {
		t.Fatalf("expected OCI target, got %#v", got)
	}
	if oci, ok := got["example.com/ns/repo:tag_nydus_v3"]; !ok || oci {
		t.Fatalf("expected nydus target, got %#v", got)
	}
}

func TestLoadBuildSpecsUsesSourceContextHashForScheduleKey(t *testing.T) {
	root := t.TempDir()
	for _, item := range []struct {
		name   string
		target string
		data   string
	}{
		{name: "image-a", target: "example.com/ns/repo:tag-a", data: "alpha\n"},
		{name: "image-b", target: "example.com/ns/repo:tag-b", data: "beta\n"},
	} {
		imageDir := filepath.Join(root, item.name)
		if err := os.MkdirAll(imageDir, 0o755); err != nil {
			t.Fatalf("mkdir image dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatalf("write Dockerfile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(imageDir, "metadata.json"), []byte(`{"target":"`+item.target+`"}`), 0o644); err != nil {
			t.Fatalf("write metadata.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(imageDir, "context.txt"), []byte(item.data), 0o644); err != nil {
			t.Fatalf("write context.txt: %v", err)
		}
	}

	specs, cleanup, err := loadBuildSpecs(root, "", []bool{false}, nil)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("load specs: %v", err)
	}
	jobs := groupBuildSpecs(specs)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 grouped jobs, got %d", len(jobs))
	}
	if jobs[0].key == jobs[1].key {
		t.Fatalf("expected distinct target keys, got %q", jobs[0].key)
	}
	if jobs[0].scheduleKey == "" || jobs[0].scheduleKey == jobs[1].scheduleKey {
		t.Fatalf("expected different source context schedule keys, got %q and %q", jobs[0].scheduleKey, jobs[1].scheduleKey)
	}
}
