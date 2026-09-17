package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// resultEntry is stored in LMDB and exported as JSONL.
type resultEntry struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Elapsed    string `json:"elapsed"`
	Target     string `json:"target"`
	NodeIP     string `json:"node_ip,omitempty"`
	Success    bool   `json:"success"`
	Logs       string `json:"logs,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (r *resultEntry) UnmarshalJSON(data []byte) error {
	type rawResultEntry struct {
		StartedAt  string          `json:"started_at"`
		FinishedAt string          `json:"finished_at"`
		Elapsed    json.RawMessage `json:"elapsed"`
		Target     string          `json:"target"`
		NodeIP     string          `json:"node_ip,omitempty"`
		Success    bool            `json:"success"`
		Logs       string          `json:"logs,omitempty"`
		Reason     string          `json:"reason,omitempty"`
	}

	var raw rawResultEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	elapsed, err := parseElapsedJSON(raw.Elapsed)
	if err != nil {
		return err
	}

	r.StartedAt = raw.StartedAt
	r.FinishedAt = raw.FinishedAt
	r.Elapsed = elapsed
	r.Target = raw.Target
	r.NodeIP = raw.NodeIP
	r.Success = raw.Success
	r.Logs = raw.Logs
	r.Reason = raw.Reason
	return nil
}

func parseElapsedJSON(data json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	var elapsedString string
	if err := json.Unmarshal(trimmed, &elapsedString); err == nil {
		return elapsedString, nil
	}

	var elapsedNumber json.Number
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&elapsedNumber); err != nil {
		return "", fmt.Errorf("decode elapsed: %w", err)
	}

	seconds, err := elapsedNumber.Float64()
	if err != nil {
		return "", fmt.Errorf("parse elapsed number: %w", err)
	}
	if seconds < 0 {
		return "", fmt.Errorf("parse elapsed number: negative value %v", seconds)
	}

	return formatElapsed(time.Duration(seconds * float64(time.Second))), nil
}

type resultStore struct {
	db   *lmdbResultDB
	logs *failureLogStore
}

type buildResultReader interface {
	Get(target string) (resultEntry, bool, error)
}

type buildResultWriter interface {
	UpsertResult(entry resultEntry) error
}

type buildOutcomeState uint8

const (
	buildOutcomeUnknown buildOutcomeState = iota
	buildOutcomeFailed
	buildOutcomeSucceeded
)

type buildOutcomeCounters struct {
	mu        sync.Mutex
	total     int
	succeeded int
	failed    int
	states    map[string]buildOutcomeState
}

func newBuildOutcomeCounters(total int, states map[string]buildOutcomeState) *buildOutcomeCounters {
	counters := &buildOutcomeCounters{
		total:  total,
		states: make(map[string]buildOutcomeState, len(states)),
	}
	for target, state := range states {
		counters.states[target] = state
		switch state {
		case buildOutcomeSucceeded:
			counters.succeeded++
		case buildOutcomeFailed:
			counters.failed++
		}
	}
	return counters
}

func (c *buildOutcomeCounters) apply(target string, success bool) (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.states[target]
	next := buildOutcomeFailed
	if success {
		next = buildOutcomeSucceeded
	}

	if previous != next {
		switch previous {
		case buildOutcomeSucceeded:
			c.succeeded--
		case buildOutcomeFailed:
			c.failed--
		}
		switch next {
		case buildOutcomeSucceeded:
			c.succeeded++
		case buildOutcomeFailed:
			c.failed++
		}
		c.states[target] = next
	}

	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) applyExisting(target string, state buildOutcomeState) (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.states[target]
	if previous == state {
		return c.succeeded, c.total, c.failed
	}
	switch previous {
	case buildOutcomeSucceeded:
		c.succeeded--
	case buildOutcomeFailed:
		c.failed--
	}
	switch state {
	case buildOutcomeSucceeded:
		c.succeeded++
	case buildOutcomeFailed:
		c.failed++
	}
	c.states[target] = state
	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) decrementTotal() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total > 0 {
		c.total--
	}
	return c.succeeded, c.total, c.failed
}

func (c *buildOutcomeCounters) snapshot() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.succeeded, c.total, c.failed
}

func newResultStore(resultPath, logsPath string) (*resultStore, error) {
	db, err := openLMDBResultDB(resultPath)
	if err != nil {
		return nil, err
	}
	return &resultStore{
		db:   db,
		logs: newFailureLogStore(logsPath),
	}, nil
}

func (rs *resultStore) UpsertResult(entry resultEntry) error {
	// Write failure log BEFORE writing to LMDB.
	if !entry.Success && entry.Logs != "" {
		if err := rs.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
			logError("Failed to append failure log for %s: %v", entry.Target, err)
		}
		// Clear verbose logs before persisting in LMDB to save space.
		entry.Logs = ""
	}
	return rs.db.Put(entry)
}

func persistBuildResult(store buildResultWriter, counters *buildOutcomeCounters, entry resultEntry) (int, int, int, error) {
	if err := store.UpsertResult(entry); err != nil {
		return 0, 0, 0, fmt.Errorf("store result for %s: %w", entry.Target, err)
	}
	succeeded, total, failed := counters.apply(entry.Target, entry.Success)
	return succeeded, total, failed, nil
}

func (rs *resultStore) Close() error {
	if rs.db != nil {
		return rs.db.Close()
	}
	return nil
}

func resolveOptionalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	return filepath.Abs(path)
}

func printBuildResult(entry resultEntry, succeededCount, totalCount, failedCount int) {
	status := "OK"
	if !entry.Success {
		status = "FAIL"
	}
	nodeIP := entry.NodeIP
	if strings.TrimSpace(nodeIP) == "" {
		nodeIP = "unknown"
	}
	logInfo("[%s] success=%d/%d fail=%d target=%s node-ip=%s elapsed=%s", status, succeededCount, totalCount, failedCount, entry.Target, nodeIP, entry.Elapsed)
}

func printSummary(results []resultEntry) {
	var succeeded, failed int
	for _, r := range results {
		if r.Success {
			succeeded++
		} else {
			failed++
		}
	}
	logInfo("Summary: %d total, %d succeeded, %d failed", len(results), succeeded, failed)
}
