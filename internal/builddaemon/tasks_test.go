package builddaemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

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
