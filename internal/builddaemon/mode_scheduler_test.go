package builddaemon

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestParseBuildModesLegacyDefault(t *testing.T) {
	modes, err := parseBuildModes("", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	name, cfg, err := modes.resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if name != defaultBuildMode || cfg.RoutingEnabled || modes.explicit {
		t.Fatalf("unexpected legacy mode: name=%q cfg=%#v explicit=%t", name, cfg, modes.explicit)
	}
}

func TestParseBuildModesExplicitAndValidatesGlobalLimit(t *testing.T) {
	raw := `{
		"default_mode":{"concurrency":2,"routingEnabled":false},
		"production_mode":{"concurrency":3,"routingEnabled":true}
	}`
	modes, err := parseBuildModes(raw, defaultBuildMode, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !modes.explicit || !reflect.DeepEqual(modes.names(), []string{"default_mode", "production_mode"}) {
		t.Fatalf("unexpected parsed modes: %#v", modes)
	}
	if _, _, err := modes.resolve("missing"); err == nil {
		t.Fatal("expected unknown mode to fail")
	}
	if _, err := parseBuildModes(raw, defaultBuildMode, 4); err == nil {
		t.Fatal("expected mode concurrency above global limit to fail")
	}
}

func TestBuildModeSchedulerPreservesFIFOWithinMode(t *testing.T) {
	modes, err := parseBuildModes(`{"default_mode":{"concurrency":1,"routingEnabled":false}}`, defaultBuildMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 3)
	release := make(chan struct{}, 3)
	scheduler, err := newBuildModeScheduler(modes, func(id string) {
		started <- id
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.close()

	for _, id := range []string{"first", "second", "third"} {
		if err := scheduler.enqueue(defaultBuildMode, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"first", "second", "third"} {
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("expected %q to start, got %q", want, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
		release <- struct{}{}
	}
}

func TestBuildModeSchedulerRunsModesIndependently(t *testing.T) {
	modes, err := parseBuildModes(`{
		"default_mode":{"concurrency":1,"routingEnabled":false},
		"production_mode":{"concurrency":1,"routingEnabled":true}
	}`, defaultBuildMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 3)
	releases := map[string]chan struct{}{
		"production-first":  make(chan struct{}),
		"production-second": make(chan struct{}),
		"ant":               make(chan struct{}),
	}
	var releaseMu sync.Mutex
	scheduler, err := newBuildModeScheduler(modes, func(id string) {
		started <- id
		releaseMu.Lock()
		release := releases[id]
		releaseMu.Unlock()
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.close()

	if err := scheduler.enqueue("production_mode", "production-first"); err != nil {
		t.Fatal(err)
	}
	if got := receiveStartedTask(t, started); got != "production-first" {
		t.Fatalf("expected first production task, got %q", got)
	}
	if err := scheduler.enqueue("production_mode", "production-second"); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.enqueue(defaultBuildMode, "ant"); err != nil {
		t.Fatal(err)
	}
	if got := receiveStartedTask(t, started); got != "ant" {
		t.Fatalf("expected ant task to start independently, got %q", got)
	}
	select {
	case got := <-started:
		t.Fatalf("production queue exceeded concurrency before release: %q", got)
	case <-time.After(30 * time.Millisecond):
	}

	close(releases["production-first"])
	if got := receiveStartedTask(t, started); got != "production-second" {
		t.Fatalf("expected second production task after release, got %q", got)
	}
	close(releases["production-second"])
	close(releases["ant"])
}

func receiveStartedTask(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case id := <-started:
		return id
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scheduled task")
		return ""
	}
}
