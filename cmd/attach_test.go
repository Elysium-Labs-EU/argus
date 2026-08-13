package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
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
	// RepoRoot must be resolved (not left empty) so RunE's repoconfig.Load
	// actually fires for --attach the same way it does for spawn — see
	// TestSuperviseAttachHonorsConfigMaxDiffLines for the end-to-end proof.
	if w.RepoRoot != wt {
		t.Errorf("RepoRoot = %q, want %q (wt is its own repo root, not a linked worktree)", w.RepoRoot, wt)
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

// TestSuperviseAttachRequiresExplicitBase covers the issue-14 fix: --attach must
// not silently fall back to the spawn-mode --base default (origin/main), since an
// attached worktree may have been branched from something else entirely — that
// silently measured/gated/reviewed the diff against the wrong ref. --attach must
// fail fast unless the operator states the real base explicitly.
func TestSuperviseAttachRequiresExplicitBase(t *testing.T) {
	// supervise's RunE now checks its prerequisites (herdr here, for --attach)
	// are on PATH before reaching the --base validation; stub the lookPath seam
	// so this test is hermetic on a box without herdr installed.
	setBinaryLookPath(t, found)
	wt := attachWorktree(t, "attached")
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--attach", "--worktrees", wt})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--attach without --base should error")
	}
	if !strings.Contains(err.Error(), "--base") {
		t.Errorf("error should mention --base, got: %v", err)
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

// TestSuperviseAttachHonorsConfigMaxDiffLines covers issue #626: --attach used
// to gate every worktree against the built-in 400-line default because
// attachWorkers never set Worker.RepoRoot, so RunE's `if repoRoot != ""`
// repoconfig.Load never fired for --attach (see attachWorkers' RepoRoot
// resolution and RunE's repoRoot handling in cmd/supervise.go). Here the
// repo's own .argus/config.yml sets max_diff_lines to 1 — far below the
// 5-line code-only diff this test manufactures — so the run only escalates
// on diff size if the config value actually won. Before the fix this diff
// would have sailed through against the 400-line default with no
// max-diff-lines reason at all.
func TestSuperviseAttachHonorsConfigMaxDiffLines(t *testing.T) {
	setBinaryLookPath(t, found)
	t.Setenv("HOME", t.TempDir())

	wt := attachWorktree(t, "attached")
	maxDiffLines := 1
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{MaxDiffLines: &maxDiffLines}); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	// An untracked, non-test/doc file: MeasureDiff counts every line of an
	// untracked file as a code insertion (see measure.go's untrackedFiles
	// handling), so this alone produces a 5 code-line diff with --base HEAD
	// (merge-base(HEAD, HEAD) == HEAD, so only uncommitted/untracked changes
	// count — see MeasureDiff/ResolveEffectiveDiffBase).
	extra := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(filepath.Join(wt, "extra.go"), []byte(extra), 0o600); err != nil {
		t.Fatalf("writing extra.go: %v", err)
	}
	// Self-reported DiffStat matches what MeasureDiff will measure (1 file, 5
	// insertions), so the under-report and zero-files hard checks
	// (applyMeasuredChecks) stay quiet and the only escalation reason left is
	// the MaxDiffLines ceiling itself — isolating exactly what this test
	// means to prove.
	status := &protocol.Status{
		Task:     "attached",
		Phase:    protocol.PhaseAwaitingReview,
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 5},
		Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
	}
	if err := protocol.Write(protocol.StatusPath(wt), status); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--attach", "--worktrees", wt, "--base", "HEAD", "--timeout", "5s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("supervise --attach: %v", err)
	}

	approval, found, err := protocol.LoadApproval(wt)
	if err != nil {
		t.Fatalf("LoadApproval: %v", err)
	}
	if !found {
		t.Fatalf("no verdict written; output:\n%s", buf.String())
	}
	if approval.Approved {
		t.Errorf("want the config's max_diff_lines=1 to escalate a 5-line diff, got approved with reasons %v", approval.Reasons)
	}
	wantReason := "exceeds max 1"
	hasReason := false
	for _, r := range approval.Reasons {
		if strings.Contains(r, wantReason) {
			hasReason = true
		}
	}
	if !hasReason {
		t.Errorf("reasons %v should contain %q (the repo config's max_diff_lines, not the 400-line built-in default)", approval.Reasons, wantReason)
	}
}
