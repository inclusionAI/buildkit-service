//go:build linux && cgo

package buildbatch

import (
	"path/filepath"
	"testing"
)

func TestLMDBResultDBLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.lmdb")
	db, err := openLMDBResultDB(path)
	if err != nil {
		t.Fatal(err)
	}

	first := resultEntry{Target: "example.com/ns/first:tag", Success: false, Reason: "initial failure"}
	if err := db.Put(first); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.Get(first.Target)
	if err != nil || !found || got != first {
		t.Fatalf("initial Get() = %#v, %v, %v", got, found, err)
	}

	updated := resultEntry{Target: first.Target, Success: true, Elapsed: "1.2s"}
	second := resultEntry{Target: "example.com/ns/second:tag", Success: true, Elapsed: "2.3s"}
	if err := db.Put(updated); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(second); err != nil {
		t.Fatal(err)
	}
	entries, err := db.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("All() returned %d entries, want 2: %#v", len(entries), entries)
	}
	byTarget := make(map[string]resultEntry, len(entries))
	for _, entry := range entries {
		byTarget[entry.Target] = entry
	}
	if byTarget[updated.Target] != updated || byTarget[second.Target] != second {
		t.Fatalf("All() did not preserve overwritten and new entries: %#v", byTarget)
	}
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}

	reopened, err := openLMDBResultDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, found, err = reopened.Get(updated.Target)
	if err != nil || !found || got != updated {
		t.Fatalf("reopened Get() = %#v, %v, %v", got, found, err)
	}
}
