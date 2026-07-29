package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
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
		{"issue ref does not collapse the line, title is kept", "fix #42: reduce CRAP", "fix #42: reduce CRAP"},
		{"issue ref at start, rest of line kept", "#7 tidy up logging", "#7 tidy up logging"},
		{"multiple issue refs, whole line kept", "see #1 and #2", "see #1 and #2"},
		{"hash with no trailing digits falls back to the line", "hash with no digits # end", "hash with no digits # end"},
		{"trailing bare hash falls back to the line", "task #", "task #"},
		{"digits stop at first non-digit but line is kept whole", "#123abc rest", "#123abc rest"},
		{"no hash, short line, unchanged", "no hash here", "no hash here"},
		{
			"issue-driven task keeps repo and title, not just the bare issue number",
			"Fix argus issue #143: fix taskLabel\n\nbody text",
			"Fix argus issue #143: fix taskLabel",
		},
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
	plans, err := BuildPlan([]Worker{
		{Task: "eos#42", Branch: "feat-x", RepoRoot: "/repo-a"},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
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
	if !strings.Contains(p.Brief, protocol.WriterBrief("origin/main")) {
		t.Errorf("brief should embed the shared status-writing contract")
	}
	if !strings.Contains(p.Brief, want) {
		t.Errorf("brief should tell the worker to stay in its worktree")
	}
}

func TestWorktreePathDefaultsToDotClaudeWorktrees(t *testing.T) {
	got, err := WorktreePath("/repo-a", "", "feat-x")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	want := "/repo-a/.claude/worktrees/feat-x"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorktreePathRelativeDirJoinsUnderRepoRoot(t *testing.T) {
	// ".." is the escape hatch for a repo whose own convention is a sibling
	// directory next to the checkout, named directly after the branch.
	got, err := WorktreePath("/repo-a", "..", "feat-x")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	want := "/feat-x"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorktreePathRelativeDirWithSubdirJoinsUnderRepoRoot(t *testing.T) {
	got, err := WorktreePath("/repo-a", "../worktrees", "feat-x")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	want := "/worktrees/feat-x"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorktreePathAbsoluteDirIgnoresRepoRoot(t *testing.T) {
	got, err := WorktreePath("/repo-a", "/elsewhere/worktrees", "feat-x")
	if err != nil {
		t.Fatalf("WorktreePath: %v", err)
	}
	want := "/elsewhere/worktrees/feat-x"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWorktreePathRefusesResolvedPathEqualToRepoRoot(t *testing.T) {
	if _, err := WorktreePath("/repo-a", "/repo-a", ""); err == nil {
		t.Fatal("want an error when the resolved worktree path is the repo root itself")
	}
}

func TestWorktreePathRefusesBranchWithTraversalSegment(t *testing.T) {
	if _, err := WorktreePath("/repo-a", "", "../../etc"); err == nil {
		t.Fatal(`want an error for a branch containing a ".." path segment`)
	}
}

func TestWorktreePathAllowsBranchResemblingButNotContainingATraversalSegment(t *testing.T) {
	got, err := WorktreePath("/repo-a", "", "fix-..-typo")
	if err != nil {
		t.Fatalf(`branch has no actual ".." path segment, want no error: %v`, err)
	}
	if want := "/repo-a/.claude/worktrees/fix-..-typo"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildPlanPropagatesWorktreePathError(t *testing.T) {
	if _, err := BuildPlan([]Worker{
		{Task: "t", Branch: "../../etc", RepoRoot: "/repo-a"},
	}, "origin/main", nil, nil); err == nil {
		t.Fatal(`want an error when a worker's branch contains a ".." path segment`)
	}
}

func TestBuildPlanHonorsWorktreeDir(t *testing.T) {
	plans, err := BuildPlan([]Worker{
		{Task: "eos#42", Branch: "feat-x", RepoRoot: "/repo-a", WorktreeDir: ".."},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := "/feat-x"
	if got := plans[0].Worktree; got != want {
		t.Errorf("worktree: got %q want %q", got, want)
	}
}

func TestBuildPlanExplicitWorktreeWinsOverWorktreeDir(t *testing.T) {
	// --attach's workers set Worktree explicitly (an already-existing
	// directory being observed); a repo's configured WorktreeDir must not
	// override that.
	plans, err := BuildPlan([]Worker{
		{Branch: "feat-x", RepoRoot: "/repo-a", Worktree: "/pinned/path", WorktreeDir: ".."},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := "/pinned/path"
	if got := plans[0].Worktree; got != want {
		t.Errorf("worktree: got %q want %q", got, want)
	}
}

func TestBuildPlanLabelExplicitWins(t *testing.T) {
	plans, err := BuildPlan([]Worker{
		{Task: "eos#42", Branch: "feat-x", Label: "my-custom-label", RepoRoot: "/repo-a"},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := plans[0].Label; got != "my-custom-label" {
		t.Errorf("an explicit Label should win over any derived default; got %q", got)
	}
}

func TestBuildPlanLabelDefaultsFromTask(t *testing.T) {
	// A worker whose branch is slugged/truncated should still get a
	// human-readable label — folding in the task text is the whole point,
	// since a generic "argus-worker-N" branch name alone carries zero
	// information about what the worker is doing.
	plans, err := BuildPlan([]Worker{
		{Task: "fix bug in parser", Branch: "argus-fix-bug-in-parser", RepoRoot: "/repo-a"},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := plans[0].Label; got != "fix bug in parser" {
		t.Errorf("label should derive from task text; got %q", got)
	}
}

func TestBuildPlanLabelFallsBackToBranchWithNoTask(t *testing.T) {
	// The true no-context case (no task, no label given) still needs some
	// label; the branch itself is the only thing left to show.
	plans, err := BuildPlan([]Worker{
		{Branch: "argus-worker-1", RepoRoot: "/repo-a"},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := plans[0].Label; got != "argus-worker-1" {
		t.Errorf("label should fall back to branch when there's no task; got %q", got)
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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

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

func TestPrepareWorktreePassesLabelNotBranch(t *testing.T) {
	repo := t.TempDir()
	rr := &recordingRunner{}
	cfg := &Config{Client: herdr.NewWithRunner(rr.run), Base: "main"}
	// Task differs from Branch so a label equal to Branch (the old default
	// before task text was folded in) is distinguishable from a label derived
	// off Task.
	plans, err := BuildPlan([]Worker{{Task: "fix bug in parser", Branch: "argus-fix-bug-in-parser", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := prepareWorktree(context.Background(), cfg, &plans[0]); err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	var labelArg string
	for _, args := range rr.calls {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			for i, a := range args {
				if a == "--label" && i+1 < len(args) {
					labelArg = args[i+1]
				}
			}
		}
	}
	if labelArg != "fix bug in parser" {
		t.Errorf("--label sent to herdr: got %q, want the task-derived label %q, not the branch", labelArg, "fix bug in parser")
	}
}

// TestPrepareWorktreeNestsViaPaneMove pins down the actual nesting mechanism:
// a follow-up PaneMove call, not WorktreeCreate's own --workspace param,
// which a real-herdr repro showed never actually nests the pane (see its doc
// comment). With cfg.ParentWorkspace set, prepareWorktree must call
// herdr.Client.PaneMove on the pane WorktreeCreate opened, and the returned
// Worktree must carry PaneMove's new pane id, not WorktreeCreate's original
// one — every downstream use (pane registry, resolvePaneID) needs the pane
// that's actually still open.
func TestPrepareWorktreeNestsViaPaneMove(t *testing.T) {
	repo := t.TempDir()
	var paneMoveArgs []string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "create":
			return []byte(`{"result":{"root_pane":{"pane_id":"wAP:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		case len(args) >= 2 && args[0] == "pane" && args[1] == "move":
			paneMoveArgs = args
			return []byte(`{"result":{"move_result":{"pane":{"pane_id":"w3X:p2"}}}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	}
	cfg := &Config{Client: herdr.NewWithRunner(runner), Base: "main", ParentWorkspace: "w3X"}
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	wt, err := prepareWorktree(context.Background(), cfg, &plans[0])
	if err != nil {
		t.Fatalf("prepareWorktree: %v", err)
	}
	want := []string{"pane", "move", "wAP:p1", "--workspace", "w3X", "--new-tab", "--no-focus"}
	if strings.Join(paneMoveArgs, " ") != strings.Join(want, " ") {
		t.Errorf("pane move args: got %v want %v", paneMoveArgs, want)
	}
	if wt.RootPaneID != "w3X:p2" {
		t.Errorf("RootPaneID: got %q want w3X:p2 (PaneMove's new pane, not WorktreeCreate's original wAP:p1)", wt.RootPaneID)
	}
	reg, rerr := protocol.LoadPaneRegistry(repo)
	if rerr != nil {
		t.Fatalf("LoadPaneRegistry: %v", rerr)
	}
	if reg.Panes[wt.Path] != "w3X:p2" {
		t.Errorf("pane registry entry: got %q want w3X:p2, so prune later closes the pane that's actually still open", reg.Panes[wt.Path])
	}
}

// TestPrepareWorktreeFailsClosedWhenNestingFails proves a PaneMove error
// surfaces as a hard prepareWorktree error rather than silently leaving the
// worker in the unrequested top-level workspace WorktreeCreate opened —
// --worker-placement tab is an explicit ask, so failing to honor it should
// not be quietly downgraded to "workspace" mode.
func TestPrepareWorktreeFailsClosedWhenNestingFails(t *testing.T) {
	repo := t.TempDir()
	sentinel := errors.New("boom")
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "worktree" && args[1] == "create":
			return []byte(`{"result":{"root_pane":{"pane_id":"wAP:p1"},"worktree":{"path":"` + repo + `/.claude/worktrees/feat-x"}}}`), nil
		case len(args) >= 2 && args[0] == "pane" && args[1] == "move":
			return nil, sentinel
		default:
			return []byte(`{"result":{}}`), nil
		}
	}
	cfg := &Config{Client: herdr.NewWithRunner(runner), Base: "main", ParentWorkspace: "w3X"}
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := prepareWorktree(context.Background(), cfg, &plans[0]); !errors.Is(err, sentinel) {
		t.Fatalf("prepareWorktree: got %v, want it to wrap the PaneMove error", err)
	}
}

// TestExecuteRefusesToSpawnIntoAPaneWithALiveAgent guards against a headless
// spawn silently attaching to a stale, unrelated session's state. An earlier
// fix only gated the symptom — a terminal-phase worker reporting zero
// measured file changes — after the fact, and never addressed the underlying
// mechanism. PaneRun's own doc comment already
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
			// Simulate the hazard being guarded against: the pane execute is
			// about to type into already has a live, unrelated session running
			// in it.
			return []byte(`{"result":{"agent":{"pane_id":"w9:p1","cwd":"/tmp/relocated-worktree","agent":"claude","agent_session":{"agent":"claude","value":"stale-session-uuid"}}}}`), nil
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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := execute(context.Background(), cfg, plans); err == nil {
		t.Fatal("execute should refuse to spawn into a pane with a live agent session, got nil error")
	} else if !strings.Contains(err.Error(), "stale-session-uuid") {
		t.Errorf("error should name the live session it refused to attach to, got: %v", err)
	} else if !strings.Contains(err.Error(), "/tmp/relocated-worktree") {
		t.Errorf("error should surface the offending pane's cwd so a relocated/removed worktree is obvious without a separate `herdr pane list` query, got: %v", err)
	}
	if paneRunCalled {
		t.Error("execute must not call PaneRun once AgentGet reports a live agent already occupies the pane")
	}
}

// worktreeCreateFakeReply is the herdr `worktree create` JSON response used
// across the worktree_setup_cmd tests below: unlike a real `git worktree
// add`, the fake herdr runner never actually creates worktreePath on disk, so
// each test creates it itself first — the same state a real worktree create
// would have left behind by the time RunWorktreeSetupCmd runs.
func worktreeCreateFakeReply(worktreePath string) string {
	return `{"result":{"root_pane":{"pane_id":"w9:p1"},"worktree":{"path":"` + worktreePath + `"}}}`
}

// TestPrepareWorktreeRunsConfiguredWorktreeSetupCmd proves a configured
// worktree_setup_cmd actually runs, with the freshly created worktree as its
// working directory — the mechanism issue #304 asks for: a repo whose task
// depends on gitignored per-developer local config (env files, local
// settings) needs a hook to bootstrap that config into every new worktree,
// since a bare `git worktree add` never copies it.
func TestPrepareWorktreeRunsConfiguredWorktreeSetupCmd(t *testing.T) {
	repo := t.TempDir()
	worktreePath := filepath.Join(repo, ".claude", "worktrees", "feat-x")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("simulating git worktree add's own directory: %v", err)
	}

	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(worktreeCreateFakeReply(worktreePath)), nil
		}
		return []byte(`{"result":{}}`), nil
	}
	cfg := &Config{
		Client:           herdr.NewWithRunner(runner),
		Base:             "main",
		WorktreeSetupCmd: "pwd > setup-ran.txt",
	}
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, perr := prepareWorktree(context.Background(), cfg, &plans[0]); perr != nil {
		t.Fatalf("prepareWorktree: %v", perr)
	}

	got, err := os.ReadFile(filepath.Join(worktreePath, "setup-ran.txt"))
	if err != nil {
		t.Fatalf("worktree_setup_cmd should have run with the worktree as cwd and written setup-ran.txt: %v", err)
	}
	if strings.TrimSpace(string(got)) != worktreePath {
		t.Errorf("worktree_setup_cmd ran with cwd %q, want %q", strings.TrimSpace(string(got)), worktreePath)
	}
}

// TestExecuteAbortsSpawnWhenWorktreeSetupCmdFails is the regression test for
// issue #304's other requirement: a non-zero exit from worktree_setup_cmd
// must fail worktree creation the same way a `git worktree add` failure
// already does, blocking the worker's agent from ever being spawned.
func TestExecuteAbortsSpawnWhenWorktreeSetupCmdFails(t *testing.T) {
	repo := t.TempDir()
	worktreePath := filepath.Join(repo, ".claude", "worktrees", "feat-x")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("simulating git worktree add's own directory: %v", err)
	}

	var paneRunCalled bool
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(worktreeCreateFakeReply(worktreePath)), nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "run" {
			paneRunCalled = true
		}
		return []byte(`{"result":{}}`), nil
	}
	cfg := &Config{
		Client:           herdr.NewWithRunner(runner),
		Now:              time.Now,
		Base:             "main",
		Log:              eventlog.New(io.Discard, "supervise", "r", nil),
		WorktreeSetupCmd: "echo boom >&2; exit 1",
	}
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, err := execute(context.Background(), cfg, plans); err == nil {
		t.Fatal("execute should fail worktree creation when worktree_setup_cmd exits non-zero, got nil error")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the failing command's captured output, got: %v", err)
	}
	if paneRunCalled {
		t.Error("execute must not spawn the worker's agent once worktree_setup_cmd has failed")
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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

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
	// wrongly resolves it, the wrong absolute host path leaks
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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: "/repo"}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
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
	reviewEscalations(context.Background(), cfg, []*workerState{clean, escalated}, nil)

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

// TestReviewEscalationsHardReasonSurvivesReviewerApprove guards a hard-reason
// escalation from being overturned by a reviewer's "approve": a worker whose
// real diff dwarfs its self-report is exactly the case a reviewer must not be
// able to talk past, because the
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
	reviewEscalations(context.Background(), cfg, []*workerState{liar}, nil)

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

// TestReworkRoundNotBlockedByStaleCumulativeUnderReport: a rework round only
// ever self-reports its own incremental delta, never the cumulative diff
// since base, so the hard under-report gate must judge a later round against
// what changed since that round's own prior verdict — otherwise every
// further round on an already-large change fails it regardless of
// correctness.
func TestReworkRoundNotBlockedByStaleCumulativeUnderReport(t *testing.T) {
	wt := bigGitWorktree(t, "cmd/root.go", 500)
	cfg := &Config{
		Base:     "HEAD",
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"big but correct","findings":[]}`)),
	}

	// Round 1: a large, honestly self-reported feature diff that needs
	// --review — expected and correct for a big feature.
	round1 := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "feature", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 500},
			Tests:    []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}}, // genuinely re-run by VerifyTests (see reconcile)
		},
	}
	reconcile(context.Background(), cfg, []*workerState{round1})
	reviewEscalations(context.Background(), cfg, []*workerState{round1}, nil)

	approval, found, err := protocol.LoadApproval(wt)
	if err != nil || !found || !approval.Approved {
		t.Fatalf("round 1 should have been approved via review: found=%v approved=%v err=%v", found, approval.Approved, err)
	}

	// Round 2: a small follow-up fix (e.g. a CI lint fix) dispatched on the
	// SAME worktree. The worker correctly reports only its own small delta —
	// it has no way to honestly report the cumulative diff since base.
	if writeErr := os.WriteFile(filepath.Join(wt, "cmd", "lint_fix.go"), []byte(strings.Repeat("x\n", 16)), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	round2 := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "feature", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase:    protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 16},
			Tests:    []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}}, // genuinely re-run by VerifyTests (see reconcile)
		},
		// A real rework round gets this from JudgeOne's own pre-dispatch
		// snapshot (see priorMeasuredOK's doc), not from a reconcile-time disk
		// read — mirror that here rather than relying on reconcile to find it.
		priorMeasured:   approval.MeasuredDiff,
		priorMeasuredOK: true,
	}
	reconcile(context.Background(), cfg, []*workerState{round2})
	v := gateVerdict(round2, nil)
	if hasReasonContaining(v.HardReasons, "under-reported diff") {
		t.Fatalf("round 2's small honest delta must not trip the under-report hard gate just because the cumulative diff since base is already large, got HardReasons=%v", v.HardReasons)
	}

	// The round must actually be shippable: --review approving it must stick,
	// not get silently overridden back to not-approved by a stale hard reason.
	reviewEscalations(context.Background(), cfg, []*workerState{round2}, nil)
	approval2, found2, err := protocol.LoadApproval(wt)
	if err != nil || !found2 || !approval2.Approved {
		t.Fatalf("round 2 should be approvable now that the gate judges only its own delta: found=%v approved=%v err=%v summary=%q",
			found2, approval2.Approved, err, approval2.Summary)
	}
}

func TestReviewEscalationsThreadsPriorFindingsFromVerdict(t *testing.T) {
	// A prior request-changes verdict on this worktree must reach the next
	// review's prompt, so the reviewer re-checks the specific defect instead
	// of a fresh holistic pass that can miss it again.
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
	reviewEscalations(context.Background(), cfg, []*workerState{escalated}, nil)

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
	reviewEscalations(context.Background(), cfg, []*workerState{escalated}, nil)
	if escalated.review != nil || escalated.reviewErr != nil {
		t.Errorf("no reviewer should mean no verdict and no error: review=%v err=%v", escalated.review, escalated.reviewErr)
	}
}

// recordingBroker is a CredentialBroker that records every Revoke call, so
// tests can assert exactly which worker's access argus cut off.
type recordingBroker struct {
	revoked []string
	mu      sync.Mutex
}

func (b *recordingBroker) WorkerEnv(string, string) []string { return nil }

func (b *recordingBroker) Revoke(agent string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked = append(b.revoked, agent)
	return 1
}

// TestRecordApprovalRevokesCredentialBroker guards against a judged worker's
// sentinel being left un-revoked: it must be revoked the moment its verdict
// is recorded, whether that verdict approves or rejects — the gate has ruled
// either way, and nothing else in this process learns a worker's fate is
// decided. A nil Broker (the no-proxy default) must not panic.
func TestRecordApprovalRevokesCredentialBroker(t *testing.T) {
	broker := &recordingBroker{}
	cfg := &Config{Broker: broker}

	approved := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "clean", Worktree: t.TempDir()}}}
	recordApproval(cfg, approved, true, "gate", "auto-approved", nil)

	rejected := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "bad", Worktree: t.TempDir()}}}
	recordApproval(cfg, rejected, false, "gate", "escalated, awaiting human decision", []string{"reason"})

	want := []string{"clean", "bad"}
	if !slices.Equal(broker.revoked, want) {
		t.Errorf("revoked = %v, want %v", broker.revoked, want)
	}

	noBroker := &Config{}
	unbrokered := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "no-broker", Worktree: t.TempDir()}}}
	recordApproval(noBroker, unbrokered, true, "gate", "auto-approved", nil)
}

// TestReviewEscalationsGateSurvivesExhaustedReviewSem guards against
// reviewEscalations acquiring its concurrency semaphore around the whole
// function instead of just the reviewer call: doing so would queue a
// worker's free/local gate verdict behind whichever other workers currently
// held a review slot, so a fleet larger than ReviewConcurrency could go a
// long time with no gate/verdict event at all even though the worker had
// already reached a terminal phase. sem is pre-filled here to simulate
// another worker's still-in-flight `claude -p` review holding the only slot;
// the gate verdict for this worker must still be logged immediately, with
// only the reviewer call itself waiting on the slot.
func TestReviewEscalationsGateSurvivesExhaustedReviewSem(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	st := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "second", Branch: "b", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}}, // forces escalation
		},
	}

	sem := make(chan struct{}, 1)
	sem <- struct{}{} // occupied, as if another worker's review is already running

	buf := &syncBuffer{}
	cfg := &Config{
		Base:     "HEAD",
		Log:      eventlog.New(buf, "supervise", "r", nil),
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"ok","findings":[]}`)),
	}

	done := make(chan struct{})
	go func() {
		reviewEscalations(context.Background(), cfg, []*workerState{st}, sem)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("reviewEscalations returned while the review sem was still exhausted — it should be waiting on the reviewer call, not the whole function")
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(buf.String(), `"action":"gate","target":"second","outcome":"escalate"`) {
		t.Fatalf("gate verdict must be logged even while the review sem is exhausted by another worker, log so far:\n%s", buf.String())
	}

	<-sem // free the slot, as if the other worker's review completed
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reviewEscalations did not finish once the review sem slot freed")
	}
	if st.review == nil || st.review.Decision != "approve" {
		t.Errorf("expected the review to complete once the slot freed, got %+v", st.review)
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
		pollStatus(context.Background(), herdr.Client{}, 5*time.Millisecond, 40*time.Millisecond, nil, st, time.Now)
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

	pollStatus(context.Background(), herdr.Client{}, 5*time.Millisecond, 30*time.Millisecond, logger, st, time.Now)

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
	pollStatus(ctx, herdr.Client{}, 10*time.Millisecond, 0, nil, st, time.Now)

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

// TestPollStatusAdvancesOwnerHeartbeatOnEachTick pins pollStatus's own side
// effect on a worktree's owner lease (see internal/ownership): every tick
// must advance HeartbeatAt to the caller-supplied clock's current value,
// without disturbing SpawnedAt — the same "advance heartbeat, never spawn"
// contract ownership.Heartbeat itself already guarantees, exercised here
// through the actual poll loop rather than called directly.
func TestPollStatusAdvancesOwnerHeartbeatOnEachTick(t *testing.T) {
	wt := t.TempDir()
	spawnedAt := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	if err := ownership.Spawn(wt, "sess-1", "test-owner", spawnedAt); err != nil {
		t.Fatalf("seeding owner lease: %v", err)
	}

	// A pre-seeded terminal status means pollStatus returns after its very
	// first tick, so exactly one heartbeat update is expected — not an
	// unbounded number racing the test's own assertions.
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task: "a", Phase: protocol.PhaseDone,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	fakeNow := time.Date(2026, 7, 29, 10, 30, 0, 0, time.UTC)
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, herdr.Client{}, 10*time.Millisecond, 0, nil, st, func() time.Time { return fakeNow })

	o, found, err := ownership.Load(wt)
	if err != nil || !found {
		t.Fatalf("ownership.Load: found=%v err=%v", found, err)
	}
	if !o.HeartbeatAt.Equal(fakeNow) {
		t.Errorf("HeartbeatAt = %v, want it advanced to pollStatus's clock %v", o.HeartbeatAt, fakeNow)
	}
	if !o.SpawnedAt.Equal(spawnedAt) {
		t.Errorf("SpawnedAt = %v, want it left unchanged at %v", o.SpawnedAt, spawnedAt)
	}
}

// TestPollStatusHeartbeatNoOpsWithNoOwnerLease confirms pollStatus's
// heartbeat write is silently a no-op (not an error, not a log event) for a
// worktree with no recorded owner lease — e.g. one predating this feature,
// or an --attach target argus never spawned — the same "missing owner.json
// treated as unowned" contract ownership.Heartbeat itself guarantees.
func TestPollStatusHeartbeatNoOpsWithNoOwnerLease(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task: "a", Phase: protocol.PhaseDone,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	var buf bytes.Buffer
	logger := eventlog.New(&buf, "supervise", "run1", nil)
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, herdr.Client{}, 10*time.Millisecond, 0, logger, st, time.Now)

	if _, found, _ := ownership.Load(wt); found {
		t.Error("pollStatus's heartbeat should not create a lease where none existed")
	}
	if strings.Contains(buf.String(), "owner_heartbeat") {
		t.Errorf("a missing lease should log no heartbeat event, got:\n%s", buf.String())
	}
}

// TestPollStatusIgnoresStatusFromBeforeDispatch guards against a
// worktree carrying a stale terminal-phase status.json from an unrelated
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
	pollStatus(context.Background(), herdr.Client{}, 5*time.Millisecond, 40*time.Millisecond, nil, st, time.Now)

	if st.hasFile {
		t.Error("pollStatus treated a pre-dispatch stale status as this worker's report")
	}
	if st.status.Phase == protocol.PhaseDone {
		t.Error("stale phase leaked into workerState.status")
	}
}

// TestPollStatusAcceptsStatusWithLyingUpdatedAt guards against the staleness
// check above trusting the worker's self-reported UpdatedAt field instead of
// the status file's own mtime. A worker that writes a garbage/template
// UpdatedAt (e.g. copied from an example in its brief) that reads as before
// dispatchedAt must still have its real, post-dispatch write picked up —
// trusting the lying UpdatedAt instead caused a real hang, where the guard
// discarded every poll forever and pollStatus never returned.
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
	pollStatus(ctx, herdr.Client{}, 10*time.Millisecond, 0, nil, st, time.Now)

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
	pollStatus(ctx, herdr.Client{}, 10*time.Millisecond, 0, nil, st, time.Now)

	if !st.hasFile || st.status.Phase != protocol.PhaseDone {
		t.Fatalf("pollStatus should accept a status written after dispatch, got hasFile=%v phase=%q", st.hasFile, st.status.Phase)
	}
}

// fakeAgentWaitRunner answers `agent wait` by blocking until unblock closes
// (simulating herdr's own blocking wait for a real pane transition), then
// records that a call happened before returning a matched idle pane. Any
// other command gets an empty result so PaneList calls from checkHerdrStuck
// don't error.
func fakeAgentWaitRunner(unblock <-chan struct{}, calls *int32, mu *sync.Mutex) herdr.Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "wait" {
			mu.Lock()
			*calls++
			mu.Unlock()
			select {
			case <-unblock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent_status":"idle"}}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
}

// TestPollStatusWakesOnHerdrAgentWaitNotFixedInterval: a pane-backed worker
// whose status.json turns terminal must be noticed close to the moment
// herdr's own `agent wait` unblocks, not up to a full --interval later.
// interval is set far
// longer than the test's own timeout budget, so the old fixed-sleep poll
// would fail this test by never noticing the write in time.
func TestPollStatusWakesOnHerdrAgentWaitNotFixedInterval(t *testing.T) {
	wt := t.TempDir()
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}, paneID: "w1:p1"}

	unblock := make(chan struct{})
	var mu sync.Mutex
	var calls int32
	client := herdr.NewWithRunner(fakeAgentWaitRunner(unblock, &calls, &mu))

	done := make(chan struct{})
	go func() {
		pollStatus(context.Background(), client, time.Hour, 0, nil, st, time.Now)
		close(done)
	}()

	// Give pollStatus a moment to reach its first blocking AgentWait call —
	// comfortably longer than herdrWaitInstantThreshold, so waitForWake reads
	// this as a genuine transition rather than an already-matching pane — then
	// write the terminal status and unblock it, mirroring a worker writing
	// status.json right as its pane goes idle.
	time.Sleep(200 * time.Millisecond)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:  "a",
		Phase: protocol.PhaseAwaitingReview,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	close(unblock)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pollStatus did not wake on herdr agent wait; it appears to be sleeping for the full interval instead")
	}

	if st.status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("phase: got %q want awaiting_review", st.status.Phase)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got == 0 {
		t.Error("pollStatus never called herdr agent wait for a pane-backed worker")
	}
}

// TestPollStatusFallsBackToIntervalWhenNoPane confirms an attach with no
// resolvable pane (--attach --worktrees) never calls herdr at all and keeps
// polling status.json on the plain --interval timer — there is no pane to
// wait on.
func TestPollStatusFallsBackToIntervalWhenNoPane(t *testing.T) {
	wt := t.TempDir()
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}

	called := false
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		called = true
		return []byte(`{"result":{}}`), nil
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = protocol.Write(protocol.StatusPath(wt), &protocol.Status{
			Task:  "a",
			Phase: protocol.PhaseAwaitingReview,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pollStatus(ctx, client, 5*time.Millisecond, 0, nil, st, time.Now)

	if st.status.Phase != protocol.PhaseAwaitingReview {
		t.Fatalf("phase: got %q want awaiting_review", st.status.Phase)
	}
	if called {
		t.Error("pollStatus must not call herdr at all for a worker with no pane id")
	}
}

// TestWaitForWakeFloorsRepeatCallsWhenAlreadyMatching guards the busy-loop
// hazard herdr's level-triggered wait creates: a pane already sitting in a
// herdrWaitStates state (e.g. idle, but status.json never reached a terminal
// phase — a worker that idled without finishing) must not make waitForWake
// return a near-zero duration, or pollStatus would spin AgentWait calls in a
// tight loop instead of waiting for something to actually change.
func TestWaitForWakeFloorsRepeatCallsWhenAlreadyMatching(t *testing.T) {
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		// Returns immediately, as herdr does for a pane already in a matching state.
		return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent_status":"idle"}}}`), nil
	})
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}

	var lastWaitErr string
	got := waitForWake(context.Background(), client, nil, st, time.Hour, &lastWaitErr)
	if got < herdrWaitBackoff {
		t.Errorf("waitForWake returned %v for an instantly-matching pane, want at least herdrWaitBackoff (%v)", got, herdrWaitBackoff)
	}
}

// TestExecuteInvalidatesStaleStatusBeforeSpawn guards against a worktree
// directory carrying a leftover status.json/verdict.json from an
// unrelated prior task even though the branch itself is freshly created
// (e.g. directory reuse in worktree creation). execute must remove both
// before the worker's pane is spawned, mirroring InvalidateStatus's existing
// use in cmd/rebase.go.
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
	plans, err := BuildPlan([]Worker{{Task: "t", Branch: "feat-x", RepoRoot: repo}}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if _, eerr := execute(context.Background(), cfg, plans); eerr != nil {
		t.Fatalf("execute: %v", eerr)
	}

	// The stale content itself must be gone — but execute now
	// writes its own fresh status.json right back, recording the base branch,
	// so the file existing again is expected; only its
	// content must no longer be the unrelated prior task's.
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Task == "unrelated-old-task" || got.Phase == protocol.PhaseDone {
		t.Errorf("stale status.json content should have been removed before spawn, got %+v", got)
	}
	if got.Base != "main" {
		t.Errorf("Base = %q, want the resolved base %q recorded before spawn", got.Base, "main")
	}
	if _, err := os.Stat(protocol.VerdictPath(wt)); !os.IsNotExist(err) {
		t.Errorf("stale verdict.json should have been removed before spawn, stat err = %v", err)
	}
}

// fakePaneListRunner answers `pane list` with a single pane whose agent_status
// is whatever agentStatus() currently returns, so a test can flip herdr's
// reported state between calls without a real herdr process.
func fakePaneListRunner(paneID string, agentStatus func() string) herdr.Runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			return []byte(`{"result":{"panes":[{"pane_id":"` + paneID + `","agent_status":"` + agentStatus() + `"}]}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
}

// TestCheckHerdrStuckEscalatesAfterThreshold: a worker externally blocked (an
// unanswered prompt) or done (its agent process exited) can never write that
// into status.json itself, so pollStatus must learn it from herdr's own
// agent_status instead of waiting on a self-report that will never arrive.
// tick is fed in threshold-sized steps rather than waiting on
// herdrStuckThreshold (2 minutes) in real time.
func TestCheckHerdrStuckEscalatesAfterThreshold(t *testing.T) {
	client := herdr.NewWithRunner(fakePaneListRunner("w1:p1", func() string { return "blocked" }))
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}
	log := eventlog.New(io.Discard, "supervise", "run1", nil)

	if checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("must not escalate before herdrStuckThreshold is crossed")
	}
	if st.herdrEscalation != "" {
		t.Error("no escalation should be recorded before the threshold")
	}

	if !checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("must escalate once accumulated stuck time crosses herdrStuckThreshold")
	}
	if st.herdrEscalation == "" {
		t.Fatal("expected herdrEscalation to be set")
	}
	if !strings.Contains(st.herdrEscalation, "blocked") {
		t.Errorf("escalation reason should name herdr's reported agent_status, got %q", st.herdrEscalation)
	}
}

// TestCheckHerdrStuckResetsWhenPaneRecovers guards against a stale escalation:
// a pane that was blocked (e.g. on a permission prompt) and then recovered
// (the prompt got answered) must not accumulate stuck time across the gap, so
// a brief blip never counts toward the same threshold as a genuinely wedged
// pane.
func TestCheckHerdrStuckResetsWhenPaneRecovers(t *testing.T) {
	status := "blocked"
	client := herdr.NewWithRunner(fakePaneListRunner("w1:p1", func() string { return status }))
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}

	checkHerdrStuck(context.Background(), client, nil, st, time.Minute)
	if st.herdrStuckElapsed == 0 {
		t.Fatal("expected stuck time to accumulate while herdr reports blocked")
	}

	status = "idle"
	checkHerdrStuck(context.Background(), client, nil, st, time.Minute)
	if st.herdrStuckElapsed != 0 {
		t.Errorf("expected stuck time to reset once the pane recovers, got %v", st.herdrStuckElapsed)
	}
	if st.herdrEscalation != "" {
		t.Error("a recovered pane must never be escalated")
	}
}

// TestCheckHerdrStuckSkipsWorkerWithNoPane confirms the check is a no-op for
// a worker with no resolvable pane (e.g. an Attach without a supplied pane
// id) — there is nothing to ask herdr about, so it must not be treated as
// stuck.
func TestCheckHerdrStuckSkipsWorkerWithNoPane(t *testing.T) {
	called := false
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		called = true
		return []byte(`{"result":{"panes":[]}}`), nil
	})
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}}

	if checkHerdrStuck(context.Background(), client, nil, st, time.Minute) {
		t.Fatal("a worker with no pane id must never escalate")
	}
	if called {
		t.Error("checkHerdrStuck should not call herdr at all when st.paneID is empty")
	}
}

// fakePaneListAndPromptRunner answers `pane list` like fakePaneListRunner,
// and additionally intercepts `agent prompt` calls: each one increments
// *promptCalls and returns promptErr (nil for success). Any other command
// gets an empty result.
func fakePaneListAndPromptRunner(paneID string, agentStatus func() string, promptErr error, promptCalls *int) herdr.Runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			return []byte(`{"result":{"panes":[{"pane_id":"` + paneID + `","agent_status":"` + agentStatus() + `"}]}}`), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			*promptCalls++
			if promptErr != nil {
				return nil, promptErr
			}
			return []byte(`{"result":{}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
}

// TestCheckHerdrStuckNudgesDoneBeforeEscalating verifies that a
// worker whose pane herdr reports "done" gets exactly one AgentPrompt
// reminder to run `argus worker report` before the gate escalates to a
// human — the mismatch is entirely mechanical, so a re-prompt gets first
// crack at fixing it. A worker that ignores the nudge and stays stuck for a
// second full threshold window still escalates, and never gets a second
// nudge.
func TestCheckHerdrStuckNudgesDoneBeforeEscalating(t *testing.T) {
	var promptCalls int
	client := herdr.NewWithRunner(fakePaneListAndPromptRunner("w1:p1", func() string { return "done" }, nil, &promptCalls))
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}
	log := eventlog.New(io.Discard, "supervise", "run1", nil)

	if checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("must not escalate before herdrStuckThreshold is crossed")
	}

	if checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("crossing the threshold for the first time must nudge, not escalate")
	}
	if promptCalls != 1 {
		t.Fatalf("expected exactly one AgentPrompt nudge, got %d", promptCalls)
	}
	if st.herdrEscalation != "" {
		t.Error("a freshly nudged worker must not be escalated yet")
	}
	if st.herdrStuckElapsed != 0 {
		t.Errorf("nudging should reset the stuck timer to give the worker a fresh window, got %v", st.herdrStuckElapsed)
	}

	// The worker ignores the nudge and stays "done" for another full
	// threshold window: this time it must escalate, and must not nudge again.
	if checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("must not escalate before the second threshold window is crossed")
	}
	if !checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("must escalate once the worker stays stuck through a second threshold window")
	}
	if promptCalls != 1 {
		t.Errorf("must not nudge a worker more than once per stuck streak, got %d prompt calls", promptCalls)
	}
	if st.herdrEscalation == "" {
		t.Error("expected herdrEscalation to be set")
	}
}

// TestCheckHerdrStuckNudgeFailureEscalatesImmediately: if the nudge itself
// can't be delivered (herdr errors on the AgentPrompt call), there is no
// working channel left to recover through, so checkHerdrStuck must escalate
// in the same call rather than silently swallowing the failure and waiting
// out another threshold window.
func TestCheckHerdrStuckNudgeFailureEscalatesImmediately(t *testing.T) {
	var promptCalls int
	client := herdr.NewWithRunner(fakePaneListAndPromptRunner("w1:p1", func() string { return "done" }, errors.New("boom"), &promptCalls))
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}
	log := eventlog.New(io.Discard, "supervise", "run1", nil)

	checkHerdrStuck(context.Background(), client, log, st, time.Minute)
	if !checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("a failed nudge must escalate immediately, not wait out another threshold window")
	}
	if promptCalls != 1 {
		t.Errorf("expected exactly one attempted nudge, got %d", promptCalls)
	}
	if st.herdrEscalation == "" {
		t.Error("expected herdrEscalation to be set after a failed nudge")
	}
}

