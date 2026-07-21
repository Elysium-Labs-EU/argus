package herdr

import (
	"context"
	"errors"
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
