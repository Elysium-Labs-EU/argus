package supervisor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
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

func TestTaskLabel(t *testing.T) {
	cases := []struct {
		name string
		task string
		want string
	}{
		{"issue ref mid-string wins over the line", "fix #42: reduce CRAP", "#42"},
		{"issue ref at start", "#7 tidy up logging", "#7"},
		{"first of multiple issue refs", "see #1 and #2", "#1"},
		{"hash with no trailing digits falls back to the line", "hash with no digits # end", "hash with no digits # end"},
		{"trailing bare hash falls back to the line", "task #", "task #"},
		{"digits stop at first non-digit", "#123abc rest", "#123"},
		{"no hash, short line, unchanged", "no hash here", "no hash here"},
		{"multi-line uses only the first line", "first line\nsecond line", "first line"},
		{"line is trimmed after newline split", "  hello  \nworld", "hello"},
		{"line over 60 chars is truncated to 60", strings.Repeat("a", 90), strings.Repeat("a", 60)},
		{"truncation retrims trailing whitespace at the cut", strings.Repeat("a", 59) + "   more text after the boundary", strings.Repeat("a", 59)},
		{"empty task", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskLabel(tc.task); got != tc.want {
				t.Errorf("taskLabel(%q) = %q, want %q", tc.task, got, tc.want)
			}
		})
	}
}

func TestBuildPlanDerivesWorktreeAndBrief(t *testing.T) {
	plans := BuildPlan([]Worker{
		{Task: "eos#42", Branch: "feat-x", RepoRoot: "/repo-a"},
	}, nil)
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

func TestResolvePaneID(t *testing.T) {
	cases := []struct {
		name    string
		plan    *WorkerPlan
		wt      herdr.Worktree
		want    string
		wantErr bool
	}{
		{
			name: "caller-supplied pane wins",
			plan: &WorkerPlan{Worker: Worker{Task: "t", PaneID: "1-2"}},
			wt:   herdr.Worktree{RootPaneID: "w9:p1"},
			want: "1-2",
		},
		{
			name: "falls back to the worktree's root pane",
			plan: &WorkerPlan{Worker: Worker{Task: "t"}},
			wt:   herdr.Worktree{RootPaneID: "w9:p1"},
			want: "w9:p1",
		},
		{
			name:    "neither present is an error",
			plan:    &WorkerPlan{Worker: Worker{Task: "t"}},
			wt:      herdr.Worktree{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePaneID(tc.plan, tc.wt)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolvePaneID() = %q, want %q", got, tc.want)
			}
		})
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
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, nil)

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

func TestExecuteWrapsSpawnLineViaRuntimeAdapterWhenConfigured(t *testing.T) {
	writeFakeAdapter(t, "fake", `echo "ISOLATED: $ARGUS_RUNTIME_CMD"`)

	repo := t.TempDir()
	rr := &recordingRunner{}
	runner := func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		}
		return rr.run(ctx, args...)
	}
	cfg := &Config{
		Client:        herdr.NewWithRunner(runner),
		Now:           time.Now,
		Base:          "main",
		WorkerRuntime: "fake",
	}
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, nil)

	if _, err := execute(context.Background(), cfg, plans); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var spawnLine string
	for _, call := range rr.subcommands() {
		if strings.HasPrefix(call, "pane run ") {
			spawnLine = call
		}
	}
	if !strings.Contains(spawnLine, "ISOLATED:") {
		t.Errorf("spawn line should come from the runtime adapter's stdout, got %q", spawnLine)
	}
	// The adapter path must never fall back to the plain cd+launcher line.
	if strings.Contains(spawnLine, "cd "+plans[0].Worktree) {
		t.Errorf("spawn line should not contain the unwrapped cd command when a runtime adapter is configured: %q", spawnLine)
	}
}