// TestCheckHerdrStuckBlockedNeverNudges: a "blocked" pane is waiting on an
// unanswered permission prompt, which no text nudge resolves, so it must
// escalate immediately without ever calling AgentPrompt.
func TestCheckHerdrStuckBlockedNeverNudges(t *testing.T) {
	var promptCalls int
	client := herdr.NewWithRunner(fakePaneListAndPromptRunner("w1:p1", func() string { return "blocked" }, nil, &promptCalls))
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}
	log := eventlog.New(io.Discard, "supervise", "run1", nil)

	checkHerdrStuck(context.Background(), client, log, st, time.Minute)
	if !checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("a blocked pane must escalate on the first threshold crossing")
	}
	if promptCalls != 0 {
		t.Errorf("a blocked pane must never receive an AgentPrompt nudge, got %d calls", promptCalls)
	}
}

// TestCheckHerdrStuckLogsPaneListErrorOnce: a herdr transport failure says
// nothing about the worker's real state, so it must never itself count as
// evidence of being stuck (checkHerdrStuck must return false and leave
// herdrEscalation unset), while still surfacing to the operator — but only
// once per distinct error, mirroring pollStatus's own status_unreadable
// dedupe, so a persistent herdr outage doesn't spam the run log on every
// tick.
func TestCheckHerdrStuckLogsPaneListErrorOnce(t *testing.T) {
	callErr := errors.New("boom")
	client := herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, callErr
	})
	var buf bytes.Buffer
	log := eventlog.New(&buf, "supervise", "run1", nil)
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}, paneID: "w1:p1"}

	if checkHerdrStuck(context.Background(), client, log, st, time.Minute) {
		t.Fatal("a herdr transport error must never be treated as stuck")
	}
	if st.herdrEscalation != "" {
		t.Error("a herdr transport error must not set herdrEscalation")
	}
	if st.herdrErr != callErr.Error() {
		t.Errorf("herdrErr = %q, want %q", st.herdrErr, callErr.Error())
	}

	checkHerdrStuck(context.Background(), client, log, st, time.Minute)
	checkHerdrStuck(context.Background(), client, log, st, time.Minute)

	logged := strings.Count(buf.String(), "herdr_status_unreadable")
	if logged != 1 {
		t.Errorf("expected the repeated identical herdr error to be logged once, got %d occurrences in %q", logged, buf.String())
	}
}

// TestGateVerdictEscalatesOnHerdrStuckWorkerWithNoStatusFile: before this fix,
// reviewEscalations/logRunSummary (and gateVerdict's callers) skipped any
// worker with hasFile == false outright, so a worker herdr reported blocked
// or done — which can never write status.json — was invisible to the gate,
// indistinguishable from one that simply hadn't reported yet. A non-empty
// herdrEscalation must force escalation with an unwaivable hard reason even
// with no status file at all.
func TestGateVerdictEscalatesOnHerdrStuckWorkerWithNoStatusFile(t *testing.T) {
	st := &workerState{
		hasFile:         false,
		herdrEscalation: `herdr reports pane w1:p1 agent_status="blocked" for over 2m0s, but status.json is still at phase ""`,
		plan:            &WorkerPlan{Worker: Worker{Task: "t"}},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("a herdr-stuck worker must never auto-approve, even with no status.json at all")
	}
	if len(v.HardReasons) == 0 {
		t.Fatal("the herdr-stuck reason must be an unwaivable hard reason")
	}
}
