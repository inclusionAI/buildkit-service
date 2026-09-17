package buildbatch

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigsPreserveCLIValues(t *testing.T) {
	if got, want := DefaultConfig(), (Config{
		Concurrency: 1,
		OOMCooldown: 2 * time.Minute,
		ResultPath:  "result.lmdb",
		LogsPath:    "logs.jsonl",
		Timeout:     300,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("build defaults changed: got %#v, want %#v", got, want)
	}
	if got, want := DefaultExportConfig(), (ExportConfig{FromResultPath: "result.lmdb", ResultPath: "result.jsonl"}); got != want {
		t.Fatalf("export defaults changed: got %#v, want %#v", got, want)
	}
	if got, want := DefaultPreheatConfig(), (PreheatConfig{FromResultPath: "result.lmdb", Concurrency: 1, Interval: 5, Timeout: 5}); got != want {
		t.Fatalf("preheat defaults changed: got %#v, want %#v", got, want)
	}
	if got, want := DefaultDaemonConfig(), (DaemonConfig{Socket: "/tmp/buildctl-batch.sock"}); got != want {
		t.Fatalf("daemon defaults changed: got %#v, want %#v", got, want)
	}
}

func TestRunPreservesAddressBeforeVariableValidationOrder(t *testing.T) {
	err := Run(context.Background(), Config{Addrs: ",", Variables: []string{"invalid"}})
	if err == nil || !strings.Contains(err.Error(), "no valid buildkitd addresses") {
		t.Fatalf("expected address validation error first, got %v", err)
	}
}
