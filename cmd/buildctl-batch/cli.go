package main

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	cli "github.com/urfave/cli/v2"
)

const pprofServerEnv = "BUILDCTL_BATCH_PPROF_SERVER"

// options holds CLI flags shared across subcommands.
type options struct {
	imageDirs              string
	addrs                  []*buildkitAddr
	addrsRaw               string
	concurrency            int
	ctx                    context.Context
	failfast               bool
	oci                    bool
	bothFormats            bool
	oomCooldown            time.Duration
	resultPath             string
	logsPath               string
	vars                   map[string]string
	timeout                int
	verbose                bool
	target                 string
	skipFail               bool
	fromResultPath         string
	retry                  int
	dragonflySchedulerAddr string
	interval               int
	withFail               bool
}

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
					logInfo("Starting pprof server on %s", addr)
					if err := http.ListenAndServe(addr, nil); err != nil {
						logError("pprof server: %v", err)
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
	return &cli.Command{
		Name: "build",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "image-dirs", Usage: "path to a directory whose child directories each contain Dockerfile, metadata.json, and optional context"},
			&cli.StringFlag{Name: "addrs", Usage: "comma-separated buildkitd addresses; hostnames are resolved to IPs for scheduling"},
			&cli.StringSliceFlag{Name: "var", Usage: "replace occurrences of $KEY or ${KEY} in Dockerfile and metadata.json before build; repeatable, format KEY=value"},
			&cli.IntFlag{Name: "concurrency", Value: 1, Usage: "total number of concurrent buildctl invocations across all buildkitd addresses"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first buildctl failure"},
			&cli.BoolFlag{Name: "oci", Usage: "build standard OCI images with gzip compression instead of nydus format"},
			&cli.BoolFlag{Name: "both-formats", Usage: "build both nydus and OCI images; runs buildctl twice per target"},
			&cli.DurationFlag{Name: "oom-cooldown", Value: defaultBuildkitOOMCooldown, Usage: "pause scheduling new tasks for the affected buildkit address after detecting an OOM-style connection refusal"},
			&cli.StringFlag{Name: "result", Value: "result.lmdb", Usage: "path to LMDB result database"},
			&cli.StringFlag{Name: "logs", Value: "logs.jsonl", Usage: "path to JSONL file storing failed build logs"},
			&cli.IntFlag{Name: "timeout", Value: 300, Usage: "kill a buildctl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.IntFlag{Name: "retry", Value: 0, Usage: "number of times to retry a failed build before giving up; 0 disables retry"},
			&cli.BoolFlag{Name: "skip-fail", Usage: "skip targets that previously failed (recorded in LMDB)"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream buildctl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			addrs, err := parseBuildkitAddrs(c.String("addrs"))
			if err != nil {
				return err
			}
			buildVars, err := parseBuildVariables(c.StringSlice("var"))
			if err != nil {
				return err
			}
			oomCooldown := c.Duration("oom-cooldown")
			for _, addr := range addrs {
				addr.cooldown = oomCooldown
			}
			return runBuildCommand(options{
				imageDirs:   c.String("image-dirs"),
				addrs:       addrs,
				addrsRaw:    c.String("addrs"),
				concurrency: c.Int("concurrency"),
				failfast:    c.Bool("fail-fast"),
				oci:         c.Bool("oci"),
				bothFormats: c.Bool("both-formats"),
				oomCooldown: oomCooldown,
				resultPath:  c.String("result"),
				logsPath:    c.String("logs"),
				vars:        buildVars,
				timeout:     c.Int("timeout"),
				retry:       c.Int("retry"),
				verbose:     c.Bool("verbose"),
				target:      strings.TrimSpace(c.Args().First()),
				skipFail:    c.Bool("skip-fail"),
			})
		},
	}
}

func exportCLICommand() *cli.Command {
	return &cli.Command{
		Name: "export",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: "result.lmdb", Usage: "path to the LMDB result database to read"},
			&cli.StringFlag{Name: "result", Value: "result.jsonl", Usage: "path to write exported JSONL"},
			&cli.BoolFlag{Name: "oci", Usage: "export only OCI (non-nydus) targets without suffix filtering changes"},
			&cli.BoolFlag{Name: "with-fail", Usage: "also export failed targets"},
		},
		Action: func(c *cli.Context) error {
			return runExport(options{
				fromResultPath: c.String("from-result"),
				resultPath:     c.String("result"),
				oci:            c.Bool("oci"),
				withFail:       c.Bool("with-fail"),
			})
		},
	}
}

func preheatCLICommand() *cli.Command {
	return &cli.Command{
		Name: "preheat",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-result", Value: "result.lmdb", Usage: "path to the LMDB result database to read and update"},
			&cli.StringFlag{Name: "dragonfly-scheduler-addr", Usage: "Dragonfly Scheduler gRPC address (e.g. 10.0.0.1:8002)"},
			&cli.IntFlag{Name: "concurrency", Value: 1, Usage: "number of concurrent grpcurl invocations"},
			&cli.IntFlag{Name: "interval", Value: 5, Usage: "minimum interval in seconds between starting grpcurl preheat tasks; 0 disables throttling"},
			&cli.IntFlag{Name: "timeout", Value: 5, Usage: "kill a grpcurl task if it runs longer than the given number of seconds; 0 disables timeout"},
			&cli.BoolFlag{Name: "fail-fast", Usage: "stop immediately after the first preheat failure"},
			&cli.BoolFlag{Name: "oci", Usage: "only preheat OCI (non-nydus) targets"},
			&cli.BoolFlag{Name: "verbose", Usage: "stream grpcurl subprocess output to the console"},
		},
		Action: func(c *cli.Context) error {
			return runPreheat(options{
				fromResultPath:         c.String("from-result"),
				dragonflySchedulerAddr: c.String("dragonfly-scheduler-addr"),
				concurrency:            c.Int("concurrency"),
				interval:               c.Int("interval"),
				timeout:                c.Int("timeout"),
				failfast:               c.Bool("fail-fast"),
				oci:                    c.Bool("oci"),
				verbose:                c.Bool("verbose"),
			})
		},
	}
}
