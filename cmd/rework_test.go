package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// initGitDirWithDiff is initGitDir plus an untracked file, so MeasureDiff
// reports a non-empty, non-zero-files diff against the branch's own tip —
// required for a round to actually reach Approved=true: the gate's hard,
// unwaivable "zero files changed despite a claimed terminal phase" check
// (issue #105) would otherwise override even a reviewer "approve" back to
// not-approved, regardless of what the reviewer says.
func initGitDirWithDiff(t *testing.T) string {
	t.Helper()
	dir := initGitDir(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("seeding untracked diff file: %v", err)
	}
	return dir
}

// sequenceReviewer returns each result in turn on successive Review calls,
// repeating the last for any calls beyond the list, and counts how many times
// it ran — the cmd-package analog of internal/supervisor's own test-only
// sequenceReviewRunner, needed here because rework's loop can call the
// reviewer once per round.
type sequenceReviewer struct {
	results []supervisor.ReviewResult
	calls   int
	mu      sync.Mutex
}

func (s *sequenceReviewer) Review(_ context.Context, _ *supervisor.ReviewRequest) (supervisor.ReviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.calls++
	return s.results[i], nil
}

func (s *sequenceReviewer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// reworkTestPaneID is the pane every fakeReworkClient below answers with —
// every test dispatches into the same single worktree/pane pair.
const reworkTestPaneID = "w1:p1"

// fakeReworkClient models a worktree whose pane starts bare and comes up live
// after the first spawn (mirroring fakeRebaseClient in rebase_test.go): every
// dispatch — whether the initial PaneRun spawn or a later AgentPrompt reuse —
// writes status shortly after, as a real worker eventually would.
func fakeReworkClient(worktree string, status *protocol.Status) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	writeStatusSoon := func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = protocol.Write(protocol.StatusPath(worktree), status)
		}()
	}
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			mu.Unlock()
			writeStatusSoon()
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			writeStatusSoon()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// fakeReworkClientRounds is fakeReworkClient generalized to report a
// different status on each successive dispatch, keyed by a 1-based round
// counter — needed to grow the worktree's real diff between rounds and
// self-report only that round's own delta, the shape that exercises
// gateVerdict's priorMeasured subtraction (see
// TestRunReworkSubtractsPriorMeasuredAcrossRounds).
func fakeReworkClientRounds(worktree string, statusFor func(round int) *protocol.Status) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	var round int
	dispatch := func() {
		mu.Lock()
		round++
		r := round
		mu.Unlock()
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = protocol.Write(protocol.StatusPath(worktree), statusFor(r))
		}()
	}
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, reworkTestPaneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			mu.Unlock()
			dispatch()
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			dispatch()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// writeLinesFile overwrites path with n newline-terminated lines, so
// MeasureDiff's untracked-file line count (see countLines in measure.go)
// reports exactly n insertions for it.
func writeLinesFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("line\n", n)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// reworkStatus is the failing-test status fakeReworkClient's worker "reports"
// after every dispatch in the tests below: it forces the gate to escalate
// every round, so the reviewer's own decision (not diff/plan-evidence
// bookkeeping) drives the loop.
func reworkStatus() *protocol.Status {
	return &protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
	}
}

func reworkLogger() *eventlog.Logger {
	return eventlog.New(nil, "rework", "test-run", nil)
}

func TestRunReworkEmptyWorktree(t *testing.T) {
	cmd, _ := testCmd()
	err := runRework(cmd, herdr.New(), &fakeReviewer{}, reworkLogger(), &reworkOpts{})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for an empty worktree, got %v", err)
	}
}

func TestRunReworkNoVerdictErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	cmd, _ := testCmd()

	err := runRework(cmd, herdr.New(), &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3})
	if err == nil || !strings.Contains(err.Error(), "no argus verdict") {
		t.Fatalf("want a no-verdict error, got %v", err)
	}
}

func TestRunReworkAlreadyApprovedIsNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: true, Source: "review", Summary: "ok"}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	// A client that errors on any call proves runRework never dispatches once
	// it sees an already-approving verdict.
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call for an already-approved worktree: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to rework") {
		t.Errorf("expected a nothing-to-rework message:\n%s", buf.String())
	}
}

