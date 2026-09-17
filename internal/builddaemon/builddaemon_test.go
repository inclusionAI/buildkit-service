package builddaemon

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
)

type fakeRunner struct {
	mu         sync.Mutex
	requests   []buildRunRequest
	block      chan struct{}
	err        error
	errs       []error
	omitDigest bool
}

const fakeImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeImageChecker struct {
	result imageCheckResult
	err    error
	image  string
}

func (f *fakeImageChecker) Check(ctx context.Context, image string) (imageCheckResult, error) {
	f.image = image
	return f.result, f.err
}

func (f *fakeRunner) Build(ctx context.Context, req buildRunRequest, log io.Writer) (map[string]string, error) {
	if f.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.block:
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	_, _ = io.WriteString(log, "fake build complete\n")
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.omitDigest {
		return map[string]string{"image.name": req.Image}, nil
	}
	return map[string]string{
		"image.name":            req.Image,
		"containerimage.digest": fakeImageDigest,
	}, nil
}

func (f *fakeRunner) Close() error { return nil }

func (f *fakeRunner) snapshot() []buildRunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]buildRunRequest(nil), f.requests...)
}

func TestOutputAttrs(t *testing.T) {
	nydus := outputAttrs("example.com/ns/repo:tag_nydus_v3", imageTypeNydus)
	if nydus["compression"] != "nydus" || nydus["fs-version"] != "5" || nydus["push"] != "true" {
		t.Fatalf("unexpected nydus attrs: %#v", nydus)
	}
	oci := outputAttrs("example.com/ns/repo:tag", imageTypeOCI)
	if oci["compression"] != "gzip" || oci["name"] != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected oci attrs: %#v", oci)
	}
}

func TestApplyNoFileLimitNoop(t *testing.T) {
	if err := applyNoFileLimit(0); err != nil {
		t.Fatalf("applyNoFileLimit(0): %v", err)
	}
}

func TestBuildStepsForBothFormats(t *testing.T) {
	steps := buildStepsForImageType("example.com/ns/repo:tag", imageTypeBoth)
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0].Format != imageTypeOCI || steps[0].Image != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected first step: %#v", steps[0])
	}
	if steps[1].Format != imageTypeNydus || steps[1].Image != "example.com/ns/repo:tag_nydus_v3" {
		t.Fatalf("unexpected second step: %#v", steps[1])
	}
}

