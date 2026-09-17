package builddaemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
