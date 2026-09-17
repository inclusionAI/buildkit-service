package builddaemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	buildStatusQueued    = "queued"
	buildStatusRunning   = "running"
	buildStatusSucceeded = "succeeded"
	buildStatusFailed    = "failed"
	buildStatusCanceled  = "canceled"
)

type buildTask struct {
	ID               string            `json:"id"`
	Status           string            `json:"status"`
	Image            string            `json:"image"`
	Mode             string            `json:"mode"`
	RoutedTarget     string            `json:"routed_target"`
	ImageType        string            `json:"image_type"`
	Target           string            `json:"target,omitempty"`
	BuildkitAddr     string            `json:"buildkitd_addr,omitempty"`
	NodeIP           string            `json:"node_ip,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	Error            string            `json:"error,omitempty"`
	LogPath          string            `json:"log_path,omitempty"`
	WorkDir          string            `json:"work_dir,omitempty"`
	ExporterResponse map[string]string `json:"exporter_response,omitempty"`

	contextDir    string
	scheduleKey   string
	routingDigest string
	noCache       bool
	retry         int
	retryInterval time.Duration
	buildArgs     map[string]string
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
}

type buildTaskView struct {
	ID                   string     `json:"id"`
	Status               string     `json:"status"`
	Image                string     `json:"image"`
	Mode                 string     `json:"mode"`
	RoutedTarget         string     `json:"routed_target"`
	ImageType            string     `json:"image_type"`
	Target               string     `json:"target,omitempty"`
	BuildkitAddr         string     `json:"buildkitd_addr,omitempty"`
	NodeIP               string     `json:"node_ip,omitempty"`
	Retry                int        `json:"retry"`
	RetryIntervalSeconds int        `json:"retry_interval_seconds"`
	CreatedAt            time.Time  `json:"created_at"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	Error                string     `json:"error,omitempty"`
}

type taskStore struct {
	mu    sync.RWMutex
	tasks map[string]*buildTask
}

func newTaskStore() *taskStore {
	return &taskStore{tasks: make(map[string]*buildTask)}
}

func (s *taskStore) add(task *buildTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
}

func (s *taskStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tasks)
}

func (s *taskStore) get(id string) (*buildTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	return cloneTask(task), true
}

func (s *taskStore) countByStatus(status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, task := range s.tasks {
		if task.Status == status {
			n++
		}
	}
	return n
}

func (s *taskStore) countByModeAndStatus(mode, status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, task := range s.tasks {
		if task.Mode == mode && task.Status == status {
			n++
		}
	}
	return n
}

func (s *taskStore) list() []*buildTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*buildTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, cloneTask(task))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *taskStore) update(id string, fn func(*buildTask)) (*buildTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	fn(task)
	return cloneTask(task), true
}

func (s *taskStore) cancel(id string) (*buildTask, bool) {
	var cancel context.CancelFunc
	snapshot, ok := s.update(id, func(task *buildTask) {
		if task.Status != buildStatusQueued && task.Status != buildStatusRunning {
			return
		}
		now := time.Now()
		task.Status = buildStatusCanceled
		task.FinishedAt = &now
		cancel = task.cancel
	})
	if cancel != nil {
		cancel()
	}
	return snapshot, ok
}

func (s *taskStore) delete(id string) (*buildTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	snapshot := cloneTask(task)
	delete(s.tasks, id)
	return snapshot, true
}

func (s *taskStore) deleteExpired(now time.Time, keepTTL time.Duration) []*buildTask {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*buildTask
	for id, task := range s.tasks {
		if task == nil || task.FinishedAt == nil {
			continue
		}
		if now.Before(task.FinishedAt.Add(keepTTL)) {
			continue
		}
		expired = append(expired, cloneTask(task))
		delete(s.tasks, id)
	}
	return expired
}

