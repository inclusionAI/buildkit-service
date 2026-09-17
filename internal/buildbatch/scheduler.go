package buildbatch

import (
	"context"
	"sort"
	"sync"
	"time"
)

const defaultBuildkitOOMCooldown = 2 * time.Minute

// buildkitAddr represents a single resolved buildkitd endpoint.
type buildkitAddr struct {
	original      string
	addr          string
	nodeIP        string
	cooldown      time.Duration
	mu            sync.Mutex
	cooldownUntil time.Time
}

type addrSlot struct {
	addr *buildkitAddr
	sem  chan struct{}
}

type addrPoolSnapshot struct {
	slots []*addrSlot
	hash  *consistentHash
}

type addrPool struct {
	mu          sync.RWMutex
	concurrency int
	slots       []*addrSlot
	hash        *consistentHash
}

func (a *buildkitAddr) isInCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownUntil)
}

func (a *buildkitAddr) setCooldown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = time.Now().Add(a.cooldown)
}

func newAddrPool(addrs []*buildkitAddr, concurrency int) *addrPool {
	p := &addrPool{concurrency: concurrency}
	p.replace(addrs)
	return p
}

func (p *addrPool) snapshot() addrPoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	slots := append([]*addrSlot(nil), p.slots...)
	return addrPoolSnapshot{slots: slots, hash: p.hash}
}

func (p *addrPool) addresses() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slotAddressKeys(p.slots)
}

func (p *addrPool) replace(addrs []*buildkitAddr) {
	p.mu.Lock()
	defer p.mu.Unlock()

	existing := make(map[string]*addrSlot, len(p.slots))
	for _, slot := range p.slots {
		if slot != nil && slot.addr != nil {
			existing[slot.addr.addr] = slot
		}
	}

	newSlots := make([]*addrSlot, 0, len(addrs))
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		if slot, ok := existing[addr.addr]; ok {
			newSlots = append(newSlots, slot)
			continue
		}
		newSlots = append(newSlots, &addrSlot{
			addr: addr,
			sem:  make(chan struct{}, p.concurrency),
		})
	}

	sort.Slice(newSlots, func(i, j int) bool {
		return newSlots[i].addr.addr < newSlots[j].addr.addr
	})

	p.slots = newSlots
	if len(newSlots) == 0 {
		p.hash = nil
		return
	}
	p.hash = newConsistentHash(len(newSlots), 150)
}
func pickAddrSlot(snapshot addrPoolSnapshot, key string) *addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}

	order := snapshot.hash.getSlotOrder(key)
	if len(order) == 0 {
		return nil
	}

	var fallback *addrSlot
	for _, idx := range order {
		if idx < 0 || idx >= len(snapshot.slots) {
			continue
		}
		slot := snapshot.slots[idx]
		if slot == nil || slot.addr == nil {
			continue
		}
		if fallback == nil {
			fallback = slot
		}
		if !slot.addr.isInCooldown() {
			return slot
		}
	}

	return fallback
}

func pickAvailableAddrSlot(snapshot addrPoolSnapshot, key string) *addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}

	order := snapshot.hash.getSlotOrder(key)
	if len(order) == 0 {
		return nil
	}

	for _, requireReady := range []bool{true, false} {
		for _, idx := range order {
			if idx < 0 || idx >= len(snapshot.slots) {
				continue
			}
			slot := snapshot.slots[idx]
			if slot == nil || slot.addr == nil {
				continue
			}
			if requireReady && slot.addr.isInCooldown() {
				continue
			}
			if !tryAcquireAddrSlot(slot.sem) {
				continue
			}
			return slot
		}
	}

	return nil
}

func tryAcquireAddrSlot(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func waitForAvailableAddrSlot(ctx context.Context, pool *addrPool, key string) *addrSlot {
	for {
		if ctx.Err() != nil {
			return nil
		}

		snapshot := pool.snapshot()
		slot := pickAvailableAddrSlot(snapshot, key)
		if slot != nil {
			return slot
		}

		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
	}
}

func acquireGlobalSlotOrReleaseWorker(ctx context.Context, globalSem chan struct{}, slot *addrSlot) bool {
	select {
	case globalSem <- struct{}{}:
		return true
	case <-ctx.Done():
		<-slot.sem
		return false
	}
}
func slotAddressKeys(slots []*addrSlot) []string {
	keys := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot == nil || slot.addr == nil {
			continue
		}
		keys = append(keys, slot.addr.addr)
	}
	return keys
}
