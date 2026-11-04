package builddaemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestAsyncTraceSurvivesRequestAndTaskCleanup(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) }()
	block := make(chan struct{})
	server := newTestServer(t, "", &fakeRunner{block: block})
	body, ct := multipartZipRequest(t, map[string]string{"image": "registry.example/test:private", "image_type": "oci"}, map[string]string{"Dockerfile": "FROM scratch\n"})
	requestCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/builds", body).WithContext(requestCtx)
	req.Header.Set("Content-Type", ct)
	const traceID = "00112233445566778899aabbccddeeff"
	req.Header.Set("traceparent", "00-"+traceID+"-0011223344556677-01")
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var task buildTaskView
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	cancel()
	if task.TraceID != traceID {
		t.Fatalf("lost caller trace: %q", task.TraceID)
	}
	close(block)
	terminal := waitForTaskStatus(t, server, task.ID, buildStatusSucceeded)
	server.finishTask(task.ID, buildStatusSucceeded, nil, nil)
	server.cleanupExpiredTasks(time.Now().Add(defaultKeepTTL + time.Hour))
	if server.store.len() != 0 {
		t.Fatal("cleanup did not run")
	}
	server.metrics.init()
	if n := counterValue(t, server.metrics.canceled); n != 0 {
		t.Fatal("asynchronous request cancellation canceled build")
	}
	var out strings.Builder
	if err := server.extraMetrics(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `buildctl_daemon_build_duration_seconds_count{image_type="oci",mode="default_mode",result="succeeded"} 1`) {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), "registry.example") || strings.Contains(out.String(), task.ID) {
		t.Fatal("unbounded identifier in metric labels")
	}
	for until := time.Now().Add(time.Second); ; {
		found := false
		for _, span := range exporter.GetSpans() {
			if span.Name == "build" && span.SpanContext.TraceID().String() == traceID {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("terminal task span missing: %#v", terminal)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueuedCancelEndsObservationOnce(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := &buildTask{ID: "queued", Mode: defaultBuildMode, ImageType: "oci", Status: buildStatusQueued, CreatedAt: time.Now().Add(-time.Second), ctx: ctx, cancel: cancel}
	server.store.add(task)
	for range 2 {
		req := httptest.NewRequest(http.MethodDelete, "/v1/builds/queued", nil)
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatal(rec.Code)
		}
	}
	server.finishTask(task.ID, buildStatusCanceled, nil, context.Canceled)
	server.metrics.init()
	if n := counterValue(t, server.metrics.canceled); n != 1 {
		t.Fatalf("cancellation counted %g times", n)
	}
}

func TestHTTPMetricsUseRouteTemplates(t *testing.T) {
	server := newTestServer(t, "", &fakeRunner{})
	for _, id := range []string{"private-id-a", "private-id-b"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/builds/"+id, nil)
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
	}
	var out strings.Builder
	if err := server.extraMetrics(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-id") || !strings.Contains(out.String(), `route="/v1/builds/:id"`) {
		t.Fatal(out.String())
	}
}

func counterValue(t *testing.T, c prometheus.Metric) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestCanceledTaskCannotBeReassignedAfterObservation(t *testing.T) {
	runner := &fakeRunner{}
	server := newTestServer(t, "", runner)
	task := &buildTask{ID: "canceled", Mode: defaultBuildMode, Status: buildStatusCanceled, observed: true, ctx: context.Background()}
	server.store.add(task)
	_, err := server.runBuildStepAttempt(task.ID, task, buildStep{}, &strings.Builder{})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
	current, _ := server.store.get(task.ID)
	if current.Status != buildStatusCanceled || len(runner.snapshot()) != 0 {
		t.Fatal("canceled task reassigned")
	}
}
