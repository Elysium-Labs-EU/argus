package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeAdapter drops an executable shell script named argus-runtime-<name>
// into a fresh temp dir and points $PATH at that dir alone, so
// LaunchViaRuntime's exec.LookPath can only ever resolve this fake — never a
// real backend. No real Docker/Podman is required to exercise the contract.
func writeFakeAdapter(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "argus-runtime-"+name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake adapter: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestLaunchViaRuntimeReturnsAdapterStdoutLine(t *testing.T) {
	writeFakeAdapter(t, "fake", `echo "wrapped: $ARGUS_RUNTIME_CMD"`)

	line, err := LaunchViaRuntime(context.Background(), "fake", "/repo/wt", "claude --permission-mode auto", nil)
	if err != nil {
		t.Fatalf("LaunchViaRuntime: %v", err)
	}
	want := `wrapped: claude --permission-mode auto "` + initialPrompt + `"`
	if line != want {
		t.Errorf("line: got %q want %q", line, want)
	}
}

func TestLaunchViaRuntimePassesWorktreeVerbatim(t *testing.T) {
	writeFakeAdapter(t, "fake", `echo "$ARGUS_RUNTIME_WORKTREE"`)

	line, err := LaunchViaRuntime(context.Background(), "fake", "/repo/my wt", "claude", nil)
	if err != nil {
		t.Fatalf("LaunchViaRuntime: %v", err)
	}
	if line != "/repo/my wt" {
		t.Errorf("worktree: got %q want %q", line, "/repo/my wt")
	}
}

func TestLaunchViaRuntimePassesEnvAsJSON(t *testing.T) {
	// The fake adapter's one stdout line *is* the ARGUS_RUNTIME_ENV it
	// received, so the test can assert exactly what crossed the contract
	// boundary — the credproxy sentinel and base URL, round-tripped as JSON.
	writeFakeAdapter(t, "fake", `echo "$ARGUS_RUNTIME_ENV"`)

	env := []string{"ANTHROPIC_API_KEY=argus-sentinel-abc", "ANTHROPIC_BASE_URL=http://127.0.0.1:9999"}
	line, err := LaunchViaRuntime(context.Background(), "fake", "/repo/wt", "claude", env)
	if err != nil {
		t.Fatalf("LaunchViaRuntime: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("adapter's ARGUS_RUNTIME_ENV was not valid JSON: %v (%s)", err, line)
	}
	want := map[string]string{
		"ANTHROPIC_API_KEY":  "argus-sentinel-abc",
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:9999",
	}
	if got["ANTHROPIC_API_KEY"] != want["ANTHROPIC_API_KEY"] || got["ANTHROPIC_BASE_URL"] != want["ANTHROPIC_BASE_URL"] {
		t.Errorf("ARGUS_RUNTIME_ENV round-trip: got %v want %v", got, want)
	}
}

func TestLaunchViaRuntimeEmptyEnvIsEmptyJSONObject(t *testing.T) {
	// A nil/empty workerEnv (no credential broker configured) must still cross
	// as valid JSON, not an empty string a receiving adapter's json.Unmarshal
	// would choke on.
	writeFakeAdapter(t, "fake", `echo "$ARGUS_RUNTIME_ENV"`)

	line, err := LaunchViaRuntime(context.Background(), "fake", "/repo/wt", "claude", nil)
	if err != nil {
		t.Fatalf("LaunchViaRuntime: %v", err)
	}
	if line != "{}" {
		t.Errorf("empty env: got %q want %q", line, "{}")
	}
}

func TestLaunchViaRuntimeNonZeroExitIsHardError(t *testing.T) {
	writeFakeAdapter(t, "broken", `echo "boom" >&2; exit 1`)

	_, err := LaunchViaRuntime(context.Background(), "broken", "/repo/wt", "claude", nil)
	if err == nil {
		t.Fatal("want error when adapter exits non-zero, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "argus-runtime-broken") {
		t.Errorf("error should name the adapter: %v", err)
	}
}

func TestLaunchViaRuntimeEmptyOutputIsHardError(t *testing.T) {
	writeFakeAdapter(t, "silent", `true`)

	_, err := LaunchViaRuntime(context.Background(), "silent", "/repo/wt", "claude", nil)
	if err == nil {
		t.Fatal("want error when adapter prints nothing, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "argus-runtime-silent") {
		t.Errorf("error should name the adapter: %v", err)
	}
}

func TestLaunchViaRuntimeMissingAdapterIsHardError(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH: nothing named argus-runtime-* resolves

	_, err := LaunchViaRuntime(context.Background(), "does-not-exist", "/repo/wt", "claude", nil)
	if err == nil {
		t.Fatal("want error when the adapter binary is missing, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "argus-runtime-does-not-exist") {
		t.Errorf("error should name the missing adapter: %v", err)
	}
}
