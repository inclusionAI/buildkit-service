package main

import (
	"testing"
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
