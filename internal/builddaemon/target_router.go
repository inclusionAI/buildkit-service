package builddaemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/reference"
)

const (
	minRoutingTargets = 2
	maxRoutingTargets = 32
)

type targetRouter interface {
	Route(image, contextSHA256 string) (string, error)
}

type registryRoutingTarget struct {
	prefix     string
	host       string
	pathPrefix string
}

type registryTargetRouter struct {
	targets []registryRoutingTarget
}

func parseRoutingTargets(raw string) (*registryTargetRouter, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var prefixes []string
	if err := json.Unmarshal([]byte(raw), &prefixes); err != nil {
		return nil, fmt.Errorf("parse --routing-targets-json: %w", err)
	}
	if len(prefixes) < minRoutingTargets || len(prefixes) > maxRoutingTargets {
		return nil, fmt.Errorf("routing targets must contain between %d and %d registry prefixes", minRoutingTargets, maxRoutingTargets)
	}

	unique := make(map[string]struct{}, len(prefixes))
	targets := make([]registryRoutingTarget, 0, len(prefixes))
	for _, rawPrefix := range prefixes {
		target, err := parseRoutingTarget(rawPrefix)
		if err != nil {
			return nil, err
		}
		if _, ok := unique[target.prefix]; ok {
			return nil, fmt.Errorf("duplicate routing target %q", rawPrefix)
		}
		unique[target.prefix] = struct{}{}
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].prefix < targets[j].prefix })
	return &registryTargetRouter{targets: targets}, nil
}

func parseRoutingTarget(raw string) (registryRoutingTarget, error) {
	prefix := strings.TrimRight(strings.TrimSpace(raw), "/")
	if prefix == "" || strings.Contains(prefix, "://") {
		return registryRoutingTarget{}, fmt.Errorf("invalid routing target %q", raw)
	}
	firstComponent := strings.SplitN(prefix, "/", 2)[0]
	if firstComponent != "localhost" && !strings.Contains(firstComponent, ".") && !strings.Contains(firstComponent, ":") {
		return registryRoutingTarget{}, fmt.Errorf("invalid routing target %q", raw)
	}

	const probeRepository = "buildctl-route-probe"
	probe, err := reference.ParseNormalizedNamed(prefix + "/" + probeRepository + ":latest")
	if err != nil {
		return registryRoutingTarget{}, fmt.Errorf("invalid routing target %q: %w", raw, err)
	}
	host := reference.Domain(probe)
	path := reference.Path(probe)
	pathPrefix := strings.TrimSuffix(path, "/"+probeRepository)
	if path == probeRepository {
		pathPrefix = ""
	} else if pathPrefix == path {
		return registryRoutingTarget{}, fmt.Errorf("invalid routing target %q", raw)
	}
	normalized := host
	if pathPrefix != "" {
		normalized += "/" + pathPrefix
	}
	return registryRoutingTarget{prefix: normalized, host: host, pathPrefix: pathPrefix}, nil
}

func (r *registryTargetRouter) Route(image, contextSHA256 string) (string, error) {
	if r == nil || len(r.targets) == 0 {
		return "", fmt.Errorf("routing targets are not configured")
	}
	digest, err := normalizeContextSHA256(contextSHA256)
	if err != nil {
		return "", err
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return "", fmt.Errorf("parse image for registry routing: %w", err)
	}

	selected := r.targets[0]
	selectedScore := rendezvousScore(selected.prefix, digest)
	for _, target := range r.targets[1:] {
		score := rendezvousScore(target.prefix, digest)
		if bytes.Compare(score[:], selectedScore[:]) > 0 {
			selected = target
			selectedScore = score
		}
	}

	path := reference.Path(named)
	if selected.pathPrefix != "" && path != selected.pathPrefix && !strings.HasPrefix(path, selected.pathPrefix+"/") {
		path = selected.pathPrefix + "/" + path
	}
	suffix := ""
	if tagged, ok := named.(reference.NamedTagged); ok {
		suffix += ":" + tagged.Tag()
	}
	if canonical, ok := named.(reference.Canonical); ok {
		suffix += "@" + canonical.Digest().String()
	}
	return selected.host + "/" + path + suffix, nil
}

func normalizeContextSHA256(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("context_sha256 must be a 64-character hexadecimal digest")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("context_sha256 must be a 64-character hexadecimal digest")
	}
	return value, nil
}

func rendezvousScore(registryPrefix, contextSHA256 string) [sha256.Size]byte {
	return sha256.Sum256([]byte(registryPrefix + "\x00" + contextSHA256))
}
