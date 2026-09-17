package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxBuildLogBytes int64 = 1 << 20

type buildOutputCapture struct {
	mu        sync.Mutex
	buffer    []byte
	start     int
	size      int
	truncated bool
}

func newBuildOutputCapture() (*buildOutputCapture, error) {
	return &buildOutputCapture{buffer: make([]byte, maxBuildLogBytes)}, nil
}

func (c *buildOutputCapture) writer(console io.Writer) io.Writer {
	if console != nil {
		return io.MultiWriter(console, c)
	}
	return c
}

func (c *buildOutputCapture) closeAndReadTail(readLogs bool) (string, error) {
	if c == nil || !readLogs {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.size)
	if c.size > 0 {
		first := copy(out, c.buffer[c.start:])
		copy(out[first:], c.buffer[:c.size-first])
	}
	if c.truncated {
		return fmt.Sprintf("[buildctl output truncated to last %d bytes]\n%s", maxBuildLogBytes, out), nil
	}
	return string(out), nil
}

func (c *buildOutputCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	written := len(data)
	if len(data) >= len(c.buffer) {
		copy(c.buffer, data[len(data)-len(c.buffer):])
		c.start = 0
		c.size = len(c.buffer)
		c.truncated = true
		return written, nil
	}
	if c.size+len(data) > len(c.buffer) {
		drop := c.size + len(data) - len(c.buffer)
		c.start = (c.start + drop) % len(c.buffer)
		c.size -= drop
		c.truncated = true
	}
	end := (c.start + c.size) % len(c.buffer)
	first := copy(c.buffer[end:], data)
	copy(c.buffer, data[first:])
	c.size += len(data)
	return written, nil
}

func readFileTail(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	start := int64(0)
	truncated := false
	if maxBytes > 0 && size > maxBytes {
		start = size - maxBytes
		truncated = true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}

	var out bytes.Buffer
	if truncated {
		fmt.Fprintf(&out, "[buildctl output truncated to last %d bytes]\n", maxBytes)
	}
	if _, err := io.Copy(&out, file); err != nil {
		return "", err
	}
	return out.String(), nil
}

func executeBuild(ctx context.Context, spec buildSpec, addr *buildkitAddr, opts options) resultEntry {
	startedAt := time.Now()
	nodeIP := addr.nodeIP
	if strings.TrimSpace(nodeIP) == "" {
		nodeIP = nodeIPFromBuildkitAddr(addr.addr)
	}
	if nodeIP == "" {
		nodeIP = "unknown"
	}

	var buildCtx context.Context
	var buildCancel context.CancelFunc
	if opts.timeout > 0 {
		buildCtx, buildCancel = context.WithTimeout(ctx, time.Duration(opts.timeout)*time.Second)
	} else {
		buildCtx, buildCancel = context.WithCancel(ctx)
	}
	defer buildCancel()

	args := buildCommandArgs(spec, addr)

	cmd := exec.CommandContext(buildCtx, "buildctl", args...)
	output, outputErr := newBuildOutputCapture()
	if outputErr != nil {
		return resultEntry{
			StartedAt:  startedAt.Format(time.RFC3339),
			FinishedAt: time.Now().Format(time.RFC3339),
			Elapsed:    formatElapsed(time.Since(startedAt)),
			Target:     spec.target,
			NodeIP:     nodeIP,
			Success:    false,
			Reason:     fmt.Sprintf("create build output capture: %v", outputErr),
		}
	}
	cmd.Stdout = output.writer(nil)
	if opts.verbose {
		cmd.Stdout = output.writer(os.Stdout)
	}
	cmd.Stderr = output.writer(nil)
	if opts.verbose {
		cmd.Stderr = output.writer(os.Stderr)
	}

	err := cmd.Run()
	finishedAt := time.Now()
	elapsed := finishedAt.Sub(startedAt)
	logs, logErr := output.closeAndReadTail(err != nil)

	entry := resultEntry{
		StartedAt:  startedAt.Format(time.RFC3339),
		FinishedAt: finishedAt.Format(time.RFC3339),
		Elapsed:    formatElapsed(elapsed),
		Target:     spec.target,
		NodeIP:     nodeIP,
		Success:    err == nil,
	}

	if err != nil {
		entry.Logs = logs
		entry.Reason = err.Error()
		if logErr != nil {
			entry.Reason = fmt.Sprintf("%s; read build output: %v", entry.Reason, logErr)
		}

		lowerLogs := strings.ToLower(logs)
		if strings.Contains(lowerLogs, "connection refused") {
			logInfo("Detected OOM-style failure for %s, cooling down %s for %s",
				spec.target, addr.addr, addr.cooldown)
			addr.setCooldown()
		}
	}

	return entry
}

func buildCommandArgs(spec buildSpec, addr *buildkitAddr) []string {
	args := []string{
		"--addr", addr.addr,
		"build",
		"--frontend=dockerfile.v0",
	}

	if spec.dir != "" {
		args = append(args,
			"--local", "context="+spec.dir,
			"--local", "dockerfile="+spec.dir,
		)
	}

	output := fmt.Sprintf("type=image,name=%s,push=true,force-compression=true,oci-mediatypes=true", spec.target)
	if spec.oci {
		output += ",compression=gzip"
	} else {
		output += ",compression=nydus,fs-version=5"
	}
	args = append(args, "--output", output)

	return args
}
