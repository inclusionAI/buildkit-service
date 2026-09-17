package main

import (
	"context"
	"testing"
	"time"
)

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
