package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// noPanes is a herdr client whose pane list is empty, standing in for the real
// herdr the report enriches from — enough that renderReport runs without a live
// multiplexer.
func noPanes() herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(`{"result":{"panes":[]}}`), nil
	})
}

// TestAttachSupervisesExistingWorker proves --attach's core: given a worktree
// whose worker has already reached a terminal phase, Attach watches its typed
// status, measures the real diff, gates, and reports — without spawning anything
// (no herdr Client is set) and without reading scrollback.
func TestAttachSupervisesExistingWorker(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // real git checkout with an uncommitted change vs HEAD
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:     "attached-worker",
		Phase:    protocol.PhaseAwaitingReview,
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 2},
		Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	var buf bytes.Buffer
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   noPanes(),
		Base:     "HEAD",
		Home:     t.TempDir(),
		Interval: 2 * time.Millisecond,
		Timeout:  time.Second,
		Policy:   &policy,
	}
	workers := []Worker{{Task: "attached-worker", Branch: "feat", Worktree: wt}}

	if err := Attach(context.Background(), cfg, workers); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !strings.Contains(buf.String(), "supervise report") {
		t.Errorf("expected a report:\n%s", buf.String())
	}
	if _, found, _ := protocol.LoadApproval(wt); !found {
		t.Error("Attach did not record a verdict for the attached worker")
	}
}

// TestAttachTimesOutOnNonTerminalWorker: a worker that never reaches a terminal
// phase must not hang Attach forever — the per-worker deadline returns, and with
// no readable terminal status the worker is skipped by the gate (no verdict), not
// falsely approved.
func TestAttachTimesOutOnNonTerminalWorker(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task:  "slow",
		Phase: protocol.PhaseWorking, // non-terminal
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	var buf bytes.Buffer
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Out:      &buf,
		Now:      time.Now,
		Client:   noPanes(),
		Base:     "HEAD",
		Home:     t.TempDir(),
		Interval: 2 * time.Millisecond,
		Timeout:  30 * time.Millisecond,
		Policy:   &policy,
	}
	done := make(chan error, 1)
	go func() {
		done <- Attach(context.Background(), cfg, []Worker{{Task: "slow", Branch: "feat", Worktree: wt}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after the worker's deadline passed")
	}
	// A worker stuck non-terminal was seen (hasFile) but is still gated normally;
	// it must not have been auto-approved on an unfinished phase.
	if a, found, _ := protocol.LoadApproval(wt); found && a.Approved {
		t.Errorf("a non-terminal worker must not be approved: %+v", a)
	}
}

// TestAttachFailsFastOnUnresolvableBaseRef pins the fail-fast fix for
// --attach: a bad --base (required explicitly for --attach — see
// cmd/supervise.go) must be caught up front, before any worker is watched or
// judged, and must never surface as a gate/review escalation on the attached
// worker.
func TestAttachFailsFastOnUnresolvableBaseRef(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Task: "attached-worker", Phase: protocol.PhaseAwaitingReview,
	}); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	cfg := &Config{
		Out: &bytes.Buffer{}, Now: time.Now, Client: noPanes(),
		Base: "origin/does-not-exist", BaseSource: BaseSourceFlag,
		Home: t.TempDir(), Interval: 2 * time.Millisecond, Timeout: time.Second,
	}
	workers := []Worker{{Task: "attached-worker", Branch: "feat", Worktree: wt}}

	err := Attach(context.Background(), cfg, workers)
	if err == nil {
		t.Fatal("an unresolvable base ref should fail Attach fast, got nil")
	}
	if !strings.Contains(err.Error(), `"origin/does-not-exist" does not exist`) {
		t.Errorf("error = %q, want it to name the unresolvable ref", err)
	}
	if _, found, _ := protocol.LoadApproval(wt); found {
		t.Error("a fail-fast base-ref error must never reach the gate and record a verdict")
	}
}

// TestAttachPropagatesBuildPlanError proves a BuildPlan failure (e.g. a
// branch name BuildPlan itself refuses) short-circuits Attach before it ever
// gets to worktree-collision or base-ref checks.
func TestAttachPropagatesBuildPlanError(t *testing.T) {
	cfg := &Config{Now: time.Now}
	workers := []Worker{{Task: "a", Branch: "../evil", RepoRoot: "/repo"}}
	if err := Attach(context.Background(), cfg, workers); err == nil {
		t.Fatal("want an error when BuildPlan fails, got nil")
	}
}

