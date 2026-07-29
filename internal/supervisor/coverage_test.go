package supervisor

import (
	"bytes"
	"context"
	"errors"
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

func gitInit(t *testing.T, dir string) func(args ...string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	return run
}

func TestReconcileMeasuresAndRecordsError(t *testing.T) {
	good := gitWorktreeWithDiff(t) // has an uncommitted change vs HEAD
	states := []*workerState{
		{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "good", Worktree: good}}},
		{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "bad", Worktree: "/definitely/not/a/repo"}}},
		{hasFile: false, plan: &WorkerPlan{Worker: Worker{Task: "skip", Worktree: good}}},
	}
	cfg := &Config{Base: "HEAD"}
	reconcile(context.Background(), cfg, states)

	if !states[0].measuredOK || states[0].measured.Insertions == 0 {
		t.Errorf("good worker should have a measured diff: %+v", states[0].measured)
	}
	if states[1].diffErr == nil || states[1].measuredOK {
		t.Errorf("bad worktree should record a diff error, not measure")
	}
	if states[2].measuredOK {
		t.Errorf("a worker with no status file should be skipped")
	}
}

func TestJudgeEachReturnsWhenWorkerReachesTerminal(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Task: "a", Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	cfg := &Config{Interval: 5 * time.Millisecond, Base: "HEAD"}
	states := []*workerState{{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}}

	done := make(chan struct{})
	go func() { judgeEach(context.Background(), cfg, states); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("judgeEach did not return after the worker reached a terminal phase")
	}
	if !states[0].hasFile || states[0].status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("judgeEach did not record the terminal status: %+v", states[0].status)
	}
}

