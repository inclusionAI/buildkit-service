package buildbatch

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseBuildkitAddrsRejectsEmptyInput(t *testing.T) {
	if _, err := parseBuildkitAddrs("  "); err == nil || err.Error() != "--addrs is required" {
		t.Fatalf("empty address error changed: %v", err)
	}
}

func TestParseBuildkitAddrsNormalizesAndSortsLiteralAddresses(t *testing.T) {
	addrs, err := parseBuildkitAddrs("10.0.0.2:9094,tcp://10.0.0.1:9094")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		got = append(got, addr.addr+"|"+addr.nodeIP)
	}
	want := []string{
		"tcp://10.0.0.1:9094|10.0.0.1",
		"tcp://10.0.0.2:9094|10.0.0.2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized addresses changed: got %v, want %v", got, want)
	}
}

func TestParseBuildkitAddrsPreservesMalformedHostPort(t *testing.T) {
	addrs, err := parseBuildkitAddrs("buildkit")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].original != "buildkit" || addrs[0].addr != "tcp://buildkit" || addrs[0].nodeIP != "" {
		t.Fatalf("malformed host:port handling changed: %#v", addrs)
	}
}

func TestParseBuildkitAddrsExpandsLocalhostAndSortsResults(t *testing.T) {
	addrs, err := parseBuildkitAddrs("localhost:9094")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) == 0 {
		t.Fatal("localhost did not resolve to any addresses")
	}
	for idx, addr := range addrs {
		if addr.original != "localhost:9094" || !strings.HasPrefix(addr.addr, "tcp://") || addr.nodeIP == "" {
			t.Fatalf("unexpected localhost expansion: %#v", addr)
		}
		if idx > 0 && addrs[idx-1].addr > addr.addr {
			t.Fatalf("localhost expansion is not sorted: %#v", addrs)
		}
	}
}

func TestConsistentHashOrderIsStableAndUnique(t *testing.T) {
	hash := newConsistentHash(4, 150)
	first := hash.getSlotOrder("source:stable-key")
	second := hash.getSlotOrder("source:stable-key")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("consistent hash order changed between calls: %v then %v", first, second)
	}
	if len(first) != 4 {
		t.Fatalf("slot order contains %d nodes, want 4: %v", len(first), first)
	}
	seen := make(map[int]struct{}, len(first))
	for _, node := range first {
		if _, ok := seen[node]; ok {
			t.Fatalf("slot order contains duplicate node %d: %v", node, first)
		}
		seen[node] = struct{}{}
	}
}

func TestPickAvailableAddrSlotPrefersReadyFallbackOverCooldownPrimary(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{
		{addr: "tcp://10.0.0.1:9094", cooldown: time.Hour},
		{addr: "tcp://10.0.0.2:9094", cooldown: time.Hour},
	}, 1)
	snapshot := pool.snapshot()
	primary := pickAddrSlot(snapshot, "target-cooldown")
	if primary == nil {
		t.Fatal("expected a primary slot")
	}
	primary.addr.setCooldown()

	selected := pickAvailableAddrSlot(snapshot, "target-cooldown")
	if selected == nil {
		t.Fatal("expected a ready fallback slot")
	}
	defer func() { <-selected.sem }()
	if selected == primary {
		t.Fatal("selected cooldown primary while a ready fallback was available")
	}
}

func TestAddrPoolReplaceReusesExistingSlotAndSemaphoreState(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{
		{addr: "tcp://10.0.0.1:9094"},
		{addr: "tcp://10.0.0.2:9094"},
	}, 1)
	before := pool.snapshot()
	existing := before.slots[0]
	if !tryAcquireAddrSlot(existing.sem) {
		t.Fatal("expected to occupy existing slot")
	}
	defer func() { <-existing.sem }()

	pool.replace([]*buildkitAddr{
		{addr: existing.addr.addr},
		{addr: "tcp://10.0.0.3:9094"},
	})
	after := pool.snapshot()
	var reused *addrSlot
	for _, slot := range after.slots {
		if slot.addr.addr == existing.addr.addr {
			reused = slot
		}
		if slot.addr.addr == "tcp://10.0.0.2:9094" {
			t.Fatal("removed address remained in the pool")
		}
	}
	if reused != existing {
		t.Fatalf("existing address did not reuse its slot: before=%p after=%p", existing, reused)
	}
	if len(reused.sem) != 1 {
		t.Fatalf("reused semaphore state changed: len=%d", len(reused.sem))
	}
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

func TestWaitForAvailableAddrSlotStopsOnContextCancellation(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{{addr: "tcp://10.0.0.1:9094"}}, 1)
	slot := pool.snapshot().slots[0]
	if !tryAcquireAddrSlot(slot.sem) {
		t.Fatal("expected to occupy slot")
	}
	t.Cleanup(func() { <-slot.sem })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *addrSlot, 1)
	go func() {
		done <- waitForAvailableAddrSlot(ctx, pool, "target")
	}()
	cancel()

	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("expected cancellation to stop slot wait, got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("slot wait did not stop after context cancellation")
	}
	if len(slot.sem) != 1 {
		t.Fatalf("waiting changed occupied slot count to %d", len(slot.sem))
	}
}

func TestAcquireGlobalSlotOrReleaseWorkerOnCancellation(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{{addr: "tcp://10.0.0.1:9094"}}, 1)
	slot := pickAvailableAddrSlot(pool.snapshot(), "target")
	if slot == nil {
		t.Fatal("expected worker slot")
	}
	globalSem := make(chan struct{}, 1)
	globalSem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- acquireGlobalSlotOrReleaseWorker(ctx, globalSem, slot)
	}()
	cancel()

	select {
	case acquired := <-done:
		if acquired {
			t.Fatal("expected cancellation while waiting for global slot")
		}
	case <-time.After(time.Second):
		t.Fatal("global slot wait did not stop after context cancellation")
	}
	if len(slot.sem) != 0 {
		t.Fatalf("worker slot was not released after cancellation: len=%d", len(slot.sem))
	}
	if len(globalSem) != 1 {
		t.Fatalf("occupied global slot changed unexpectedly: len=%d", len(globalSem))
	}
	<-globalSem
}

func TestBuildkitAddrRefresherStopsOnContextCancellation(t *testing.T) {
	pool := newAddrPool([]*buildkitAddr{{addr: "tcp://10.0.0.1:9094"}}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runBuildkitAddrRefresher(ctx, pool, "127.0.0.1:9094", time.Minute, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for !sameStringSlice(pool.addresses(), []string{"tcp://127.0.0.1:9094"}) {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("address refresher did not update pool: %v", pool.addresses())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("address refresher did not exit after context cancellation")
	}
}
