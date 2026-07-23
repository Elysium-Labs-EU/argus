package supervisor

import (
	"bytes"
	"context"
	"io"
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
	if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
		return nil, herdr.ErrAgentNotFound
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
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			return nil, herdr.ErrAgentNotFound
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
	reg, rerr := protocol.LoadPaneRegistry(repo)
	if rerr != nil {
		t.Fatalf("LoadPaneRegistry: %v", rerr)
	}
	if reg.Panes[wt] != "w9:p1" {
		t.Errorf("pane registry entry for %s: got %q want w9:p1 (herdr's root pane), so prune can later close it", wt, reg.Panes[wt])
	}
}

// TestExecuteRefusesToSpawnIntoAPaneWithALiveAgent covers argus issue #107: PR
// #17 (closing issue #15) only gated the symptom — a terminal-phase worker
// reporting zero measured file changes — after the fact. It never addressed
// the mechanism #15 actually described: a headless spawn attaching to a
// stale, unrelated session's state. PaneRun's own doc comment already
// explains why that can happen — it delivers its command line as a literal
// chat message if the target pane already has a live agent sitting in it —
// so execute must check AgentGet and refuse rather than call PaneRun blind.
func TestExecuteRefusesToSpawnIntoAPaneWithALiveAgent(t *testing.T) {
	repo := t.TempDir()
	var paneRunCalled bool
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			// Simulate the exact #15 hazard: the pane execute is about to type
			// into already has a live, unrelated session running in it.
			return []byte(`{"result":{"agent":{"pane_id":"w9:p1","agent":"claude","agent_session":{"agent":"claude","value":"stale-session-uuid"}}}}`), nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "run" {
			paneRunCalled = true
		}
		return []byte(`{"result":{}}`), nil
	}
	cfg := &Config{
		Client: herdr.NewWithRunner(runner),
		Now:    time.Now,
		Base:   "main",
		Log:    eventlog.New(io.Discard, "supervise", "r", nil),
	}
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, nil)

	if _, err := execute(context.Background(), cfg, plans); err == nil {
		t.Fatal("execute should refuse to spawn into a pane with a live agent session, got nil error")
	} else if !strings.Contains(err.Error(), "stale-session-uuid") {
		t.Errorf("error should name the live session it refused to attach to, got: %v", err)
	}
	if paneRunCalled {
		t.Error("execute must not call PaneRun once AgentGet reports a live agent already occupies the pane")
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
	want := `claude "` + InitialPrompt + `"`
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
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			return nil, herdr.ErrAgentNotFound
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
		hasFile:         true,
		planEvidenceOK:  true,
		hasPlanEvidence: true,
		plan:            &WorkerPlan{Worker: Worker{Task: "clean", Branch: "b", Worktree: wt}},
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

// TestReviewEscalationsHardReasonSurvivesReviewerApprove is the regression for
// issue #105: a worker whose real diff dwarfs its self-report is exactly the
// case a reviewer's "approve" must not be able to talk past, because the
// discrepancy is evidence status.json can't be trusted for this change, not a
// stylistic judgment call. Wire a fake reviewer that always approves and assert
// the persisted verdict is still not-approved.
func TestReviewEscalationsHardReasonSurvivesReviewerApprove(t *testing.T) {
	wt := bigGitWorktree(t, "cmd/root.go", 50)
	liar := &workerState{
		hasFile:    true,
		measuredOK: true,
		plan:       &WorkerPlan{Worker: Worker{Task: "liar", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 1},
			Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	ds, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	liar.measured = ds
	liar.measuredFiles = files

	cfg := &Config{
		Base:     "HEAD",
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"looks fine to me","findings":[]}`)),
	}
	reviewEscalations(context.Background(), cfg, []*workerState{liar})

	if liar.review == nil || liar.review.Decision != "approve" {
		t.Fatalf("expected the fake reviewer to return approve, got %+v", liar.review)
	}
	approval, found, err := protocol.LoadApproval(wt)
	if err != nil {
		t.Fatalf("LoadApproval: %v", err)
	}
	if !found {
		t.Fatal("expected a persisted verdict")
	}
	if approval.Approved {
		t.Fatalf("a material under-report must stay not-approved even when --review approves, got %+v", approval)
	}
	if !strings.Contains(approval.Summary, "unwaivable") {
		t.Errorf("summary should call out the discrepancy as unwaivable, got %q", approval.Summary)
	}
}

func TestReviewEscalationsThreadsPriorFindingsFromVerdict(t *testing.T) {
	// argus issue #108: a prior request-changes verdict on this worktree must
	// reach the next review's prompt, so the reviewer re-checks the specific
	// defect instead of a fresh holistic pass that can miss it again.
	wt := gitWorktreeWithDiff(t)
	if err := protocol.WriteApproval(wt, &protocol.Approval{
		Approved: false,
		Source:   "review",
		Summary:  "found a defect",
		Reasons:  []string{"--dry-run mutates lifecycle.json on disk"},
	}); err != nil {
		t.Fatalf("seeding prior verdict: %v", err)
	}

	var gotPrompt string
	runner := func(_ context.Context, _, stdin string, _ ...string) ([]byte, error) {
		gotPrompt = stdin
		return []byte(`{"decision":"approve","summary":"ok","findings":[]}`), nil
	}

	escalated := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "bad", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
		},
	}
	policy := DefaultReviewPolicy()
	cfg := &Config{Base: "HEAD", Policy: &policy, Reviewer: NewReviewerWithRunner(runner)}
	reviewEscalations(context.Background(), cfg, []*workerState{escalated})

	if !strings.Contains(gotPrompt, "--dry-run mutates lifecycle.json on disk") {
		t.Errorf("review prompt did not carry the prior verdict's finding:\n%s", gotPrompt)
	}
	if !strings.Contains(strings.ToLower(gotPrompt), "prior review") {
		t.Errorf("review prompt did not instruct re-checking the prior finding:\n%s", gotPrompt)
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

// TestPollStatusIgnoresStatusFromBeforeDispatch covers argus issue #75: a
// worktree can carry a stale terminal-phase status.json from an unrelated
// prior task even after InvalidateStatus runs (e.g. a race, or invalidation
// failing silently in some other caller). pollStatus's own dispatchedAt
// comparison, based on the file's real mtime rather than its self-reported
// UpdatedAt, is the independent second guard: it must not report success for
// a status file that predates dispatch.
func TestPollStatusIgnoresStatusFromBeforeDispatch(t *testing.T) {
	wt := t.TempDir()
	path := protocol.StatusPath(wt)

	if err := protocol.Write(path, &protocol.Status{
		Task:      "unrelated-old-task",
		Phase:     protocol.PhaseDone,
		UpdatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seeding stale status: %v", err)
	}
	// Back-date the file's real mtime so it genuinely predates dispatchedAt,
	// as a leftover from an unrelated prior task would.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdating mtime: %v", err)
	}

	dispatchedAt := time.Now()
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}, dispatchedAt: dispatchedAt}

	// No fresh write ever arrives, so a correct pollStatus can only reach its
	// deadline — reporting hasFile/PhaseDone here would mean the stale file
	// was mistaken for this worker's own outcome.
	pollStatus(context.Background(), 5*time.Millisecond, 40*time.Millisecond, nil, st)

	if st.hasFile {
		t.Error("pollStatus treated a pre-dispatch stale status as this worker's report")
	}
	if st.status.Phase == protocol.PhaseDone {
		t.Error("stale phase leaked into workerState.status")
	}
}

// TestPollStatusAcceptsStatusWithLyingUpdatedAt covers argus issue #90: the
// staleness guard above must judge freshness by the status file's own mtime,
// not by the worker's self-reported UpdatedAt field. A worker that writes a
// garbage/template UpdatedAt (e.g. copied from an example in its brief) that
// reads as before dispatchedAt must still have its real, post-dispatch write
// picked up — the #80 hang was exactly this: the guard trusted the lying
// UpdatedAt and discarded every poll forever.
func TestPollStatusAcceptsStatusWithLyingUpdatedAt(t *testing.T) {
	wt := t.TempDir()
	dispatchedAt := time.Now()

	// The file is written after dispatchedAt (a real mtime later than
	// dispatch), but its UpdatedAt content lies and claims to be from before.
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:      "a",
		Phase:     protocol.PhaseAwaitingReview,
		UpdatedAt: dispatchedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seeding status with lying UpdatedAt: %v", err)
	}

	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}, dispatchedAt: dispatchedAt}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, 10*time.Millisecond, 0, nil, st)

	if !st.hasFile || st.status.Phase != protocol.PhaseAwaitingReview {
		t.Fatalf("pollStatus discarded a real post-dispatch status because of a lying UpdatedAt: hasFile=%v phase=%q", st.hasFile, st.status.Phase)
	}
}

// TestPollStatusAcceptsStatusWrittenAfterDispatch guards against the
// dispatchedAt filter being too strict: a real worker's own terminal status,
// timestamped after dispatch, must still be picked up immediately.
func TestPollStatusAcceptsStatusWrittenAfterDispatch(t *testing.T) {
	wt := t.TempDir()
	dispatchedAt := time.Now()
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}, dispatchedAt: dispatchedAt}

	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:      "a",
		Phase:     protocol.PhaseDone,
		UpdatedAt: dispatchedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, 10*time.Millisecond, 0, nil, st)

	if !st.hasFile || st.status.Phase != protocol.PhaseDone {
		t.Fatalf("pollStatus should accept a status written after dispatch, got hasFile=%v phase=%q", st.hasFile, st.status.Phase)
	}
}

// TestExecuteInvalidatesStaleStatusBeforeSpawn covers argus issue #75: a
// worktree directory can carry a leftover status.json/verdict.json from an
// unrelated prior task even though the branch itself is freshly created
// (e.g. directory reuse in worktree creation). execute must remove both
// before the worker's pane is spawned, mirroring InvalidateStatus's existing
// use in cmd/rebase.go (issue #50).
func TestExecuteInvalidatesStaleStatusBeforeSpawn(t *testing.T) {
	repo := t.TempDir()
	wt := filepath.Join(repo, ".claude", "worktrees", "feat-x")
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + wt + `"}}}`), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			return nil, herdr.ErrAgentNotFound
		}
		return []byte(`{"result":{}}`), nil
	}

	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:  "unrelated-old-task",
		Phase: protocol.PhaseDone,
	}); err != nil {
		t.Fatalf("seeding stale status: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(protocol.VerdictPath(wt)), 0o750); err != nil {
		t.Fatalf("seeding verdict dir: %v", err)
	}
	if err := os.WriteFile(protocol.VerdictPath(wt), []byte(`{"approved":true}`), 0o600); err != nil {
		t.Fatalf("seeding stale verdict: %v", err)
	}

	cfg := &Config{
		Client: herdr.NewWithRunner(runner),
		Now:    time.Now,
		Base:   "main",
	}
	plans := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, nil)

	if _, err := execute(context.Background(), cfg, plans); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if _, err := os.Stat(protocol.StatusPath(wt)); !os.IsNotExist(err) {
		t.Errorf("stale status.json should have been removed before spawn, stat err = %v", err)
	}
	if _, err := os.Stat(protocol.VerdictPath(wt)); !os.IsNotExist(err) {
		t.Errorf("stale verdict.json should have been removed before spawn, stat err = %v", err)
	}
}
