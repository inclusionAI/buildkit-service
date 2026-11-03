package builddaemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inclusionAI/buildkit-service/pkg/telemetry"
	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Counters survive task TTL cleanup; registries are per server, not global.
type daemonMetrics struct {
	succeeded       int64
	failed          int64
	once            sync.Once
	registry        *prometheus.Registry
	duration        *prometheus.HistogramVec
	queue           *prometheus.HistogramVec
	attempt         *prometheus.HistogramVec
	retries         *prometheus.CounterVec
	canceled        prometheus.Counter
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func (m *daemonMetrics) init() {
	m.once.Do(func() {
		m.registry = prometheus.NewRegistry()
		buckets := []float64{0.01, .1, .5, 1, 5, 15, 30, 60, 120, 300, 900, 3600}
		m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "buildctl_daemon_build_duration_seconds", Help: "Time from admission to terminal state, including queueing and retries.", Buckets: buckets}, []string{"mode", "result", "image_type"})
		m.queue = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "buildctl_daemon_queue_wait_seconds", Help: "Time from admission to the first worker assignment.", Buckets: buckets}, []string{"mode"})
		m.attempt = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "buildctl_daemon_build_attempt_duration_seconds", Help: "Duration of an individual BuildKit solve attempt.", Buckets: buckets}, []string{"format", "result"})
		m.retries = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "buildctl_daemon_retries_total", Help: "Retries partitioned by bounded reason."}, []string{"reason"})
		m.canceled = prometheus.NewCounter(prometheus.CounterOpts{Name: "buildctl_daemon_builds_canceled_total", Help: "Builds canceled, including queued builds and deadlines."})
		m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "buildctl_daemon_http_requests_total", Help: "HTTP requests by route template, method and status."}, []string{"route", "method", "code"})
		m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "buildctl_daemon_http_request_duration_seconds", Help: "HTTP request duration by route template.", Buckets: buckets}, []string{"route", "method"})
		m.registry.MustRegister(m.duration, m.queue, m.attempt, m.retries, m.canceled, m.requests, m.requestDuration, prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	})
}

func (s *buildServer) extraMetrics(out *strings.Builder) error {
	s.metrics.init()
	families, err := s.metrics.registry.Gather()
	if err != nil {
		return err
	}
	for _, family := range families {
		if _, err := expfmt.MetricFamilyToText(out, family); err != nil {
			return err
		}
	}
	gauge := func(name, help string, value float64) {
		fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, value)
	}
	gauge("buildctl_daemon_max_concurrency", "Configured global execution slot limit; zero means unlimited.", float64(s.cfg.MaxConcurrency))
	gauge("buildctl_daemon_retained_tasks", "Tasks currently retained in memory.", float64(s.store.len()))
	oldest := 0.0
	for _, task := range s.store.list() {
		if task.Status == buildStatusQueued {
			since := task.queuedAt
			if since.IsZero() {
				since = task.CreatedAt
			}
			if !since.IsZero() {
				oldest = max(oldest, time.Since(since).Seconds())
			}
		}
	}
	gauge("buildctl_daemon_queue_oldest_age_seconds", "Age of the oldest currently queued interval.", oldest)
	out.WriteString("# HELP buildctl_daemon_worker_slots_active Execution slots held per worker, including every caller of this daemon only.\n# TYPE buildctl_daemon_worker_slots_active gauge\n# HELP buildctl_daemon_worker_slots_capacity Configured execution slots per discovered worker.\n# TYPE buildctl_daemon_worker_slots_capacity gauge\n")
	if s.pool != nil {
		for _, slot := range s.pool.snapshot().slots {
			fmt.Fprintf(out, "buildctl_daemon_worker_slots_active{worker=%q} %d\nbuildctl_daemon_worker_slots_capacity{worker=%q} %d\n", slot.addr.addr, len(slot.sem), slot.addr.addr, cap(slot.sem))
		}
	}
	return nil
}

func (s *buildServer) observeHTTP(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		route := c.Path()
		if route == "" {
			route = "unmatched"
		}
		if route == "/metrics" || route == "/healthz" {
			return next(c)
		}
		method := c.Request().Method
		switch method {
		case "GET", "POST", "HEAD", "DELETE", "PUT", "PATCH", "OPTIONS":
		default:
			method = "other"
		}
		ctx := propagation.TraceContext{}.Extract(c.Request().Context(), propagation.HeaderCarrier(c.Request().Header))
		ctx, span := otel.Tracer(telemetry.Scope).Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.route", route), attribute.String("http.request.method", method)))
		defer span.End()
		c.SetRequest(c.Request().WithContext(ctx))
		started := time.Now()
		err := next(c)
		code := c.Response().Status
		if err != nil {
			code = http.StatusInternalServerError
			var he *echo.HTTPError
			if errors.As(err, &he) {
				code = he.Code
			}
		}
		if code >= 500 {
			span.SetStatus(codes.Error, "server error")
		}
		span.SetAttributes(attribute.Int("http.response.status_code", code))
		s.metrics.init()
		s.metrics.requests.WithLabelValues(route, method, strconv.Itoa(code)).Inc()
		s.metrics.requestDuration.WithLabelValues(route, method).Observe(time.Since(started).Seconds())
		return err
	}
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case isDeterministicBuildError(err):
		return "deterministic"
	default:
		return "infrastructure"
	}
}

func (s *buildServer) observeTerminal(task *buildTask, err error) {
	s.metrics.init()
	mode := task.Mode
	if mode == "" {
		mode = defaultBuildMode
	}
	format := task.ImageType
	if format == "" {
		format = "unknown"
	}
	if task.Status == buildStatusCanceled {
		s.metrics.canceled.Inc()
	}
	if !task.CreatedAt.IsZero() {
		s.metrics.duration.WithLabelValues(mode, task.Status, format).Observe(time.Since(task.CreatedAt).Seconds())
	}
	ctx := task.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	telemetry.Event(ctx, "build_finished", "build_id", task.ID, "mode", mode, "result", task.Status, "error_class", outcome(err), "worker", task.BuildkitAddr)
	if task.queueSpan != nil {
		task.queueSpan.End()
	}
	if task.span != nil {
		task.span.SetAttributes(attribute.String("build.result", task.Status))
		if err != nil {
			task.span.SetStatus(codes.Error, outcome(err))
		}
		task.span.End()
	}
}
