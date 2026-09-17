package builddaemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
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

// Config contains the runtime configuration accepted by Run.
type Config struct {
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
	TLS                TLSConfig
}

// TLSConfig contains the BuildKit client TLS settings.
type TLSConfig struct {
	CACert     string
	Cert       string
	Key        string
	Dir        string
	ServerName string
}

// DefaultConfig returns the defaults exposed by the command-line interface.
func DefaultConfig() Config {
	return Config{
		Listen:            defaultListenAddr,
		BuildkitdAddrs:    defaultBuildkitdAddr,
		WorkDir:           defaultWorkDir,
		DefaultMode:       defaultBuildMode,
		KeepTTL:           defaultKeepTTL,
		MaxLogBytes:       defaultMaxLogBytes,
		MaxRequestBytes:   defaultMaxRequestBytes,
		MaxExtractedBytes: defaultMaxExtractedBytes,
		MaxArchiveFiles:   defaultMaxArchiveFiles,
		MaxRetainedTasks:  defaultMaxRetainedTasks,
		MaxWorkDirBytes:   defaultMaxWorkDirBytes,
		UploadReadTimeout: defaultUploadReadTimeout,
		AddrConcurrency:   defaultAddrConcurrency,
		RegistryTimeout:   defaultRegistryTimeout,
		RegistryChecks:    defaultRegistryChecks,
	}
}

func normalizeConfig(cfg *Config) error {
	if err := loadAuthToken(cfg); err != nil {
		return err
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = defaultMaxLogBytes
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.MaxExtractedBytes <= 0 {
		cfg.MaxExtractedBytes = defaultMaxExtractedBytes
	}
	if cfg.MaxArchiveFiles <= 0 {
		cfg.MaxArchiveFiles = defaultMaxArchiveFiles
	}
	if cfg.MaxRetainedTasks <= 0 {
		cfg.MaxRetainedTasks = defaultMaxRetainedTasks
	}
	if cfg.MaxWorkDirBytes <= 0 {
		cfg.MaxWorkDirBytes = defaultMaxWorkDirBytes
	}
	if cfg.UploadReadTimeout <= 0 {
		cfg.UploadReadTimeout = defaultUploadReadTimeout
	}
	if cfg.AddrConcurrency <= 0 {
		cfg.AddrConcurrency = defaultAddrConcurrency
	}
	if cfg.MaxConcurrency < 0 {
		cfg.MaxConcurrency = 0
	}
	if cfg.KeepTTL < 0 {
		return fmt.Errorf("--keep-ttl must not be negative")
	}
	if cfg.RegistryTimeout <= 0 {
		cfg.RegistryTimeout = defaultRegistryTimeout
	}
	if cfg.RegistryChecks <= 0 {
		cfg.RegistryChecks = defaultRegistryChecks
	}
	return nil
}

func loadAuthToken(cfg *Config) error {
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
