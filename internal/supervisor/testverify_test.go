package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestVerifyTestsFlagsFabricatedPass(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1", Target: "unit", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry for a claimed pass that actually fails", mismatches)
	}
}

func TestVerifyTestsAcceptsGenuinePass(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 0", Target: "unit", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none for a genuinely passing command", mismatches)
	}
}

func TestVerifyTestsSkipsFailAndSkippedClaims(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1", Target: "unit", Result: protocol.ResultFail},
		{Cmd: "exit 1", Target: "unit", Result: protocol.ResultSkipped},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — only claimed-pass results are re-verified", mismatches)
	}
}

func TestVerifyTestsReportsTimeout(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "sleep 5", Target: "unit", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, 50*time.Millisecond)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 timeout entry", mismatches)
	}
}

func TestGateEscalatesWhenClaimedTestPassDoesNotReproduce(t *testing.T) {
	st := &workerState{
		hasFile:        true,
		measuredOK:     true,
		measured:       protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:  []string{"cmd/root.go"},
		testMismatches: []string{`worker claimed "go test ./..." passed, but re-running it failed: exit status 1`},
		plan:           &WorkerPlan{Worker: Worker{Task: "fabricated-pass"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a small, clean diff whose claimed test pass did not reproduce")
	}
	if !hasReasonContaining(v.HardReasons, "re-running it failed") {
		t.Errorf("expected an unwaivable test-mismatch reason, got %v", v.HardReasons)
	}
}

// TestReconcileWiresVerifyTestsIntoTestMismatches is the end-to-end companion
// to TestGateEscalatesWhenClaimedTestPassDoesNotReproduce: that test sets
// st.testMismatches by hand to prove gateVerdict reacts to it, but never
// proves reconcile itself actually populates the field from a real re-run —
// a wrong field name or an inverted phase check there would slip past it.
// This drives reconcile() directly against a real git worktree instead.
func TestReconcileWiresVerifyTestsIntoTestMismatches(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "fabricated-pass", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "exit 1", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if len(states[0].testMismatches) != 1 {
		t.Fatalf("reconcile should have wired VerifyTests' mismatch into st.testMismatches, got %v", states[0].testMismatches)
	}
}

// TestReconcileSkipsTestVerificationForNonTerminalPhase is the control for
// TestReconcileWiresVerifyTestsIntoTestMismatches: a worker still mid-flight
// has nothing final to re-verify yet, mirroring how the plan-evidence and
// diff-under-report checks stay silent until a terminal phase.
func TestReconcileSkipsTestVerificationForNonTerminalPhase(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseWorking,
			Tests: []protocol.TestRun{{Cmd: "exit 1", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].testMismatches != nil {
		t.Fatalf("test verification must not run for a non-terminal phase, got %v", states[0].testMismatches)
	}
}

// TestReconcilePassesRealWorktreeToVerifyTests proves reconcile threads
// st.plan.Worktree (not argus's own cwd, not some other worker's worktree)
// through to VerifyTests: the command only succeeds when run inside wt.
func TestReconcilePassesRealWorktreeToVerifyTests(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	if err := os.WriteFile(filepath.Join(wt, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "genuine-pass", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "test -f marker.txt", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if len(states[0].testMismatches) != 0 {
		t.Fatalf("marker-file check should have succeeded inside the real worktree, got mismatches %v", states[0].testMismatches)
	}
}

// TestReconcileCapturesReviewDiffBeforeTestSideEffects is the regression for
// the ordering hazard: VerifyTests re-runs an arbitrary worker-supplied
// command, which can itself write to the worktree. If reviewOne's diff were
// fetched after that ran, the LLM reviewer could see the test run's own
// output instead of only the worker's actual change. reconcile must snapshot
// it first.
func TestReconcileCapturesReviewDiffBeforeTestSideEffects(t *testing.T) {
	wt := gitWorktreeWithDiff(t) // f.go already has an uncommitted tracked edit
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "side-effecting-test", Worktree: wt}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "printf '\\nSIDEEFFECT\\n' >> f.go", Result: protocol.ResultPass}},
		},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].reviewDiffErr != nil {
		t.Fatalf("reviewDiffErr: %v", states[0].reviewDiffErr)
	}
	if strings.Contains(states[0].reviewDiff, "SIDEEFFECT") {
		t.Errorf("reviewDiff must be captured before VerifyTests runs; it must not contain a test command's own side effect, got %q", states[0].reviewDiff)
	}
	onDisk, err := os.ReadFile(filepath.Join(wt, "f.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "SIDEEFFECT") {
		t.Fatal("expected the test command to actually have run and mutated f.go — otherwise this test proves nothing")
	}
}

func TestGateAutoApprovesWhenTestMismatchesEmpty(t *testing.T) {
	st := &workerState{
		hasFile:       true,
		measuredOK:    true,
		measured:      protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles: []string{"cmd/root.go"},
		plan:          &WorkerPlan{Worker: Worker{Task: "genuine-pass"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	if v := gateVerdict(st, nil); !v.AutoApprove {
		t.Errorf("gate must still auto-approve a clean worker once its test claim reproduces (testMismatches empty): reasons %v", v.Reasons)
	}
}
