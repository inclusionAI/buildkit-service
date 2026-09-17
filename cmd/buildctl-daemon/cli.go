package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/inclusionAI/buildkit-service/internal/builddaemon"
	cli "github.com/urfave/cli/v2"
)

type daemonRunner func(context.Context, builddaemon.Config) error

func runCommand(args []string, stderr io.Writer, run daemonRunner) int {
	app := newDaemonApp(run)
	if err := app.Run(args); err != nil {
		fmt.Fprintf(stderr, "buildctl-daemon: %v\n", err)
		return 1
	}
	return 0
}

func newDaemonApp(run daemonRunner) *cli.App {
	defaults := builddaemon.DefaultConfig()
	return &cli.App{
		Name:  "buildctl-daemon",
		Usage: "HTTP API daemon for direct BuildKit image builds",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Value: defaults.Listen, Usage: "HTTP listen address"},
			&cli.StringFlag{Name: "buildkitd-addr", Value: defaults.BuildkitdAddrs, Usage: "comma-separated buildkitd addresses"},
			&cli.StringFlag{Name: "auth-token", Usage: "Bearer token required for /v1 APIs; empty disables auth"},
			&cli.StringFlag{Name: "auth-token-file", Usage: "read the Bearer token from a file; mutually exclusive with --auth-token"},
			&cli.StringFlag{Name: "work-dir", Value: defaults.WorkDir, Usage: "directory for uploaded archives, extracted contexts, and logs"},
			&cli.StringFlag{Name: "default-mode", Value: defaults.DefaultMode, Usage: "build mode used when requests omit the mode form field"},
			&cli.StringFlag{Name: "modes-json", Usage: "JSON object mapping build mode names to concurrency and routingEnabled settings; empty preserves legacy scheduling"},
			&cli.StringFlag{Name: "routing-targets-json", Usage: "JSON array of 2-32 registry prefixes available to routing-enabled build modes"},
			&cli.DurationFlag{Name: "keep-ttl", Value: defaults.KeepTTL, Usage: "duration to keep finished task status and logs before deleting them from memory and disk"},
			&cli.StringFlag{Name: "pprof-listen", Usage: "optional pprof listen address, for example 0.0.0.0:6060", EnvVars: []string{"BUILDCTL_DAEMON_PPROF_SERVER"}},
			&cli.Int64Flag{Name: "max-log-bytes", Value: defaults.MaxLogBytes, Usage: "maximum bytes stored per build log and returned by status APIs"},
			&cli.Int64Flag{Name: "max-request-bytes", Value: defaults.MaxRequestBytes, Usage: "maximum multipart build request size"},
			&cli.Int64Flag{Name: "max-extracted-bytes", Value: defaults.MaxExtractedBytes, Usage: "maximum total uncompressed size of an uploaded build context"},
			&cli.IntFlag{Name: "max-archive-files", Value: defaults.MaxArchiveFiles, Usage: "maximum number of entries in an uploaded build context"},
			&cli.IntFlag{Name: "max-retained-tasks", Value: defaults.MaxRetainedTasks, Usage: "maximum queued, running, and retained tasks"},
			&cli.Int64Flag{Name: "max-work-dir-bytes", Value: defaults.MaxWorkDirBytes, Usage: "maximum total bytes retained under work-dir"},
			&cli.DurationFlag{Name: "upload-read-timeout", Value: defaults.UploadReadTimeout, Usage: "deadline for reading a multipart build request body"},
			&cli.IntFlag{Name: "addr-concurrency", Value: defaults.AddrConcurrency, Usage: "maximum concurrent builds per buildkitd address"},
			&cli.IntFlag{Name: "max-concurrency", Usage: "maximum concurrent builds across all buildkitd addresses; extra tasks wait in queue; 0 disables the global limit"},
			&cli.Uint64Flag{Name: "rlimit-nofile", Usage: "raise RLIMIT_NOFILE to this value before serving; 0 keeps the runtime default"},
			&cli.DurationFlag{Name: "registry-check-timeout", Value: defaults.RegistryTimeout, Usage: "timeout for HEAD image existence checks"},
			&cli.StringFlag{Name: "registry-check-host-rules", Usage: "semicolon-separated registry=network-host|token-host rules; empty disables checks"},
			&cli.IntFlag{Name: "registry-check-concurrency", Value: defaults.RegistryChecks, Usage: "maximum concurrent HEAD image checks"},
			&cli.StringFlag{Name: "tlscacert", Usage: "CA certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlscert", Usage: "client certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlskey", Usage: "client key for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsdir", Usage: "directory containing ca.pem, cert.pem, key.pem for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsservername", Usage: "server name for buildkitd TLS verification"},
		},
		Action: func(c *cli.Context) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return run(ctx, configFromCLI(c))
		},
	}
}

func configFromCLI(c *cli.Context) builddaemon.Config {
	return builddaemon.Config{
		Listen:             c.String("listen"),
		BuildkitdAddrs:     c.String("buildkitd-addr"),
		AuthToken:          c.String("auth-token"),
		AuthTokenFile:      c.String("auth-token-file"),
		WorkDir:            c.String("work-dir"),
		DefaultMode:        c.String("default-mode"),
		ModesJSON:          c.String("modes-json"),
		RoutingTargetsJSON: c.String("routing-targets-json"),
		KeepTTL:            c.Duration("keep-ttl"),
		PprofListen:        c.String("pprof-listen"),
		MaxLogBytes:        c.Int64("max-log-bytes"),
		MaxRequestBytes:    c.Int64("max-request-bytes"),
		MaxExtractedBytes:  c.Int64("max-extracted-bytes"),
		MaxArchiveFiles:    c.Int("max-archive-files"),
		MaxRetainedTasks:   c.Int("max-retained-tasks"),
		MaxWorkDirBytes:    c.Int64("max-work-dir-bytes"),
		UploadReadTimeout:  c.Duration("upload-read-timeout"),
		AddrConcurrency:    c.Int("addr-concurrency"),
		MaxConcurrency:     c.Int("max-concurrency"),
		RLimitNoFile:       c.Uint64("rlimit-nofile"),
		RegistryTimeout:    c.Duration("registry-check-timeout"),
		RegistryRules:      c.String("registry-check-host-rules"),
		RegistryChecks:     c.Int("registry-check-concurrency"),
		TLS: builddaemon.TLSConfig{
			CACert:     c.String("tlscacert"),
			Cert:       c.String("tlscert"),
			Key:        c.String("tlskey"),
			Dir:        c.String("tlsdir"),
			ServerName: c.String("tlsservername"),
		},
	}
}
