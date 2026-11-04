package telemetry

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestExporterIsOptInAndRejectsInvalidConfiguration(t *testing.T) {
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_TRACES_EXPORTER"} {
		t.Setenv(key, "")
	}
	shutdown, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	shutdown()
	t.Setenv("OTEL_TRACES_EXPORTER", "not-supported")
	if _, err := Start(context.Background(), "test"); err == nil {
		t.Fatal("unsupported exporter accepted")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "not-supported")
	if _, err := Start(context.Background(), "test"); err == nil {
		t.Fatal("unsupported protocol accepted")
	}
}

func TestBatchSubprocessTraceContext(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(context.Background())
	ctx, span := provider.Tracer("test").Start(context.Background(), "batch")
	defer span.End()
	env := TraceEnvironment(ctx, []string{"PATH=/usr/bin"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "TRACEPARENT=00-"+span.SpanContext().TraceID().String()) || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatal(env)
	}
}
