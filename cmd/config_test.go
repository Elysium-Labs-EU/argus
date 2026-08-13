package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/permission"
)

func TestConfigSetWritesCredentialOverride(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	cmd := newConfigCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"set", "credential.github.com", "MY_GH_TOKEN"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(buf.String(), "credential.github.com") {
		t.Errorf("expected confirmation output naming the key, got %q", buf.String())
	}

	overrides, err := resolveCredentialOverrides(nil)
	if err != nil {
		t.Fatalf("resolveCredentialOverrides: %v", err)
	}
	if overrides["github.com"] != "MY_GH_TOKEN" {
		t.Errorf("persisted override = %v, want github.com=MY_GH_TOKEN", overrides)
	}
}

func TestConfigGetRoundTripsValueWrittenBySet(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	setCmd := newConfigCmd()
	setCmd.SetOut(&bytes.Buffer{})
	setCmd.SetArgs([]string{"set", "credential.github.com", "MY_GH_TOKEN"})
	if err := setCmd.Execute(); err != nil {
		t.Fatalf("config set: %v", err)
	}

	getCmd := newConfigCmd()
	buf := &bytes.Buffer{}
	getCmd.SetOut(buf)
	getCmd.SetArgs([]string{"get", "credential.github.com"})
	if err := getCmd.Execute(); err != nil {
		t.Fatalf("config get: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "MY_GH_TOKEN" {
		t.Errorf("config get output = %q, want MY_GH_TOKEN", got)
	}
}

func TestConfigShowAliasRoundTripsValueWrittenBySet(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	setCmd := newConfigCmd()
	setCmd.SetOut(&bytes.Buffer{})
	setCmd.SetArgs([]string{"set", "credential.anthropic", "MY_CLAUDE_KEY"})
	if err := setCmd.Execute(); err != nil {
		t.Fatalf("config set: %v", err)
	}

	showCmd := newConfigCmd()
	buf := &bytes.Buffer{}
	showCmd.SetOut(buf)
	showCmd.SetArgs([]string{"show", "credential.anthropic"})
	if err := showCmd.Execute(); err != nil {
		t.Fatalf("config show: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "MY_CLAUDE_KEY" {
		t.Errorf("config show output = %q, want MY_CLAUDE_KEY", got)
	}
}

func TestConfigGetUnsetKeyErrorsNonZero(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	cmd := newConfigCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"get", "credential.github.com"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for an unset key, got nil")
	}
	if !strings.Contains(err.Error(), "not set") {
		t.Errorf("error = %q, want it to mention \"not set\"", err.Error())
	}
}

func TestConfigGetRejectsUnsupportedKey(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	cmd := newConfigCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"get", "launcher"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an unsupported config key, got nil")
	}
}

func TestConfigSetRejectsUnsupportedKey(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	cmd := newConfigCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"set", "launcher", "codex"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an unsupported config key, got nil")
	}
}

func TestResolveCredentialOverridesCLIWinsOverPersisted(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", filepath.Join(t.TempDir(), "config.toml"))

	cmd := newConfigCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"set", "credential.anthropic", "CONFIG_VAR"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config set: %v", err)
	}

	overrides, err := resolveCredentialOverrides(map[string]string{"anthropic": "CLI_VAR"})
	if err != nil {
		t.Fatalf("resolveCredentialOverrides: %v", err)
	}
	if overrides["anthropic"] != "CLI_VAR" {
		t.Errorf("CLI override = %q, want CLI_VAR to win over the persisted CONFIG_VAR", overrides["anthropic"])
	}
}

func TestConfigCheckReportsMissingAllowlistEntry(t *testing.T) {
	repo := t.TempDir()

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when no allow entry covers argus")
	}
	if !strings.Contains(buf.String(), "no Bash permission allowlist entry for argus") {
		t.Errorf("expected a warning naming the gap, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Bash(argus *)") {
		t.Errorf("expected the fix snippet to name the default entry, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "raw herdr pane-mutation calls are not denied") {
		t.Errorf("expected the deny gap reported alongside the allow gap, got %q", buf.String())
	}
	for _, e := range permission.DefaultDenyEntries() {
		if !strings.Contains(buf.String(), e) {
			t.Errorf("expected the fix snippet to name %s, got %q", e, buf.String())
		}
	}
}

func TestConfigCheckReportsExistingAllowlistEntry(t *testing.T) {
	repo := t.TempDir()
	settingsPath := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	denyJSON, err := json.Marshal(permission.DefaultDenyEntries())
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"permissions":{"allow":["Bash(argus *)"],"deny":` + string(denyJSON) + `}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config check: %v", err)
	}
	if !strings.Contains(buf.String(), "argus is allowlisted") {
		t.Errorf("expected a success message, got %q", buf.String())
	}
}

