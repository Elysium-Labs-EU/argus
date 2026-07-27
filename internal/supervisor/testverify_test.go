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
		{Cmd: "exit 1", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry for a claimed pass that actually fails", mismatches)
	}
}

func TestVerifyTestsAcceptsGenuinePass(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 0", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none for a genuinely passing command", mismatches)
	}
}

// TestVerifyTestsJoinsCmdAndTarget is the regression for the gate silently
// dropping Target when re-running a worker's claimed pass: "test -f" alone
// (no path operand) always fails, so this only passes if the re-run actually
// appends Target to Cmd — matching the go-tool-with-a-package-arg shape
// (e.g. "go tool fieldalignment" + "./...") this field split exists for.
func TestVerifyTestsJoinsCmdAndTarget(t *testing.T) {
	wt := t.TempDir()
	marker := filepath.Join(wt, "marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []protocol.TestRun{
		{Cmd: "test -f", Target: "marker.txt", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — re-run must join Cmd and Target, not run Cmd alone", mismatches)
	}
}

func TestVerifyTestsSkipsFailAndSkippedClaims(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "exit 1", Result: protocol.ResultFail},
		{Cmd: "exit 1", Result: protocol.ResultSkipped},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — only claimed-pass results are re-verified", mismatches)
	}
}

// TestVerifyTestsRetriesOnceBeforeFlagging is the regression for the
// under-load flake this check kept mistaking for a real regression: a
// command that fails on its first sample but passes on retry (the load-flake
// shape) must not be reported as a mismatch.
func TestVerifyTestsRetriesOnceBeforeFlagging(t *testing.T) {
	wt := t.TempDir()
	marker := filepath.Join(wt, "attempted")
	tests := []protocol.TestRun{
		{Cmd: "test -f " + marker + " && exit 0 || { touch " + marker + "; exit 1; }", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 0 {
		t.Fatalf("mismatches = %v, want none — a fail-then-pass pair is a flake, not a real mismatch", mismatches)
	}
}

// TestVerifyTestsFlagsRepeatedFailureWithBothOutputs proves a command that
// fails on both attempts is still reported (the retry only forgives a single
// bad sample), and that the mismatch string carries each attempt's captured
// output — the diagnosis a bare "exit status 2" never gave anyone.
func TestVerifyTestsFlagsRepeatedFailureWithBothOutputs(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "echo boom-output-here; exit 2", Result: protocol.ResultPass},
	}
	mismatches := VerifyTests(context.Background(), wt, tests, time.Second)
	if len(mismatches) != 1 {
		t.Fatalf("mismatches = %v, want 1 entry for a command that fails on both attempts", mismatches)
	}
	if !strings.Contains(mismatches[0], "boom-output-here") {
		t.Errorf("mismatch should carry the failing command's captured output, got %q", mismatches[0])
	}
	if !strings.Contains(mismatches[0], "failed twice") {
		t.Errorf("mismatch should note both attempts failed, got %q", mismatches[0])
	}
}

func TestVerifyTestsReportsTimeout(t *testing.T) {
	wt := t.TempDir()
	tests := []protocol.TestRun{
		{Cmd: "sleep 5", Result: protocol.ResultPass},
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

func TestRunVerifyCommandSkipsWhenUnconfigured(t *testing.T) {
	wt := t.TempDir()
	if m := RunVerifyCommand(context.Background(), wt, ""); m != "" {
		t.Errorf("mismatch = %q, want none — an empty verify command must not run anything", m)
	}
}

func TestRunVerifyCommandAcceptsCleanRun(t *testing.T) {
	wt := t.TempDir()
	if m := RunVerifyCommand(context.Background(), wt, "exit 0"); m != "" {
		t.Errorf("mismatch = %q, want none for a genuinely clean command", m)
	}
}

func TestRunVerifyCommandFlagsRepeatedFailure(t *testing.T) {
	wt := t.TempDir()
	m := RunVerifyCommand(context.Background(), wt, "echo lint-boom; exit 1")
	if m == "" {
		t.Fatal("mismatch = \"\", want a reason for a command that fails on both attempts")
	}
	if !strings.Contains(m, "lint-boom") {
		t.Errorf("mismatch should carry the failing command's captured output, got %q", m)
	}
}

func TestRunVerifyCommandRetriesOnceBeforeFlagging(t *testing.T) {
	wt := t.TempDir()
	marker := filepath.Join(wt, "verify-attempted")
	cmd := "test -f " + marker + " && exit 0 || { touch " + marker + "; exit 1; }"
	if m := RunVerifyCommand(context.Background(), wt, cmd); m != "" {
		t.Fatalf("mismatch = %q, want none — a fail-then-pass pair is a flake, not a real mismatch", m)
	}
}

func TestGateEscalatesWhenVerifyCommandFails(t *testing.T) {
	st := &workerState{
		hasFile:        true,
		measuredOK:     true,
		measured:       protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:  []string{"cmd/root.go"},
		verifyMismatch: `repo's verify command "make lint" failed twice in a row: exit status 1`,
		plan:           &WorkerPlan{Worker: Worker{Task: "lint-failure"}},
		status: protocol.Status{
			Phase:        protocol.PhaseAwaitingReview,
			Tests:        []protocol.TestRun{{Cmd: "go test ./...", Result: protocol.ResultPass}},
			FilesTouched: []string{"cmd/root.go"},
			DiffStat:     protocol.DiffStat{Insertions: 3},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a small, clean diff whose repo verify command failed")
	}
	if !hasReasonContaining(v.HardReasons, "make lint") {
		t.Errorf("expected an unwaivable verify-command reason, got %v", v.HardReasons)
	}
}

func TestReconcileWiresVerifyCommandIntoVerifyMismatch(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "lint-failure", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD", VerifyCommand: "exit 1"}, states)

	if states[0].verifyMismatch == "" {
		t.Fatal("reconcile should have wired RunVerifyCommand's failure into st.verifyMismatch")
	}
}

func TestReconcileSkipsVerifyCommandWhenUnconfigured(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "no-verify-cmd", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD"}, states)

	if states[0].verifyMismatch != "" {
		t.Fatalf("verifyMismatch = %q, want none — no verify command was configured", states[0].verifyMismatch)
	}
}

func TestReconcileSkipsVerifyCommandForNonTerminalPhase(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseWorking},
	}}
	reconcile(context.Background(), &Config{Base: "HEAD", VerifyCommand: "exit 1"}, states)

	if states[0].verifyMismatch != "" {
		t.Fatalf("verify command must not run for a non-terminal phase, got %q", states[0].verifyMismatch)
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
