package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"
)

func runBuildCommand(opts options) error {
	if opts.ctx == nil {
		resetCommandStartTime(time.Now())
	}

	if len(opts.addrs) == 0 {
		return fmt.Errorf("--addrs is required")
	}

	buildModes, err := resolveBuildModes(opts.oci, opts.bothFormats)
	if err != nil {
		return err
	}
	if opts.imageDirs != "" {
		return runBuildCommandStreamingImageDirs(opts, buildModes)
	}

	specs, cleanup, err := loadBuildSpecs(opts.imageDirs, opts.target, buildModes, opts.vars)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	if len(specs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	jobs := groupBuildSpecs(specs)
	if len(jobs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	store, err := newResultStore(opts.resultPath, opts.logsPath)
	if err != nil {
		return err
	}
	defer store.Close()

	totalTargets := len(jobs)
	jobs, existingOutcomes, skippedSucceeded, skippedFailed, err := filterBuildJobs(jobs, store.db, opts.skipFail)
	if err != nil {
		return err
	}

	if skippedSucceeded > 0 || skippedFailed > 0 {
		logInfo("Skipped %d previously successful target(s) and %d previously failed target(s)", skippedSucceeded, skippedFailed)
	}
	if len(jobs) == 0 {
		logInfo("No targets to build")
		return nil
	}

	resetLogProgress(len(jobs))
	defer clearLogProgress()

	logInfo("Building %d target(s) across %d address(es), concurrency=%d, retry=%d",
		len(jobs), len(opts.addrs), opts.concurrency, opts.retry)

	addrPool := newAddrPool(opts.addrs, opts.concurrency)
	globalSem := make(chan struct{}, opts.concurrency)
	outcomeCounters := newBuildOutcomeCounters(totalTargets, existingOutcomes)

	baseCtx := opts.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	startBuildkitAddrRefresher(ctx, addrPool, opts.addrsRaw, opts.oomCooldown)

	if opts.ctx == nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			logInfo("Received interrupt, cancelling builds...")
			cancel()
		}()
	}

	type buildTask struct {
		job     buildJob
		attempt int
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []resultEntry
		failed   atomic.Bool
		fatalMu  sync.Mutex
		fatalErr error
	)

	recordFatalErr := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
	}

	// retryQueue collects failed tasks that still have retry budget.
	retryQueue := make(chan buildTask, len(jobs))

	// scheduleTask picks a slot via consistent hash and launches the build goroutine.
	scheduleTask := func(task buildTask) bool {
		if failed.Load() && opts.failfast {
			return false
		}
		if ctx.Err() != nil {
			return false
		}

		slot := waitForAvailableAddrSlot(ctx, addrPool, task.job.scheduleKey)
		if slot == nil {
			return false
		}
		if !acquireGlobalSlotOrReleaseWorker(ctx, globalSem, slot) {
			return false
		}

		wg.Add(1)
		go func(task buildTask, slot *addrSlot) {
			defer wg.Done()
			defer func() { <-globalSem }()
			defer func() { <-slot.sem }()

			taskStartedAt := time.Now()
			taskEntry := resultEntry{Target: task.job.key}
			taskSuccess := true

			for idx, spec := range task.job.specs {
				entry := executeBuild(ctx, spec, slot.addr, opts)
				taskEntry.NodeIP = entry.NodeIP

				if !entry.Success && task.attempt < opts.retry {
					if entry.Logs != "" {
						if err := store.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
							logError("Failed to append failure log for %s: %v", entry.Target, err)
						}
					}
					remainingSpecs := append([]buildSpec(nil), task.job.specs[idx:]...)
					logInfo("[RETRY %d/%d] %s", task.attempt+1, opts.retry, entry.Target)
					retryQueue <- buildTask{
						job: buildJob{
							key:         task.job.key,
							scheduleKey: task.job.scheduleKey,
							specs:       remainingSpecs,
						},
						attempt: task.attempt + 1,
					}
					return
				}

				if err := store.UpsertResult(entry); err != nil {
					persistErr := fmt.Errorf("store result for %s: %w", entry.Target, err)
					logError("Failed to store result for %s: %v", entry.Target, err)
					recordFatalErr(persistErr)
					failed.Store(true)
					cancel()
					return
				}

				if !entry.Success {
					taskSuccess = false
					break
				}
			}

			advanceLogProgress()

			taskEntry.Success = taskSuccess
			taskEntry.Elapsed = formatElapsed(time.Since(taskStartedAt))
			succeededCount, totalCount, failedCount := outcomeCounters.apply(task.job.key, taskSuccess)

			mu.Lock()
			results = append(results, taskEntry)
			mu.Unlock()

			printBuildResult(taskEntry, succeededCount, totalCount, failedCount)

			if !taskSuccess {
				failed.Store(true)
				if opts.failfast {
					cancel()
				}
			}
		}(task, slot)

		return true
	}

	// First pass: schedule all initial tasks.
	for _, job := range jobs {
		if !scheduleTask(buildTask{job: job, attempt: 0}) {
			break
		}
	}

	// Drain retry queue: wait for in-flight builds to finish, then re-schedule retries.
	for {
		wg.Wait()
		select {
		case task := <-retryQueue:
			// There may be more queued retries; drain them all before waiting again.
			tasks := []buildTask{task}
			for {
				select {
				case t := <-retryQueue:
					tasks = append(tasks, t)
				default:
					goto schedule
				}
			}
		schedule:
			for _, t := range tasks {
				if !scheduleTask(t) {
					break
				}
			}
		default:
			// No retries pending, we're done.
			goto done
		}
	}