func TestConfigCheckWarnsWhenEntryCoversShipForce(t *testing.T) {
	repo := t.TempDir()

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo, "--write"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config check --write: %v", err)
	}
	if !strings.Contains(buf.String(), "also authorizes `argus ship --force`") {
		t.Errorf("expected a warning that the default blanket entry covers ship --force, got %q", buf.String())
	}
}

func TestConfigCheckNoWarningForScopedNonShipEntry(t *testing.T) {
	repo := t.TempDir()

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo, "--write", "--entry", "Bash(argus supervise *)"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config check --write: %v", err)
	}
	if strings.Contains(buf.String(), "also authorizes") {
		t.Errorf("expected no ship --force warning for a supervise-scoped entry, got %q", buf.String())
	}
}

func TestConfigCheckWriteAddsEntry(t *testing.T) {
	repo := t.TempDir()

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo, "--write"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config check --write: %v", err)
	}
	if !strings.Contains(buf.String(), "added Bash(argus *)") {
		t.Errorf("expected a confirmation naming the added entry, got %q", buf.String())
	}
	for _, e := range permission.DefaultDenyEntries() {
		if !strings.Contains(buf.String(), e) {
			t.Errorf("expected the deny write to report %s, got %q", e, buf.String())
		}
	}

	// A second run must find both already covered rather than writing again.
	cmd2 := newConfigCheckCmd()
	buf2 := &bytes.Buffer{}
	cmd2.SetOut(buf2)
	cmd2.SetArgs([]string{"--repo", repo})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("config check (second run): %v", err)
	}
	if !strings.Contains(buf2.String(), "argus is allowlisted") {
		t.Errorf("expected the written entry to be picked up, got %q", buf2.String())
	}
	if !strings.Contains(buf2.String(), "raw herdr pane-mutation calls are denied") {
		t.Errorf("expected the written deny block to be picked up, got %q", buf2.String())
	}
}

func TestConfigCheckWriteAppendsDenyToExistingDifferentBlock(t *testing.T) {
	repo := t.TempDir()
	settingsPath := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"permissions":{"deny":["Bash(rm -rf *)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo, "--write"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config check --write: %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	deny, ok := raw["permissions"].(map[string]any)["deny"].([]any)
	if !ok {
		t.Fatalf("deny list missing or wrong shape: %v", raw["permissions"])
	}
	if len(deny) != 1+len(permission.DefaultDenyEntries()) {
		t.Fatalf("deny = %v, want the pre-existing entry plus %d new ones", deny, len(permission.DefaultDenyEntries()))
	}
	if deny[0] != "Bash(rm -rf *)" {
		t.Errorf("pre-existing deny entry disturbed: %v", deny)
	}
}

func TestConfigCheckWriteIsNoOpWhenAllDenyEntriesPresent(t *testing.T) {
	repo := t.TempDir()
	settingsPath := filepath.Join(repo, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	denyJSON, err := json.Marshal(permission.DefaultDenyEntries())
	if err != nil {
		t.Fatal(err)
	}
	settings := `{"permissions":{"allow":["Bash(argus *)"],"deny":` + string(denyJSON) + `}}`
	if err = os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := newConfigCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo, "--write"})
	if err = cmd.Execute(); err != nil {
		t.Fatalf("config check --write: %v", err)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("--write rewrote the file even though every deny entry was already present")
	}
}

func TestStartCredentialProxyNoneResolvableReturnsNilProxy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	proxy, extraScrub, cleanup, err := startCredentialProxy(nil, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("startCredentialProxy: %v", err)
	}
	if proxy != nil {
		t.Errorf("expected a nil proxy when no known agent key resolves, got %v", proxy)
	}
	if len(extraScrub) != 0 {
		t.Errorf("expected no extra scrub vars, got %v", extraScrub)
	}
}

func TestStartCredentialProxyFrontsOverriddenAgentKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("MY_CLAUDE_KEY", "sk-real-key")

	proxy, extraScrub, cleanup, err := startCredentialProxy(nil, map[string]string{"anthropic": "MY_CLAUDE_KEY"})
	defer cleanup()
	if err != nil {
		t.Fatalf("startCredentialProxy: %v", err)
	}
	if proxy == nil {
		t.Fatal("expected a proxy fronting the overridden anthropic key")
	}
	if len(extraScrub) != 1 || extraScrub[0] != "MY_CLAUDE_KEY" {
		t.Errorf("extraScrub = %v, want [MY_CLAUDE_KEY] so the real secret's var is withheld from the worker", extraScrub)
	}
}