func TestFindBuildContextRootAndNested(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := findBuildContext(root)
	if err != nil || ctx != root {
		t.Fatalf("root context = %q, %v", ctx, err)
	}

	nestedRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(nestedRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedRoot, "src", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err = findBuildContext(nestedRoot)
	if err != nil || ctx != filepath.Join(nestedRoot, "src") {
		t.Fatalf("nested context = %q, %v", ctx, err)
	}
}

func TestResolveBuildImagePrefersFormImage(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "metadata.json"), []byte(`{"target":"example.com/ns/from-metadata:tag"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := resolveBuildImage(contextDir, "example.com/ns/from-form:tag")
	if err != nil {
		t.Fatal(err)
	}
	if image != "example.com/ns/from-form:tag" {
		t.Fatalf("expected form image to win, got %q", image)
	}
}

func TestResolveBuildImageFallsBackToMetadataTarget(t *testing.T) {
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "metadata.json"), []byte(`{"target":"example.com/ns/from-metadata:tag"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	image, err := resolveBuildImage(contextDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if image != "example.com/ns/from-metadata:tag" {
		t.Fatalf("expected metadata target, got %q", image)
	}
}

func TestBearerAuthMiddleware(t *testing.T) {
	server := newTestServer(t, "secret", &fakeRunner{})
	req := httptest.NewRequest(http.MethodGet, "/v1/builds", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/builds", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHeadImageRequiresBearerAuth(t *testing.T) {
	server := newTestServer(t, "secret", &fakeRunner{})
	req := httptest.NewRequest(http.MethodHead, "/v1/images?image=registry.example.com/ns/repo:tag", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHeadImage(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		result     imageCheckResult
		err        error
		wantStatus int
	}{
		{
			name: "exists",
			url:  "/v1/images?image=registry.example.com/ns/repo:tag",
			result: imageCheckResult{
				Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Size:      1234,
			},
			wantStatus: http.StatusOK,
		},
		{name: "missing image parameter", url: "/v1/images", wantStatus: http.StatusBadRequest},
		{name: "not found", url: "/v1/images?image=registry.example.com/ns/repo:missing", err: containerderrdefs.ErrNotFound, wantStatus: http.StatusNotFound},
		{name: "invalid reference", url: "/v1/images?image=invalid", err: &invalidImageReferenceError{image: "invalid", err: errors.New("invalid")}, wantStatus: http.StatusBadRequest},
		{name: "registry forbidden", url: "/v1/images?image=registry.example.com/ns/repo:tag", err: &registryNotAllowedError{host: "registry.example.com"}, wantStatus: http.StatusForbidden},
		{name: "registry checks saturated", url: "/v1/images?image=registry.example.com/ns/repo:tag", err: errRegistryCheckBusy, wantStatus: http.StatusTooManyRequests},
		{name: "registry timeout", url: "/v1/images?image=registry.example.com/ns/repo:tag", err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout},
		{name: "registry failure", url: "/v1/images?image=registry.example.com/ns/repo:tag", err: errors.New("registry unavailable"), wantStatus: http.StatusBadGateway},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := &fakeImageChecker{result: test.result, err: test.err}
			server := newTestServer(t, "", &fakeRunner{})
			server.images = checker
			req := httptest.NewRequest(http.MethodHead, test.url, nil)
			rec := httptest.NewRecorder()
			server.routes().ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("expected %d, got %d", test.wantStatus, rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("expected an empty HEAD response body, got %q", rec.Body.String())
			}
			if test.name == "exists" {
				if checker.image != "registry.example.com/ns/repo:tag" {
					t.Fatalf("unexpected checked image %q", checker.image)
				}
				if got := rec.Header().Get("Docker-Content-Digest"); got != test.result.Digest {
					t.Fatalf("unexpected digest header %q", got)
				}
				if got := rec.Header().Get("Content-Type"); got != test.result.MediaType {
					t.Fatalf("unexpected content type %q", got)
				}
				if got := rec.Header().Get("Content-Length"); got != "1234" {
					t.Fatalf("unexpected content length %q", got)
				}
			}
		})
	}
}

func TestNormalizeRegistryImageReference(t *testing.T) {
	tests := []struct {
		input        string
		want         string
		wantRegistry string
		wantErr      bool
	}{
		{input: "registry.example.com/ns/repo:tag", want: "registry.example.com/ns/repo:tag", wantRegistry: "registry.example.com"},
		{input: "registry.example.com/ns/repo", want: "registry.example.com/ns/repo:latest", wantRegistry: "registry.example.com"},
		{input: "localhost:5000/repo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantErr: true},
		{input: "ubuntu:22.04", wantErr: true},
		{input: "namespace/repo:tag", wantErr: true},
		{input: "https://registry.example.com/ns/repo:tag", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, registry, err := normalizeRegistryImageReference(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want || registry != test.wantRegistry {
				t.Fatalf("got reference=%q registry=%q, want reference=%q registry=%q", got, registry, test.want, test.wantRegistry)
			}
		})
	}
}

func TestRegistryImageChecker(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		if r.URL.Path == "/v2/ns/repo/manifests/tag" {
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", "456")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	checker := newTestRegistryImageChecker(registryHost, time.Second, "")
	result, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != digest || result.MediaType != "application/vnd.oci.image.manifest.v1+json" || result.Size != 456 {
		t.Fatalf("unexpected result: %#v", result)
	}

	_, err = checker.Check(context.Background(), registryHost+"/ns/repo:missing")
	if !containerderrdefs.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestRegistryImageCheckerBearerAuth(t *testing.T) {
	const (
		username = "registry-user"
		password = "registry-password"
		digest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/ns/repo/manifests/tag":
			if r.Header.Get("Authorization") != "Bearer manifest-token" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test-registry",scope="repository:ns/repo:pull"`, registry.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Content-Length", "789")
			w.WriteHeader(http.StatusOK)
		case "/token":
			gotUsername, gotPassword, ok := r.BasicAuth()
			if !ok || gotUsername != username || gotPassword != password {
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"manifest-token"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	dockerConfigDir := t.TempDir()
	dockerConfig := fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registryHost, base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), []byte(dockerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := newTestRegistryImageChecker(registryHost, time.Second, dockerConfigDir)
	result, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != digest || result.Size != 789 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRegistryImageCheckerRejectsUnlistedRegistry(t *testing.T) {
	checker := newTestRegistryImageChecker("registry.example.com", time.Second, "")
	_, err := checker.Check(context.Background(), "other.example.com/ns/repo:tag")
	var notAllowed *registryNotAllowedError
	if !errors.As(err, &notAllowed) {
		t.Fatalf("expected registry not allowed, got %v", err)
	}
}

func TestRegistryImageCheckerRejectsRedirect(t *testing.T) {
	redirectTargetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/internal", http.StatusTemporaryRedirect)
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	checker := newTestRegistryImageChecker(registryHost, time.Second, "")
	_, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if err == nil {
		t.Fatal("expected redirect to fail")
	}
	if redirectTargetCalled {
		t.Fatal("registry checker followed a cross-host redirect")
	}
}

func TestRegistryImageCheckerRejectsUntrustedBearerRealm(t *testing.T) {
	tokenServerCalled := false
	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenServerCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer tokenServer.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="malicious-registry",scope="repository:ns/repo:pull"`, tokenServer.URL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	checker := newTestRegistryImageChecker(registryHost, time.Second, "")
	checker.transport = tokenServer.Client().Transport
	_, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if err == nil {
		t.Fatal("expected untrusted bearer realm to fail")
	}
	if !strings.Contains(err.Error(), "untrusted host") {
		t.Fatalf("expected untrusted host error, got %v", err)
	}
	if tokenServerCalled {
		t.Fatal("registry checker contacted an untrusted bearer realm")
	}
}

func TestRegistryImageCheckerConcurrencyLimit(t *testing.T) {
	registryHost := "registry.example.com"
	checker := newTestRegistryImageChecker(registryHost, time.Second, "")
	checker.slots = make(chan struct{}, 1)
	checker.slots <- struct{}{}
	_, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if !errors.Is(err, errRegistryCheckBusy) {
		t.Fatalf("expected busy error, got %v", err)
	}
}

func TestRegistryImageCheckerTotalTimeout(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer registry.Close()

	registryHost := strings.TrimPrefix(registry.URL, "http://")
	checker := newTestRegistryImageChecker(registryHost, 20*time.Millisecond, "")
	_, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func newTestRegistryImageChecker(host string, timeout time.Duration, configDir string) *registryImageChecker {
	normalizedHost := normalizeRegistryHost(host)
	return &registryImageChecker{
		timeout:   timeout,
		configDir: configDir,
		hostRules: map[string]map[string]struct{}{
			normalizedHost: {normalizedHost: {}},
		},
		slots: make(chan struct{}, 4),
	}
}

func TestMetricsEndpoint(t *testing.T) {
	server := newTestServer(t, "secret", &fakeRunner{})
	server.globalSem = newGlobalSem(2)
	server.globalSem <- struct{}{}

	// Queued/running gauges are derived from the store; counters from finishTask.
	server.store.add(&buildTask{ID: "q1", Mode: defaultBuildMode, Status: buildStatusQueued})
	server.store.add(&buildTask{ID: "r1", Mode: defaultBuildMode, Status: buildStatusRunning})
	server.store.add(&buildTask{ID: "s1", Mode: defaultBuildMode, Status: buildStatusRunning})
	server.finishTask("s1", buildStatusSucceeded, nil, nil)
	server.store.add(&buildTask{ID: "f1", Mode: defaultBuildMode, Status: buildStatusRunning})
	server.finishTask("f1", buildStatusFailed, nil, errors.New("boom"))

	// /metrics must be reachable without auth.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected metrics 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"buildctl_daemon_builds_queued 1",
		"buildctl_daemon_builds_running 1",
		"buildctl_daemon_builds_active 1",
		"buildctl_daemon_builds_succeeded_total 1",
		"buildctl_daemon_builds_failed_total 1",
		`buildctl_daemon_mode_builds_queued{mode="default_mode"} 1`,
		`buildctl_daemon_mode_builds_running{mode="default_mode"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q in:\n%s", want, body)
		}
	}
}

func TestCreateBuildBothFormats(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":             "example.com/ns/repo:tag",
		"image_type":        "both",
		"build_arg.VERSION": "1.2.3",
		"target":            "release",
		"no_cache":          "true",
	}, map[string]string{
		"Dockerfile": "FROM scratch\nARG VERSION\n",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var created buildTaskView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Retry != defaultBuildRetry || created.RetryIntervalSeconds != int(defaultRetryInterval/time.Second) {
		t.Fatalf("unexpected default retry config: retry=%d interval=%d", created.Retry, created.RetryIntervalSeconds)
	}
	if created.Mode != defaultBuildMode || created.RoutedTarget != "example.com/ns/repo:tag" {
		t.Fatalf("legacy request did not use the default unrouted mode: %#v", created)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusSucceeded {
		t.Fatalf("expected succeeded, got %#v", task)
	}
	requests := runner.snapshot()
	if len(requests) != 2 {
		t.Fatalf("expected 2 build requests, got %d", len(requests))
	}
	if requests[0].Format != imageTypeOCI || requests[0].Image != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected first request: %#v", requests[0])
	}
	if requests[1].Format != imageTypeNydus || requests[1].Image != "example.com/ns/repo:tag_nydus_v3" {
		t.Fatalf("unexpected second request: %#v", requests[1])
	}
	if requests[0].Target != "release" || !requests[0].NoCache || requests[0].BuildArgs["VERSION"] != "1.2.3" {
		t.Fatalf("request options were not propagated: %#v", requests[0])
	}
	if requests[1].ContextDir == requests[0].ContextDir {
		t.Fatalf("nydus request reused original context: %#v", requests[1])
	}
	if requests[1].Target != "" || len(requests[1].BuildArgs) != 0 || !requests[1].NoCache {
		t.Fatalf("nydus conversion request inherited original Dockerfile options: %#v", requests[1])
	}
	dockerfile, err := os.ReadFile(filepath.Join(requests[1].ContextDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	wantDockerfile := "FROM example.com/ns/repo:tag@" + fakeImageDigest + "\n"
	if string(dockerfile) != wantDockerfile {
		t.Fatalf("unexpected nydus conversion Dockerfile: got %q, want %q", dockerfile, wantDockerfile)
	}

	assertBuildResponseOmitsInternalFields(t, server, created.ID)
}

func TestCreateBuildBothFormatsStopsWhenOCIFails(t *testing.T) {
	runner := &fakeRunner{err: errors.New("OCI build failed")}
	server := newTestServer(t, "", runner)
	created := createTestBuild(t, server, map[string]string{
		"image":          "example.com/ns/repo:tag",
		"image_type":     "both",
		"retry":          "0",
		"retry-interval": "0",
	})

	waitForTaskStatus(t, server, created.ID, buildStatusFailed)
	requests := runner.snapshot()
	if len(requests) != 1 {
		t.Fatalf("expected only the failed OCI request, got %d requests", len(requests))
	}
	if requests[0].Format != imageTypeOCI || requests[0].Image != "example.com/ns/repo:tag" {
		t.Fatalf("unexpected first request: %#v", requests[0])
	}
}

func TestCreateBuildBothFormatsUsesRoutedOCIAsNydusBase(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	modes, err := parseBuildModes(`{
		"default_mode":{"concurrency":1,"routingEnabled":false},
		"production_mode":{"concurrency":1,"routingEnabled":true}
	}`, defaultBuildMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	router, err := parseRoutingTargets(`["registry-a.example.com","registry-b.example.com"]`)
	if err != nil {
		t.Fatal(err)
	}
	server.modes = modes
	server.router = router

	created := createTestBuild(t, server, map[string]string{
		"image":      "logical.example.com/team/image:both",
		"image_type": "both",
		"mode":       "production_mode",
	})
	waitForTaskStatus(t, server, created.ID, buildStatusSucceeded)
	if created.RoutedTarget == created.Image {
		t.Fatalf("production target was not routed: %#v", created)
	}

	requests := runner.snapshot()
	if len(requests) != 2 {
		t.Fatalf("expected two build requests, got %d", len(requests))
	}
	if requests[0].Image != created.RoutedTarget || requests[0].Format != imageTypeOCI {
		t.Fatalf("OCI request did not use routed target: %#v", requests[0])
	}
	if requests[1].Image != created.RoutedTarget+nydusV3TargetSuffix || requests[1].Format != imageTypeNydus {
		t.Fatalf("nydus request did not use routed target: %#v", requests[1])
	}
	dockerfile, err := os.ReadFile(filepath.Join(requests[1].ContextDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	wantDockerfile := "FROM " + created.RoutedTarget + "@" + fakeImageDigest + "\n"
	if string(dockerfile) != wantDockerfile {
		t.Fatalf("unexpected routed nydus Dockerfile: got %q, want %q", dockerfile, wantDockerfile)
	}
}

func TestCreateBuildBothFormatsRequiresOCIDigest(t *testing.T) {
	runner := &fakeRunner{omitDigest: true}
	server := newTestServer(t, "", runner)
	created := createTestBuild(t, server, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "both",
	})

	task := waitForTaskStatus(t, server, created.ID, buildStatusFailed)
	if !strings.Contains(task.Error, exporterImageDigestKey) {
		t.Fatalf("unexpected missing digest error: %q", task.Error)
	}
	requests := runner.snapshot()
	if len(requests) != 1 || requests[0].Format != imageTypeOCI {
		t.Fatalf("nydus build ran without a pinned OCI digest: %#v", requests)
	}
}

func TestCreateBuildRetriesFailedBuildFromAPI(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("transient build failure"), nil}}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":          "example.com/ns/repo:tag",
		"image_type":     "nydus",
		"retry":          "1",
		"retry-interval": "0",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusSucceeded {
		t.Fatalf("expected retry to succeed, got %#v", task)
	}
	if requests := runner.snapshot(); len(requests) != 2 {
		t.Fatalf("expected initial attempt plus one retry, got %d", len(requests))
	}
}

func TestCreateBuildDoesNotRetryDeterministicFailure(t *testing.T) {
	runner := &fakeRunner{err: errors.New(`failed to solve: process "/bin/sh -c false" did not complete successfully: exit code: 1`)}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":          "example.com/ns/repo:tag",
		"image_type":     "nydus",
		"retry":          "3",
		"retry-interval": "0",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusFailed {
		t.Fatalf("expected failed task, got %#v", task)
	}
	if requests := runner.snapshot(); len(requests) != 1 {
		t.Fatalf("expected deterministic failure to run once, got %d attempts", len(requests))
	}
}

func TestDeterministicBuildError(t *testing.T) {
	for _, err := range []error{
		errors.New(`process "/bin/sh -c false" did not complete successfully: exit code: 1`),
		errors.New("dockerfile parse error on line 3: unknown instruction"),
		errors.New("failed to parse dockerfile: syntax error"),
	} {
		if !isDeterministicBuildError(err) {
			t.Fatalf("expected deterministic error: %v", err)
		}
	}
	if isDeterministicBuildError(errors.New("unexpected status from PATCH request: 502 Bad Gateway")) {
		t.Fatal("registry 502 must remain retryable")
	}
}

func TestCreateBuildRetryZeroDisablesFailedBuildRetry(t *testing.T) {
	runner := &fakeRunner{err: errors.New("permanent build failure")}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":          "example.com/ns/repo:tag",
		"image_type":     "nydus",
		"retry":          "0",
		"retry-interval": "0",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusFailed {
		t.Fatalf("expected failed task, got %#v", task)
	}
	if requests := runner.snapshot(); len(requests) != 1 {
		t.Fatalf("expected no retries, got %d attempts", len(requests))
	}
}

func TestCreateBuildRejectsInvalidRetryParams(t *testing.T) {
	for name, fields := range map[string]map[string]string{
		"negative retry":          {"retry": "-1"},
		"bad retry interval":      {"retry-interval": "nope"},
		"negative retry interval": {"retry-interval": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			fields["image"] = "example.com/ns/repo:tag"
			fields["image_type"] = "nydus"
			server := newTestServer(t, "", &fakeRunner{})
			body, contentType := multipartZipRequest(t, fields, map[string]string{"Dockerfile": "FROM scratch\n"})

			req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			server.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateBuildUsesMetadataTargetWhenImageMissing(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image_type": "nydus",
	}, map[string]string{
		"Dockerfile":    "FROM scratch\n",
		"metadata.json": `{"target":"example.com/ns/from-metadata:tag"}`,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		requests := runner.snapshot()
		if len(requests) > 0 {
			if requests[0].Image != "example.com/ns/from-metadata:tag_nydus_v3" {
				t.Fatalf("expected metadata target to be used, got %#v", requests[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for build request for task %s", created.ID)
}

func TestCreateBuildPreprocessesLegacyDockerfileHeredoc(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "nydus",
	}, map[string]string{
		"Dockerfile": `FROM ubuntu:22.04

# Create a project README that mentions the primary download URL
RUN cat > /workspace/README.md << 'READMEEOF'
# Episode Processing Project

## Source
Download episode videos from the media archive server:
https://media.example.invalid/episodes/

## Notes
- Episode files are large MP4 files (H.264 video, AAC audio)
- Network issues have been reported with the media archive server
- A local cache of previously downloaded episodes may be available
READMEEOF

# Create a download script that references the blocked URL
RUN cat > /workspace/download_episode.sh << 'DLEOF'
#!/bin/bash
# Download episode video from the media archive
EPISODE_URL="https://media.example.invalid/episodes/episode42.mp4"
OUTPUT="/workspace/episode42.mp4"

echo "Downloading episode from ${EPISODE_URL}..."
curl -f -L -o "${OUTPUT}" "${EPISODE_URL}"
if [ $? -ne 0 ]; then
    echo "ERROR: Download failed from media archive."
    echo "Check network connectivity or use a cached version if available."
    exit 1
fi
echo "Download complete: ${OUTPUT}"
DLEOF
RUN chmod +x /workspace/download_episode.sh

# Create a cache README
RUN cat > /workspace/cache/README.txt << 'CACEOF'
Local Cache
===========
This directory contains cached copies of media files for offline use.
Files in this cache are identical to the originals on the media server.
CACEOF
`,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusSucceeded {
		t.Fatalf("expected succeeded, got %#v", task)
	}
	rewritten, err := os.ReadFile(filepath.Join(task.contextDir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(rewritten)
	for _, want := range []string{
		"COPY <<'READMEEOF' /workspace/README.md",
		"COPY <<'DLEOF' /workspace/download_episode.sh",
		"COPY <<'CACEOF' /workspace/cache/README.txt",
		`echo "Downloading episode from ${EPISODE_URL}..."`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("rewritten Dockerfile missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "RUN cat >") {
		t.Fatalf("legacy heredoc RUN remains after rewrite:\n%s", content)
	}
}

func TestCreateBuildUsesSourceContextHashForScheduleKey(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	var keys []string

	for _, data := range []string{"alpha\n", "beta\n"} {
		body, contentType := multipartZipRequest(t, map[string]string{
			"image":      "example.com/ns/repo:tag",
			"image_type": "nydus",
		}, map[string]string{
			"Dockerfile":  "FROM scratch\n",
			"context.txt": data,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
		}
		var created buildTask
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			task, ok := server.store.get(created.ID)
			if ok && task.Status == buildStatusSucceeded {
				if !strings.HasPrefix(task.scheduleKey, "source:") {
					t.Fatalf("expected source schedule key, got %q", task.scheduleKey)
				}
				keys = append(keys, task.scheduleKey)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if len(keys) != 2 {
		t.Fatalf("expected two schedule keys, got %d", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("expected different schedule keys for different context content, got %q", keys[0])
	}
}

func TestSourceContextContentHashMatchesRFCAlgorithm(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "data.txt"), []byte("alpha\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := sourceContextContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	const want = "source:bf08700ef6b428e939efe2b57613e45067678b99a98a5fce7ef21a1ed9a6b0c9"
	if got != want {
		t.Fatalf("context digest mismatch: got %q, want %q", got, want)
	}

	if err := os.Chmod(filepath.Join(dir, "nested", "data.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotAfterModeChange, err := sourceContextContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfterModeChange != want {
		t.Fatalf("file mode unexpectedly affected RFC context digest: got %q, want %q", gotAfterModeChange, want)
	}
}

func TestBuildRetriesRetryableWorkerAddressError(t *testing.T) {
	runner := &fakeRunner{errs: []error{
		errors.New("rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: dial tcp 10.0.0.1:9094: connect: no route to host\""),
		nil,
	}}
	server := newTestServer(t, "", runner)
	addrs, err := parseBuildkitAddrs("10.0.0.1:9094,10.0.0.2:9094")
	if err != nil {
		t.Fatal(err)
	}
	server.cfg.BuildkitdAddrs = "10.0.0.1:9094,10.0.0.2:9094"
	server.pool = newAddrPool(addrs, 4)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusSucceeded {
		t.Fatalf("expected retry to succeed, got %#v", task)
	}
	requests := runner.snapshot()
	if len(requests) != 2 {
		t.Fatalf("expected two build attempts, got %d", len(requests))
	}
	if requests[0].BuildkitAddr == requests[1].BuildkitAddr {
		t.Fatalf("expected retry to use another worker, got same address %q", requests[0].BuildkitAddr)
	}
	if task.BuildkitAddr != requests[1].BuildkitAddr {
		t.Fatalf("expected final task address %q, got %q", requests[1].BuildkitAddr, task.BuildkitAddr)
	}
}

func TestRetryableBuildkitAddrErrorRequiresBuildkitAddressForDialErrors(t *testing.T) {
	workerErr := errors.New("rpc error: code = Unavailable desc = connection error: desc = \"transport: Error while dialing: dial tcp 10.0.0.1:9094: connect: connection refused\"")
	if !isRetryableBuildkitAddrError(workerErr, "tcp://10.0.0.1:9094") {
		t.Fatal("expected worker dial failure to be retryable")
	}

	registryErr := errors.New(`failed to solve: image export stage: export default image: failed to do request: Head "http://localhost:5000/v2/node/blobs/sha256:abc": dial tcp [::1]:5000: connect: connection refused`)
	if isRetryableBuildkitAddrError(registryErr, "tcp://10.0.0.1:9094") {
		t.Fatal("expected target registry connection failure not to trigger worker failover")
	}
}

func TestCancelRunningBuild(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := server.store.get(created.ID)
		if task.Status == buildStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	req = httptest.NewRequest(http.MethodDelete, "/v1/builds/"+created.ID, nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	task, _ := server.store.get(created.ID)
	if task.Status != buildStatusCanceled {
		t.Fatalf("expected canceled, got %#v", task)
	}
}

func TestBuildWaitsQueuedUntilWorkerSlotAssigned(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	server := newTestServer(t, "", runner)
	server.pool = newAddrPool([]*buildkitAddr{{original: "127.0.0.1:9094", addr: "tcp://127.0.0.1:9094", nodeIP: "127.0.0.1"}}, 1)

	firstBody, firstContentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:first",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/builds", firstBody)
	firstReq.Header.Set("Content-Type", firstContentType)
	firstRec := httptest.NewRecorder()
	server.routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusAccepted {
		t.Fatalf("expected first create 202, got %d: %s", firstRec.Code, firstRec.Body.String())
	}
	var first buildTask
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := server.store.get(first.ID)
		if task.Status == buildStatusRunning && task.BuildkitAddr != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstTask, _ := server.store.get(first.ID)
	if firstTask.Status != buildStatusRunning || firstTask.BuildkitAddr == "" || firstTask.StartedAt == nil {
		t.Fatalf("expected first task to occupy worker slot, got %#v", firstTask)
	}

	secondBody, secondContentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:second",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/builds", secondBody)
	secondReq.Header.Set("Content-Type", secondContentType)
	secondRec := httptest.NewRecorder()
	server.routes().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusAccepted {
		t.Fatalf("expected second create 202, got %d: %s", secondRec.Code, secondRec.Body.String())
	}
	var second buildTask
	if err := json.Unmarshal(secondRec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	secondTask, _ := server.store.get(second.ID)
	if secondTask.Status != buildStatusQueued {
		t.Fatalf("expected second task to stay queued while waiting for slot, got %#v", secondTask)
	}
	if secondTask.StartedAt != nil || secondTask.BuildkitAddr != "" || secondTask.NodeIP != "" {
		t.Fatalf("expected queued task to have no worker assignment, got %#v", secondTask)
	}

	close(runner.block)
}

func TestBuildWaitsQueuedUntilGlobalSlotAssigned(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	server := newTestServer(t, "", runner)
	// Plenty of per-address slots; only the global limit of 1 constrains.
	server.pool = newAddrPool([]*buildkitAddr{
		{original: "127.0.0.1:9094", addr: "tcp://127.0.0.1:9094", nodeIP: "127.0.0.1"},
		{original: "127.0.0.1:9095", addr: "tcp://127.0.0.1:9095", nodeIP: "127.0.0.1"},
	}, 4)
	server.globalSem = newGlobalSem(1)

	firstBody, firstContentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:first",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/builds", firstBody)
	firstReq.Header.Set("Content-Type", firstContentType)
	firstRec := httptest.NewRecorder()
	server.routes().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusAccepted {
		t.Fatalf("expected first create 202, got %d: %s", firstRec.Code, firstRec.Body.String())
	}
	var first buildTask
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := server.store.get(first.ID)
		if task.Status == buildStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstTask, _ := server.store.get(first.ID)
	if firstTask.Status != buildStatusRunning {
		t.Fatalf("expected first task to hold the global slot, got %#v", firstTask)
	}

	secondBody, secondContentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:second",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/builds", secondBody)
	secondReq.Header.Set("Content-Type", secondContentType)
	secondRec := httptest.NewRecorder()
	server.routes().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusAccepted {
		t.Fatalf("expected second create 202, got %d: %s", secondRec.Code, secondRec.Body.String())
	}
	var second buildTask
	if err := json.Unmarshal(secondRec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	secondTask, _ := server.store.get(second.ID)
	if secondTask.Status != buildStatusQueued {
		t.Fatalf("expected second task to stay queued on global limit, got %#v", secondTask)
	}
	if secondTask.StartedAt != nil || secondTask.BuildkitAddr != "" {
		t.Fatalf("expected globally queued task to have no worker assignment, got %#v", secondTask)
	}

	close(runner.block)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := server.store.get(second.ID)
		if task.Status == buildStatusSucceeded {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(second.ID)
	t.Fatalf("expected second task to run after global slot freed, got %#v", task)
}

func TestCreateBuildUsesIndependentModeQueuesAndRoutesProduction(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	server := newTestServer(t, "", runner)
	modes, err := parseBuildModes(`{
		"default_mode":{"concurrency":1,"routingEnabled":false},
		"production_mode":{"concurrency":1,"routingEnabled":true}
	}`, defaultBuildMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	router, err := parseRoutingTargets(`["registry-a.example.com","registry-b.example.com"]`)
	if err != nil {
		t.Fatal(err)
	}
	server.modes = modes
	server.router = router
	server.scheduler, err = newBuildModeScheduler(modes, server.runBuildTask)
	if err != nil {
		t.Fatal(err)
	}
	defer server.scheduler.close()

	firstProduction := createTestBuild(t, server, map[string]string{
		"image":      "logical.example.com/team/image:first",
		"image_type": "nydus",
		"mode":       "production_mode",
	})
	waitForTaskStatus(t, server, firstProduction.ID, buildStatusRunning)
	if firstProduction.Mode != "production_mode" || firstProduction.RoutedTarget == firstProduction.Image {
		t.Fatalf("production task was not routed: %#v", firstProduction)
	}

	secondProduction := createTestBuild(t, server, map[string]string{
		"image":      "logical.example.com/team/image:second",
		"image_type": "nydus",
		"mode":       "production_mode",
	})
	time.Sleep(50 * time.Millisecond)
	secondSnapshot, _ := server.store.get(secondProduction.ID)
	if secondSnapshot.Status != buildStatusQueued {
		t.Fatalf("expected second production task to remain queued, got %#v", secondSnapshot)
	}

	antTask := createTestBuild(t, server, map[string]string{
		"image":      "ant.example.com/team/image:online",
		"image_type": "nydus",
	})
	waitForTaskStatus(t, server, antTask.ID, buildStatusRunning)
	if antTask.Mode != defaultBuildMode || antTask.RoutedTarget != antTask.Image {
		t.Fatalf("legacy ant task unexpectedly changed: %#v", antTask)
	}

	close(runner.block)
	waitForTaskStatus(t, server, firstProduction.ID, buildStatusSucceeded)
	waitForTaskStatus(t, server, secondProduction.ID, buildStatusSucceeded)
	waitForTaskStatus(t, server, antTask.ID, buildStatusSucceeded)

	requests := runner.snapshot()
	if len(requests) != 3 {
		t.Fatalf("expected three build requests, got %d", len(requests))
	}
	routedBuildFound := false
	for _, request := range requests {
		if strings.HasSuffix(request.Image, "/team/image:first_nydus_v3") {
			routedBuildFound = strings.HasPrefix(request.Image, "registry-a.example.com/") || strings.HasPrefix(request.Image, "registry-b.example.com/")
		}
	}
	if !routedBuildFound {
		t.Fatalf("runner did not receive the routed production target: %#v", requests)
	}
}

func TestCreateBuildRejectsUnknownMode(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	body, contentType := multipartZipRequest(t, map[string]string{
		"image": "example.com/ns/repo:tag",
		"mode":  "missing_mode",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMarkTaskQueuedClearsWorkerAssignment(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	started := time.Now()
	server.store.add(&buildTask{
		ID:           "retrying",
		Status:       buildStatusRunning,
		BuildkitAddr: "tcp://10.0.0.1:9094",
		NodeIP:       "10.0.0.1",
		StartedAt:    &started,
	})

	server.markTaskQueued("retrying")
	task, ok := server.store.get("retrying")
	if !ok {
		t.Fatal("task disappeared")
	}
	if task.Status != buildStatusQueued {
		t.Fatalf("expected queued status, got %q", task.Status)
	}
	if task.BuildkitAddr != "" || task.NodeIP != "" {
		t.Fatalf("expected worker assignment to be cleared, got addr=%q node=%q", task.BuildkitAddr, task.NodeIP)
	}
	if task.StartedAt == nil || !task.StartedAt.Equal(started) {
		t.Fatalf("expected first start time to be preserved, got %v", task.StartedAt)
	}
}

func TestCreateBuildSyncWaitsAndReturnsJSON(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "nydus",
		"sync":       "true",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("expected JSON response, got %q: %v", rec.Body.String(), err)
	}
	if got["status"] != buildStatusSucceeded {
		t.Fatalf("expected sync response status succeeded, got %#v", got)
	}
	assertNoInternalResponseFields(t, got)
	id, ok := got["id"].(string)
	if !ok || id == "" {
		t.Fatalf("expected sync response id, got %#v", got)
	}
	if _, ok := server.store.get(id); !ok {
		t.Fatalf("expected sync task to remain queryable until keep ttl")
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/builds/"+id+"/logs", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected logs to remain available after sync response, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fake build complete") {
		t.Fatalf("expected logs endpoint to return build logs after sync response, got %q", rec.Body.String())
	}
}

func TestCleanupExpiredFinishedTask(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	server.cfg.KeepTTL = time.Second
	body, contentType := multipartZipRequest(t, map[string]string{
		"image":      "example.com/ns/repo:tag",
		"image_type": "nydus",
	}, map[string]string{"Dockerfile": "FROM scratch\n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTask
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(created.ID)
		if ok && task.Status == buildStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, ok := server.store.get(created.ID)
	if !ok || task.Status != buildStatusSucceeded {
		t.Fatalf("expected succeeded task before cleanup, got %#v ok=%v", task, ok)
	}
	if _, err := os.Stat(task.WorkDir); err != nil {
		t.Fatalf("expected work dir to exist before cleanup: %v", err)
	}

	server.cleanupExpiredTasks(task.FinishedAt.Add(server.cfg.KeepTTL - time.Nanosecond))
	if _, ok := server.store.get(created.ID); !ok {
		t.Fatalf("task was cleaned before keep ttl elapsed")
	}

	server.cleanupExpiredTasks(task.FinishedAt.Add(server.cfg.KeepTTL + time.Nanosecond))
	if _, ok := server.store.get(created.ID); ok {
		t.Fatalf("expected task to be removed after keep ttl")
	}
	if _, err := os.Stat(task.WorkDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected work dir to be removed after cleanup, got %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/builds/"+created.ID, nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected get after cleanup to return 404, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/builds/"+created.ID+"/logs", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected logs after cleanup to return 404, got %d", rec.Code)
	}
}

func newTestServer(t *testing.T, token string, runner buildRunner) *buildServer {
	t.Helper()
	addrs, err := parseBuildkitAddrs("127.0.0.1:9094")
	if err != nil {
		t.Fatal(err)
	}
	modes, err := parseBuildModes("", defaultBuildMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &buildServer{
		cfg:    Config{WorkDir: t.TempDir(), AuthToken: token, KeepTTL: defaultKeepTTL, MaxLogBytes: defaultMaxLogBytes},
		store:  newTaskStore(),
		pool:   newAddrPool(addrs, 4),
		runner: runner,
		images: &fakeImageChecker{result: imageCheckResult{Size: -1}},
		modes:  modes,
	}
}

func TestCreateBuildRejectsWhenRetainedTaskLimitReached(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	server.cfg.MaxRetainedTasks = 1
	server.store.add(&buildTask{ID: "existing", Status: buildStatusQueued})

	body, contentType := multipartZipRequest(t, map[string]string{"image": "example.com/ns/repo:tag"}, map[string]string{"Dockerfile": "FROM scratch\n"})
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected retained task limit to return 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBuildRejectsWhenWorkDirBudgetExhausted(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	server.cfg.MaxWorkDirBytes = 4
	if err := os.WriteFile(filepath.Join(server.cfg.WorkDir, "existing"), []byte("full"), 0o644); err != nil {
		t.Fatal(err)
	}

	body, contentType := multipartZipRequest(t, map[string]string{"image": "example.com/ns/repo:tag"}, map[string]string{"Dockerfile": "FROM scratch\n"})
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected work-dir budget exhaustion to return 507, got %d: %s", rec.Code, rec.Body.String())
	}
}

func createTestBuild(t *testing.T, server *buildServer, fields map[string]string) buildTaskView {
	t.Helper()
	body, contentType := multipartZipRequest(t, fields, map[string]string{"Dockerfile": "FROM scratch\n"})
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected create 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var created buildTaskView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func waitForTaskStatus(t *testing.T, server *buildServer, id, status string) *buildTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := server.store.get(id)
		if ok && task.Status == status {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := server.store.get(id)
	t.Fatalf("task %s did not reach %s: %#v", id, status, task)
	return nil
}

func assertBuildResponseOmitsInternalFields(t *testing.T, server *buildServer, id string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/builds/"+id, nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected get build status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var single map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	assertNoInternalResponseFields(t, single)

	req = httptest.NewRequest(http.MethodGet, "/v1/builds", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected list builds 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one listed build, got %d", len(list))
	}
	assertNoInternalResponseFields(t, list[0])

	req = httptest.NewRequest(http.MethodGet, "/v1/builds/"+id+"/logs", nil)
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected get build logs 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fake build complete") {
		t.Fatalf("expected logs endpoint to return build logs, got %q", rec.Body.String())
	}
}

func assertNoInternalResponseFields(t *testing.T, fields map[string]any) {
	t.Helper()
	for _, field := range []string{"log_path", "logs", "exporter_response"} {
		if _, ok := fields[field]; ok {
			t.Fatalf("response unexpectedly contains %q: %#v", field, fields)
		}
	}
}

func TestExtractZipEnforcesArchiveLimits(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "source.zip")
	var zipBuf bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuf)
	for name, content := range map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"payload.txt": "payload",
	} {
		writer, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zipBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "files"), 1<<20, 1); !errors.Is(err, errArchiveLimit) {
		t.Fatalf("expected file-count limit error, got %v", err)
	}
	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "bytes"), 4, 10); !errors.Is(err, errArchiveLimit) {
		t.Fatalf("expected extracted-size limit error, got %v", err)
	}
	if err := extractZip(zipPath, filepath.Join(t.TempDir(), "ok"), 1<<20, 10); err != nil {
		t.Fatalf("expected archive within limits to extract: %v", err)
	}
}

func TestLimitedLogWriterDiscardsOutputAfterLimit(t *testing.T) {
	var output bytes.Buffer
	writer := &limitedLogWriter{writer: &output, remaining: 5}

	for _, value := range []string{"abc", "def", "ghi"} {
		written, err := writer.Write([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if written != len(value) {
			t.Fatalf("Write returned %d, want %d", written, len(value))
		}
	}
	if got := output.String(); got != "abcde" {
		t.Fatalf("stored log is %q, want %q", got, "abcde")
	}
}

func multipartZipRequest(t *testing.T, fields map[string]string, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var zipBuf bytes.Buffer
	zipWriter := zip.NewWriter(&zipBuf)
	for name, content := range files {
		writer, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", "source.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(zipBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func TestCreateBuildRejectsChainedHeredocBeforeScheduling(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	body, contentType := multipartZipRequest(t, map[string]string{"image": "example.com/ns/repo:tag"}, map[string]string{
		"Dockerfile": "FROM alpine\nRUN mkdir -p /opt/demo && \\\n cat > /opt/demo/app.conf << 'EOF'\nlisten_port=5140\nEOF\n",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Dockerfile line 2:") || !strings.Contains(rec.Body.String(), "standalone RUN cat") {
		t.Fatalf("expected actionable 400 at submit time, got %d: %s", rec.Code, rec.Body.String())
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.requests) != 0 {
		t.Fatal("invalid Dockerfile reached the build runner")
	}
	entries, err := os.ReadDir(server.cfg.WorkDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected upload was not cleaned up: %v, %v", entries, err)
	}
}