func TestRunReworkDryRunPrintsPlanWithoutDispatching(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()

	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		t.Fatalf("unexpected herdr call during --dry-run: %v", args)
		return nil, nil
	})

	err := runRework(cmd, client, &fakeReviewer{}, reworkLogger(), &reworkOpts{worktree: dir, base: "feat-x", maxRounds: 3, dryRun: true})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if !strings.Contains(buf.String(), "rework plan (dry run)") || !strings.Contains(buf.String(), "missing nil check") {
		t.Errorf("expected a dry-run plan with the recorded finding:\n%s", buf.String())
	}
}

func TestRunReworkApprovesFirstRound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "fixed"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Errorf("want exactly 1 review call, got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "round 1/3") || !strings.Contains(out, "approve") {
		t.Errorf("expected a round-1 approve report:\n%s", out)
	}
	if strings.Contains(out, "escalating") {
		t.Errorf("an approved round must not escalate:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkBuildsReviewerFromRepoConfigWhenNil covers rework's lazy
// reviewer construction: RunE passes a nil reviewer (it can't resolve
// review_effort's flag/config precedence before .argus/config.yml is loaded
// inside runRework), so runRework must build one itself once rc is read, and
// resolveReviewEffort's own precedence (explicit flag > config > default)
// must still hold at that later construction point.
func TestRunReworkBuildsReviewerFromRepoConfigWhenNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".argus"), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".argus", "config.yml"), []byte("review_effort: \"high\"\n"), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}

	type captured struct{ model, effort string }
	var got captured
	original := newReviewer
	newReviewer = func(model, effort string, _ *eventlog.Logger) supervisor.Reviewer {
		got = captured{model: model, effort: effort}
		return &fakeReviewer{res: supervisor.ReviewResult{Decision: "approve", Summary: "ok"}}
	}
	t.Cleanup(func() { newReviewer = original })

	cmd, _ := testCmd()
	client := fakeReworkClient(dir, reworkStatus())

	err := runRework(cmd, client, nil, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		reviewModel: "sonnet", gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if got.model != "sonnet" || got.effort != "high" {
		t.Errorf("newReviewer got model=%q effort=%q, want model=sonnet effort=high (from .argus/config.yml)", got.model, got.effort)
	}
}

// TestRunReworkExplicitEffortFlagWinsOverRepoConfig is the same setup as
// TestRunReworkBuildsReviewerFromRepoConfigWhenNil but with --review-effort
// passed explicitly, which must win over the repo's config value.
func TestRunReworkExplicitEffortFlagWinsOverRepoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".argus"), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".argus", "config.yml"), []byte("review_effort: \"high\"\n"), 0o644); err != nil {
		t.Fatalf("writing config.yml: %v", err)
	}

	var gotEffort string
	original := newReviewer
	newReviewer = func(_, effort string, _ *eventlog.Logger) supervisor.Reviewer {
		gotEffort = effort
		return &fakeReviewer{res: supervisor.ReviewResult{Decision: "approve", Summary: "ok"}}
	}
	t.Cleanup(func() { newReviewer = original })

	cmd, _ := testCmd()
	client := fakeReworkClient(dir, reworkStatus())

	err := runRework(cmd, client, nil, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		reviewEffort: "low", reviewEffortExplicit: true, gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if gotEffort != "low" {
		t.Errorf("newReviewer got effort=%q, want the explicit flag value \"low\" over the repo config's \"high\"", gotEffort)
	}
}

