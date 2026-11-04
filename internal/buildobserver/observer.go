// Package buildobserver samples read-only BuildKit APIs independently of callers.
package buildobserver

import (
	"context"
	"io"
	"net/http"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Snapshot struct {
	Active         int
	Bytes, Records map[string]int64
}

// Summarize uses buildctl du's accounting: positive record sizes are summed,
// and !InUse is reclaimable. This is logical API accounting, not filesystem use.
func Summarize(usage []*client.UsageInfo) Snapshot {
	s := Snapshot{Bytes: map[string]int64{"in_use": 0, "reclaimable": 0}, Records: map[string]int64{"in_use": 0, "reclaimable": 0}}
	for _, r := range usage {
		state := "reclaimable"
		if r.InUse {
			state = "in_use"
		}
		s.Records[state]++
		if r.Size > 0 {
			s.Bytes[state] += r.Size
		}
	}
	return s
}

func Read(ctx context.Context, c *client.Client) (Snapshot, error) {
	usage, err := c.DiskUsage(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Summarize(usage)
	history, err := c.ControlClient().ListenBuildHistory(ctx, &controlapi.BuildHistoryRequest{ActiveOnly: true, EarlyExit: true})
	if err != nil {
		return Snapshot{}, err
	}
	active := map[string]bool{}
	for {
		event, err := history.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Snapshot{}, err
		}
		if event.Record != nil && event.Record.CompletedAt == nil {
			active[event.Record.Ref] = true
		}
	}
	snapshot.Active = len(active)
	return snapshot, nil
}

type Metrics struct {
	registry                   *prometheus.Registry
	up, last, duration, active prometheus.Gauge
	errors                     prometheus.Counter
	bytes, records             *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	m := &Metrics{registry: prometheus.NewRegistry()}
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
		m.registry.MustRegister(g)
		return g
	}
	m.up = gauge("buildkit_worker_observer_up", "Whether the last read-only API snapshot succeeded.")
	m.last = gauge("buildkit_worker_observer_last_success_timestamp_seconds", "Unix timestamp of the last successful snapshot.")
	m.duration = gauge("buildkit_worker_observer_poll_duration_seconds", "Duration of the latest API snapshot.")
	m.active = gauge("buildkit_worker_builds_active", "Active build history records across all clients of this worker; consult observer_up and freshness.")
	m.errors = prometheus.NewCounter(prometheus.CounterOpts{Name: "buildkit_worker_observer_errors_total", Help: "Failed read-only API snapshots."})
	m.bytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "buildkit_worker_cache_bytes", Help: "Logical cache record sizes reported by BuildKit; not physical filesystem bytes."}, []string{"state"})
	m.records = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "buildkit_worker_cache_records", Help: "Cache records reported by BuildKit."}, []string{"state"})
	m.registry.MustRegister(m.errors, m.bytes, m.records)
	return m
}

func (m *Metrics) Sample(ctx context.Context, read func(context.Context) (Snapshot, error)) {
	started := time.Now()
	s, err := read(ctx)
	m.duration.Set(time.Since(started).Seconds())
	if err != nil {
		m.up.Set(0)
		m.errors.Inc()
		return
	}
	for state, value := range s.Bytes {
		m.bytes.WithLabelValues(state).Set(float64(value))
	}
	for state, value := range s.Records {
		m.records.WithLabelValues(state).Set(float64(value))
	}
	m.active.Set(float64(s.Active))
	m.last.SetToCurrentTime()
	m.up.Set(1)
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
