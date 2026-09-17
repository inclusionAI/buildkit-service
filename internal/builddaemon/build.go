package builddaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	buildkit "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	digest "github.com/opencontainers/go-digest"
)

const (
	nydusV3TargetSuffix    = "_nydus_v3"
	imageTypeNydus         = "nydus"
	imageTypeOCI           = "oci"
	imageTypeBoth          = "both"
	exporterImageDigestKey = "containerimage.digest"
	buildkitDockerFrontend = "dockerfile.v0"
)

type buildRunner interface {
	Build(ctx context.Context, req buildRunRequest, log io.Writer) (map[string]string, error)
	Close() error
}

type buildRunRequest struct {
	ContextDir   string
	BuildkitAddr string
	Image        string
	Format       string
	Target       string
	NoCache      bool
	BuildArgs    map[string]string
}

func isRetryableBuildkitAddrError(err error, buildkitAddr string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connect: no route to host",
		"connect: connection refused",
		"failed to dial",
		"i/o timeout",
	} {
		if strings.Contains(msg, needle) {
			return errorMentionsBuildkitAddr(msg, buildkitAddr)
		}
	}
	for _, needle := range []string{
		"connection reset by peer",
		"connection error",
		"error reading server preface",
		"transport is closing",
		"unavailable",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isDeterministicBuildError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"did not complete successfully: exit code:",
		"dockerfile parse error",
		"failed to parse dockerfile",
		"syntax error",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func errorMentionsBuildkitAddr(msg, buildkitAddr string) bool {
	addr := strings.ToLower(strings.TrimSpace(buildkitAddr))
	if addr == "" {
		return false
	}
	hostPort := strings.TrimPrefix(strings.TrimPrefix(addr, "tcp://"), "http://")
	hostPort = strings.TrimPrefix(hostPort, "https://")
	hostPort = strings.Trim(hostPort, "/")
	if hostPort == "" {
		return false
	}
	return strings.Contains(msg, hostPort)
}

type buildStep struct {
	Image      string
	Format     string
	BaseImage  string
	ContextDir string
}

func buildStepsForImageType(image, imageType string) []buildStep {
	switch imageType {
	case imageTypeOCI:
		return []buildStep{{Image: stripNydusV3Suffix(image), Format: imageTypeOCI}}
	case imageTypeBoth:
		ociImage := stripNydusV3Suffix(image)
		return []buildStep{
			{Image: ociImage, Format: imageTypeOCI},
			{Image: ensureNydusV3Suffix(ociImage), Format: imageTypeNydus, BaseImage: ociImage},
		}
	default:
		return []buildStep{{Image: ensureNydusV3Suffix(image), Format: imageTypeNydus}}
	}
}

func createNydusFromOCIContext(workDir, image string, exporter map[string]string) (string, string, error) {
	digestValue := strings.TrimSpace(exporter[exporterImageDigestKey])
	if digestValue == "" {
		return "", "", fmt.Errorf("OCI build did not return %s", exporterImageDigestKey)
	}
	parsedDigest, err := digest.Parse(digestValue)
	if err != nil {
		return "", "", fmt.Errorf("parse OCI image digest %q: %w", digestValue, err)
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return "", "", fmt.Errorf("parse OCI image reference %q: %w", image, err)
	}
	canonical, err := reference.WithDigest(named, parsedDigest)
	if err != nil {
		return "", "", fmt.Errorf("pin OCI image reference %q: %w", image, err)
	}

	baseImage := reference.FamiliarString(canonical)
	contextDir := filepath.Join(workDir, "nydus-from-oci")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create nydus conversion context: %w", err)
	}
	dockerfile := []byte("FROM " + baseImage + "\n")
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), dockerfile, 0o644); err != nil {
		return "", "", fmt.Errorf("write nydus conversion Dockerfile: %w", err)
	}
	return contextDir, baseImage, nil
}

func parseImageType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", imageTypeNydus:
		return imageTypeNydus, nil
	case imageTypeOCI, "gzip":
		return imageTypeOCI, nil
	case imageTypeBoth:
		return imageTypeBoth, nil
	default:
		return "", fmt.Errorf("image_type must be one of nydus, oci, both")
	}
}

func outputAttrs(image, format string) map[string]string {
	attrs := map[string]string{
		"name":              image,
		"push":              "true",
		"force-compression": "true",
		"oci-mediatypes":    "true",
	}
	if format == imageTypeOCI {
		attrs["compression"] = "gzip"
	} else {
		attrs["compression"] = "nydus"
		attrs["fs-version"] = "5"
	}
	return attrs
}

func frontendAttrs(target string, noCache bool, buildArgs map[string]string) map[string]string {
	attrs := make(map[string]string, len(buildArgs)+2)
	if strings.TrimSpace(target) != "" {
		attrs["target"] = strings.TrimSpace(target)
	}
	if noCache {
		attrs["no-cache"] = ""
	}
	for k, v := range buildArgs {
		attrs["build-arg:"+k] = v
	}
	return attrs
}

