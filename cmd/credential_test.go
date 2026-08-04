package cmd

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
)

// TestResolveCredentialOverridesConfigPathError covers config.Path()'s
// error return: on Unix, os.UserHomeDir fails when both $HOME and
// $ARGUS_CONFIG_FILE are unset, since it has nothing left to resolve
// ~/.argus/config.toml against.
func TestResolveCredentialOverridesConfigPathError(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", "")
	t.Setenv("HOME", "")

	if _, err := resolveCredentialOverrides(nil); err == nil {
		t.Fatal("expected an error when neither $ARGUS_CONFIG_FILE nor $HOME is set")
	}
}

// TestResolveCredentialOverridesConfigLoadError covers config.Load's error
// return: a config file whose TOML is present but malformed (here, a
// quoted value with an incomplete escape sequence) must surface as an
// error rather than being silently dropped.
func TestResolveCredentialOverridesConfigLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[credential]\nanthropic = \"\\x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_CONFIG_FILE", path)

	if _, err := resolveCredentialOverrides(nil); err == nil {
		t.Fatal("expected an error for malformed config.toml")
	}
}

// anthropicBaseURLFrom extracts the ANTHROPIC_BASE_URL WorkerEnv issued, so
// a test can address the running proxy without reaching into its
// unexported fields.
func anthropicBaseURLFrom(t *testing.T, env []string) string {
	t.Helper()
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "ANTHROPIC_BASE_URL" {
			return v
		}
	}
	t.Fatalf("no ANTHROPIC_BASE_URL in %v", env)
	return ""
}

// TestStartCredentialProxyLogsGatedCall covers the logger callback
// startCredentialProxy wires into credproxy.New: an unauthenticated request
// against the running proxy is refused before ever reaching the real
// upstream, but must still reach the eventlog through the callback the
// proxy is required to invoke on every gated call.
func TestStartCredentialProxyLogsGatedCall(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("MY_CLAUDE_KEY", "sk-real-key")

	var buf bytes.Buffer
	logger := eventlog.New(&buf, "test", "run", nil)

	proxy, _, cleanup, err := startCredentialProxy(logger, map[string]string{"anthropic": "MY_CLAUDE_KEY"})
	defer cleanup()
	if err != nil {
		t.Fatalf("startCredentialProxy: %v", err)
	}
	if proxy == nil {
		t.Fatal("expected a proxy fronting the overridden anthropic key")
	}

	env := proxy.WorkerEnv("agent-1", "feat/x")
	base := anthropicBaseURLFrom(t, env)

	// Deliberately omit the sentinel WorkerEnv issued: gate() logs even an
	// unrecognized caller, before it ever forwards to the real upstream.
	resp, err := http.Get(base + "/v1/messages")
	if err != nil {
		t.Fatalf("request against proxy: %v", err)
	}
	if resp == nil {
		t.Fatal("Get returned a nil response")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an unrecognized sentinel", resp.StatusCode)
	}

	if !strings.Contains(buf.String(), `"action":"credproxy"`) {
		t.Errorf("expected the gated call to be logged via the callback, got %q", buf.String())
	}
}