func TestRunReworkLoopsOnRequestChangesThenApproves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDirWithDiff(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"nil check in foo.go"}},
		{Decision: "approve", Summary: "fixed now"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls (one retry), got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "round 1/3") || !strings.Contains(out, "round 2/3") {
		t.Errorf("expected both rounds reported:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict after round 2, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

// TestRunReworkSubtractsPriorMeasuredAcrossRounds is the regression test for
// InvalidateStatus deleting verdict.json before every rework round, which
// permanently defeated gateVerdict's under-report subtraction (see
// priorMeasured in loop.go) from round 2 onward: the worktree's real diff
// grows from 20 to 55 lines between round 1 and round 2, and round 2's
// self-report (35) is exactly that round's own delta since round 1's
// verdict, not the 55-line cumulative total — the same shape as the false
// under-report this bug produced in production. Without the fix, round 2's
// gate always sees priorMeasuredOK=false, compares the self-report against
// the full 55-line cumulative diff instead, and flags an unwaivable
// "under-reported diff" that keeps the final verdict not-approved even
// though the reviewer approves.
func TestRunReworkSubtractsPriorMeasuredAcrossRounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	f := filepath.Join(dir, "f.txt")

	statusFor := func(round int) *protocol.Status {
		switch round {
		case 1:
			writeLinesFile(t, f, 20)
			return &protocol.Status{
				Phase:    protocol.PhaseAwaitingReview,
				Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
				DiffStat: protocol.DiffStat{Files: 1, Insertions: 20},
			}
		default:
			writeLinesFile(t, f, 55)
			return &protocol.Status{
				Phase:    protocol.PhaseAwaitingReview,
				Tests:    []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
				DiffStat: protocol.DiffStat{Files: 1, Insertions: 35},
			}
		}
	}

	cmd, buf := testCmd()
	client := fakeReworkClientRounds(dir, statusFor)
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still missing something", Findings: []string{"finding"}},
		{Decision: "approve", Summary: "delta looks right"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls, got %d", reviewer.callCount())
	}
	out := buf.String()
	if strings.Contains(out, "under-reported") {
		t.Errorf("round 2's self-report matches its own delta since round 1's verdict — must not be flagged as an under-report:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || !approval.Approved {
		t.Errorf("want a persisted approved verdict once round 2's delta-only self-report clears the gate, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

func TestRunReworkExhaustsRoundsAndEscalates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "request-changes", Summary: "still wrong", Findings: []string{"finding"}},
	}} // repeats for every round

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 2, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 2 {
		t.Fatalf("want exactly 2 review calls (max-rounds), got %d", reviewer.callCount())
	}
	out := buf.String()
	if !strings.Contains(out, "rework rounds exhausted (2/2)") {
		t.Errorf("expected an exhausted-rounds escalation message:\n%s", out)
	}
	approval, found, aerr := protocol.LoadApproval(dir)
	if aerr != nil || !found || approval.Approved {
		t.Errorf("want a persisted not-approved verdict after exhausting rounds, found=%v approval=%+v err=%v", found, approval, aerr)
	}
}

func TestRunReworkNeedsHumanEscalatesImmediately(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	client := fakeReworkClient(dir, reworkStatus())
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{
		{Decision: "needs-human", Summary: "can't tell"},
		{Decision: "request-changes", Summary: "should never be reached"},
	}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 1 {
		t.Errorf("want rework to stop after round 1 on needs-human, got %d review calls", reviewer.callCount())
	}
	if !strings.Contains(buf.String(), "needs-human") {
		t.Errorf("expected a needs-human escalation message:\n%s", buf.String())
	}
}

func TestRunReworkStopsImmediatelyWhenWorkerReportsBlocked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	if err := protocol.WriteApproval(dir, &protocol.Approval{Approved: false, Source: "review", Reasons: []string{"missing nil check"}}); err != nil {
		t.Fatalf("seeding approval: %v", err)
	}
	cmd, buf := testCmd()
	blocked := &protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "need a design decision"}
	client := fakeReworkClient(dir, blocked)
	reviewer := &sequenceReviewer{results: []supervisor.ReviewResult{{Decision: "approve", Summary: "should never be reached"}}}

	err := runRework(cmd, client, reviewer, reworkLogger(), &reworkOpts{
		worktree: dir, base: "feat-x", maxRounds: 3, interval: 5 * time.Millisecond,
		gate: gateFlags{},
	})
	if err != nil {
		t.Fatalf("runRework: %v", err)
	}
	if reviewer.callCount() != 0 {
		t.Errorf("a blocked worker must not reach the reviewer, got %d calls", reviewer.callCount())
	}
	if !strings.Contains(buf.String(), "need a design decision") {
		t.Errorf("expected the blocked reason surfaced:\n%s", buf.String())
	}
}

func TestReworkOptsDispatchTargetCopiesFields(t *testing.T) {
	o := &reworkOpts{
		worktree: "/wt", launcher: "claude", workerRuntime: "podman", noCredProxy: true,
		credentialEnv:   map[string]string{"github.com": "MY_TOKEN"},
		livenessTimeout: time.Second, livenessInterval: time.Millisecond,
	}
	target := o.dispatchTarget()
	if target.worktree != o.worktree || target.launcher != o.launcher || target.workerRuntime != o.workerRuntime ||
		target.noCredProxy != o.noCredProxy || target.livenessTimeout != o.livenessTimeout || target.livenessInterval != o.livenessInterval {
		t.Errorf("dispatchTarget() = %+v, want it to mirror reworkOpts %+v", target, o)
	}
	if target.credentialEnv["github.com"] != "MY_TOKEN" {
		t.Errorf("dispatchTarget() dropped credentialEnv: %+v", target)
	}
}