// TestJudgeEachDoesNotBarrierOnSlowestWorker is the regression test for issue
// #116: a fast worker's gate verdict must land as soon as IT reaches a
// terminal phase, without waiting for a slower sibling still in progress.
func TestJudgeEachDoesNotBarrierOnSlowestWorker(t *testing.T) {
	fastWT := gitWorktreeWithDiff(t)
	slowWT := t.TempDir()

	if err := protocol.Write(protocol.StatusPath(fastWT), &protocol.Status{
		Task: "fast", Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seeding fast status: %v", err)
	}
	// The slow worker's status file only appears after a delay well past when
	// the fast worker's own gate verdict should already have landed — under
	// the old full-batch barrier, the fast worker's verdict could not appear
	// before this.
	slowDone := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = protocol.Write(protocol.StatusPath(slowWT), &protocol.Status{
			Task: "slow", Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now(),
		})
		close(slowDone)
	}()

	buf := &syncBuffer{}
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Base:     "HEAD",
		Interval: 5 * time.Millisecond,
		Policy:   &policy,
		Log:      eventlog.New(buf, "supervise", "r", nil),
	}
	states := []*workerState{
		{plan: &WorkerPlan{Worker: Worker{Task: "fast", Worktree: fastWT}}},
		{plan: &WorkerPlan{Worker: Worker{Task: "slow", Worktree: slowWT}}},
	}

	done := make(chan struct{})
	go func() { judgeEach(context.Background(), cfg, states); close(done) }()

	// Well before the slow worker's file appears, the fast worker should
	// already have a verdict logged.
	time.Sleep(60 * time.Millisecond)
	if !strings.Contains(buf.String(), `"target":"fast"`) {
		t.Fatalf("fast worker should have been gated before its slow sibling finished, log so far:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"target":"slow"`) {
		t.Fatalf("slow worker should not have reported anything yet, log so far:\n%s", buf.String())
	}

	<-slowDone
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("judgeEach did not return once both workers reached a terminal phase")
	}
	if !strings.Contains(buf.String(), `"target":"slow"`) {
		t.Errorf("slow worker should eventually be gated too, log so far:\n%s", buf.String())
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer: the eventlog.Logger writing from
// several judgeEach goroutines is safe on its own (see eventlog.Logger's own
// mutex), but a plain bytes.Buffer read concurrently with those writes is a
// data race in the *test* observing it, not in the code under test.
type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWaitForStatusReadsTerminal(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Task: "r", Phase: protocol.PhaseBlocked, BlockedReason: "need decision", UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	status, seen := WaitForStatus(context.Background(), herdr.Client{}, "", wt, 5*time.Millisecond, time.Now().Add(-time.Minute), nil)
	if !seen {
		t.Fatal("WaitForStatus should have seen the status file")
	}
	if status.Phase != protocol.PhaseBlocked {
		t.Errorf("phase: got %q want blocked", status.Phase)
	}
}

// TestWaitForStatusIgnoresStaleStatus is the regression case for argus issue
// #50: a status.json written before since (the dispatch time) must not be
// mistaken for the outcome of a worker dispatched after it.
func TestWaitForStatusIgnoresStaleStatus(t *testing.T) {
	wt := t.TempDir()
	path := protocol.StatusPath(wt)
	if err := protocol.Write(path, &protocol.Status{Task: "r", Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seeding stale status: %v", err)
	}
	// Written moments ago in wall-clock terms, but its mtime is set to an hour
	// back so it reads as a genuine leftover from well outside staleTolerance,
	// not merely adjacent-in-time to since (which the tolerance must absorb).
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("setting stale mtime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, seen := WaitForStatus(ctx, herdr.Client{}, "", wt, 5*time.Millisecond, time.Now(), nil)
	if seen {
		t.Fatal("WaitForStatus should not report a status written before since")
	}
}

// TestWaitForStatusAcceptsLyingUpdatedAt verifies the rebase call site's
// fix: WaitForStatus must judge freshness by the status file's
// mtime, not the worker's self-reported UpdatedAt. A worker that writes a
// garbage/template UpdatedAt reading as before since must still have its
// real, post-since write picked up.
func TestWaitForStatusAcceptsLyingUpdatedAt(t *testing.T) {
	wt := t.TempDir()
	since := time.Now()

	// Written after since (a real mtime later than since), but its UpdatedAt
	// content lies and claims to be from an hour before since.
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:      "r",
		Phase:     protocol.PhaseAwaitingReview,
		UpdatedAt: since.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seeding status with lying UpdatedAt: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, seen := WaitForStatus(ctx, herdr.Client{}, "", wt, 5*time.Millisecond, since, nil)
	if !seen {
		t.Fatal("WaitForStatus discarded a real post-since status because of a lying UpdatedAt")
	}
	if status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("phase: got %q want awaiting_review", status.Phase)
	}
}

// TestWaitForStatusAcceptsMtimeSkewUnderTolerance verifies isStale's mtime
// tolerance: isStale must give the file's mtime a grace window below since, not an
// exact boundary, because a filesystem's effective mtime resolution can be
// coarser than time.Now()'s. A real post-dispatch write can therefore read
// back an mtime a few hundred milliseconds *before* since despite happening
// after it. os.Chtimes pins the mtime deterministically so this no longer
// depends on real clock/filesystem timing racing to reproduce the flake.
func TestWaitForStatusAcceptsMtimeSkewUnderTolerance(t *testing.T) {
	wt := t.TempDir()
	since := time.Now()
	path := protocol.StatusPath(wt)

	if err := protocol.Write(path, &protocol.Status{
		Task:      "r",
		Phase:     protocol.PhaseAwaitingReview,
		UpdatedAt: since,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	skewed := since.Add(-300 * time.Millisecond)
	if err := os.Chtimes(path, skewed, skewed); err != nil {
		t.Fatalf("setting skewed mtime: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, seen := WaitForStatus(ctx, herdr.Client{}, "", wt, 5*time.Millisecond, since, nil)
	if !seen {
		t.Fatal("WaitForStatus discarded a real post-since status because its mtime skewed slightly before since")
	}
	if status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("phase: got %q want awaiting_review", status.Phase)
	}
}

// TestWaitForStatusReportsHerdrBlockedPane is a regression test: a worker
// parked on an unanswered permission prompt (a repo
// `Ask` rule overriding auto mode for one command) never writes status.json
// to reflect that, so a caller polling the file alone sees nothing but an
// undifferentiated "waiting" for however long the prompt sits unanswered.
// WaitForStatus must cross-check herdr's own agent_status for the dispatched
// pane and print a distinct notice naming the pane, not just fall silent.
func TestWaitForStatusReportsHerdrBlockedPane(t *testing.T) {
	wt := t.TempDir()
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			return []byte(`{"result":{"panes":[{"pane_id":"w1:p1","agent_status":"blocked"}]}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	})

	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, seen := WaitForStatus(ctx, client, "w1:p1", wt, 5*time.Millisecond, time.Now(), &buf)
	if seen {
		t.Fatal("no status.json was ever written; WaitForStatus should not report one seen")
	}
	if got := buf.String(); !strings.Contains(got, "blocked: awaiting permission approval in pane w1:p1") {
		t.Errorf("want a blocked-pane notice naming the pane, got %q", got)
	}
}

func TestCurrentBranchAndRemoteOwnerRepo(t *testing.T) {
	wt := t.TempDir()
	run := gitInit(t, wt)
	run("checkout", "-q", "-b", "feat-x")
	run("commit", "-q", "--allow-empty", "-m", "init")
	run("remote", "add", "origin", "git@codeberg.org:Elysium_Labs/eos.git")

	branch, err := CurrentBranch(context.Background(), wt)
	if err != nil || branch != "feat-x" {
		t.Errorf("CurrentBranch: got %q err %v", branch, err)
	}
	owner, repo, err := RemoteOwnerRepo(context.Background(), wt)
	if err != nil || owner != "Elysium_Labs" || repo != "eos" {
		t.Errorf("RemoteOwnerRepo: got %s/%s err %v", owner, repo, err)
	}
}

func TestPushToBareRemote(t *testing.T) {
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	wt := t.TempDir()
	run := gitInit(t, wt)
	run("checkout", "-q", "-b", "feat-x")
	run("commit", "-q", "--allow-empty", "-m", "init")
	run("remote", "add", "origin", remote)

	if err := Push(context.Background(), wt, "feat-x"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// The bare remote now has the branch.
	out, err := exec.Command("git", "-C", remote, "branch", "--list", "feat-x").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "feat-x") {
		t.Errorf("branch not pushed to remote: %q err %v", out, err)
	}
}

func TestRunFullPathToReport(t *testing.T) {
	// A git worktree with an uncommitted change, so Run exercises execute ->
	// watch -> reconcile -> gate -> report end to end without a real worker or
	// herdr. The terminal status is written from the "pane run" leg of the mock
	// runner rather than seeded upfront, since execute now invalidates any
	// status.json already sitting in the worktree before it spawns
	// — a status written before dispatch would be discarded as stale, same as
	// it must be for a real leftover file.
	wt := gitWorktreeWithDiff(t)

	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w:p1"}}}`), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			return nil, herdr.ErrAgentNotFound
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "run" {
			if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
				Task:      "t",
				Phase:     protocol.PhaseAwaitingReview,
				UpdatedAt: time.Now(),
				DiffStat:  protocol.DiffStat{Files: 1, Insertions: 2},
				Tests:     []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
			}); err != nil {
				t.Errorf("writing status from mock worker: %v", err)
			}
		}
		return []byte(`{"result":{}}`), nil
	}
	var buf bytes.Buffer
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   herdr.NewWithRunner(runner),
		Base:     "HEAD",
		Home:     t.TempDir(),
		Interval: 2 * time.Millisecond,
		Timeout:  time.Second,
		Policy:   &policy,
	}
	workers := []Worker{{Task: "t", Branch: "feat", RepoRoot: t.TempDir(), Worktree: wt}}
	if err := Run(context.Background(), cfg, workers, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(buf.String(), "supervise report") {
		t.Errorf("expected a report:\n%s", buf.String())
	}
	// Run should have recorded a verdict for the worker.
	if _, found, _ := protocol.LoadApproval(wt); !found {
		t.Error("Run did not record a verdict for the worker")
	}
}

func TestExecuteReportsOrphansOnPartialFailure(t *testing.T) {
	// herdr: worktree create always returns a root pane; the first pane run
	// succeeds, the second fails — so worker 0 is launched, then worker 1 aborts.
	runs := 0
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w:p1"}}}`), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "get" {
			return nil, herdr.ErrAgentNotFound
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "run" {
			runs++
			if runs == 2 {
				return nil, errors.New("pane run failed")
			}
		}
		return []byte(`{"result":{}}`), nil
	}
	var buf bytes.Buffer
	cfg := &Config{
		Client: herdr.NewWithRunner(runner),
		Now:    time.Now,
		Base:   "main",
		Log:    eventlog.New(&buf, "supervise", "r", nil),
	}
	plans, err := BuildPlan([]Worker{
		{Task: "a", Branch: "feat-a", RepoRoot: t.TempDir()},
		{Task: "b", Branch: "feat-b", RepoRoot: t.TempDir()},
	}, "origin/main", nil, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, err := execute(context.Background(), cfg, plans); err == nil {
		t.Fatal("execute should return the spawn error")
	}
	if !strings.Contains(buf.String(), `"action":"orphaned"`) {
		t.Errorf("expected an orphaned event for the already-launched worker:\n%s", buf.String())
	}
}

func TestReportTokensKnownAndUnknown(t *testing.T) {
	home := t.TempDir()
	session := "abc123"
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	line := `{"message":{"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cfg := &Config{Home: home, Log: nil, Out: &buf}
	st := &workerState{plan: &WorkerPlan{Worker: Worker{Task: "t"}}}

	got := reportTokens(cfg, st, session)
	// Total excludes cache-read (100+50+10 = 160), not 165.
	if !strings.Contains(got, "160 total") {
		t.Errorf("token line: got %q want total 160", got)
	}
	if unknown := reportTokens(cfg, st, ""); !strings.Contains(unknown, "unknown") {
		t.Errorf("no session should be unknown, got %q", unknown)
	}
}
