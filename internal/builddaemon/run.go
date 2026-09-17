package builddaemon

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"
)

// Run initializes and serves the build daemon until ctx is canceled or the
// HTTP server exits.
func Run(ctx context.Context, cfg Config) error {
	if err := normalizeConfig(&cfg); err != nil {
		return err
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

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	startBuildkitAddrRefresher(runCtx, pool, cfg.BuildkitdAddrs)

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
	server.startTaskReaper(runCtx)
	e := server.routes()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return serveHTTP(runCtx, httpSrv, httpSrv.ListenAndServe)
}

func serveHTTP(ctx context.Context, httpSrv *http.Server, listenAndServe func() error) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := listenAndServe(); err != nil && err != http.ErrServerClosed {
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
