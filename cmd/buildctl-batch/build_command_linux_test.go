//go:build linux && cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildctlLifecycleHelperProcess(t *testing.T) {
	if os.Getenv("BUILDCTL_BATCH_HELPER_PROCESS") != "1" {
		return
	}

	args := strings.Join(os.Args, " ")
	appendHelperEvent(os.Getenv("BUILDCTL_BATCH_HELPER_EVENTS"), args)
	switch os.Getenv("BUILDCTL_BATCH_HELPER_BEHAVIOR") {
	case "success":
		fmt.Fprintln(os.Stdout, "build succeeded")
	case "fail":
		fmt.Fprintln(os.Stderr, "ordinary build failure")
		os.Exit(7)
	case "oom":
		fmt.Fprintln(os.Stderr, "dial tcp: connection refused")
		os.Exit(7)
	case "retry-once":
		attempt := incrementHelperCounter(os.Getenv("BUILDCTL_BATCH_HELPER_COUNTER"))
		if attempt == 1 {
			fmt.Fprintln(os.Stderr, "first attempt failed")
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, "retry succeeded")
	case "fail-first-target":
		if strings.Contains(args, "name=example.com/team/image:00,") {
			fmt.Fprintln(os.Stderr, "first target failed")
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, "build succeeded")
	case "oom-first-target":
		if strings.Contains(args, "name=example.com/team/image:00,") {
			fmt.Fprintln(os.Stderr, "dial tcp: connection refused")
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, "build succeeded")
	case "fail-fast-concurrent":
		if strings.Contains(args, "name=example.com/team/image:00,") {
			waitForHelperEvent("name=example.com/team/image:01,")
			fmt.Fprintln(os.Stderr, "first target failed")
			os.Exit(7)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "block":
		for {
			time.Sleep(time.Hour)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown helper behavior %q\n", os.Getenv("BUILDCTL_BATCH_HELPER_BEHAVIOR"))
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRunBuildCommandRetriesReleasesSlotsAndPersistsFinalResult(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "retry-once")
	root := t.TempDir()
	target := "example.com/team/image:retry"
	opts := lifecycleBuildOptions(t, root, target)
	opts.retry = 1

	if err := runBuildCommand(opts); err != nil {
		t.Fatalf("run build command: %v", err)
	}
	if got := len(readHelperEvents(t, events)); got != 2 {
		t.Fatalf("buildctl invocation count = %d, want 2", got)
	}

	entry := readLifecycleResult(t, opts.resultPath, target)
	if !entry.Success || entry.Target != target {
		t.Fatalf("final result = %#v, want successful retry", entry)
	}
	logs, err := loadLatestFailureLogs(opts.logsPath, map[string]struct{}{target: {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs[target], "first attempt failed") {
		t.Fatalf("retry failure log not preserved: %#v", logs)
	}
}

func TestRunBuildCommandPersistsFinalFailureAfterRetriesExhausted(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "fail")
	root := t.TempDir()
	target := "example.com/team/image:failed"
	opts := lifecycleBuildOptions(t, root, target)
	opts.retry = 2

	err := runBuildCommand(opts)
	if err == nil || err.Error() != "one or more builds failed" {
		t.Fatalf("failed build returned %v", err)
	}
	if got := len(readHelperEvents(t, events)); got != 3 {
		t.Fatalf("buildctl invocation count = %d, want 3", got)
	}
	entry := readLifecycleResult(t, opts.resultPath, target)
	if entry.Success || !strings.Contains(entry.Reason, "exit status 7") {
		t.Fatalf("final result = %#v, want exhausted failure", entry)
	}
	logs, err := loadLatestFailureLogs(opts.logsPath, map[string]struct{}{target: {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs[target], "ordinary build failure") {
		t.Fatalf("final failure log not preserved: %#v", logs)
	}
}

func TestRunBuildCommandCancellationStopsBuildAndClosesStore(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "block")
	root := t.TempDir()
	target := "example.com/team/image:cancel"
	opts := lifecycleBuildOptions(t, root, target)
	ctx, cancel := context.WithCancel(context.Background())
	opts.ctx = ctx
	done := make(chan error, 1)
	go func() { done <- runBuildCommand(opts) }()

	waitForHelperEvents(t, events, 1)
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "one or more builds failed" {
			t.Fatalf("canceled build returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("build command did not exit after context cancellation")
	}

	entry := readLifecycleResult(t, opts.resultPath, target)
	if entry.Success || entry.Reason == "" {
		t.Fatalf("canceled result = %#v", entry)
	}
}

func TestRunBuildCommandTimeoutPersistsFailure(t *testing.T) {
	installBuildctlLifecycleHelper(t, "block")
	root := t.TempDir()
	target := "example.com/team/image:timeout"
	opts := lifecycleBuildOptions(t, root, target)
	opts.timeout = 1

	started := time.Now()
	err := runBuildCommand(opts)
	if err == nil || err.Error() != "one or more builds failed" {
		t.Fatalf("timed-out build returned %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("timeout elapsed %s, want approximately one second", elapsed)
	}
	entry := readLifecycleResult(t, opts.resultPath, target)
	if entry.Success || entry.Reason == "" {
		t.Fatalf("timeout result = %#v", entry)
	}
}

func TestRunBuildCommandStreamingFailFastStopsSchedulingAndCleansWorkspace(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "fail-first-target")
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeBuildImageDir(t, root, fmt.Sprintf("image-%02d", i), fmt.Sprintf("example.com/team/image:%02d", i))
	}
	before := lifecycleWorkspaceSet(t)
	stateRoot := t.TempDir()
	opts := lifecycleBuildOptions(t, stateRoot, "")
	opts.imageDirs = root
	opts.failfast = true

	err := runBuildCommand(opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fail-fast build returned %v", err)
	}
	if got := len(readHelperEvents(t, events)); got != 1 {
		t.Fatalf("fail-fast invoked buildctl %d times, want 1", got)
	}
	if leaked := addedLifecycleWorkspaces(before, lifecycleWorkspaceSet(t)); len(leaked) != 0 {
		t.Fatalf("streaming workspace leaked: %v", leaked)
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("image-%02d", i), "Dockerfile")); err != nil {
			t.Fatalf("source workspace changed: %v", err)
		}
	}
}

func TestRunBuildCommandFailFastCancelsStartedBuilds(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "fail-fast-concurrent")
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		writeBuildImageDir(t, root, fmt.Sprintf("image-%02d", i), fmt.Sprintf("example.com/team/image:%02d", i))
	}
	stateRoot := t.TempDir()
	opts := lifecycleBuildOptions(t, stateRoot, "")
	opts.imageDirs = root
	opts.failfast = true
	opts.concurrency = 2

	err := runBuildCommand(opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent fail-fast build returned %v", err)
	}
	invocations := readHelperEvents(t, events)
	if len(invocations) != 2 {
		t.Fatalf("fail-fast invoked buildctl %d times, want 2 already-started builds", len(invocations))
	}
	for _, invocation := range invocations {
		if strings.Contains(invocation, "name=example.com/team/image:02,") {
			t.Fatalf("fail-fast scheduled pending target: %s", invocation)
		}
	}
	if entry := readLifecycleResult(t, opts.resultPath, "example.com/team/image:00"); entry.Success {
		t.Fatalf("triggering result = %#v, want failure", entry)
	}
	if entry := readLifecycleResult(t, opts.resultPath, "example.com/team/image:01"); entry.Success {
		t.Fatalf("canceled in-flight result = %#v, want failure", entry)
	}
}

