package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultBuildkitOOMCooldown = 2 * time.Minute

const defaultBuildkitAddrRefreshInterval = 15 * time.Second

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

// consistentHash maps keys to node indices using a hash ring with virtual nodes,
// ensuring the same target is consistently scheduled to the same buildkitd address.
type consistentHash struct {
	ring     []uint32
	nodeMap  map[uint32]int
	numNodes int
}

func newConsistentHash(numNodes, replicas int) *consistentHash {
	ch := &consistentHash{
		nodeMap:  make(map[uint32]int),
		numNodes: numNodes,
	}
	for i := 0; i < numNodes; i++ {
		for r := 0; r < replicas; r++ {
			h := fnv1a(fmt.Sprintf("node-%d-replica-%d", i, r))
			ch.ring = append(ch.ring, h)
			ch.nodeMap[h] = i
		}
	}
	sort.Slice(ch.ring, func(a, b int) bool { return ch.ring[a] < ch.ring[b] })
	return ch
}

// getSlotOrder returns node indices in preference order for the given key.
// The first element is the primary; subsequent elements are fallbacks.
func (ch *consistentHash) getSlotOrder(key string) []int {
	if len(ch.ring) == 0 {
		return nil
	}
	h := fnv1a(key)
	idx := sort.Search(len(ch.ring), func(i int) bool { return ch.ring[i] >= h })
	if idx >= len(ch.ring) {
		idx = 0
	}

	seen := make(map[int]bool)
	order := make([]int, 0, ch.numNodes)
	for len(order) < ch.numNodes {
		nodeIdx := ch.nodeMap[ch.ring[idx%len(ch.ring)]]
		if !seen[nodeIdx] {
			seen[nodeIdx] = true
			order = append(order, nodeIdx)
		}
		idx++
	}
	return order
}

func fnv1a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
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

func startBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string, oomCooldown time.Duration) {
	if strings.TrimSpace(addrsRaw) == "" {
		return
	}

	go func() {
		ticker := time.NewTicker(defaultBuildkitAddrRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			refreshedAddrs, err := parseBuildkitAddrs(addrsRaw)
			if err != nil {
				logError("Failed to refresh buildkit addresses from %q: %v", addrsRaw, err)
				continue
			}
			for _, addr := range refreshedAddrs {
				if addr != nil {
					addr.cooldown = oomCooldown
				}
			}

			before := pool.addresses()
			pool.replace(refreshedAddrs)
			after := pool.addresses()
			if !sameStringSlice(before, after) {
				logInfo("Refreshed buildkit address pool: %d -> %d endpoint(s): [%s] -> [%s]",
					len(before), len(after), strings.Join(before, ", "), strings.Join(after, ", "))
			}
		}
	}()
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

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func parseBuildkitAddrs(s string) ([]*buildkitAddr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("--addrs is required")
	}

	parts := strings.Split(s, ",")
	var addrs []*buildkitAddr

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		addr := part
		if !strings.Contains(addr, "://") {
			addr = "tcp://" + addr
		}

		hostPort := strings.TrimPrefix(addr, "tcp://")
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}

		if ip := net.ParseIP(host); ip != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: ip.String()})
			continue
		}

		ips, err := net.LookupHost(host)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}

		for _, ip := range ips {
			resolved := fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, port))
			addrs = append(addrs, &buildkitAddr{original: part, addr: resolved, nodeIP: ip})
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("no valid buildkitd addresses")
	}
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i].addr < addrs[j].addr
	})
	return addrs, nil
}

func nodeIPFromBuildkitAddr(addr string) string {
	hostPort := strings.TrimPrefix(strings.TrimSpace(addr), "tcp://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}
