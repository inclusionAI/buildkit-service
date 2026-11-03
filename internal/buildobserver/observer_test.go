package buildobserver

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/buildkit/client"
)

func TestCacheAccountingAndFailedPollKeepsFreshness(t *testing.T) {
	snapshot := Summarize([]*client.UsageInfo{{Size: 100, InUse: true}, {Size: 200}, {Size: -1}})
	if snapshot.Bytes["in_use"] != 100 || snapshot.Bytes["reclaimable"] != 200 || snapshot.Records["reclaimable"] != 2 {
		t.Fatal(snapshot)
	}
	m := NewMetrics()
	m.Sample(context.Background(), func(context.Context) (Snapshot, error) { return snapshot, nil })
	m.Sample(context.Background(), func(context.Context) (Snapshot, error) { return Snapshot{}, errors.New("unavailable") })
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	text := rec.Body.String()
	for _, want := range []string{"buildkit_worker_observer_up 0", "buildkit_worker_observer_errors_total 1", `buildkit_worker_cache_bytes{state="reclaimable"} 200`} {
		if !strings.Contains(text, want) {
			t.Fatal(text)
		}
	}
}