func cloneTask(task *buildTask) *buildTask {
	clone := *task
	clone.buildArgs = cloneStringMap(task.buildArgs)
	clone.ExporterResponse = cloneStringMap(task.ExporterResponse)
	return &clone
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func viewTask(task *buildTask) buildTaskView {
	if task == nil {
		return buildTaskView{}
	}
	return buildTaskView{
		ID:                   task.ID,
		Status:               task.Status,
		Image:                task.Image,
		Mode:                 task.Mode,
		RoutedTarget:         task.RoutedTarget,
		ImageType:            task.ImageType,
		Target:               task.Target,
		BuildkitAddr:         task.BuildkitAddr,
		NodeIP:               task.NodeIP,
		Retry:                task.retry,
		RetryIntervalSeconds: int(task.retryInterval / time.Second),
		CreatedAt:            task.CreatedAt,
		StartedAt:            task.StartedAt,
		FinishedAt:           task.FinishedAt,
		Error:                task.Error,
	}
}

func viewTasks(tasks []*buildTask) []buildTaskView {
	views := make([]buildTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, viewTask(task))
	}
	return views
}

func (s *buildServer) runBuildTask(id string) {
	defer func() {
		s.store.update(id, func(task *buildTask) {
			if task.done != nil {
				close(task.done)
				task.done = nil
			}
		})
	}()

	snapshot, ok := s.store.get(id)
	if !ok {
		return
	}
	if snapshot.Status == buildStatusCanceled {
		return
	}

	logFile, err := os.OpenFile(snapshot.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.finishTask(id, buildStatusFailed, nil, err)
		return
	}
	defer logFile.Close()
	maxLogBytes := s.cfg.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	logWriter := &limitedLogWriter{writer: logFile, remaining: maxLogBytes}
	if snapshot.RoutedTarget != "" && snapshot.RoutedTarget != snapshot.Image {
		fmt.Fprintf(logWriter, "route algorithm=rendezvous_hash context_sha256=%s source=%s routed_target=%s\n", snapshot.routingDigest, snapshot.Image, snapshot.RoutedTarget)
	}

	buildImage := snapshot.RoutedTarget
	if buildImage == "" {
		buildImage = snapshot.Image
	}
	steps := buildStepsForImageType(buildImage, snapshot.ImageType)
	var exporter map[string]string
	for _, step := range steps {
		if step.BaseImage != "" {
			contextDir, baseImage, err := createNydusFromOCIContext(snapshot.WorkDir, step.BaseImage, exporter)
			if err != nil {
				s.finishTask(id, buildStatusFailed, exporter, err)
				return
			}
			step.ContextDir = contextDir
			fmt.Fprintf(logWriter, "build nydus from OCI image %s\n", baseImage)
		}
		var err error
		for attempt := 0; attempt <= snapshot.retry; attempt++ {
			if attempt > 0 {
				s.markTaskQueued(id)
				fmt.Fprintf(logWriter, "retry build %s image %s attempt %d/%d after error: %v\n", step.Format, step.Image, attempt, snapshot.retry, err)
				if !waitRetryInterval(snapshot.ctx, snapshot.retryInterval) {
					s.finishTask(id, buildStatusCanceled, exporter, snapshot.ctx.Err())
					return
				}
			}
			exporter, err = s.runBuildStepAttempt(id, snapshot, step, logWriter)
			if err == nil {
				break
			}
			if snapshot.ctx.Err() != nil {
				s.finishTask(id, buildStatusCanceled, exporter, snapshot.ctx.Err())
				return
			}
			if isDeterministicBuildError(err) {
				break
			}
		}
		if err != nil {
			s.finishTask(id, buildStatusFailed, exporter, err)
			return
		}
	}
	s.finishTask(id, buildStatusSucceeded, exporter, nil)
}

