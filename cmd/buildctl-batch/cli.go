package main

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/inclusionAI/buildkit-service/internal/buildbatch"
	cli "github.com/urfave/cli/v2"
)

const pprofServerEnv = "BUILDCTL_BATCH_PPROF_SERVER"

func newCLIApp() *cli.App {
	return &cli.App{
		Name:  "buildctl-batch",
		Usage: "batch helper for build, export, and preheat workflows",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "pprof-server",
				Usage: "listen address for net/http/pprof server, e.g. 0.0.0.0:5241",
			},
		},
		Before: func(c *cli.Context) error {
			if addr := resolvePprofServerAddr(c.String("pprof-server")); addr != "" {
				go func() {
					buildbatch.LogInfo("Starting pprof server on %s", addr)
					if err := http.ListenAndServe(addr, nil); err != nil {
						buildbatch.LogError("pprof server: %v", err)
					}
				}()
			}
			return nil
		},
		Commands: []*cli.Command{
			buildCLICommand(),
			exportCLICommand(),
			preheatCLICommand(),
			daemonCLICommand(),
		},
	}
}

func resolvePprofServerAddr(flagValue string) string {
	if trimmed := strings.TrimSpace(flagValue); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(os.Getenv(pprofServerEnv))
}

func buildCLICommand() *cli.Command {
	defaults := buildbatch.DefaultConfig()
	return &cli.Command{
		Name: "build",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "image-dirs", Usage: "path to a directory whose child directories each contain Dockerfile, metadata.json, and optional context"},
			&cli.StringFlag{Name: "addrs", Usage: "comma-separated buildkitd addresses; hostnames are resolved to IPs for scheduling"},
			&cli.StringSliceFlag{Name: "var", Usage: "replace occurrences of $KEY or ${KEY} in Dockerfile and metadata.json before build; repeatable, format KEY=value"},
			&cli.IntFlag{Name: "concurrency", Value: defaults.Concurrency, Usage: "total number of concurrent buildctl invocations across all buildkitd addresses"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first buildctl failure"},
			&cli.BoolFlag{Name: "oci", Usage: "build standard OCI images with gzip compression instead of nydus format"},
			&cli.BoolFlag{Name: "both-formats", Usage: "build both nydus and OCI images; runs buildctl twice per target"},
			&cli.DurationFlag{Name: "oom-cooldown", Value: defaults.OOMCooldown, Usage: "pause scheduling new tasks for the affected buildkit address after detecting an OOM-style connection refusal"},
			&cli.StringFlag{Name: "result", Value: defaults.ResultPath, Usage: "path to LMDB result database"},
			&cli.StringFlag{Name: "logs", Value: defaults.LogsPath, Usage: "path to JSONL file storing failed build logs"},
			&cli.IntFlag{Name: "timeout", Value: defaults.Timeout, Usage: "kill a buildctl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.IntFlag{Name: "retry", Value: 0, Usage: "number of times to retry a failed build before giving up; 0 disables retry"},
			&cli.BoolFlag{Name: "skip-fail", Usage: "skip targets that previously failed (recorded in LMDB)"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream buildctl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			ctx := buildSignalContext()
			return buildbatch.Run(ctx, buildConfigFromCLI(c))
		},
	}
}

func exportCLICommand() *cli.Command {
	defaults := buildbatch.DefaultExportConfig()
	return &cli.Command{
		Name: "export",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: defaults.FromResultPath, Usage: "path to the LMDB result database to read"},
			&cli.StringFlag{Name: "result", Value: defaults.ResultPath, Usage: "path to write exported JSONL"},
			&cli.BoolFlag{Name: "oci", Usage: "export only OCI (non-nydus) targets without suffix filtering changes"},
			&cli.BoolFlag{Name: "with-fail", Usage: "also export failed targets"},
		},
		Action: func(c *cli.Context) error {
			return buildbatch.Export(c.Context, buildbatch.ExportConfig{
				FromResultPath: c.String("from-result"),
				ResultPath:     c.String("result"),
				OCI:            c.Bool("oci"),
				WithFail:       c.Bool("with-fail"),
			})
		},
	}
}

func preheatCLICommand() *cli.Command {
	defaults := buildbatch.DefaultPreheatConfig()
	return &cli.Command{
		Name: "preheat",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: defaults.FromResultPath, Usage: "path to the LMDB result database to read and update"},
			&cli.StringFlag{Name: "dragonfly-scheduler-addr", Usage: "Dragonfly Scheduler gRPC address (e.g. 10.0.0.1:8002)"},
			&cli.IntFlag{Name: "concurrency", Value: defaults.Concurrency, Usage: "number of concurrent grpcurl invocations"},
			&cli.IntFlag{Name: "interval", Value: defaults.Interval, Usage: "minimum interval in seconds between starting grpcurl preheat tasks; 0 disables throttling"},
			&cli.IntFlag{Name: "timeout", Value: defaults.Timeout, Usage: "kill a grpcurl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first preheat failure"},
			&cli.BoolFlag{Name: "oci", Usage: "only preheat OCI (non-nydus) targets"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream grpcurl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			return buildbatch.Preheat(c.Context, buildbatch.PreheatConfig{
				FromResultPath:         c.String("from-result"),
				DragonflySchedulerAddr: c.String("dragonfly-scheduler-addr"),
				Concurrency:            c.Int("concurrency"),
				Interval:               c.Int("interval"),
				Timeout:                c.Int("timeout"),
				FailFast:               c.Bool("fail-fast"),
				OCI:                    c.Bool("oci"),
				Verbose:                c.Bool("verbose"),
			})
		},
	}
}

func daemonCLICommand() *cli.Command {
	defaults := buildbatch.DefaultDaemonConfig()
	return &cli.Command{
		Name:  "daemon",
		Usage: "start HTTP server on a unix domain socket for build/export/preheat",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "socket", Value: defaults.Socket, Usage: "unix socket path"},
			&cli.StringFlag{Name: "addrs", Usage: "default comma-separated buildkitd addresses"},
			&cli.StringFlag{Name: "auth", Usage: "base64-encoded registry auth JSON; written to /root/.docker/config.json"},
		},
		Action: func(c *cli.Context) error {
			ctx := daemonSignalContext()
			return buildbatch.RunDaemon(ctx, buildbatch.DaemonConfig{
				Socket: c.String("socket"),
				Addrs:  c.String("addrs"),
				Auth:   c.String("auth"),
			})
		},
	}
}

func buildConfigFromCLI(c *cli.Context) buildbatch.Config {
	return buildbatch.Config{
		ImageDirs:   c.String("image-dirs"),
		Addrs:       c.String("addrs"),
		Variables:   c.StringSlice("var"),
		Concurrency: c.Int("concurrency"),
		FailFast:    c.Bool("fail-fast"),
		OCI:         c.Bool("oci"),
		BothFormats: c.Bool("both-formats"),
		OOMCooldown: c.Duration("oom-cooldown"),
		ResultPath:  c.String("result"),
		LogsPath:    c.String("logs"),
		Timeout:     c.Int("timeout"),
		Retry:       c.Int("retry"),
		Verbose:     c.Bool("verbose"),
		Target:      strings.TrimSpace(c.Args().First()),
		SkipFail:    c.Bool("skip-fail"),
	}
}

func buildSignalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		buildbatch.LogInfo("Received interrupt, cancelling builds...")
		cancel()
	}()
	return ctx
}

func daemonSignalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		buildbatch.LogInfo("Received %s, shutting down daemon", sig)
		cancel()
	}()
	return ctx
}
