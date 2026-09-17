package buildbatch

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

const defaultBuildkitAddrRefreshInterval = 15 * time.Second

func startBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string, oomCooldown time.Duration) {
	if strings.TrimSpace(addrsRaw) == "" {
		return
	}

	go runBuildkitAddrRefresher(ctx, pool, addrsRaw, oomCooldown, defaultBuildkitAddrRefreshInterval)
}

func runBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string, oomCooldown, interval time.Duration) {
	ticker := time.NewTicker(interval)
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
