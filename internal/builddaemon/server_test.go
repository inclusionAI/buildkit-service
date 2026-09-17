package builddaemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
