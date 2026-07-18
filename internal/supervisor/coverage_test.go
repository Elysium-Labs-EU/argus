package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/Elysium_Labs/argus/internal/eventlog"
	"codeberg.org/Elysium_Labs/argus/internal/herdr"
	"codeberg.org/Elysium_Labs/argus/internal/protocol"
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

func TestWatchReturnsWhenWorkerReachesTerminal(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Task: "a", Phase: protocol.PhaseAwaitingReview}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	cfg := &Config{Interval: 5 * time.Millisecond}
	states := []*workerState{{plan: &WorkerPlan{Worker: Worker{Task: "a", Worktree: wt}}}}

	done := make(chan struct{})
	go func() { watch(context.Background(), cfg, states); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after the worker reached a terminal phase")
	}
	if !states[0].hasFile || states[0].status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("watch did not record the terminal status: %+v", states[0].status)
	}
}

func TestWaitForStatusReadsTerminal(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Task: "r", Phase: protocol.PhaseBlocked, BlockedReason: "need decision"}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	status, seen := WaitForStatus(context.Background(), wt, 5*time.Millisecond)
	if !seen {
		t.Fatal("WaitForStatus should have seen the status file")
	}
	if status.Phase != protocol.PhaseBlocked {
		t.Errorf("phase: got %q want blocked", status.Phase)
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
	// A git worktree with an uncommitted change and a seeded terminal status, so
	// Run exercises execute -> watch -> reconcile -> gate -> report end to end
	// without a real worker or herdr.
	wt := gitWorktreeWithDiff(t)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:     "t",
		Phase:    protocol.PhaseAwaitingReview,
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 2},
		Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	runner := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w:p1"}}}`), nil
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
	plans := BuildPlan([]Worker{
		{Task: "a", Branch: "feat-a", RepoRoot: t.TempDir()},
		{Task: "b", Branch: "feat-b", RepoRoot: t.TempDir()},
	})
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
