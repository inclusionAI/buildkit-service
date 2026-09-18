package buildbatch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildVariableParsingAndReplacement(t *testing.T) {
	vars, err := parseBuildVariables([]string{
		"BUILDCTL_BATCH_REGISTRY=registry.example.com",
		"BUILDCTL_BATCH_TAG=v1=debug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if vars["BUILDCTL_BATCH_REGISTRY"] != "registry.example.com" || vars["BUILDCTL_BATCH_TAG"] != "v1=debug" {
		t.Fatalf("unexpected parsed variables: %#v", vars)
	}

	got, err := replaceBuildVariables(
		[]byte("FROM $BUILDCTL_BATCH_REGISTRY/ns/repo:${BUILDCTL_BATCH_TAG}\n"),
		"Dockerfile",
		vars,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "FROM registry.example.com/ns/repo:v1=debug\n"; string(got) != want {
		t.Fatalf("replacement = %q, want %q", got, want)
	}

	if _, err := replaceBuildVariables([]byte("FROM $BUILDCTL_BATCH_MISSING/repo\n"), "Dockerfile", vars); err == nil || err.Error() != "Dockerfile contains unresolved build variable(s): BUILDCTL_BATCH_MISSING" {
		t.Fatalf("unexpected unresolved variable error: %v", err)
	}
	if _, err := parseBuildVariables([]string{"REGISTRY=example.com"}); err == nil || err.Error() != `invalid --var "REGISTRY=example.com", key must start with BUILDCTL_BATCH_` {
		t.Fatalf("unexpected invalid variable error: %v", err)
	}
}

func TestBuildVariableEdgeCases(t *testing.T) {
	if _, err := parseBuildVariables([]string{"BUILDCTL_BATCH_MISSING"}); err == nil || err.Error() != `invalid --var "BUILDCTL_BATCH_MISSING", expected KEY=value` {
		t.Fatalf("missing equals error changed: %v", err)
	}
	if _, err := parseBuildVariables([]string{"=value"}); err == nil || err.Error() != `invalid --var "=value", key must not be empty` {
		t.Fatalf("empty key error changed: %v", err)
	}
	vars, err := parseBuildVariables([]string{"BUILDCTL_BATCH_VALUE=first", "BUILDCTL_BATCH_VALUE=second"})
	if err != nil {
		t.Fatal(err)
	}
	if vars["BUILDCTL_BATCH_VALUE"] != "second" {
		t.Fatalf("duplicate key result changed: %#v", vars)
	}
	_, err = replaceBuildVariables(
		[]byte("$BUILDCTL_BATCH_Z ${BUILDCTL_BATCH_A} $BUILDCTL_BATCH_Z"),
		"Dockerfile",
		map[string]string{"BUILDCTL_BATCH_KNOWN": "value"},
	)
	if err == nil || err.Error() != "Dockerfile contains unresolved build variable(s): BUILDCTL_BATCH_A, BUILDCTL_BATCH_Z" {
		t.Fatalf("unresolved variable ordering changed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(path, []byte("FROM $BUILDCTL_BATCH_BASE\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := replaceBuildVariablesInFile(path, map[string]string{"BUILDCTL_BATCH_BASE": "scratch"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("replacement changed file permissions: got %o", info.Mode().Perm())
	}
}

func TestReplaceBuildVariablesInFileDoesNotRewriteUnchangedContent(t *testing.T) {
	for _, test := range []struct {
		name string
		vars map[string]string
	}{
		{name: "no variables"},
		{name: "no matching variable", vars: map[string]string{"BUILDCTL_BATCH_UNUSED": "value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Dockerfile")
			original := []byte("FROM scratch\n")
			if err := os.WriteFile(path, original, 0o444); err != nil {
				t.Fatal(err)
			}
			if err := replaceBuildVariablesInFile(path, test.vars); err != nil {
				t.Fatalf("unchanged read-only file was rewritten: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("unchanged content changed: got %q, want %q", got, original)
			}
		})
	}
}
