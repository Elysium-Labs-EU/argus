package supervisor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/Elysium_Labs/argus/internal/herdr"
	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

// recordingRunner records every herdr invocation and answers `pane list` with a
// fixed reply, so tests can assert exactly which side effects a run performed.
type recordingRunner struct {
	paneList string
	calls    [][]string
	mu       sync.Mutex
}

func (r *recordingRunner) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
		return []byte(r.paneList), nil
	}
	return []byte(`{"result":{}}`), nil
}

func (r *recordingRunner) subcommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

const twoPaneList = `{"result":{"panes":[
{"pane_id":"1-2","cwd":"/repo-a","agent":"claude","agent_status":"idle"},
{"pane_id":"1-3","cwd":"/repo-b","agent":"claude","agent_status":"idle"}
]}}`

func TestBuildPlanDerivesWorktreeAndBrief(t *testing.T) {
	plans := BuildPlan([]Worker{
		{Task: "eos#42", Branch: "feat-x", RepoRoot: "/repo-a"},
	})
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(plans))
	}
	p := plans[0]
	want := "/repo-a/.claude/worktrees/feat-x"
	if p.Worktree != want {
		t.Errorf("worktree: got %q want %q", p.Worktree, want)
	}
	if !strings.Contains(p.Brief, "eos#42") {
		t.Errorf("brief should carry the task")
	}
	if !strings.Contains(p.Brief, protocol.WriterBrief) {
		t.Errorf("brief should embed the shared status-writing contract")
	}
	if !strings.Contains(p.Brief, want) {
		t.Errorf("brief should tell the worker to stay in its worktree")
	}
}

func TestRunDryRunHasNoSideEffects(t *testing.T) {
	rr := &recordingRunner{paneList: twoPaneList}
	var buf bytes.Buffer
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   herdr.NewWithRunner(rr.run),
		Base:     "origin/main",
		Interval: time.Second,
	}
	workers := []Worker{
		{Task: "a", Branch: "feat-a", RepoRoot: "/repo-a", PaneID: "1-2"},
		{Task: "b", Branch: "feat-b", RepoRoot: "/repo-b", PaneID: "1-3"},
	}
	if err := Run(context.Background(), cfg, workers, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "dry run") {
		t.Errorf("dry run output should say so; got:\n%s", out)
	}
	if !strings.Contains(out, "/repo-a/.claude/worktrees/feat-a") {
		t.Errorf("dry run should print the derived worktree path")
	}

	// A dry run performs no herdr calls at all — it's pure planning.
	if calls := rr.subcommands(); len(calls) != 0 {
		t.Errorf("dry run performed side effects: %v", calls)
	}
}

func TestRunRefusesCollidingWorktrees(t *testing.T) {
	rr := &recordingRunner{paneList: twoPaneList}
	cfg := &Config{
		Out:    &bytes.Buffer{},
		Now:    time.Now,
		Client: herdr.NewWithRunner(rr.run),
	}
	// Same repo + same branch → same worktree → must be refused, even though the
	// two panes are distinct.
	workers := []Worker{
		{Task: "a", Branch: "feat-x", RepoRoot: "/repo", PaneID: "1-2"},
		{Task: "b", Branch: "feat-x", RepoRoot: "/repo", PaneID: "1-3"},
	}
	if err := Run(context.Background(), cfg, workers, true); err == nil {
		t.Fatal("want error when two workers target the same worktree, got nil")
	}

	// Two panes launched from the same repo root but with distinct branches is
	// fine — the shared launch cwd is not a collision.
	ok := []Worker{
		{Task: "a", Branch: "feat-a", RepoRoot: "/repo", PaneID: "1-2"},
		{Task: "b", Branch: "feat-b", RepoRoot: "/repo", PaneID: "1-3"},
	}
	if err := Run(context.Background(), cfg, ok, true); err != nil {
		t.Fatalf("distinct branches from one repo root should pass, got %v", err)
	}
}

func TestExecuteWritesSettingsBriefAndSpawnsInRootPane(t *testing.T) {
	repo := t.TempDir()
	// herdr: worktree create returns a root pane; everything else succeeds.
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
	cfg := &Config{
		Client: herdr.NewWithRunner(runner),
		Now:    time.Now,
		Base:   "main",
	}
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}})

	states, err := execute(context.Background(), cfg, plans)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("want 1 state, got %d", len(states))
	}
	// With no --panes, the worker runs in the pane herdr opened in the worktree.
	if states[0].paneID != "w9:p1" {
		t.Errorf("paneID: got %q want w9:p1 (worktree root pane)", states[0].paneID)
	}
	wt := plans[0].Worktree
	if _, err := os.Stat(filepath.Join(wt, ".claude", "settings.local.json")); err != nil {
		t.Errorf("settings not written: %v", err)
	}
	if _, err := os.Stat(protocol.BriefPath(wt)); err != nil {
		t.Errorf("brief not written: %v", err)
	}
}

func TestPollStatusReadsTypedFileNotScrollback(t *testing.T) {
	wt := t.TempDir()
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}

	// Worker writes a terminal status; pollStatus must pick it up and stop.
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:     "a",
		Phase:    protocol.PhaseDone,
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 10},
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, 10*time.Millisecond, st)

	if !st.hasFile {
		t.Fatal("pollStatus should have read the status file")
	}
	if st.status.Phase != protocol.PhaseDone {
		t.Errorf("phase: got %q want done", st.status.Phase)
	}
	if ctx.Err() != nil {
		t.Errorf("pollStatus should have returned on terminal phase, not on timeout")
	}
}
