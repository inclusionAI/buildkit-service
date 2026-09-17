package builddaemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// newGlobalSem builds the global build-concurrency semaphore; a limit of 0
// (or below) returns nil, which disables the global cap.
func newGlobalSem(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

// acquireGlobalSlot blocks until a global build slot frees up (keeping the
// task in queued status) or ctx is done. The returned release func must be
// called exactly once; it is a no-op when no global limit is configured.
func (s *buildServer) acquireGlobalSlot(ctx context.Context) (func(), error) {
	if s.globalSem == nil {
		return func() {}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.globalSem <- struct{}{}:
		return func() { <-s.globalSem }, nil
	}
}

func (s *buildServer) acquireAddrSlot(ctx context.Context, key string, excluded map[string]bool) (*addrSlot, error) {
	snapshot := s.pool.snapshot()
	slot := pickAvailableAddrSlot(snapshot, key, excluded)
	if slot != nil {
		return slot, nil
	}

	slots := filterExcludedAddrSlots(orderedAddrSlots(snapshot, key), excluded)
	if len(slots) == 0 {
		return nil, fmt.Errorf("no buildkitd addresses available")
	}
	if len(slots) == 1 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case slots[0].sem <- struct{}{}:
			return slots[0], nil
		}
	}
	selectCases := make([]reflect.SelectCase, 0, len(slots)+1)
	selectCases = append(selectCases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
	for _, slot := range slots {
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectSend,
			Chan: reflect.ValueOf(slot.sem),
			Send: reflect.ValueOf(struct{}{}),
		})
	}
	chosen, _, _ := reflect.Select(selectCases)
	if chosen == 0 {
		return nil, ctx.Err()
	}
	return slots[chosen-1], nil
}

func (s *buildServer) refreshBuildkitAddrs() error {
	refreshed, err := parseBuildkitAddrs(s.cfg.BuildkitdAddrs)
	if err != nil {
		return err
	}
	before := s.pool.addresses()
	s.pool.replace(refreshed)
	after := s.pool.addresses()
	if !sameStringSlice(before, after) {
		fmt.Fprintf(os.Stderr, "refreshed buildkit addresses: [%s] -> [%s]\n", strings.Join(before, ","), strings.Join(after, ","))
	}
	return nil
}

type buildkitAddr struct {
	original string
	addr     string
	nodeIP   string
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
		newSlots = append(newSlots, &addrSlot{addr: addr, sem: make(chan struct{}, p.concurrency)})
	}
	sort.Slice(newSlots, func(i, j int) bool { return newSlots[i].addr.addr < newSlots[j].addr.addr })
	p.slots = newSlots
	if len(newSlots) == 0 {
		p.hash = nil
		return
	}
	p.hash = newConsistentHash(len(newSlots), 150)
}

type consistentHash struct {
	ring     []uint32
	nodeMap  map[uint32]int
	numNodes int
}

func newConsistentHash(numNodes, replicas int) *consistentHash {
	ch := &consistentHash{nodeMap: make(map[uint32]int), numNodes: numNodes}
	for i := 0; i < numNodes; i++ {
		for r := 0; r < replicas; r++ {
			h := fnv1a(fmt.Sprintf("node-%d-replica-%d", i, r))
			ch.ring = append(ch.ring, h)
			ch.nodeMap[h] = i
		}
	}
	sort.Slice(ch.ring, func(i, j int) bool { return ch.ring[i] < ch.ring[j] })
	return ch
}

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

func pickAvailableAddrSlot(snapshot addrPoolSnapshot, key string, excluded map[string]bool) *addrSlot {
	for _, slot := range filterExcludedAddrSlots(orderedAddrSlots(snapshot, key), excluded) {
		if !tryAcquireAddrSlot(slot.sem) {
			continue
		}
		return slot
	}
	return nil
}

func filterExcludedAddrSlots(slots []*addrSlot, excluded map[string]bool) []*addrSlot {
	if len(slots) == 0 || len(excluded) == 0 {
		return slots
	}
	filtered := make([]*addrSlot, 0, len(slots))
	for _, slot := range slots {
		if slot == nil || slot.addr == nil || excluded[slot.addr.addr] {
			continue
		}
		filtered = append(filtered, slot)
	}
	return filtered
}

func orderedAddrSlots(snapshot addrPoolSnapshot, key string) []*addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}
	order := snapshot.hash.getSlotOrder(key)
	slots := make([]*addrSlot, 0, len(order))
	for _, idx := range order {
		if idx < 0 || idx >= len(snapshot.slots) {
			continue
		}
		slot := snapshot.slots[idx]
		if slot == nil || slot.addr == nil {
			continue
		}
		slots = append(slots, slot)
	}
	return slots
}

func tryAcquireAddrSlot(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func startBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string) {
	if strings.TrimSpace(addrsRaw) == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(defaultAddrRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			refreshed, err := parseBuildkitAddrs(addrsRaw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "refresh buildkit addresses: %v\n", err)
				continue
			}
			before := pool.addresses()
			pool.replace(refreshed)
			after := pool.addresses()
			if !sameStringSlice(before, after) {
				fmt.Fprintf(os.Stderr, "refreshed buildkit addresses: [%s] -> [%s]\n", strings.Join(before, ","), strings.Join(after, ","))
			}
		}
	}()
}

func slotAddressKeys(slots []*addrSlot) []string {
	keys := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot != nil && slot.addr != nil {
			keys = append(keys, slot.addr.addr)
		}
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

func parseBuildkitAddrs(raw string) ([]*buildkitAddr, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("--buildkitd-addr is required")
	}
	parts := strings.Split(raw, ",")
	addrs := make([]*buildkitAddr, 0, len(parts))
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
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].addr < addrs[j].addr })
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