// TestAttachRefusesCollidingWorktrees proves two workers sharing one
// worktree are refused before Attach ever watches or judges either one.
func TestAttachRefusesCollidingWorktrees(t *testing.T) {
	cfg := &Config{Now: time.Now}
	workers := []Worker{
		{Task: "a", Branch: "feat-x", Worktree: "/same"},
		{Task: "b", Branch: "feat-y", Worktree: "/same"},
	}
	if err := Attach(context.Background(), cfg, workers); err == nil {
		t.Fatal("want an error for colliding worktrees, got nil")
	}
}

// TestAttachFailsFastOnUnresolvableBaseRefInNonFirstTarget pins the review
// fix: every attach target's own worktree is validated, not just the first —
// --attach can watch worktrees from different repos in one run, so an
// earlier target resolving cfg.Base fine does not mean a later one will. A
// bad ref on the second target must still fail fast, naming that target,
// instead of slipping through to a per-worker measure_diff failure
// indistinguishable from a real review escalation.
func TestAttachFailsFastOnUnresolvableBaseRefInNonFirstTarget(t *testing.T) {
	good := gitWorktreeWithDiff(t) // has a commit, so "HEAD" resolves
	bad := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", bad).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", bad, err, out)
	}
	// bad has no commit at all (unborn HEAD), so "HEAD" never resolves there.

	for _, wt := range []string{good, bad} {
		if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
			Task: "attached-worker", Phase: protocol.PhaseAwaitingReview,
		}); err != nil {
			t.Fatalf("seeding status for %s: %v", wt, err)
		}
	}

	cfg := &Config{
		Out: &bytes.Buffer{}, Now: time.Now, Client: noPanes(),
		Base: "HEAD", BaseSource: BaseSourceFlag,
		Home: t.TempDir(), Interval: 2 * time.Millisecond, Timeout: time.Second,
	}
	workers := []Worker{
		{Task: "good", Branch: "feat-a", Worktree: good},
		{Task: "bad", Branch: "feat-b", Worktree: bad},
	}

	err := Attach(context.Background(), cfg, workers)
	if err == nil {
		t.Fatal("want an error when a non-first target's base ref is unresolvable, got nil")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error = %q, want it to name the offending target %q", err, bad)
	}
	if !strings.Contains(err.Error(), `"HEAD" does not exist`) {
		t.Errorf("error = %q, want it to name the unresolvable ref", err)
	}
	if _, found, _ := protocol.LoadApproval(good); found {
		t.Error("a fail-fast base-ref error must never reach the gate and record a verdict for any target, including one already checked fine before the bad one")
	}
}

// TestAttachDoesNotMaskUnderReportAcrossReattaches proves a plain re-attach
// (no rework dispatch in between) judges the worker's self-reported diff
// against the full measured diff every time, not against whatever a prior
// --attach --review call already recorded in verdict.json. Subtracting a
// prior verdict's measurement is only valid for a rework round, which
// supplies its own explicit pre-dispatch snapshot (see JudgeOne) — a plain
// re-attach has no such snapshot, and status.json's self-reported DiffStat is
// always cumulative-since-base, never incremental-since-last-verdict.
func TestAttachDoesNotMaskUnderReportAcrossReattaches(t *testing.T) {
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
	var lines strings.Builder
	lines.WriteString("package x\n\n")
	for i := range 50 {
		fmt.Fprintf(&lines, "var Added%d = %d\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	underReport := func() error {
		return protocol.Write(protocol.StatusPath(wt), &protocol.Status{
			Task: "repro", Phase: protocol.PhaseAwaitingReview,
			DiffStat: protocol.DiffStat{Files: 1, Insertions: 1}, // real diff is ~50 lines
		})
	}
	if err := underReport(); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	policy := DefaultReviewPolicy()
	attachOnce := func() string {
		var buf bytes.Buffer
		cfg := &Config{
			Out: &buf, Now: time.Now, Client: noPanes(), Base: "HEAD",
			Home: t.TempDir(), Interval: 2 * time.Millisecond, Timeout: time.Second, Policy: &policy,
		}
		if err := Attach(context.Background(), cfg, []Worker{{Task: "repro", Branch: "feat", Worktree: wt}}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		return buf.String()
	}

	if out := attachOnce(); !strings.Contains(out, "under-reported diff") {
		t.Fatalf("first attach should flag the under-report:\n%s", out)
	}
	// Re-attach with nothing changed on disk except that a verdict.json from
	// the first attach now exists. The worker still under-reports the same
	// way — this must escalate again, not be masked by the prior verdict.
	if err := underReport(); err != nil {
		t.Fatalf("re-seeding status: %v", err)
	}
	if out := attachOnce(); !strings.Contains(out, "under-reported diff") {
		t.Fatalf("re-attach must still flag the same under-report, not be masked by the prior verdict:\n%s", out)
	}
}