func TestRunBuildCommandStreamingReleasesSlotAfterOOMFailure(t *testing.T) {
	events := installBuildctlLifecycleHelper(t, "oom-first-target")
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		writeBuildImageDir(t, root, fmt.Sprintf("image-%02d", i), fmt.Sprintf("example.com/team/image:%02d", i))
	}
	stateRoot := t.TempDir()
	opts := lifecycleBuildOptions(t, stateRoot, "")
	opts.imageDirs = root
	opts.addrs[0].cooldown = time.Minute

	err := runBuildCommand(opts)
	if err == nil || err.Error() != "one or more builds failed" {
		t.Fatalf("mixed streaming build returned %v", err)
	}
	if got := len(readHelperEvents(t, events)); got != 3 {
		t.Fatalf("buildctl invocation count = %d, want 3", got)
	}
	if !opts.addrs[0].isInCooldown() {
		t.Fatal("OOM-style failure did not place address in cooldown")
	}
	if entry := readLifecycleResult(t, opts.resultPath, "example.com/team/image:00"); entry.Success {
		t.Fatalf("first result = %#v, want failure", entry)
	}
	if entry := readLifecycleResult(t, opts.resultPath, "example.com/team/image:01"); !entry.Success {
		t.Fatalf("second result = %#v, want success after slot release", entry)
	}
	if entry := readLifecycleResult(t, opts.resultPath, "example.com/team/image:02"); !entry.Success {
		t.Fatalf("third result = %#v, want success after successful worker release", entry)
	}
}

