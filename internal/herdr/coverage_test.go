package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaneRunPropagatesError(t *testing.T) {
	c := NewWithRunner(fakeRunner(`{"result":{}}`, nil))
	if err := c.PaneRun(context.Background(), "w1:p1", "echo hi"); err != nil {
		t.Errorf("PaneRun: %v", err)
	}
	sentinel := errors.New("herdr down")
	c = NewWithRunner(fakeRunner("", sentinel))
	if err := c.PaneRun(context.Background(), "w1:p1", "echo"); !errors.Is(err, sentinel) {
		t.Errorf("PaneRun should propagate the runner error, got %v", err)
	}
}

func TestWorktreeOpenReturnsRootPane(t *testing.T) {
	reply := `{"result":{"root_pane":{"pane_id":"wZ:p1"},"worktree":{"path":"/wt/x"}}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	wt, err := c.WorktreeOpen(context.Background(), "/repo", "/wt/x")
	if err != nil {
		t.Fatalf("WorktreeOpen: %v", err)
	}
	if wt.RootPaneID != "wZ:p1" || wt.Path != "/wt/x" {
		t.Errorf("unexpected worktree: %+v", wt)
	}
}

func TestWorktreeOpenFallsBackToRequestedPath(t *testing.T) {
	// herdr omitted the worktree path; the client falls back to the one asked for.
	reply := `{"result":{"root_pane":{"pane_id":"wZ:p1"}}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	wt, err := c.WorktreeOpen(context.Background(), "/repo", "/asked/path")
	if err != nil {
		t.Fatalf("WorktreeOpen: %v", err)
	}
	if wt.Path != "/asked/path" {
		t.Errorf("path fallback failed: %+v", wt)
	}
}

func TestWorktreeOpenPassesCwd(t *testing.T) {
	// herdr's `worktree open` treats the caller's own pane as "not inside a
	// git work tree" unless --cwd names the repo the linked worktree belongs
	// to (see WorktreeOpen's doc comment) — assert the client actually sends
	// it, since a caller whose own pane isn't repo-rooted would otherwise get
	// a confusing "not_git_worktree" error with no indication why.
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{"root_pane":{"pane_id":"wZ:p1"}}}`), nil
	})
	if _, err := c.WorktreeOpen(context.Background(), "/repo/root", "/repo/root/.claude/worktrees/feat-x"); err != nil {
		t.Fatalf("WorktreeOpen: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--cwd /repo/root ") {
		t.Errorf("WorktreeOpen args = %q, want --cwd /repo/root before --path", joined)
	}
}

func TestPaneSplitErrorsWhenNoID(t *testing.T) {
	c := NewWithRunner(fakeRunner(`{"result":{"pane":{}}}`, nil))
	if _, err := c.PaneSplit(context.Background(), "w1:p1", "right"); err == nil {
		t.Error("PaneSplit should error when herdr returns no pane id")
	}
}

func TestNewUsesRealBinaryAndSurfacesExecError(t *testing.T) {
	// New() wires execRunner("herdr"); with no herdr on PATH the exec fails and
	// the error is surfaced (exercises execRunner's error path).
	t.Setenv("PATH", t.TempDir())
	c := New()
	if _, err := c.PaneList(context.Background()); err == nil {
		t.Error("PaneList should error when the herdr binary is absent")
	}
}

// fakeHerdrBinary drops an executable named "herdr" into a fresh directory
// that prints reply to stderr and exits 1 — the real shape of a failing herdr
// invocation (see execRunner) — and returns that directory for PATH.
func fakeHerdrBinary(t *testing.T, reply string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "herdr")
	body := "#!/bin/sh\nprintf '%s' " + shellSingleQuote(reply) + " 1>&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("writing fake herdr binary: %v", err)
	}
	return dir
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestExecRunnerMapsAgentNotFoundToSentinel exercises execRunner's real
// ExitError/stderr path (not the fake Runner tests substitute elsewhere) to
// confirm a genuine herdr "agent_not_found" envelope becomes ErrAgentNotFound,
// which AgentGet depends on to distinguish "no live agent" from a real failure.
func TestExecRunnerMapsAgentNotFoundToSentinel(t *testing.T) {
	dir := fakeHerdrBinary(t, `{"error":{"code":"agent_not_found","message":"agent target w1:p1 not found"}}`)
	t.Setenv("PATH", dir)

	c := New()
	_, ok, err := c.AgentGet(context.Background(), "w1:p1")
	if err != nil {
		t.Fatalf("AgentGet: want no error for agent_not_found, got %v", err)
	}
	if ok {
		t.Error("want ok=false for agent_not_found")
	}
}

// TestExecRunnerMapsTimeoutCodeToSentinel exercises execRunner's real
// ExitError/stderr path to confirm a genuine herdr "timeout" envelope (what
// `herdr agent wait` replies when its own --timeout elapses with no matching
// state observed) becomes ErrWaitTimeout, which callers depend on to
// distinguish "nothing to report yet" from a real failure.
func TestExecRunnerMapsTimeoutCodeToSentinel(t *testing.T) {
	dir := fakeHerdrBinary(t, `{"error":{"code":"timeout","message":"timed out waiting for agent status"}}`)
	t.Setenv("PATH", dir)

	c := New()
	_, err := c.AgentWait(context.Background(), "w1:p1", []string{"idle"}, 0)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("want ErrWaitTimeout, got %v", err)
	}
}

// TestExecRunnerMapsAgentPromptStalledToSentinel exercises execRunner's real
// ExitError/stderr path to confirm a genuine herdr "agent_prompt_stalled"
// envelope becomes ErrAgentPromptStalled, which dispatchIntoPane depends on
// to fall back to a plain pane submission instead of aborting.
func TestExecRunnerMapsAgentPromptStalledToSentinel(t *testing.T) {
	dir := fakeHerdrBinary(t, `{"error":{"code":"agent_prompt_stalled","message":"agent prompt produced no observed state change within 5000 ms; status is done and state_change_seq remained 3"}}`)
	t.Setenv("PATH", dir)

	c := New()
	err := c.AgentPrompt(context.Background(), "w1:p1", "hello", 0)
	if !errors.Is(err, ErrAgentPromptStalled) {
		t.Fatalf("want ErrAgentPromptStalled, got %v", err)
	}
}

// TestExecRunnerPreservesOtherErrorCodes confirms only "agent_not_found" is
// special-cased: a different herdr error code still surfaces as a real error.
func TestExecRunnerPreservesOtherErrorCodes(t *testing.T) {
	dir := fakeHerdrBinary(t, `{"error":{"code":"pane_not_found","message":"pane w1:p1 not found"}}`)
	t.Setenv("PATH", dir)

	c := New()
	_, ok, err := c.AgentGet(context.Background(), "w1:p1")
	if err == nil {
		t.Fatal("want an error for pane_not_found")
	}
	if ok {
		t.Error("want ok=false on error")
	}
	if !strings.Contains(err.Error(), "pane_not_found") {
		t.Errorf("want the original error code surfaced, got %v", err)
	}
}
