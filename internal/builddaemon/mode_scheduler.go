package builddaemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const defaultBuildMode = "default_mode"

type buildModeConfig struct {
	Concurrency    int  `json:"concurrency"`
	RoutingEnabled bool `json:"routingEnabled"`
}

type buildModes struct {
	defaultMode string
	configs     map[string]buildModeConfig
	explicit    bool
}

func parseBuildModes(raw, defaultMode string, maxConcurrency int) (*buildModes, error) {
	defaultMode = strings.TrimSpace(defaultMode)
	if defaultMode == "" {
		defaultMode = defaultBuildMode
	}
	if !validBuildModeName(defaultMode) {
		return nil, fmt.Errorf("default build mode %q must contain only letters, digits, underscores, or hyphens", defaultMode)
	}

	modes := &buildModes{
		defaultMode: defaultMode,
		configs:     make(map[string]buildModeConfig),
	}
	if strings.TrimSpace(raw) == "" {
		modes.configs[defaultMode] = buildModeConfig{}
		return modes, nil
	}

	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&modes.configs); err != nil {
		return nil, fmt.Errorf("parse --modes-json: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse --modes-json: unexpected trailing JSON data")
	}
	if len(modes.configs) == 0 {
		return nil, fmt.Errorf("--modes-json must define at least one mode")
	}
	modes.explicit = true

	totalConcurrency := 0
	for name, cfg := range modes.configs {
		if !validBuildModeName(name) {
			return nil, fmt.Errorf("build mode %q must contain only letters, digits, underscores, or hyphens", name)
		}
		if cfg.Concurrency <= 0 {
			return nil, fmt.Errorf("build mode %q concurrency must be positive", name)
		}
		totalConcurrency += cfg.Concurrency
	}
	if _, ok := modes.configs[defaultMode]; !ok {
		return nil, fmt.Errorf("default build mode %q is not defined in --modes-json", defaultMode)
	}
	if maxConcurrency > 0 && totalConcurrency > maxConcurrency {
		return nil, fmt.Errorf("sum of build mode concurrency (%d) exceeds --max-concurrency (%d)", totalConcurrency, maxConcurrency)
	}
	return modes, nil
}

func validBuildModeName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func (m *buildModes) resolve(raw string) (string, buildModeConfig, error) {
	if m == nil {
		return "", buildModeConfig{}, fmt.Errorf("build modes are not configured")
	}
	name := strings.TrimSpace(raw)
	if name == "" {
		name = m.defaultMode
	}
	cfg, ok := m.configs[name]
	if !ok {
		return "", buildModeConfig{}, fmt.Errorf("unknown build mode %q", name)
	}
	return name, cfg, nil
}

func (m *buildModes) names() []string {
	if m == nil {
		return nil
	}
	names := make([]string, 0, len(m.configs))
	for name := range m.configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (m *buildModes) routingRequired() bool {
	if m == nil {
		return false
	}
	for _, cfg := range m.configs {
		if cfg.RoutingEnabled {
			return true
		}
	}
	return false
}

var errModeSchedulerClosed = errors.New("build mode scheduler is closed")

type buildModeScheduler struct {
	queues map[string]*buildTaskQueue
	run    func(string)
	once   sync.Once
}

func newBuildModeScheduler(modes *buildModes, run func(string)) (*buildModeScheduler, error) {
	if modes == nil || !modes.explicit {
		return nil, nil
	}
	if run == nil {
		return nil, fmt.Errorf("build mode scheduler requires a task runner")
	}

	scheduler := &buildModeScheduler{
		queues: make(map[string]*buildTaskQueue, len(modes.configs)),
		run:    run,
	}
	for _, name := range modes.names() {
		cfg := modes.configs[name]
		queue := newBuildTaskQueue()
		scheduler.queues[name] = queue
		for range cfg.Concurrency {
			go scheduler.runWorker(queue)
		}
	}
	return scheduler, nil
}

func (s *buildModeScheduler) enqueue(mode, id string) error {
	if s == nil {
		return fmt.Errorf("build mode scheduler is not configured")
	}
	queue, ok := s.queues[mode]
	if !ok {
		return fmt.Errorf("unknown build mode %q", mode)
	}
	return queue.push(id)
}

func (s *buildModeScheduler) close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		for _, queue := range s.queues {
			queue.close()
		}
	})
}

func (s *buildModeScheduler) runWorker(queue *buildTaskQueue) {
	for {
		id, ok := queue.pop()
		if !ok {
			return
		}
		s.run(id)
	}
}

type buildTaskQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	ids    []string
	closed bool
}

func newBuildTaskQueue() *buildTaskQueue {
	queue := &buildTaskQueue{}
	queue.cond = sync.NewCond(&queue.mu)
	return queue
}

func (q *buildTaskQueue) push(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errModeSchedulerClosed
	}
	q.ids = append(q.ids, id)
	q.cond.Signal()
	return nil
}

func (q *buildTaskQueue) pop() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.ids) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return "", false
	}
	id := q.ids[0]
	q.ids[0] = ""
	q.ids = q.ids[1:]
	return id, true
}

func (q *buildTaskQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.ids = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}
