package buildbatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func runPreheat(opts options) error {
	resetCommandStartTime(time.Now())

	if opts.dragonflySchedulerAddr == "" {
		return fmt.Errorf("--dragonfly-scheduler-addr is required")
	}

	db, err := openLMDBResultDB(opts.fromResultPath)
	if err != nil {
		return fmt.Errorf("open result database: %w", err)
	}
	defer db.Close()

	entries, err := db.All()
	if err != nil {
		return err
	}

	var targets []resultEntry
	for _, entry := range entries {
		if !entry.Success {
			continue
		}
		if !targetMatchesMode(entry.Target, opts.oci) {
			continue
		}
		targets = append(targets, entry)
	}

	if len(targets) == 0 {
		logInfo("No targets to preheat")
		return nil
	}

	resetLogProgress(len(targets))
	defer clearLogProgress()

	logInfo("Preheating %d target(s) with concurrency=%d", len(targets), opts.concurrency)

	sem := make(chan struct{}, opts.concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		anyError bool
		lastTime time.Time
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, entry := range targets {
		if anyError && opts.failfast {
			break
		}
		if ctx.Err() != nil {
			break
		}

		// Throttle based on --interval.
		if opts.interval > 0 {
			mu.Lock()
			since := time.Since(lastTime)
			wait := time.Duration(opts.interval)*time.Second - since
			if wait > 0 {
				mu.Unlock()
				select {
				case <-time.After(wait):
				case <-ctx.Done():
				}
			} else {
				mu.Unlock()
			}
			mu.Lock()
			lastTime = time.Now()
			mu.Unlock()
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()

			err := executePreheat(ctx, target, opts)
			advanceLogProgress()
			if err != nil {
				logError("Preheat failed for %s: %v", target, err)
				mu.Lock()
				anyError = true
				mu.Unlock()
				if opts.failfast {
					cancel()
				}
			} else {
				logInfo("Preheated: %s", target)
			}
		}(entry.Target)
	}

	wg.Wait()
	if anyError {
		return fmt.Errorf("one or more preheat tasks failed")
	}
	return nil
}

func executePreheat(ctx context.Context, target string, opts options) error {
	var pCtx context.Context
	var pCancel context.CancelFunc
	if opts.timeout > 0 {
		pCtx, pCancel = context.WithTimeout(ctx, time.Duration(opts.timeout)*time.Second)
	} else {
		pCtx, pCancel = context.WithCancel(ctx)
	}
	defer pCancel()

	preheatURL, err := buildPreheatURL(target)
	if err != nil {
		return err
	}

	reqBody := map[string]any{
		"url":                preheatURL,
		"username":           "",
		"password":           "",
		"scope":              "all_seed_peers",
		"insecureSkipVerify": false,
	}
	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	args := []string{
		"-plaintext",
		"-d", "@",
		opts.dragonflySchedulerAddr,
		"scheduler.v2.Scheduler.PreheatImage",
	}

	cmd := exec.CommandContext(pCtx, "grpcurl", args...)
	cmd.Stdin = bytes.NewReader(reqJSON)
	var outputBuf bytes.Buffer
	if opts.verbose {
		cmd.Stdout = io.MultiWriter(os.Stdout, &outputBuf)
		cmd.Stderr = io.MultiWriter(os.Stderr, &outputBuf)
	} else {
		cmd.Stdout = &outputBuf
		cmd.Stderr = &outputBuf
	}

	return cmd.Run()
}

func buildPreheatURL(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", fmt.Errorf("preheat target is empty")
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed, nil
	}

	normalized := strings.TrimPrefix(strings.TrimPrefix(trimmed, "docker://"), "oci://")
	firstSlash := strings.IndexByte(normalized, '/')
	if firstSlash <= 0 || firstSlash == len(normalized)-1 {
		return "", fmt.Errorf("preheat target %q is not a fully qualified image reference", target)
	}

	registry := normalized[:firstSlash]
	remainder := normalized[firstSlash+1:]
	if !strings.Contains(registry, ".") && !strings.Contains(registry, ":") && registry != "localhost" {
		return "", fmt.Errorf("preheat target %q is not a fully qualified image reference", target)
	}

	repo := remainder
	reference := "latest"
	if at := strings.LastIndexByte(remainder, '@'); at >= 0 {
		repo = remainder[:at]
		reference = remainder[at+1:]
	} else {
		lastSlash := strings.LastIndexByte(remainder, '/')
		lastColon := strings.LastIndexByte(remainder, ':')
		if lastColon > lastSlash {
			repo = remainder[:lastColon]
			reference = remainder[lastColon+1:]
		}
	}

	if repo == "" || reference == "" {
		return "", fmt.Errorf("preheat target %q is not a valid image reference", target)
	}

	return fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, reference), nil
}
