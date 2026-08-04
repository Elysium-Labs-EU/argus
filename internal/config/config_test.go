package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestPathDefaultsToHomeArgusConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pathEnvVar, "")
	t.Setenv("HOME", home)
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".argus", "config.toml")
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestPathPropagatesUserHomeDirError(t *testing.T) {
	t.Setenv(pathEnvVar, "")
	t.Setenv("HOME", "")
	got, err := Path()
	if err == nil {
		t.Fatal("expected an error when $HOME is undefined, got nil")
	}
	if got != "" {
		t.Errorf("Path = %q, want empty string on error", got)
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

func TestLoadNonNotExistReadErrorPropagates(t *testing.T) {
	// A directory path fails ReadFile with a non-IsNotExist error, exercising
	// the branch distinct from the "file just doesn't exist yet" case.
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an error loading a directory as a config file, got nil")
	}
	if os.IsNotExist(err) {
		t.Errorf("expected a non-IsNotExist error, got %v", err)
	}
}

func TestSaveMkdirAllFailureWraps(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// blocker is a regular file, so MkdirAll(blocker/sub) cannot succeed.
	path := filepath.Join(blocker, "sub", "config.toml")
	err := Save(path, Config{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "creating config directory") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "creating config directory")
	}
}

func TestSaveWriteFileFailureWraps(t *testing.T) {
	// path is itself an existing directory, so its parent's MkdirAll
	// trivially succeeds but WriteFile(path, ...) cannot.
	dir := t.TempDir()
	err := Save(dir, Config{})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "writing config file") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "writing config file")
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

func TestEmptyConfigRoundTripsToZero(t *testing.T) {
	if got := encodeTOML(Config{}); got != "" {
		t.Errorf("encodeTOML(Config{}) = %q, want empty string", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, Config{}) {
		t.Errorf("round trip = %+v, want zero Config", got)
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

func TestParseTOMLStripsCommentAfterValueEndingInEscapedBackslash(t *testing.T) {
	// The trailing \\ is one escaped backslash, not an escaped closing quote,
	// so the quote right after it still closes the string and the following
	// " # c" must be recognized as a comment, not kept as part of the value.
	cfg, err := parseTOML(`[credential]` + "\n" + `anthropic = "a\\" # c` + "\n")
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	if cfg.Credential["anthropic"] != `a\` {
		t.Errorf(`Credential[anthropic] = %q, want "a\\"`, cfg.Credential["anthropic"])
	}
}

func TestParseTOMLRejectsBadQuotedValue(t *testing.T) {
	// \x is an incomplete Go/TOML escape (needs two hex digits), so
	// strconv.Unquote must fail on it.
	_, err := parseTOML("[credential]\n" + `anthropic = "\x"` + "\n")
	if err == nil {
		t.Fatal("expected an error for a badly-escaped quoted value, got nil")
	}
}

func TestParseTOMLDropsKeysBeforeAnySection(t *testing.T) {
	// A key line before any [section] header has section == "", which
	// matches neither "credential" nor anything else, so it is silently
	// ignored rather than erroring — pinning current behavior.
	cfg, err := parseTOML("anthropic = \"MY_KEY\"\n")
	if err != nil {
		t.Fatalf("parseTOML: %v", err)
	}
	if len(cfg.Credential) != 0 {
		t.Errorf("expected a pre-section key to be dropped, got %v", cfg.Credential)
	}
}
