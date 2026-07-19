package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// attachWorktree makes a real git checkout on branch feat-x with a seeded typed
// status file, so attachWorkers can read the branch from git and the task from
// the status — the two things it derives without the operator restating them.
func attachWorktree(t *testing.T, task string) string {
	t.Helper()
	wt := t.TempDir()
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", wt}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	git("checkout", "-q", "-b", "feat-x")
	git("commit", "-q", "--allow-empty", "-m", "base")
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Task: task, Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	return wt
}

func TestAttachWorkersFromExplicitWorktrees(t *testing.T) {
	wt := attachWorktree(t, "issue #146 degraded-mode")
	// No herdr needed for explicit --worktrees.
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("herdr should not be called for explicit worktrees")
	})
	workers, err := attachWorkers(context.Background(), client, "", []string{wt})
	if err != nil {
		t.Fatalf("attachWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want 1 worker, got %d", len(workers))
	}
	w := workers[0]
	if w.Worktree != wt || w.Branch != "feat-x" {
		t.Errorf("worker worktree/branch: got %s / %s", w.Worktree, w.Branch)
	}
	if w.Task != "issue #146 degraded-mode" {
		t.Errorf("task not read from status file: %q", w.Task)
	}
}

func TestAttachWorkersFromWorkspace(t *testing.T) {
	wt := attachWorktree(t, "") // empty task → falls back to branch
	reply, err := json.Marshal(map[string]any{
		"result": map[string]any{
			"panes": []map[string]any{
				{"pane_id": "w9:p1", "cwd": wt, "workspace_id": "w9", "agent_status": "working"},
				{"pane_id": "wX:p1", "cwd": "/other", "workspace_id": "wX", "agent_status": "idle"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return reply, nil
	})

	workers, err := attachWorkers(context.Background(), client, "w9", nil)
	if err != nil {
		t.Fatalf("attachWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want only the w9 pane, got %d workers", len(workers))
	}
	w := workers[0]
	if w.PaneID != "w9:p1" || w.Worktree != wt {
		t.Errorf("worker pane/worktree: got %s / %s", w.PaneID, w.Worktree)
	}
	if w.Task != "feat-x" { // empty status task → branch name
		t.Errorf("task should fall back to branch, got %q", w.Task)
	}
}

func TestAttachWorkersNoTargets(t *testing.T) {
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"result":{"panes":[]}}`), nil
	})
	if _, err := attachWorkers(context.Background(), client, "nope", nil); err == nil {
		t.Fatal("expected an error when no attach targets resolve")
	}
}
