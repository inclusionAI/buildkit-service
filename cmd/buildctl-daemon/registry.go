package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	containerddocker "github.com/containerd/containerd/v2/core/remotes/docker"
	containerderrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/labstack/echo/v4"
)

type imageChecker interface {
	Check(ctx context.Context, image string) (imageCheckResult, error)
}

type imageCheckResult struct {
	Digest    string
	MediaType string
	Size      int64
}

type registryImageChecker struct {
	timeout   time.Duration
	configDir string
	hostRules map[string]map[string]struct{}
	slots     chan struct{}
	transport http.RoundTripper
}

func (s *buildServer) handleHeadImage(c echo.Context) error {
	image := strings.TrimSpace(c.QueryParam("image"))
	if image == "" {
		return c.NoContent(http.StatusBadRequest)
	}
	if s.images == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}

	result, err := s.images.Check(c.Request().Context(), image)
	if err != nil {
		var notAllowed *registryNotAllowedError
		if errors.As(err, &notAllowed) {
			return c.NoContent(http.StatusForbidden)
		}
		if errors.Is(err, errRegistryCheckBusy) {
			return c.NoContent(http.StatusTooManyRequests)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return c.NoContent(http.StatusGatewayTimeout)
		}
		if errors.Is(err, containerderrdefs.ErrNotFound) {
			return c.NoContent(http.StatusNotFound)
		}
		var invalidRef *invalidImageReferenceError
		if errors.As(err, &invalidRef) {
			return c.NoContent(http.StatusBadRequest)
		}
		return c.NoContent(http.StatusBadGateway)
	}

	if result.Digest != "" {
		c.Response().Header().Set("Docker-Content-Digest", result.Digest)
	}
	if result.MediaType != "" {
		c.Response().Header().Set(echo.HeaderContentType, result.MediaType)
	}
	if result.Size >= 0 {
		c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(result.Size, 10))
	}
	return c.NoContent(http.StatusOK)
}

type invalidImageReferenceError struct {
	image string
	err   error
}

func (e *invalidImageReferenceError) Error() string {
	return fmt.Sprintf("invalid image reference %q: %v", e.image, e.err)
}

func (e *invalidImageReferenceError) Unwrap() error { return e.err }

type registryNotAllowedError struct {
	host string
}

func (e *registryNotAllowedError) Error() string {
	return fmt.Sprintf("registry host %q is not allowed", e.host)
}

var errRegistryCheckBusy = errors.New("too many concurrent registry checks")

func normalizeRegistryImageReference(image string) (string, string, error) {
	trimmed := strings.TrimSpace(image)
	firstSlash := strings.IndexByte(trimmed, '/')
	if firstSlash <= 0 {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("a fully qualified registry/repository reference is required")}
	}
	registry := trimmed[:firstSlash]
	if !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("a fully qualified registry hostname is required")}
	}

	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return "", "", &invalidImageReferenceError{image: image, err: err}
	}
	if _, ok := named.(reference.Digested); ok {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("digest references are not supported")}
	}
	named = reference.TagNameOnly(named)
	return named.String(), reference.Domain(named), nil
}

func (r *registryImageChecker) Check(ctx context.Context, image string) (imageCheckResult, error) {
	normalized, registry, err := normalizeRegistryImageReference(image)
	if err != nil {
		return imageCheckResult{}, err
	}
	allowedNetworkHosts, ok := r.hostRules[normalizeRegistryHost(registry)]
	if !ok {
		return imageCheckResult{}, &registryNotAllowedError{host: registry}
	}
	if r.slots != nil {
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		default:
			return imageCheckResult{}, errRegistryCheckBusy
		}
	}

	dockerConfig, err := dockerconfig.Load(r.configDir)
	if err != nil {
		return imageCheckResult{}, fmt.Errorf("load docker config: %w", err)
	}
	credentials := func(host string) (string, string, error) {
		for _, candidate := range registryCredentialHosts(host) {
			auth, err := dockerConfig.GetAuthConfig(candidate)
			if err != nil {
				return "", "", err
			}
			if auth.RegistryToken != "" {
				return "", auth.RegistryToken, nil
			}
			if auth.IdentityToken != "" {
				return "", auth.IdentityToken, nil
			}
			if auth.Username != "" || auth.Password != "" {
				return auth.Username, auth.Password, nil
			}
		}
		return "", "", nil
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultRegistryTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	baseTransport := r.transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport := &registryCheckTransport{
		base:           baseTransport,
		allowedHosts:   allowedNetworkHosts,
		registryHost:   normalizeRegistryHost(registry),
		allowPlainHTTP: registryUsesPlainHTTP(registry),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resolver := containerddocker.NewResolver(containerddocker.ResolverOptions{
		Client:      client,
		Credentials: credentials,
		PlainHTTP:   registryUsesPlainHTTP(registry),
	})
	_, descriptor, err := resolver.Resolve(checkCtx, normalized)
	if err != nil {
		return imageCheckResult{}, err
	}
	return imageCheckResult{
		Digest:    descriptor.Digest.String(),
		MediaType: descriptor.MediaType,
		Size:      descriptor.Size,
	}, nil
}

func registryCredentialHosts(host string) []string {
	candidates := []string{host}
	if host == "registry-1.docker.io" || host == "docker.io" {
		candidates = append(candidates, "docker.io", "https://index.docker.io/v1/")
	}
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	return unique
}

type registryCheckTransport struct {
	base           http.RoundTripper
	allowedHosts   map[string]struct{}
	registryHost   string
	allowPlainHTTP bool
}

func (t *registryCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	requestHost := normalizeRegistryHost(req.URL.Host)
	if _, ok := t.allowedHosts[requestHost]; !ok {
		return nil, fmt.Errorf("registry check refuses request to untrusted host %q", requestHost)
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme == "https" {
		return t.base.RoundTrip(req)
	}
	if scheme == "http" && t.allowPlainHTTP && normalizeRegistryHost(req.URL.Host) == t.registryHost {
		return t.base.RoundTrip(req)
	}
	return nil, fmt.Errorf("registry check refuses insecure request to %s", req.URL.Redacted())
}

func parseRegistryHostRules(raw string) map[string]map[string]struct{} {
	rules := make(map[string]map[string]struct{})
	for _, item := range strings.Split(raw, ";") {
		registry, hostsRaw, ok := strings.Cut(item, "=")
		registry = normalizeRegistryHost(registry)
		if !ok || registry == "" {
			continue
		}
		hosts := make(map[string]struct{})
		for _, host := range strings.Split(hostsRaw, "|") {
			host = normalizeRegistryHost(host)
			if host != "" {
				hosts[host] = struct{}{}
			}
		}
		if len(hosts) > 0 {
			rules[registry] = hosts
		}
	}
	return rules
}

func normalizeRegistryHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func registryUsesPlainHTTP(registry string) bool {
	host := registry
	if parsedHost, _, err := net.SplitHostPort(registry); err == nil {
		host = parsedHost
	}
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}
