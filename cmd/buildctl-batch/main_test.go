package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stubBuildResultWriter struct {
	err error
}

func (s stubBuildResultWriter) UpsertResult(entry resultEntry) error {
	return s.err
}

type stubBuildResultReader struct {
	entries map[string]resultEntry
	err     error
}

func (s stubBuildResultReader) Get(target string) (resultEntry, bool, error) {
	if s.err != nil {
		return resultEntry{}, false, s.err
	}
	entry, ok := s.entries[target]
	return entry, ok, nil
}

func TestPickAvailableAddrSlotFallsBackWhenPrimaryBusy(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{
		{addr: "tcp://10.0.0.1:9094"},
		{addr: "tcp://10.0.0.2:9094"},
		{addr: "tcp://10.0.0.3:9094"},
	}, 1)
	snapshot := pool.snapshot()
	key := "target-a"

	primary := pickAddrSlot(snapshot, key)
	if primary == nil {
		t.Fatal("expected a primary slot")
	}

	if !tryAcquireAddrSlot(primary.sem) {
		t.Fatal("expected to occupy the primary slot")
	}
	defer func() { <-primary.sem }()

	fallback := pickAvailableAddrSlot(snapshot, key)
	if fallback == nil {
		t.Fatal("expected a fallback slot")
	}
	defer func() { <-fallback.sem }()

	if fallback == primary {
		t.Fatal("expected scheduler to skip the busy primary slot")
	}
}

func TestPickAvailableAddrSlotReturnsNilWhenAllSlotsBusy(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{
		{addr: "tcp://10.0.0.1:9094"},
		{addr: "tcp://10.0.0.2:9094"},
	}, 1)
	snapshot := pool.snapshot()

	for _, slot := range snapshot.slots {
		if !tryAcquireAddrSlot(slot.sem) {
			t.Fatal("expected to occupy slot")
		}
		defer func(slot *addrSlot) { <-slot.sem }(slot)
	}

	if slot := pickAvailableAddrSlot(snapshot, "target-b"); slot != nil {
		t.Fatal("expected no available slot when every endpoint is busy")
	}
}

func TestResolvePprofServerAddrPrefersFlagThenEnv(t *testing.T) {
	t.Setenv(pprofServerEnv, "127.0.0.1:6061")

	if addr := resolvePprofServerAddr(""); addr != "127.0.0.1:6061" {
		t.Fatalf("expected env fallback, got %q", addr)
	}
	if addr := resolvePprofServerAddr(" 0.0.0.0:6060 "); addr != "0.0.0.0:6060" {
		t.Fatalf("expected trimmed flag value, got %q", addr)
	}
}

func TestPersistBuildResultAppliesCounters(t *testing.T) {
	counters := newBuildOutcomeCounters(3, map[string]buildOutcomeState{})
	entry := resultEntry{Target: "target-a", Success: true}

	succeeded, total, failed, err := persistBuildResult(stubBuildResultWriter{}, counters, entry)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if succeeded != 1 || total != 3 || failed != 0 {
		t.Fatalf("unexpected counters: success=%d total=%d failed=%d", succeeded, total, failed)
	}
}

func TestPersistBuildResultReturnsErrorOnStoreFailure(t *testing.T) {
	counters := newBuildOutcomeCounters(1, map[string]buildOutcomeState{})
	entry := resultEntry{Target: "target-b", Success: true}

	_, _, _, err := persistBuildResult(stubBuildResultWriter{err: errors.New("boom")}, counters, entry)
	if err == nil {
		t.Fatal("expected store error")
	}
	if succeeded, total, failed := counters.snapshot(); succeeded != 0 || total != 1 || failed != 0 {
		t.Fatalf("counters changed unexpectedly: success=%d total=%d failed=%d", succeeded, total, failed)
	}
}

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
}

func TestReadFileTailTruncatesLargeLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	logs, err := readFileTail(path, 4)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !strings.Contains(logs, "truncated") {
		t.Fatalf("expected truncation marker, got %q", logs)
	}
	if !strings.HasSuffix(logs, "6789") {
		t.Fatalf("expected tail bytes, got %q", logs)
	}
}

func TestBuildOutputCaptureBoundsAndKeepsTail(t *testing.T) {
	capture, err := newBuildOutputCapture()
	if err != nil {
		t.Fatal(err)
	}
	prefix := bytes.Repeat([]byte("a"), int(maxBuildLogBytes))
	tail := []byte("expected-tail")
	if _, err := capture.Write(append(prefix, tail...)); err != nil {
		t.Fatal(err)
	}
	if len(capture.buffer) != int(maxBuildLogBytes) {
		t.Fatalf("capture allocated %d bytes, want %d", len(capture.buffer), maxBuildLogBytes)
	}
	logs, err := capture.closeAndReadTail(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "truncated") || !strings.HasSuffix(logs, string(tail)) {
		t.Fatalf("expected bounded tail with marker, got suffix %q", logs[len(logs)-64:])
	}
}

func TestBuildCommandArgsUsesSpecCompression(t *testing.T) {
	addr := &buildkitAddr{addr: "tcp://127.0.0.1:1234"}

	nydusArgs := strings.Join(buildCommandArgs(buildSpec{target: "example.com/ns/repo:tag_nydus_v3"}, addr), " ")
	if !strings.Contains(nydusArgs, "compression=nydus") {
		t.Fatalf("expected nydus compression, got %q", nydusArgs)
	}
	if strings.Contains(nydusArgs, "compression=gzip") {
		t.Fatalf("did not expect gzip compression in nydus args: %q", nydusArgs)
	}

	ociArgs := strings.Join(buildCommandArgs(buildSpec{target: "example.com/ns/repo:tag", oci: true}, addr), " ")
	if !strings.Contains(ociArgs, "compression=gzip") {
		t.Fatalf("expected gzip compression, got %q", ociArgs)
	}
	if strings.Contains(ociArgs, "compression=nydus") {
		t.Fatalf("did not expect nydus compression in oci args: %q", ociArgs)
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
	prepared, _, err := prepareBuildImageDirs(root, nil)
	if err == nil || !strings.Contains(err.Error(), "line 2:") || !strings.Contains(err.Error(), "COPY") || prepared != "" {
		t.Fatalf("expected preparation to fail before build: prepared=%q err=%v", prepared, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("source context modified on rejection: %v", err)
	}
}
