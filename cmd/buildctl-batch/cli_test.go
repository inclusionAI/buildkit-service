package main

import (
	"fmt"
	"strings"
	"testing"

	cli "github.com/urfave/cli/v2"
)

func TestResolvePprofServerAddrPrefersFlagThenEnv(t *testing.T) {
	t.Setenv(pprofServerEnv, "127.0.0.1:6061")

	if addr := resolvePprofServerAddr(""); addr != "127.0.0.1:6061" {
		t.Fatalf("expected env fallback, got %q", addr)
	}
	if addr := resolvePprofServerAddr(" 0.0.0.0:6060 "); addr != "0.0.0.0:6060" {
		t.Fatalf("expected trimmed flag value, got %q", addr)
	}
}

func TestCLICommandSurface(t *testing.T) {
	app := newCLIApp()
	var got strings.Builder
	fmt.Fprintf(&got, "app|%s|%s\n", app.Name, app.Usage)
	writeFlags := func(owner string, flags []cli.Flag) {
		for _, flag := range flags {
			docFlag := flag.(cli.DocGenerationFlag)
			required := flag.(cli.RequiredFlag).IsRequired()
			fmt.Fprintf(&got, "flag|%s|%s|%s|%s|%v|%v|%s\n",
				owner,
				strings.Join(flag.Names(), ","),
				docFlag.GetValue(),
				docFlag.GetDefaultText(),
				required,
				docFlag.TakesValue(),
				docFlag.GetUsage(),
			)
			if envVars := docFlag.GetEnvVars(); len(envVars) != 0 {
				t.Fatalf("flag %s unexpectedly has environment bindings: %v", flag.Names(), envVars)
			}
		}
	}
	writeFlags("app", app.Flags)
	for _, command := range app.Commands {
		fmt.Fprintf(&got, "command|%s|%s\n", command.Name, command.Usage)
		writeFlags(command.Name, command.Flags)
	}

	const want = `app|buildctl-batch|batch helper for build, export, and preheat workflows
flag|app|pprof-server|||false|true|listen address for net/http/pprof server, e.g. 0.0.0.0:5241
command|build|
flag|build|image-dirs|||false|true|path to a directory whose child directories each contain Dockerfile, metadata.json, and optional context
flag|build|addrs|||false|true|comma-separated buildkitd addresses; hostnames are resolved to IPs for scheduling
flag|build|var|||false|true|replace occurrences of $KEY or ${KEY} in Dockerfile and metadata.json before build; repeatable, format KEY=value
flag|build|concurrency|1|1|false|true|total number of concurrent buildctl invocations across all buildkitd addresses
flag|build|fail-fast||false|false|false|stop immediately after the first buildctl failure
flag|build|oci||false|false|false|build standard OCI images with gzip compression instead of nydus format
flag|build|both-formats||false|false|false|build both nydus and OCI images; runs buildctl twice per target
flag|build|oom-cooldown|2m0s|2m0s|false|true|pause scheduling new tasks for the affected buildkit address after detecting an OOM-style connection refusal
flag|build|result|result.lmdb|"result.lmdb"|false|true|path to LMDB result database
flag|build|logs|logs.jsonl|"logs.jsonl"|false|true|path to JSONL file storing failed build logs
flag|build|timeout|300|300|false|true|kill a buildctl task if it runs longer than the given number of seconds; 0 disables timeout
flag|build|retry|0|0|false|true|number of times to retry a failed build before giving up; 0 disables retry
flag|build|skip-fail||false|false|false|skip targets that previously failed (recorded in LMDB)
flag|build|verbose||false|false|false|stream buildctl subprocess output to the console
command|export|
flag|export|from-result|result.lmdb|"result.lmdb"|false|true|path to the LMDB result database to read
flag|export|result|result.jsonl|"result.jsonl"|false|true|path to write exported JSONL
flag|export|oci||false|false|false|export only OCI (non-nydus) targets without suffix filtering changes
flag|export|with-fail||false|false|false|also export failed targets
command|preheat|
flag|preheat|from-result|result.lmdb|"result.lmdb"|false|true|path to the LMDB result database to read and update
flag|preheat|dragonfly-scheduler-addr|||false|true|Dragonfly Scheduler gRPC address (e.g. 10.0.0.1:8002)
flag|preheat|concurrency|1|1|false|true|number of concurrent grpcurl invocations
flag|preheat|interval|5|5|false|true|minimum interval in seconds between starting grpcurl preheat tasks; 0 disables throttling
flag|preheat|timeout|5|5|false|true|kill a grpcurl task if it runs longer than the given number of seconds; 0 disables timeout
flag|preheat|fail-fast||false|false|false|stop immediately after the first preheat failure
flag|preheat|oci||false|false|false|only preheat OCI (non-nydus) targets
flag|preheat|verbose||false|false|false|stream grpcurl subprocess output to the console
command|daemon|start HTTP server on a unix domain socket for build/export/preheat
flag|daemon|socket|/tmp/buildctl-batch.sock|"/tmp/buildctl-batch.sock"|false|true|unix socket path
flag|daemon|addrs|||false|true|default comma-separated buildkitd addresses
flag|daemon|auth|||false|true|base64-encoded registry auth JSON; written to /root/.docker/config.json
`
	if got.String() != want {
		t.Fatalf("CLI surface changed:\n--- got ---\n%s--- want ---\n%s", got.String(), want)
	}
}
