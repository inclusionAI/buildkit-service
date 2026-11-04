// Package telemetry supplies optional OTLP tracing and correlated JSON lifecycle logs.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const Scope = "github.com/inclusionAI/buildkit-service"

// Start is called by executable entry points, never by library init functions.
// An absent exporter/endpoint leaves tracing disabled. Export is asynchronous:
// a slow or unavailable collector cannot block a build on a full span queue.
func Start(ctx context.Context, service string) (func(), error) {
	noop := func() {}
	exporter := os.Getenv("OTEL_TRACES_EXPORTER")
	if exporter == "none" || (exporter == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "") {
		return noop, nil
	}
	if exporter != "" && exporter != "otlp" {
		return noop, fmt.Errorf("unsupported OTEL_TRACES_EXPORTER %q", exporter)
	}
	proto := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if proto == "" {
		proto = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	var exp sdktrace.SpanExporter
	var err error
	switch proto {
	case "", "grpc":
		exp, err = otlptracegrpc.New(ctx)
	case "http/protobuf":
		exp, err = otlptracehttp.New(ctx)
	default:
		return noop, fmt.Errorf("unsupported OTLP traces protocol %q", proto)
	}
	if err != nil {
		return noop, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", service)), resource.WithFromEnv())
	if err != nil {
		_ = exp.Shutdown(ctx)
		return noop, err
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(shutdown)
	}, nil
}

// Event excludes source contents, build arguments and credentials. IDs belong in
// logs and spans, not metric labels. A new handler observes redirected stderr.
func Event(ctx context.Context, event string, fields ...any) {
	span := trace.SpanContextFromContext(ctx)
	if span.IsValid() {
		fields = append(fields, "trace_id", span.TraceID().String(), "span_id", span.SpanID().String())
	}
	slog.New(slog.NewJSONHandler(os.Stderr, nil)).InfoContext(ctx, event, fields...)
}

// TraceEnvironment connects buildctl subprocess spans to their batch parent.
func TraceEnvironment(ctx context.Context, env []string) []string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	for k, v := range carrier {
		env = append(env, strings.ToUpper(k)+"="+v)
	}
	return env
}
