package builddaemon

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