func TestResolveSpawnLineLeavesLauncherUnresolvedForRuntimeAdapter(t *testing.T) {
	// The fake launcher binary sits on PATH so that, if resolveSpawnLine
	// wrongly resolves it (issue #56), the wrong absolute host path leaks
	// into the line handed to the container adapter.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("writing fake launcher: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "argus-runtime-fake"), []byte(`#!/bin/sh
echo "$ARGUS_RUNTIME_CMD"
`), 0o755); err != nil {
		t.Fatalf("writing fake adapter: %v", err)
	}
	t.Setenv("PATH", dir)

	cfg := &Config{WorkerRuntime: "fake", Launcher: "claude"}
	p := &WorkerPlan{Worker: Worker{Worktree: "/repo/wt"}}

	line, err := resolveSpawnLine(context.Background(), cfg, p, nil)
	if err != nil {
		t.Fatalf("resolveSpawnLine: %v", err)
	}
	want := `claude "` + initialPrompt + `"`
	if line != want {
		t.Errorf("resolveSpawnLine leaked a host-resolved launcher path into the runtime adapter's command: got %q want %q", line, want)
	}
}

func TestResolveSpawnLineResolvesLauncherForHostShell(t *testing.T) {
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(launcherPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("writing fake launcher: %v", err)
	}
	t.Setenv("PATH", dir)

	cfg := &Config{Launcher: "claude"}
	p := &WorkerPlan{Worker: Worker{Worktree: "/repo/wt"}}

	line, err := resolveSpawnLine(context.Background(), cfg, p, nil)
	if err != nil {
		t.Fatalf("resolveSpawnLine: %v", err)
	}
	if !strings.Contains(line, launcherPath) {
		t.Errorf("resolveSpawnLine should still resolve the launcher to an absolute path on the plain host-shell path: got %q, want it to contain %q", line, launcherPath)
	}
}

func TestExecuteFailsWhenConfiguredRuntimeAdapterIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no argus-runtime-* resolves

	repo := t.TempDir()
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
	cfg := &Config{
		Client:        herdr.NewWithRunner(runner),
		Now:           time.Now,
		Base:          "main",
		WorkerRuntime: "does-not-exist",
	}
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, nil)

	if _, err := execute(context.Background(), cfg, plans); err == nil {
		t.Fatal("want an error when the configured runtime adapter cannot be resolved, got nil")
	}
}

func TestRenderPlanNeverExecsAnAdapter(t *testing.T) {
	// A runtime name that resolves to nothing on PATH must not make renderPlan
	// (the --dry-run path) fail or hang — it only prints a note, it never execs
	// LaunchViaRuntime. This is the dry-run "makes no changes" contract.
	t.Setenv("PATH", t.TempDir())

	var buf bytes.Buffer
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: "/repo"}}, nil)
	renderPlan(&buf, "origin/main", "claude", "docker", nil, plans)

	out := buf.String()
	if !strings.Contains(out, "wrapped by runtime adapter: docker") {
		t.Errorf("dry-run output should note the configured runtime adapter: %s", out)
	}
	if !strings.Contains(out, "cd '/repo/.claude/worktrees/feat-x'") {
		t.Errorf("dry-run should still print the plain SpawnCommand line: %s", out)
	}
}

// gitWorktreeWithDiff makes a temp git repo with one committed file and an
// uncommitted edit, so DiffFor(wt, "HEAD") returns a non-empty diff.
func gitWorktreeWithDiff(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n\nvar Added = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return wt
}

func TestReviewEscalationsAutoApprovesCleanAndReviewsEscalated(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	policy := DefaultReviewPolicy()

	clean := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "clean", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 3},
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	escalated := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "bad", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
		},
	}

	cfg := &Config{
		Base:     "HEAD",
		Policy:   &policy,
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"ok","findings":[]}`)),
	}
	reviewEscalations(context.Background(), cfg, []*workerState{clean, escalated})

	if clean.review != nil {
		t.Error("auto-approved worker must not be reviewed")
	}
	if escalated.review == nil {
		t.Fatal("escalated worker should have a review verdict")
	}
	if escalated.review.Decision != "approve" {
		t.Errorf("decision: got %q want approve", escalated.review.Decision)
	}
	if escalated.reviewErr != nil {
		t.Errorf("unexpected review error: %v", escalated.reviewErr)
	}
}

func TestReviewEscalationsWithoutReviewerJustGates(t *testing.T) {
	// No reviewer configured: an escalated worker is surfaced (no verdict, no
	// error), never sent to an LLM.
	escalated := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "bad", Worktree: t.TempDir()}},
		status:  protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "needs a decision"},
	}
	cfg := &Config{Base: "HEAD", Policy: nil}
	reviewEscalations(context.Background(), cfg, []*workerState{escalated})
	if escalated.review != nil || escalated.reviewErr != nil {
		t.Errorf("no reviewer should mean no verdict and no error: review=%v err=%v", escalated.review, escalated.reviewErr)
	}
}

func TestPollStatusReturnsOnDeadlineWhenWorkerNeverTerminates(t *testing.T) {
	wt := t.TempDir()
	// A worker stuck in a non-terminal phase: without a deadline pollStatus would
	// loop forever. The status file exists but never reaches a terminal phase.
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:  "stuck",
		Phase: protocol.PhaseWorking,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "stuck", Worktree: wt}}}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		// No parent cancel; only the 40ms deadline should stop it.
		pollStatus(context.Background(), 5*time.Millisecond, 40*time.Millisecond, nil, st)
		close(done)
	}()
	select {
	case <-done:
		if time.Since(start) > time.Second {
			t.Error("pollStatus took too long; deadline did not fire promptly")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollStatus did not return on its deadline — a hung worker would block forever")
	}
	if st.status.Phase != protocol.PhaseWorking {
		t.Errorf("phase: got %q want working", st.status.Phase)
	}
}

func TestPollStatusLogsUnreadableStatus(t *testing.T) {
	wt := t.TempDir()
	// Write a malformed (non-JSON) status file: previously swallowed silently.
	if err := os.MkdirAll(filepath.Dir(protocol.StatusPath(wt)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol.StatusPath(wt), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := eventlog.New(&buf, "supervise", "run1", nil)
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "broken", Worktree: wt}}}

	pollStatus(context.Background(), 5*time.Millisecond, 30*time.Millisecond, logger, st)

	if !strings.Contains(buf.String(), "status_unreadable") {
		t.Errorf("expected a status_unreadable event, got:\n%s", buf.String())
	}
	if st.hasFile {
		t.Error("a malformed status must not be treated as a valid report")
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
	pollStatus(ctx, 10*time.Millisecond, 0, nil, st)

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