done:

	printSummary(results)

	fatalMu.Lock()
	defer fatalMu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}

	if failed.Load() {
		return fmt.Errorf("one or more builds failed")
	}
	return nil
}

func runBuildCommandStreamingImageDirs(opts options, buildModes []bool) error {
	store, err := newResultStore(opts.resultPath, opts.logsPath)
	if err != nil {
		return err
	}
	defer store.Close()

	entries, err := os.ReadDir(opts.imageDirs)
	if err != nil {
		return fmt.Errorf("read image-dirs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("image-dirs %s does not contain any image directories", opts.imageDirs)
	}

	preparedRoot, err := os.MkdirTemp("", "buildctl-batch-image-dirs-")
	if err != nil {
		return fmt.Errorf("create temp image-dirs: %w", err)
	}
	defer os.RemoveAll(preparedRoot)

	targetFilters := make(map[string]struct{}, len(buildModes))
	if strings.TrimSpace(opts.target) != "" {
		for _, oci := range buildModes {
			targetFilters[normalizeTargetForMode(opts.target, oci)] = struct{}{}
		}
	}

	baseCtx := opts.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	if opts.ctx == nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			logInfo("Received interrupt, cancelling builds...")
			cancel()
		}()
	}

	resetLogProgress(len(entries))
	defer clearLogProgress()

	logInfo("Preparing %d source context(s) from %s with streaming scheduling", len(entries), opts.imageDirs)
	logInfo("Building up to %d target(s) across %d address(es), concurrency=%d, retry=%d",
		len(entries), len(opts.addrs), opts.concurrency, opts.retry)

	addrPool := newAddrPool(opts.addrs, opts.concurrency)
	startBuildkitAddrRefresher(ctx, addrPool, opts.addrsRaw, opts.oomCooldown)
	globalSem := make(chan struct{}, opts.concurrency)
	outcomeCounters := newBuildOutcomeCounters(len(entries), nil)

	type buildTask struct {
		job     buildJob
		attempt int
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []resultEntry
		failed   atomic.Bool
		fatalMu  sync.Mutex
		fatalErr error
	)

	recordFatalErr := func(err error) {
		if err == nil {
			return
		}
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
	}

	retryQueue := make(chan buildTask, len(entries)+1)
	jobCh := make(chan buildJob, max(1, opts.concurrency))
	prepareErrCh := make(chan error, 1)

	go func() {
		defer close(jobCh)
		prepareErrCh <- streamPreparedBuildJobs(ctx, opts, buildModes, targetFilters, entries, preparedRoot, store.db, outcomeCounters, jobCh)
	}()

	scheduleTask := func(task buildTask) bool {
		if failed.Load() && opts.failfast {
			return false
		}
		if ctx.Err() != nil {
			return false
		}

		slot := waitForAvailableAddrSlot(ctx, addrPool, task.job.scheduleKey)
		if slot == nil {
			return false
		}
		if !acquireGlobalSlotOrReleaseWorker(ctx, globalSem, slot) {
			return false
		}

		wg.Add(1)
		go func(task buildTask, slot *addrSlot) {
			defer wg.Done()
			defer func() { <-globalSem }()
			defer func() { <-slot.sem }()

			taskStartedAt := time.Now()
			taskEntry := resultEntry{Target: task.job.key}
			taskSuccess := true

			for idx, spec := range task.job.specs {
				entry := executeBuild(ctx, spec, slot.addr, opts)
				taskEntry.NodeIP = entry.NodeIP

				if !entry.Success && task.attempt < opts.retry {
					if entry.Logs != "" {
						if err := store.logs.AppendFailure(entry.Target, entry.Logs); err != nil {
							logError("Failed to append failure log for %s: %v", entry.Target, err)
						}
					}
					remainingSpecs := append([]buildSpec(nil), task.job.specs[idx:]...)
					logInfo("[RETRY %d/%d] %s", task.attempt+1, opts.retry, entry.Target)
					retryQueue <- buildTask{
						job: buildJob{
							key:         task.job.key,
							scheduleKey: task.job.scheduleKey,
							specs:       remainingSpecs,
						},
						attempt: task.attempt + 1,
					}
					return
				}

				if err := store.UpsertResult(entry); err != nil {
					persistErr := fmt.Errorf("store result for %s: %w", entry.Target, err)
					logError("Failed to store result for %s: %v", entry.Target, err)
					recordFatalErr(persistErr)
					failed.Store(true)
					cancel()
					return
				}

				if !entry.Success {
					taskSuccess = false
					break
				}
			}

			advanceLogProgress()

			taskEntry.Success = taskSuccess
			taskEntry.Elapsed = formatElapsed(time.Since(taskStartedAt))
			succeededCount, totalCount, failedCount := outcomeCounters.apply(task.job.key, taskSuccess)

			mu.Lock()
			results = append(results, taskEntry)
			mu.Unlock()

			printBuildResult(taskEntry, succeededCount, totalCount, failedCount)

			if !taskSuccess {
				failed.Store(true)
				if opts.failfast {
					cancel()
				}
			}
		}(task, slot)

		return true
	}

	scheduling := true
	for job := range jobCh {
		if scheduling {
			if !scheduleTask(buildTask{job: job, attempt: 0}) {
				scheduling = false
			}
		}
	}
	if err := <-prepareErrCh; err != nil {
		recordFatalErr(err)
		failed.Store(true)
		cancel()
	}

	for {
		wg.Wait()
		select {
		case task := <-retryQueue:
			tasks := []buildTask{task}
			for {
				select {
				case t := <-retryQueue:
					tasks = append(tasks, t)
				default:
					goto schedule
				}
			}
		schedule:
			for _, t := range tasks {
				if !scheduleTask(t) {
					break
				}
			}
		default:
			goto done
		}
	}
