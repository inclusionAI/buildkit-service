package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	app := newDaemonApp(run)
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "buildctl-daemon: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if err := loadAuthToken(&cfg); err != nil {
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
	if err := applyNoFileLimit(cfg.RLimitNoFile); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	startPprofServer(cfg.PprofListen)

	addrs, err := parseBuildkitAddrs(cfg.BuildkitdAddrs)
	if err != nil {
		return err
	}
	pool := newAddrPool(addrs, cfg.AddrConcurrency)
	modes, err := parseBuildModes(cfg.ModesJSON, cfg.DefaultMode, cfg.MaxConcurrency)
	if err != nil {
		return err
	}
	router, err := parseRoutingTargets(cfg.RoutingTargetsJSON)
	if err != nil {
		return err
	}
	if modes.routingRequired() && router == nil {
		return fmt.Errorf("at least one routing target is required when a build mode enables routing")
	}
	runner, err := newRealBuildRunner(cfg.TLS)
	if err != nil {
		return err
	}
	defer runner.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startBuildkitAddrRefresher(ctx, pool, cfg.BuildkitdAddrs)

	server := &buildServer{
		cfg:    cfg,
		store:  newTaskStore(),
		pool:   pool,
		runner: runner,
		modes:  modes,
		router: router,
		images: &registryImageChecker{
			timeout:   cfg.RegistryTimeout,
			hostRules: parseRegistryHostRules(cfg.RegistryRules),
			slots:     make(chan struct{}, cfg.RegistryChecks),
		},
		globalSem: newGlobalSem(cfg.MaxConcurrency),
	}
	server.scheduler, err = newBuildModeScheduler(modes, server.runBuildTask)
	if err != nil {
		return err
	}
	defer server.scheduler.close()
	server.startTaskReaper(ctx)
	e := server.routes()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func startPprofServer(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	go func() {
		fmt.Fprintf(os.Stderr, "buildctl-daemon pprof listening on %s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "buildctl-daemon pprof server failed: %v\n", err)
		}
	}()
}
