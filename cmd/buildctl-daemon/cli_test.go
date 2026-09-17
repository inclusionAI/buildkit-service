package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/inclusionAI/buildkit-service/internal/builddaemon"
)

func TestDaemonCLIDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("BUILDCTL_DAEMON_PPROF_SERVER", "127.0.0.1:6060")
	var got builddaemon.Config
	app := newDaemonApp(func(_ context.Context, cfg builddaemon.Config) error {
		got = cfg
		return nil
	})
	if app.Name != "buildctl-daemon" || app.Usage != "HTTP API daemon for direct BuildKit image builds" {
		t.Fatalf("CLI identity changed: name=%q usage=%q", app.Name, app.Usage)
	}
	if err := app.Run([]string{"buildctl-daemon"}); err != nil {
		t.Fatal(err)
	}
	want := builddaemon.DefaultConfig()
	want.PprofListen = "127.0.0.1:6060"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CLI defaults changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDaemonCLIMapsExplicitFlags(t *testing.T) {
	var got builddaemon.Config
	app := newDaemonApp(func(_ context.Context, cfg builddaemon.Config) error {
		got = cfg
		return nil
	})
	args := []string{
		"buildctl-daemon",
		"--listen=127.0.0.1:9000",
		"--buildkitd-addr=tcp://worker:1234",
		"--auth-token=token",
		"--auth-token-file=token-file",
		"--work-dir=/tmp/work",
		"--default-mode=production",
		`--modes-json={"production":{"concurrency":2}}`,
		`--routing-targets-json=["a.example/repo","b.example/repo"]`,
		"--keep-ttl=3m",
		"--pprof-listen=127.0.0.1:6061",
		"--max-log-bytes=101",
		"--max-request-bytes=102",
		"--max-extracted-bytes=103",
		"--max-archive-files=104",
		"--max-retained-tasks=105",
		"--max-work-dir-bytes=106",
		"--upload-read-timeout=4m",
		"--addr-concurrency=7",
		"--max-concurrency=8",
		"--rlimit-nofile=4096",
		"--registry-check-timeout=9s",
		"--registry-check-host-rules=registry.example=registry.example",
		"--registry-check-concurrency=10",
		"--tlscacert=ca.pem",
		"--tlscert=cert.pem",
		"--tlskey=key.pem",
		"--tlsdir=tls",
		"--tlsservername=worker.example",
	}
	if err := app.Run(args); err != nil {
		t.Fatal(err)
	}
	want := builddaemon.Config{
		Listen:             "127.0.0.1:9000",
		BuildkitdAddrs:     "tcp://worker:1234",
		AuthToken:          "token",
		AuthTokenFile:      "token-file",
		WorkDir:            "/tmp/work",
		DefaultMode:        "production",
		ModesJSON:          `{"production":{"concurrency":2}}`,
		RoutingTargetsJSON: `["a.example/repo","b.example/repo"]`,
		KeepTTL:            3 * time.Minute,
		PprofListen:        "127.0.0.1:6061",
		MaxLogBytes:        101,
		MaxRequestBytes:    102,
		MaxExtractedBytes:  103,
		MaxArchiveFiles:    104,
		MaxRetainedTasks:   105,
		MaxWorkDirBytes:    106,
		UploadReadTimeout:  4 * time.Minute,
		AddrConcurrency:    7,
		MaxConcurrency:     8,
		RLimitNoFile:       4096,
		RegistryTimeout:    9 * time.Second,
		RegistryRules:      "registry.example=registry.example",
		RegistryChecks:     10,
		TLS: builddaemon.TLSConfig{
			CACert:     "ca.pem",
			Cert:       "cert.pem",
			Key:        "key.pem",
			Dir:        "tls",
			ServerName: "worker.example",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CLI flag mapping changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDaemonCLIHelpText(t *testing.T) {
	want := map[string]string{
		"listen":                     "HTTP listen address",
		"buildkitd-addr":             "comma-separated buildkitd addresses",
		"auth-token":                 "Bearer token required for /v1 APIs; empty disables auth",
		"auth-token-file":            "read the Bearer token from a file; mutually exclusive with --auth-token",
		"work-dir":                   "directory for uploaded archives, extracted contexts, and logs",
		"default-mode":               "build mode used when requests omit the mode form field",
		"modes-json":                 "JSON object mapping build mode names to concurrency and routingEnabled settings; empty preserves legacy scheduling",
		"routing-targets-json":       "JSON array of 2-32 registry prefixes available to routing-enabled build modes",
		"keep-ttl":                   "duration to keep finished task status and logs before deleting them from memory and disk",
		"pprof-listen":               "optional pprof listen address, for example 0.0.0.0:6060",
		"max-log-bytes":              "maximum bytes stored per build log and returned by status APIs",
		"max-request-bytes":          "maximum multipart build request size",
		"max-extracted-bytes":        "maximum total uncompressed size of an uploaded build context",
		"max-archive-files":          "maximum number of entries in an uploaded build context",
		"max-retained-tasks":         "maximum queued, running, and retained tasks",
		"max-work-dir-bytes":         "maximum total bytes retained under work-dir",
		"upload-read-timeout":        "deadline for reading a multipart build request body",
		"addr-concurrency":           "maximum concurrent builds per buildkitd address",
		"max-concurrency":            "maximum concurrent builds across all buildkitd addresses; extra tasks wait in queue; 0 disables the global limit",
		"rlimit-nofile":              "raise RLIMIT_NOFILE to this value before serving; 0 keeps the runtime default",
		"registry-check-timeout":     "timeout for HEAD image existence checks",
		"registry-check-host-rules":  "semicolon-separated registry=network-host|token-host rules; empty disables checks",
		"registry-check-concurrency": "maximum concurrent HEAD image checks",
		"tlscacert":                  "CA certificate for buildkitd TLS",
		"tlscert":                    "client certificate for buildkitd TLS",
		"tlskey":                     "client key for buildkitd TLS",
		"tlsdir":                     "directory containing ca.pem, cert.pem, key.pem for buildkitd TLS",
		"tlsservername":              "server name for buildkitd TLS verification",
	}

	app := newDaemonApp(func(context.Context, builddaemon.Config) error { return nil })
	got := make(map[string]string, len(app.Flags))
	for _, flag := range app.Flags {
		docFlag, ok := flag.(interface{ GetUsage() string })
		if !ok {
			t.Fatalf("flag %q does not expose help text", flag.Names()[0])
		}
		got[flag.Names()[0]] = docFlag.GetUsage()
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CLI help text changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRunCommandReportsDaemonError(t *testing.T) {
	var stderr bytes.Buffer
	code := runCommand([]string{"buildctl-daemon"}, &stderr, func(context.Context, builddaemon.Config) error {
		return errors.New("daemon failed")
	})
	if code != 1 || stderr.String() != "buildctl-daemon: daemon failed\n" {
		t.Fatalf("unexpected command result: code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunCommandReportsCLIParseError(t *testing.T) {
	var stderr bytes.Buffer
	called := false
	code := runCommand([]string{"buildctl-daemon", "--keep-ttl=invalid"}, &stderr, func(context.Context, builddaemon.Config) error {
		called = true
		return nil
	})
	if code != 1 || called {
		t.Fatalf("unexpected parse result: code=%d called=%v", code, called)
	}
	if stderr.String() != "buildctl-daemon: invalid value \"invalid\" for flag -keep-ttl: parse error\n" {
		t.Fatalf("unexpected parse error: %q", stderr.String())
	}
}

func TestRunCommandSuccess(t *testing.T) {
	var stderr bytes.Buffer
	var runContext context.Context
	if code := runCommand([]string{"buildctl-daemon"}, &stderr, func(ctx context.Context, _ builddaemon.Config) error {
		runContext = ctx
		return nil
	}); code != 0 {
		t.Fatalf("unexpected command result: code=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	select {
	case <-runContext.Done():
	default:
		t.Fatal("signal context was not released after the daemon returned")
	}
}