func hasNydusV3Suffix(target string) bool {
	return strings.HasSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func ensureNydusV3Suffix(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || hasNydusV3Suffix(trimmed) {
		return trimmed
	}
	return trimmed + nydusV3TargetSuffix
}

func stripNydusV3Suffix(target string) string {
	return strings.TrimSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

type realBuildRunner struct {
	tls     TLSConfig
	clients map[string]*buildkit.Client
	mu      sync.Mutex
}

func newRealBuildRunner(tls TLSConfig) (*realBuildRunner, error) {
	resolved, err := resolveTLSConfig(tls)
	if err != nil {
		return nil, err
	}
	return &realBuildRunner{tls: resolved, clients: make(map[string]*buildkit.Client)}, nil
}

func (r *realBuildRunner) Build(ctx context.Context, req buildRunRequest, log io.Writer) (map[string]string, error) {
	client, err := r.getClient(ctx, req)
	if err != nil {
		return nil, err
	}
	dockerConfig, err := dockerconfig.Load("")
	if err != nil {
		return nil, fmt.Errorf("load docker config: %w", err)
	}
	statusCh := make(chan *buildkit.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeSolveStatus(log, statusCh)
	}()

	resp, err := client.Solve(ctx, nil, buildkit.SolveOpt{
		Exports: []buildkit.ExportEntry{{
			Type:  buildkit.ExporterImage,
			Attrs: outputAttrs(req.Image, req.Format),
		}},
		LocalDirs:     map[string]string{"context": req.ContextDir, "dockerfile": req.ContextDir},
		Frontend:      buildkitDockerFrontend,
		FrontendAttrs: frontendAttrs(req.Target, req.NoCache, req.BuildArgs),
		Session:       []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{ConfigFile: dockerConfig})},
	}, statusCh)
	<-done
	if err != nil {
		return nil, err
	}
	return resp.ExporterResponse, nil
}

func (r *realBuildRunner) getClient(ctx context.Context, req buildRunRequest) (*buildkit.Client, error) {
	addr := strings.TrimSpace(req.BuildkitAddr)
	if addr == "" {
		return nil, errors.New("internal error: buildkit address was not set")
	}
	return r.clientForAddr(ctx, addr)
}

func (r *realBuildRunner) clientForAddr(ctx context.Context, addr string) (*buildkit.Client, error) {
	r.mu.Lock()
	if c, ok := r.clients[addr]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	var opts []buildkit.ClientOpt
	if r.tls.CACert != "" || r.tls.Cert != "" || r.tls.Key != "" {
		serverName := r.tls.ServerName
		if serverName == "" {
			if parsed, err := url.Parse(addr); err == nil {
				serverName = parsed.Hostname()
			}
		}
		if r.tls.CACert != "" || serverName != "" {
			opts = append(opts, buildkit.WithServerConfig(serverName, r.tls.CACert))
		}
		if r.tls.Cert != "" || r.tls.Key != "" {
			opts = append(opts, buildkit.WithCredentials(r.tls.Cert, r.tls.Key))
		}
	}

	c, err := buildkit.New(ctx, addr, opts...)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if existing, ok := r.clients[addr]; ok {
		r.mu.Unlock()
		_ = c.Close()
		return existing, nil
	}
	r.clients[addr] = c
	r.mu.Unlock()
	return c, nil
}

func (r *realBuildRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []string
	for addr, client := range r.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", addr, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func resolveTLSConfig(tls TLSConfig) (TLSConfig, error) {
	if tls.Dir == "" {
		return tls, nil
	}
	if tls.CACert != "" || tls.Cert != "" || tls.Key != "" {
		return TLSConfig{}, errors.New("cannot specify tlsdir and tlscacert/tlscert/tlskey at the same time")
	}
	return TLSConfig{
		CACert:     filepath.Join(tls.Dir, "ca.pem"),
		Cert:       filepath.Join(tls.Dir, "cert.pem"),
		Key:        filepath.Join(tls.Dir, "key.pem"),
		Dir:        tls.Dir,
		ServerName: tls.ServerName,
	}, nil
}

func writeSolveStatus(w io.Writer, statusCh <-chan *buildkit.SolveStatus) {
	encoder := json.NewEncoder(w)
	for status := range statusCh {
		for _, vertex := range status.Vertexes {
			_ = encoder.Encode(map[string]any{
				"type":      "vertex",
				"digest":    vertex.Digest.String(),
				"name":      vertex.Name,
				"cached":    vertex.Cached,
				"error":     vertex.Error,
				"started":   vertex.Started,
				"completed": vertex.Completed,
			})
		}
		for _, progress := range status.Statuses {
			_ = encoder.Encode(map[string]any{
				"type":      "status",
				"id":        progress.ID,
				"name":      progress.Name,
				"current":   progress.Current,
				"total":     progress.Total,
				"timestamp": progress.Timestamp,
			})
		}
		for _, log := range status.Logs {
			_ = encoder.Encode(map[string]any{
				"type":      "log",
				"stream":    log.Stream,
				"data":      string(log.Data),
				"timestamp": log.Timestamp,
			})
		}
		for _, warning := range status.Warnings {
			_ = encoder.Encode(map[string]any{
				"type":   "warning",
				"level":  warning.Level,
				"short":  string(warning.Short),
				"detail": warning.Detail,
				"url":    warning.URL,
			})
		}
	}
}