done:

	printSummary(results)

	fatalMu.Lock()
	defer fatalMu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}
	if failed.Load() {
		return fmt.Errorf("one or more builds failed")
	}
	return nil
}

func streamPreparedBuildJobs(ctx context.Context, opts options, buildModes []bool, targetFilters map[string]struct{}, entries []os.DirEntry, preparedRoot string, results buildResultReader, outcomeCounters *buildOutcomeCounters, out chan<- buildJob) error {
	for idx, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.IsDir() {
			return fmt.Errorf("image-dirs %s must contain only directories, found %s", opts.imageDirs, entry.Name())
		}

		srcDir := filepath.Join(opts.imageDirs, entry.Name())
		if err := validateSourceImageDir(srcDir); err != nil {
			return err
		}

		dstDir := filepath.Join(preparedRoot, entry.Name())
		startedAt := time.Now()
		logInfo("[PREPARE %d/%d] %s: copy source context", idx+1, len(entries), entry.Name())
		if err := copyDirectory(srcDir, dstDir); err != nil {
			return fmt.Errorf("copy %s: %w", srcDir, err)
		}

		logInfo("[HASH %d/%d] %s: hashing source context", idx+1, len(entries), entry.Name())
		scheduleKey, err := sourceContextContentHash(dstDir)
		if err != nil {
			return err
		}
		logInfo("[HASH %d/%d] %s: done in %s key=%s", idx+1, len(entries), entry.Name(), formatElapsed(time.Since(startedAt)), shortScheduleKey(scheduleKey))

		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "Dockerfile"), opts.vars); err != nil {
			return err
		}
		if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(dstDir, "Dockerfile")); err != nil {
			return err
		}
		if err := replaceBuildVariablesInFile(filepath.Join(dstDir, "metadata.json"), opts.vars); err != nil {
			return err
		}

		meta, err := loadImageMetadata(filepath.Join(dstDir, "metadata.json"))
		if err != nil {
			return err
		}
		if meta.Target == "" {
			return fmt.Errorf("%s has empty target in metadata.json", dstDir)
		}

		var specs []buildSpec
		for _, oci := range buildModes {
			normalizedTarget := normalizeTargetForMode(meta.Target, oci)
			if len(targetFilters) > 0 {
				if _, ok := targetFilters[normalizedTarget]; !ok {
					continue
				}
			}
			specs = append(specs, buildSpec{
				dir:         dstDir,
				target:      normalizedTarget,
				scheduleKey: scheduleKey,
				oci:         oci,
			})
		}
		if len(specs) == 0 {
			logInfo("[SKIP %d/%d] %s: target filter excluded all build modes", idx+1, len(entries), entry.Name())
			outcomeCounters.decrementTotal()
			advanceLogProgress()
			continue
		}

		jobs, existingOutcomes, skippedSucceeded, skippedFailed, err := filterBuildJobs(groupBuildSpecs(specs), results, opts.skipFail)
		if err != nil {
			return err
		}
		for target, state := range existingOutcomes {
			outcomeCounters.applyExisting(target, state)
		}
		if skippedSucceeded > 0 || skippedFailed > 0 {
			logInfo("[SKIP %d/%d] %s: skipped %d successful and %d failed target(s)", idx+1, len(entries), entry.Name(), skippedSucceeded, skippedFailed)
			advanceLogProgress()
		}
		for _, job := range jobs {
			logInfo("[SCHEDULE %d/%d] %s: target=%s key=%s", idx+1, len(entries), entry.Name(), job.key, shortScheduleKey(job.scheduleKey))
			select {
			case out <- job:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
