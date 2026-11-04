package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/inclusionAI/buildkit-service/internal/buildobserver"
	"github.com/moby/buildkit/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("addr", "tcp://127.0.0.1:9094", "single BuildKit worker address")
	listen := flag.String("listen", ":9301", "private Prometheus HTTP listener")
	interval := flag.Duration("interval", 30*time.Second, "API sampling interval")
	timeout := flag.Duration("timeout", 5*time.Second, "API sampling deadline")
	tlsDir := flag.String("tlsdir", "", "optional directory containing ca.pem, cert.pem, key.pem")
	serverName := flag.String("tlsservername", "", "TLS server name")
	flag.Parse()
	if *interval <= 0 || *timeout <= 0 {
		return fmt.Errorf("interval and timeout must be positive")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	var options []client.ClientOpt
	if *tlsDir != "" {
		options = append(options, client.WithServerConfig(*serverName, filepath.Join(*tlsDir, "ca.pem")), client.WithCredentials(filepath.Join(*tlsDir, "cert.pem"), filepath.Join(*tlsDir, "key.pem")))
	}
	c, err := client.New(ctx, *addr, options...)
	if err != nil {
		return err
	}
	defer c.Close()
	metrics := buildobserver.NewMetrics()
	go func() {
		ticker := time.NewTicker(*interval)
		defer ticker.Stop()
		for {
			poll, stop := context.WithTimeout(ctx, *timeout)
			metrics.Sample(poll, func(ctx context.Context) (buildobserver.Snapshot, error) { return buildobserver.Read(ctx, c) })
			stop()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
	}()
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
