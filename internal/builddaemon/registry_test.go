package builddaemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
)

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
	checker := newTestRegistryImageChecker(t, registryHost, time.Second, "")
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
	checker := newTestRegistryImageChecker(t, registryHost, time.Second, dockerConfigDir)
	result, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != digest || result.Size != 789 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRegistryImageCheckerRejectsUnlistedRegistry(t *testing.T) {
	checker := newTestRegistryImageChecker(t, "registry.example.com", time.Second, "")
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
	checker := newTestRegistryImageChecker(t, registryHost, time.Second, "")
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
	checker := newTestRegistryImageChecker(t, registryHost, time.Second, "")
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
	checker := newTestRegistryImageChecker(t, registryHost, time.Second, "")
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
	checker := newTestRegistryImageChecker(t, registryHost, 20*time.Millisecond, "")
	_, err := checker.Check(context.Background(), registryHost+"/ns/repo:tag")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func newTestRegistryImageChecker(t *testing.T, host string, timeout time.Duration, configDir string) *registryImageChecker {
	t.Helper()
	if configDir == "" {
		configDir = t.TempDir()
	}
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
