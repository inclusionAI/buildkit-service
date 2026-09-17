package builddaemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
