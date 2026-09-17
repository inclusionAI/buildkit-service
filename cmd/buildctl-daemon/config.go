package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	cli "github.com/urfave/cli/v2"
)

const (
	defaultListenAddr        = ":8080"
	defaultBuildkitdAddr     = "tcp://buildkit-service.buildkit-service.svc:9094"
	defaultWorkDir           = "/tmp/buildctl-daemon"
	defaultMaxLogBytes       = int64(1 << 20)
	defaultMaxRequestBytes   = int64(512 << 20)
	defaultMaxExtractedBytes = int64(4 << 30)
	defaultMaxArchiveFiles   = 100000
	defaultMaxRetainedTasks  = 100
	defaultMaxWorkDirBytes   = int64(8 << 30)
	defaultUploadReadTimeout = 5 * time.Minute
	defaultAddrConcurrency   = 5
	defaultAddrRefresh       = 15 * time.Second
	defaultKeepTTL           = 2 * time.Hour
	defaultCleanupInterval   = time.Minute
	defaultBuildRetry        = 3
	defaultRetryInterval     = 10 * time.Second
	defaultRegistryTimeout   = 30 * time.Second
	defaultRegistryChecks    = 32
)

type config struct {
	Listen             string
	BuildkitdAddrs     string
	AuthToken          string
	AuthTokenFile      string
	WorkDir            string
	DefaultMode        string
	ModesJSON          string
	RoutingTargetsJSON string
	KeepTTL            time.Duration
	PprofListen        string
	MaxLogBytes        int64
	MaxRequestBytes    int64
	MaxExtractedBytes  int64
	MaxArchiveFiles    int
	MaxRetainedTasks   int
	MaxWorkDirBytes    int64
	UploadReadTimeout  time.Duration
	AddrConcurrency    int
	MaxConcurrency     int
	RLimitNoFile       uint64
	RegistryTimeout    time.Duration
	RegistryRules      string
	RegistryChecks     int
	TLS                tlsConfig
}

type tlsConfig struct {
	CACert     string
	Cert       string
	Key        string
	Dir        string
	ServerName string
}

func newDaemonApp(runFunc func(config) error) *cli.App {
	return &cli.App{
		Name:  "buildctl-daemon",
		Usage: "HTTP API daemon for direct BuildKit image builds",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Value: defaultListenAddr, Usage: "HTTP listen address"},
			&cli.StringFlag{Name: "buildkitd-addr", Value: defaultBuildkitdAddr, Usage: "comma-separated buildkitd addresses"},
			&cli.StringFlag{Name: "auth-token", Usage: "Bearer token required for /v1 APIs; empty disables auth"},
			&cli.StringFlag{Name: "auth-token-file", Usage: "read the Bearer token from a file; mutually exclusive with --auth-token"},
			&cli.StringFlag{Name: "work-dir", Value: defaultWorkDir, Usage: "directory for uploaded archives, extracted contexts, and logs"},
			&cli.StringFlag{Name: "default-mode", Value: defaultBuildMode, Usage: "build mode used when requests omit the mode form field"},
			&cli.StringFlag{Name: "modes-json", Usage: "JSON object mapping build mode names to concurrency and routingEnabled settings; empty preserves legacy scheduling"},
			&cli.StringFlag{Name: "routing-targets-json", Usage: "JSON array of 2-32 registry prefixes available to routing-enabled build modes"},
			&cli.DurationFlag{Name: "keep-ttl", Value: defaultKeepTTL, Usage: "duration to keep finished task status and logs before deleting them from memory and disk"},
			&cli.StringFlag{Name: "pprof-listen", Usage: "optional pprof listen address, for example 0.0.0.0:6060", EnvVars: []string{"BUILDCTL_DAEMON_PPROF_SERVER"}},
			&cli.Int64Flag{Name: "max-log-bytes", Value: defaultMaxLogBytes, Usage: "maximum bytes stored per build log and returned by status APIs"},
			&cli.Int64Flag{Name: "max-request-bytes", Value: defaultMaxRequestBytes, Usage: "maximum multipart build request size"},
			&cli.Int64Flag{Name: "max-extracted-bytes", Value: defaultMaxExtractedBytes, Usage: "maximum total uncompressed size of an uploaded build context"},
			&cli.IntFlag{Name: "max-archive-files", Value: defaultMaxArchiveFiles, Usage: "maximum number of entries in an uploaded build context"},
			&cli.IntFlag{Name: "max-retained-tasks", Value: defaultMaxRetainedTasks, Usage: "maximum queued, running, and retained tasks"},
			&cli.Int64Flag{Name: "max-work-dir-bytes", Value: defaultMaxWorkDirBytes, Usage: "maximum total bytes retained under work-dir"},
			&cli.DurationFlag{Name: "upload-read-timeout", Value: defaultUploadReadTimeout, Usage: "deadline for reading a multipart build request body"},
			&cli.IntFlag{Name: "addr-concurrency", Value: defaultAddrConcurrency, Usage: "maximum concurrent builds per buildkitd address"},
			&cli.IntFlag{Name: "max-concurrency", Usage: "maximum concurrent builds across all buildkitd addresses; extra tasks wait in queue; 0 disables the global limit"},
			&cli.Uint64Flag{Name: "rlimit-nofile", Usage: "raise RLIMIT_NOFILE to this value before serving; 0 keeps the runtime default"},
			&cli.DurationFlag{Name: "registry-check-timeout", Value: defaultRegistryTimeout, Usage: "timeout for HEAD image existence checks"},
			&cli.StringFlag{Name: "registry-check-host-rules", Usage: "semicolon-separated registry=network-host|token-host rules; empty disables checks"},
			&cli.IntFlag{Name: "registry-check-concurrency", Value: defaultRegistryChecks, Usage: "maximum concurrent HEAD image checks"},
			&cli.StringFlag{Name: "tlscacert", Usage: "CA certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlscert", Usage: "client certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlskey", Usage: "client key for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsdir", Usage: "directory containing ca.pem, cert.pem, key.pem for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsservername", Usage: "server name for buildkitd TLS verification"},
		},
		Action: func(c *cli.Context) error {
			return runFunc(configFromCLI(c))
		},
	}
}

func configFromCLI(c *cli.Context) config {
	return config{
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
		TLS: tlsConfig{
			CACert:     c.String("tlscacert"),
			Cert:       c.String("tlscert"),
			Key:        c.String("tlskey"),
			Dir:        c.String("tlsdir"),
			ServerName: c.String("tlsservername"),
		},
	}
}

func loadAuthToken(cfg *config) error {
	if cfg.AuthToken != "" && cfg.AuthTokenFile != "" {
		return errors.New("--auth-token and --auth-token-file are mutually exclusive")
	}
	if cfg.AuthTokenFile == "" {
		return nil
	}
	token, err := os.ReadFile(cfg.AuthTokenFile)
	if err != nil {
		return fmt.Errorf("read auth token file: %w", err)
	}
	cfg.AuthToken = strings.TrimSpace(string(token))
	if cfg.AuthToken == "" {
		return errors.New("auth token file is empty")
	}
	return nil
}

func applyNoFileLimit(limit uint64) error {
	if limit == 0 {
		return nil
	}
	var current syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &current); err != nil {
		return fmt.Errorf("get RLIMIT_NOFILE: %w", err)
	}
	if current.Cur >= limit && current.Max >= limit {
		return nil
	}
	requested := syscall.Rlimit{Cur: limit, Max: limit}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &requested); err != nil {
		return fmt.Errorf("set RLIMIT_NOFILE to %d (current soft=%d hard=%d): %w", limit, current.Cur, current.Max, err)
	}
	return nil
}
