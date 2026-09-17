package main

import (
	"encoding/json"
	"errors"
	"testing"
)

type stubBuildResultWriter struct {
	err error
}

func (s stubBuildResultWriter) UpsertResult(entry resultEntry) error {
	return s.err
}

type stubBuildResultReader struct {
	entries map[string]resultEntry
	err     error
}

func (s stubBuildResultReader) Get(target string) (resultEntry, bool, error) {
	if s.err != nil {
		return resultEntry{}, false, s.err
	}
	entry, ok := s.entries[target]
	return entry, ok, nil
}

func TestResultEntryUnmarshalPreservesElapsedCompatibility(t *testing.T) {
	for _, test := range []struct {
		name        string
		elapsedJSON string
		wantElapsed string
	}{
		{name: "string", elapsedJSON: `"2.5s"`, wantElapsed: "2.5s"},
		{name: "number", elapsedJSON: `1.25`, wantElapsed: "1.3s"},
		{name: "null", elapsedJSON: `null`, wantElapsed: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"started_at":"start","finished_at":"finish","elapsed":` + test.elapsedJSON + `,"target":"example.com/ns/repo:tag","node_ip":"10.0.0.1","success":true,"logs":"logs","reason":"reason"}`)
			var entry resultEntry
			if err := json.Unmarshal(payload, &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Elapsed != test.wantElapsed {
				t.Fatalf("elapsed = %q, want %q", entry.Elapsed, test.wantElapsed)
			}
			if entry.StartedAt != "start" || entry.FinishedAt != "finish" || entry.Target != "example.com/ns/repo:tag" || entry.NodeIP != "10.0.0.1" || !entry.Success || entry.Logs != "logs" || entry.Reason != "reason" {
				t.Fatalf("other result fields changed during compatibility decode: %#v", entry)
			}
		})
	}
}

func TestPersistBuildResultAppliesCounters(t *testing.T) {
	counters := newBuildOutcomeCounters(3, map[string]buildOutcomeState{})
	entry := resultEntry{Target: "target-a", Success: true}

	succeeded, total, failed, err := persistBuildResult(stubBuildResultWriter{}, counters, entry)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if succeeded != 1 || total != 3 || failed != 0 {
		t.Fatalf("unexpected counters: success=%d total=%d failed=%d", succeeded, total, failed)
	}
}

func TestPersistBuildResultReturnsErrorOnStoreFailure(t *testing.T) {
	counters := newBuildOutcomeCounters(1, map[string]buildOutcomeState{})
	entry := resultEntry{Target: "target-b", Success: true}

	_, _, _, err := persistBuildResult(stubBuildResultWriter{err: errors.New("boom")}, counters, entry)
	if err == nil {
		t.Fatal("expected store error")
	}
	if succeeded, total, failed := counters.snapshot(); succeeded != 0 || total != 1 || failed != 0 {
		t.Fatalf("counters changed unexpectedly: success=%d total=%d failed=%d", succeeded, total, failed)
	}
}
