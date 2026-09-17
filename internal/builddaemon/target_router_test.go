package builddaemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var rfcRegistryPool = []string{
	"registry-1.example.com/mirror",
	"registry-2.example.com/mirror",
	"registry-3.example.com/mirror",
	"registry-4.example.com/mirror",
	"registry-5.example.com/mirror",
}

func TestRegistryTargetRouterMatchesRFCVector(t *testing.T) {
	router := mustRouter(t, rfcRegistryPool)
	digest := strings.Repeat("0123456789abcdef", 4)
	got, err := router.Route("legacy.example.com/mirror/harbor:example_0123456789ab", digest)
	if err != nil {
		t.Fatal(err)
	}
	want := rfcRegistryPool[0] + "/harbor:example_0123456789ab"
	if got != want {
		t.Fatalf("RFC route mismatch: got %q, want %q", got, want)
	}
}

func TestRegistryTargetRouterIsOrderIndependent(t *testing.T) {
	reversed := append([]string(nil), rfcRegistryPool...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	first := mustRoute(t, mustRouter(t, rfcRegistryPool), strings.Repeat("a", 64))
	second := mustRoute(t, mustRouter(t, reversed), strings.Repeat("a", 64))
	if first != second {
		t.Fatalf("registry order changed route: %q != %q", first, second)
	}
}

func TestRegistryTargetRouterDistribution(t *testing.T) {
	router := mustRouter(t, rfcRegistryPool)
	counts := make(map[string]int)
	const samples = 5000
	for i := 0; i < samples; i++ {
		routed := mustRoute(t, router, fmt.Sprintf("%064x", i))
		for _, registry := range rfcRegistryPool {
			if strings.HasPrefix(routed, registry+"/") {
				counts[registry]++
				break
			}
		}
	}
	want := samples / len(rfcRegistryPool)
	for _, registry := range rfcRegistryPool {
		if count := counts[registry]; count < want*85/100 || count > want*115/100 {
			t.Fatalf("unexpected HRW distribution for %s: got %d, want near %d", registry, count, want)
		}
	}
}

func TestRegistryTargetRouterAddingRegistryMovesOnlyToNewTarget(t *testing.T) {
	oldRouter := mustRouter(t, rfcRegistryPool[:4])
	newRouter := mustRouter(t, rfcRegistryPool)
	const samples = 5000
	moved := 0
	for i := 0; i < samples; i++ {
		digest := fmt.Sprintf("%064x", i)
		oldRoute := mustRoute(t, oldRouter, digest)
		newRoute := mustRoute(t, newRouter, digest)
		if oldRoute == newRoute {
			continue
		}
		moved++
		if !strings.HasPrefix(newRoute, rfcRegistryPool[4]+"/") {
			t.Fatalf("context %s moved between existing registries: %q -> %q", digest, oldRoute, newRoute)
		}
	}
	if fraction := float64(moved) / samples; fraction >= 0.25 {
		t.Fatalf("adding one registry moved %.2f%% of contexts, want <25%%", fraction*100)
	}
}

func TestRegistryTargetRouterNormalizesPrefixAndPreservesRepository(t *testing.T) {
	router := mustRouter(t, []string{" registry-a.example.com/team/ ", "registry-b.example.com/team"})
	routed := mustRouteImage(t, router, "source.example.com/team/image:v1", strings.Repeat("b", 64))
	if !strings.Contains(routed, "/team/image:v1") {
		t.Fatalf("routed target did not apply prefix and preserve repository/tag: %q", routed)
	}
	alreadyPrefixed := mustRouteImage(t, router, "source.example.com/team/project:v1", strings.Repeat("b", 64))
	if strings.Contains(alreadyPrefixed, "/team/team/") {
		t.Fatalf("routed target duplicated prefix: %q", alreadyPrefixed)
	}
}

func TestParseRoutingTargetsValidatesPool(t *testing.T) {
	cases := []string{
		`[]`,
		`["registry-a.example.com/team"]`,
		`["registry-a.example.com/team","registry-a.example.com/team/"]`,
		`["registry-a.example.com/team",""]`,
		`["https://registry-a.example.com/team","registry-b.example.com/team"]`,
		`["not-a-registry/team","registry-b.example.com/team"]`,
	}
	for _, raw := range cases {
		if _, err := parseRoutingTargets(raw); err == nil {
			t.Fatalf("expected invalid target list %s to fail", raw)
		}
	}

	tooMany := make([]string, maxRoutingTargets+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("registry-%d.example.com/team", i)
	}
	raw, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRoutingTargets(string(raw)); err == nil {
		t.Fatal("expected a pool larger than 32 to fail")
	}
}

func TestRegistryTargetRouterRejectsInvalidContextDigest(t *testing.T) {
	router := mustRouter(t, rfcRegistryPool)
	for _, digest := range []string{"short", strings.Repeat("z", 64)} {
		if _, err := router.Route("source.example.com/team/project:v1", digest); err == nil {
			t.Fatalf("expected digest %q to fail", digest)
		}
	}
}

func mustRouter(t *testing.T, prefixes []string) *registryTargetRouter {
	t.Helper()
	raw, err := json.Marshal(prefixes)
	if err != nil {
		t.Fatal(err)
	}
	router, err := parseRoutingTargets(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func mustRoute(t *testing.T, router *registryTargetRouter, digest string) string {
	t.Helper()
	return mustRouteImage(t, router, "source.example.com/team/project:test", digest)
}

func mustRouteImage(t *testing.T, router *registryTargetRouter, image, digest string) string {
	t.Helper()
	routed, err := router.Route(image, digest)
	if err != nil {
		t.Fatal(err)
	}
	return routed
}