func TestExecuteBuildFailureCooldownClassification(t *testing.T) {
	for _, test := range []struct {
		name         string
		behavior     string
		wantCooldown bool
	}{
		{name: "ordinary failure", behavior: "fail"},
		{name: "connection refused", behavior: "oom", wantCooldown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			installBuildctlLifecycleHelper(t, test.behavior)
			addr := &buildkitAddr{addr: "tcp://127.0.0.1:9094", cooldown: time.Minute}
			entry := executeBuild(context.Background(), buildSpec{target: "example.com/team/image:test", oci: true}, addr, options{})
			if entry.Success {
				t.Fatalf("failed helper produced successful result: %#v", entry)
			}
			if got := addr.isInCooldown(); got != test.wantCooldown {
				t.Fatalf("cooldown = %v, want %v", got, test.wantCooldown)
			}
		})
	}
}

func lifecycleBuildOptions(t *testing.T, root, target string) options {
	t.Helper()
	return options{
		addrs:       []*buildkitAddr{{addr: "tcp://127.0.0.1:9094", nodeIP: "127.0.0.1", cooldown: time.Millisecond}},
		concurrency: 1,
		ctx:         context.Background(),
		oci:         true,
		resultPath:  filepath.Join(root, "result.lmdb"),
		logsPath:    filepath.Join(root, "logs.jsonl"),
		target:      target,
	}
}

func installBuildctlLifecycleHelper(t *testing.T, behavior string) string {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "buildctl")
	content := "#!/bin/sh\nexec \"$BUILDCTL_BATCH_TEST_BINARY\" -test.run=^TestBuildctlLifecycleHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(dir, "events.log")
	t.Setenv("BUILDCTL_BATCH_HELPER_PROCESS", "1")
	t.Setenv("BUILDCTL_BATCH_HELPER_BEHAVIOR", behavior)
	t.Setenv("BUILDCTL_BATCH_HELPER_EVENTS", events)
	t.Setenv("BUILDCTL_BATCH_HELPER_COUNTER", filepath.Join(dir, "counter"))
	t.Setenv("BUILDCTL_BATCH_TEST_BINARY", executable)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return events
}

func appendHelperEvent(path, event string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(file, event); err != nil {
		_ = file.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := file.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func incrementHelperCounter(path string) int {
	raw, _ := os.ReadFile(path)
	value, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	value++
	if err := os.WriteFile(path, []byte(strconv.Itoa(value)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return value
}

func waitForHelperEvent(fragment string) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, _ := os.ReadFile(os.Getenv("BUILDCTL_BATCH_HELPER_EVENTS"))
		if strings.Contains(string(raw), fragment) {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "timed out waiting for helper event %q\n", fragment)
			os.Exit(2)
		}
		time.Sleep(time.Millisecond)
	}
}

func readHelperEvents(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func waitForHelperEvents(t *testing.T, path string, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(readHelperEvents(t, path)) < count {
		if time.Now().After(deadline) {
			t.Fatalf("helper recorded fewer than %d event(s)", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func readLifecycleResult(t *testing.T, path, target string) resultEntry {
	t.Helper()
	db, err := openLMDBResultDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entry, found, err := db.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("result %q not found", target)
	}
	return entry
}

func lifecycleWorkspaceSet(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "buildctl-batch-image-dirs-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func addedLifecycleWorkspaces(before, after []string) []string {
	existing := make(map[string]struct{}, len(before))
	for _, path := range before {
		existing[path] = struct{}{}
	}
	var added []string
	for _, path := range after {
		if _, ok := existing[path]; !ok {
			added = append(added, path)
		}
	}
	return added
}
