package builddaemon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	want := Config{
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
	if got := DefaultConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default config changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestNormalizeConfigPreservesFallbacks(t *testing.T) {
	cfg := Config{MaxConcurrency: -1}
	if err := normalizeConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	want := Config{
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
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("normalized config changed:\n got: %#v\nwant: %#v", cfg, want)
	}
}

func TestNormalizeConfigPreservesValidationOrder(t *testing.T) {
	cfg := Config{
		AuthToken:     "inline",
		AuthTokenFile: "token-file",
		KeepTTL:       -time.Second,
	}
	if err := normalizeConfig(&cfg); err == nil || err.Error() != "--auth-token and --auth-token-file are mutually exclusive" {
		t.Fatalf("unexpected first validation error: %v", err)
	}

	cfg = Config{KeepTTL: -time.Second}
	if err := normalizeConfig(&cfg); err == nil || err.Error() != "--keep-ttl must not be negative" {
		t.Fatalf("unexpected keep-ttl validation error: %v", err)
	}
}

func TestLoadAuthTokenPreservesValidationAndTrimming(t *testing.T) {
	cfg := Config{AuthToken: "inline", AuthTokenFile: "token-file"}
	if err := loadAuthToken(&cfg); err == nil || err.Error() != "--auth-token and --auth-token-file are mutually exclusive" {
		t.Fatalf("unexpected mutually exclusive token error: %v", err)
	}

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = Config{AuthTokenFile: tokenPath}
	if err := loadAuthToken(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "secret-token" {
		t.Fatalf("token was not trimmed: %q", cfg.AuthToken)
	}

	if err := os.WriteFile(tokenPath, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = Config{AuthTokenFile: tokenPath}
	if err := loadAuthToken(&cfg); err == nil || err.Error() != "auth token file is empty" {
		t.Fatalf("unexpected empty token error: %v", err)
	}
}

func TestApplyNoFileLimitNoop(t *testing.T) {
	if err := applyNoFileLimit(0); err != nil {
		t.Fatalf("applyNoFileLimit(0): %v", err)
	}
}
