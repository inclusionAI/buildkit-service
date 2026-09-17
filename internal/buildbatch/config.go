package buildbatch

import (
	"context"
	"time"
)

// Config contains the runtime configuration for a batch build.
type Config struct {
	ImageDirs   string
	Addrs       string
	Variables   []string
	Concurrency int
	FailFast    bool
	OCI         bool
	BothFormats bool
	OOMCooldown time.Duration
	ResultPath  string
	LogsPath    string
	Timeout     int
	Retry       int
	Verbose     bool
	Target      string
	SkipFail    bool
}

// DefaultConfig returns the defaults exposed by the build command.
func DefaultConfig() Config {
	return Config{
		Concurrency: 1,
		OOMCooldown: defaultBuildkitOOMCooldown,
		ResultPath:  "result.lmdb",
		LogsPath:    "logs.jsonl",
		Timeout:     300,
	}
}

// ExportConfig contains the runtime configuration for result export.
type ExportConfig struct {
	FromResultPath string
	ResultPath     string
	OCI            bool
	WithFail       bool
}

// DefaultExportConfig returns the defaults exposed by the export command.
func DefaultExportConfig() ExportConfig {
	return ExportConfig{FromResultPath: "result.lmdb", ResultPath: "result.jsonl"}
}

// PreheatConfig contains the runtime configuration for image preheating.
type PreheatConfig struct {
	FromResultPath         string
	DragonflySchedulerAddr string
	Concurrency            int
	Interval               int
	Timeout                int
	FailFast               bool
	OCI                    bool
	Verbose                bool
}

// DefaultPreheatConfig returns the defaults exposed by the preheat command.
func DefaultPreheatConfig() PreheatConfig {
	return PreheatConfig{FromResultPath: "result.lmdb", Concurrency: 1, Interval: 5, Timeout: 5}
}

// DaemonConfig contains the runtime configuration for the HTTP daemon.
type DaemonConfig struct {
	Socket string
	Addrs  string
	Auth   string
}

// DefaultDaemonConfig returns the defaults exposed by the daemon command.
func DefaultDaemonConfig() DaemonConfig {
	return DaemonConfig{Socket: defaultDaemonSocket}
}

// options is shared by the build, export, preheat, and daemon implementations.
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

// Run executes a batch build.
func Run(ctx context.Context, cfg Config) error {
	addrs, err := parseBuildkitAddrs(cfg.Addrs)
	if err != nil {
		return err
	}
	buildVars, err := parseBuildVariables(cfg.Variables)
	if err != nil {
		return err
	}
	resetCommandStartTime(time.Now())
	for _, addr := range addrs {
		addr.cooldown = cfg.OOMCooldown
	}
	return runBuildCommand(options{
		imageDirs:   cfg.ImageDirs,
		addrs:       addrs,
		addrsRaw:    cfg.Addrs,
		concurrency: cfg.Concurrency,
		ctx:         ctx,
		failfast:    cfg.FailFast,
		oci:         cfg.OCI,
		bothFormats: cfg.BothFormats,
		oomCooldown: cfg.OOMCooldown,
		resultPath:  cfg.ResultPath,
		logsPath:    cfg.LogsPath,
		vars:        buildVars,
		timeout:     cfg.Timeout,
		retry:       cfg.Retry,
		verbose:     cfg.Verbose,
		target:      cfg.Target,
		skipFail:    cfg.SkipFail,
	})
}

// Export writes stored build results as JSONL.
func Export(_ context.Context, cfg ExportConfig) error {
	return runExport(options{
		fromResultPath: cfg.FromResultPath,
		resultPath:     cfg.ResultPath,
		oci:            cfg.OCI,
		withFail:       cfg.WithFail,
	})
}

// Preheat preheats images recorded in a result database.
func Preheat(_ context.Context, cfg PreheatConfig) error {
	return runPreheat(options{
		fromResultPath:         cfg.FromResultPath,
		dragonflySchedulerAddr: cfg.DragonflySchedulerAddr,
		concurrency:            cfg.Concurrency,
		interval:               cfg.Interval,
		timeout:                cfg.Timeout,
		failfast:               cfg.FailFast,
		oci:                    cfg.OCI,
		verbose:                cfg.Verbose,
	})
}
