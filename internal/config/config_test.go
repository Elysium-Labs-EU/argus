package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPathUsesEnvOverride(t *testing.T) {
	t.Setenv(pathEnvVar, "/tmp/argus-test-config.toml")
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/argus-test-config.toml" {
		t.Errorf("Path = %q, want /tmp/argus-test-config.toml", got)
	}
}

func TestLoadMissingFileReturnsZeroConfig(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Credential) != 0 {
		t.Errorf("expected empty Credential, got %v", cfg.Credential)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	cfg := Config{Credential: map[string]string{
		"anthropic":  "MY_CLAUDE_KEY",
		"github.com": "MY_GH_TOKEN",
	}}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Credential, cfg.Credential) {
		t.Errorf("round trip = %v, want %v", got.Credential, cfg.Credential)
	}
}

func TestSetCredentialKey(t *testing.T) {
	var cfg Config
	if err := cfg.Set("credential.github.com", "MY_GH_TOKEN"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if cfg.Credential["github.com"] != "MY_GH_TOKEN" {
		t.Errorf("Credential[github.com] = %q, want MY_GH_TOKEN", cfg.Credential["github.com"])
	}
}

func TestSetRejectsUnsupportedKey(t *testing.T) {
	var cfg Config
	if err := cfg.Set("launcher", "codex"); err == nil {
		t.Fatal("expected an error for an unsupported config key, got nil")
	}
}

func TestSetRejectsEmptyCredentialName(t *testing.T) {
	var cfg Config
	if err := cfg.Set("credential.", "x"); err == nil {
		t.Fatal("expected an error for an empty credential name, got nil")
	}
}

func TestLoadRejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("not a valid line at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a malformed config file, got nil")
	}
}

func TestParseTOMLIgnoresUnknownSections(t *testing.T) {
	cfg, err := parseTOML("[future]\nsomething = \"x\"\n\n[credential]\nanthropic = \"MY_KEY\"\n")
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	if cfg.Credential["anthropic"] != "MY_KEY" {
		t.Errorf("Credential[anthropic] = %q, want MY_KEY", cfg.Credential["anthropic"])
	}
	if len(cfg.Credential) != 1 {
		t.Errorf("expected only the recognized section's key, got %v", cfg.Credential)
	}
}

func TestParseTOMLHandlesCommentsAndBlankLines(t *testing.T) {
	cfg, err := parseTOML("# a comment\n\n[credential]\n# another comment\nanthropic = \"MY_KEY\" # trailing comment\n")
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	if cfg.Credential["anthropic"] != "MY_KEY" {
		t.Errorf("Credential[anthropic] = %q, want MY_KEY", cfg.Credential["anthropic"])
	}
}