func (s *buildServer) runBuildStepAttempt(id string, snapshot *buildTask, step buildStep, logFile io.Writer) (map[string]string, error) {
	contextDir := snapshot.contextDir
	target := snapshot.Target
	buildArgs := snapshot.buildArgs
	if step.ContextDir != "" {
		contextDir = step.ContextDir
		target = ""
		buildArgs = nil
	}

	excludedAddrs := make(map[string]bool)
	for {
		s.markTaskQueued(id)
		releaseGlobal, err := s.acquireGlobalSlot(snapshot.ctx)
		if err != nil {
			return nil, err
		}
		slot, err := s.acquireAddrSlot(snapshot.ctx, snapshot.scheduleKey, excludedAddrs)
		if err != nil {
			releaseGlobal()
			return nil, err
		}

		started := time.Now()
		s.store.update(id, func(task *buildTask) {
			if task.StartedAt == nil {
				task.StartedAt = &started
			}
			task.Status = buildStatusRunning
			task.BuildkitAddr = slot.addr.addr
			task.NodeIP = slot.addr.nodeIP
		})
		fmt.Fprintf(logFile, "build %s image %s on %s\n", step.Format, step.Image, slot.addr.addr)
		exporter, err := s.runner.Build(snapshot.ctx, buildRunRequest{
			ContextDir:   contextDir,
			BuildkitAddr: slot.addr.addr,
			Image:        step.Image,
			Format:       step.Format,
			Target:       target,
			NoCache:      snapshot.noCache,
			BuildArgs:    buildArgs,
		}, logFile)
		<-slot.sem
		releaseGlobal()
		if err == nil || snapshot.ctx.Err() != nil {
			return exporter, err
		}
		if !isRetryableBuildkitAddrError(err, slot.addr.addr) {
			return exporter, err
		}
		excludedAddrs[slot.addr.addr] = true
		fmt.Fprintf(logFile, "retryable buildkit address error on %s: %v; refreshing addresses and trying another worker\n", slot.addr.addr, err)
		if refreshErr := s.refreshBuildkitAddrs(); refreshErr != nil {
			fmt.Fprintf(logFile, "refresh buildkit addresses failed: %v\n", refreshErr)
		}
	}
}

func (s *buildServer) markTaskQueued(id string) {
	s.store.update(id, func(task *buildTask) {
		if task.Status != buildStatusQueued && task.Status != buildStatusRunning {
			return
		}
		task.Status = buildStatusQueued
		task.BuildkitAddr = ""
		task.NodeIP = ""
	})
}

func waitRetryInterval(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *buildServer) finishTask(id, status string, exporter map[string]string, err error) {
	finished := time.Now()
	var applied string
	s.store.update(id, func(task *buildTask) {
		if task.Status == buildStatusCanceled && status != buildStatusCanceled {
			return
		}
		task.Status = status
		task.FinishedAt = &finished
		task.ExporterResponse = cloneStringMap(exporter)
		if err != nil {
			task.Error = err.Error()
		}
		applied = status
	})
	switch applied {
	case buildStatusSucceeded:
		atomic.AddInt64(&s.metrics.succeeded, 1)
	case buildStatusFailed:
		atomic.AddInt64(&s.metrics.failed, 1)
	}
}

func (s *buildServer) startTaskReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(defaultCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.cleanupExpiredTasks(now)
			}
		}
	}()
}

func (s *buildServer) cleanupExpiredTasks(now time.Time) {
	expired := s.store.deleteExpired(now, s.cfg.KeepTTL)
	for _, task := range expired {
		s.cleanupTaskWorkDir(task)
	}
}

func (s *buildServer) cleanupTask(id string) {
	task, ok := s.store.delete(id)
	if !ok {
		return
	}
	s.cleanupTaskWorkDir(task)
}

func (s *buildServer) cleanupTaskWorkDir(task *buildTask) {
	if task == nil || strings.TrimSpace(task.WorkDir) == "" {
		return
	}
	if err := os.RemoveAll(task.WorkDir); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup task %s work dir %s: %v\n", task.ID, task.WorkDir, err)
	}
}

func (s *buildServer) handleSyncBuild(c echo.Context, id string, done <-chan struct{}) error {
	select {
	case <-done:
	case <-c.Request().Context().Done():
		return nil
	}
	task, ok := s.store.get(id)
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusOK, viewTask(task))
}
