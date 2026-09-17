package buildbatch

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func runBuildCommandBatch(opts options, buildModes []bool) error {
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
