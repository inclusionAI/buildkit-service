package buildbatch

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildOptionsFromQueryAcceptsOneshot(t *testing.T) {
	q := url.Values{}
	q.Set("oneshot", "true")

	if _, err := buildOptionsFromQuery(q, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl"); err != nil {
		t.Fatalf("expected oneshot to be an accepted query key, got %v", err)
	}
}

func TestBuildOptionsFromQueryRejectsUnknownKey(t *testing.T) {
	q := url.Values{}
	q.Set("bogus", "1")

	if _, err := buildOptionsFromQuery(q, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl"); err == nil {
		t.Fatal("expected unknown query key to be rejected")
	}
}

func TestBuildOptionsFromQueryPreservesDefaults(t *testing.T) {
	opts, err := buildOptionsFromQuery(url.Values{}, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if opts.addrsRaw != "tcp://127.0.0.1:9094" || len(opts.addrs) != 1 || opts.concurrency != 1 || opts.timeout != 300 || opts.retry != 0 || opts.oomCooldown != 2*time.Minute {
		t.Fatalf("build option defaults changed: %#v", opts)
	}
	if opts.failfast || opts.oci || opts.bothFormats || opts.verbose || opts.skipFail || len(opts.vars) != 0 {
		t.Fatalf("boolean or variable defaults changed: %#v", opts)
	}
	if opts.resultPath != "/tmp/result.lmdb" || opts.logsPath != "/tmp/logs.jsonl" {
		t.Fatalf("result paths changed: %#v", opts)
	}
}

func TestBuildOptionsFromQueryStrictErrors(t *testing.T) {
	tests := []struct {
		name string
		q    url.Values
		want string
	}{
		{name: "unknown", q: url.Values{"bogus": {"1"}}, want: `unsupported query parameter "bogus"`},
		{name: "duplicate", q: url.Values{"timeout": {"1", "2"}}, want: `query parameter "timeout" must be specified at most once`},
		{name: "boolean", q: url.Values{"oci": {"maybe"}}, want: `query parameter "oci" must be a boolean`},
		{name: "integer", q: url.Values{"retry": {"later"}}, want: `query parameter "retry" must be an integer`},
		{name: "minimum", q: url.Values{"concurrency": {"0"}}, want: `query parameter "concurrency" must be greater than or equal to 1`},
		{name: "duration", q: url.Values{"oom-cooldown": {"soon"}}, want: `query parameter "oom-cooldown" must be a duration`},
		{name: "variable", q: url.Values{"var": {"invalid"}}, want: `invalid --var "invalid", expected KEY=value`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildOptionsFromQuery(test.q, "tcp://127.0.0.1:9094", "/tmp/result.lmdb", "/tmp/logs.jsonl")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestExportAndPreheatOptionsFromQuery(t *testing.T) {
	exportOpts, err := exportOptionsFromQuery(url.Values{}, "/tmp/result.lmdb")
	if err != nil {
		t.Fatal(err)
	}
	if exportOpts.fromResultPath != "/tmp/result.lmdb" || exportOpts.oci || exportOpts.withFail {
		t.Fatalf("export option defaults changed: %#v", exportOpts)
	}

	if _, err := preheatOptionsFromQuery(url.Values{}, "/tmp/result.lmdb"); err == nil || err.Error() != "dragonfly-scheduler-addr is required" {
		t.Fatalf("preheat required-address error changed: %v", err)
	}
	preheatOpts, err := preheatOptionsFromQuery(url.Values{"dragonfly-scheduler-addr": {" scheduler:8002 "}}, "/tmp/result.lmdb")
	if err != nil {
		t.Fatal(err)
	}
	if preheatOpts.dragonflySchedulerAddr != "scheduler:8002" || preheatOpts.concurrency != 1 || preheatOpts.interval != 5 || preheatOpts.timeout != 5 {
		t.Fatalf("preheat option defaults changed: %#v", preheatOpts)
	}
}
