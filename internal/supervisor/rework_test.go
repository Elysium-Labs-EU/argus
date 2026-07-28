package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestReworkBriefIncludesFindingsRoundAndTask(t *testing.T) {
	brief := ReworkBrief("fix the widget", "feat-x", "origin/main", []string{"nil check missing in foo.go", "no test for bar"}, 2, 3)

	for _, want := range []string{
		"branch feat-x",
		"rework round 2/3",
		"fix the widget",
		"nil check missing in foo.go",
		"no test for bar",
		"awaiting_review",
		protocol.WriterBrief("origin/main"),
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("ReworkBrief missing %q in:\n%s", want, brief)
		}
	}
}

// TestReworkBriefWarnsAgainstNarrowRetitle pins the issue-282 fix at the
// brief level: a rework round is told to leave title empty rather than
// describe only that round's fix, since runWorkerReport can only carry a
// prior title forward when the round's own report doesn't set a new one.
func TestReworkBriefWarnsAgainstNarrowRetitle(t *testing.T) {
	brief := ReworkBrief("fix the widget", "feat-x", "origin/main", []string{"finding"}, 1, 3)
	for _, want := range []string{"title", "empty", "deliberately"} {
		if !strings.Contains(brief, want) {
			t.Errorf("ReworkBrief missing %q (title-continuity guidance) in:\n%s", want, brief)
		}
	}
}

func TestReworkBriefOmitsOriginalTaskSectionWhenEmpty(t *testing.T) {
	brief := ReworkBrief("", "feat-x", "origin/main", []string{"finding"}, 1, 3)
	if strings.Contains(brief, "Original task:") {
		t.Errorf("expected no 'Original task:' section for an empty task, got:\n%s", brief)
	}
}

// reworkTestHome writes a fake session transcript with a real TodoWrite tool
// call for worktree, so HasPlanEvidence (which JudgeOne's reconcile step
// calls) reports true instead of escalating every JudgeOne test on a missing
// plan-evidence backstop unrelated to what each test actually exercises.
func reworkTestHome(t *testing.T, worktree string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	content := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return home
}

func TestJudgeOneAutoApprovesCleanWorkAndPersists(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{Now: time.Now, Base: "HEAD", Home: home, Policy: &policy}
	plan := &WorkerPlan{Worker: Worker{Task: "clean", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		// "true" is genuinely re-run by VerifyTests (see reconcile), unlike a
		// real test invocation that has nothing to build in an empty worktree.
		Tests: []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}},
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, "", "")
	if !result.Gate.AutoApprove {
		t.Fatalf("want the gate to auto-approve a clean, well-tested change, got reasons %v", result.Gate.Reasons)
	}
	if result.Review != nil {
		t.Errorf("want no reviewer call for an auto-approved worker, got %+v", result.Review)
	}
	approval, found, err := protocol.LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if !approval.Approved || approval.Source != "gate" {
		t.Errorf("want a persisted gate-approved verdict, got %+v", approval)
	}
}

func TestJudgeOneEscalatesToReviewerAndPersistsApprove(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Now: time.Now, Base: "HEAD", Home: home, Policy: &policy,
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"approve","summary":"looks right now","findings":[]}`)),
	}
	plan := &WorkerPlan{Worker: Worker{Task: "rework-1", Branch: "b", Worktree: wt}}
	// A failing test forces the gate to escalate, so the reviewer actually runs.
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, "", "")
	if result.Gate.AutoApprove {
		t.Fatal("want the gate to escalate on a failing test")
	}
	if result.Review == nil || result.Review.Decision != "approve" {
		t.Fatalf("want the reviewer's approve verdict, got %+v", result.Review)
	}
	approval, found, err := protocol.LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if !approval.Approved || approval.Source != "review" {
		t.Errorf("want a persisted review-approved verdict, got %+v", approval)
	}
}

func TestJudgeOneEscalatesToReviewerAndPersistsRequestChanges(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{
		Now: time.Now, Base: "HEAD", Home: home, Policy: &policy,
		Reviewer: NewReviewerWithRunner(fakeReviewRunner(`{"decision":"request-changes","summary":"still missing a nil check","findings":["nil check in foo.go"]}`)),
	}
	plan := &WorkerPlan{Worker: Worker{Task: "rework-1", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, "", "")
	if result.Review == nil || result.Review.Decision != "request-changes" {
		t.Fatalf("want the reviewer's request-changes verdict, got %+v", result.Review)
	}
	if len(result.Review.Findings) != 1 || result.Review.Findings[0] != "nil check in foo.go" {
		t.Errorf("want the reviewer's findings passed through, got %v", result.Review.Findings)
	}
	approval, found, err := protocol.LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if approval.Approved {
		t.Errorf("want a persisted not-approved verdict, got %+v", approval)
	}
}

// TestJudgeOneEscalatesReworkThatChangedNothing is the core issue-287 guard: a
// rework round that reaches a terminal phase with content byte-identical to the
// state its prior verdict already rejected must never auto-approve, and the
// reason must be unwaivable. JudgeOne never dispatches a real worker in-test, so
// the worktree it measures is exactly the pre-round state — passing that state's
// own hash as priorContentHash reproduces "the round changed nothing."
func TestJudgeOneEscalatesReworkThatChangedNothing(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{Now: time.Now, Base: "HEAD", Home: home, Policy: &policy}
	plan := &WorkerPlan{Worker: Worker{Task: "rework-noop", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}},
	}

	_, files, err := MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	preRound, err := ContentHash(wt, files)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	// JudgeOne never commits in-test either, so HEAD is exactly as unchanged as
	// the content above — a genuine no-op round, on both signals.
	preRoundHead, err := HeadSHA(context.Background(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, preRound, preRoundHead)
	if result.Gate.AutoApprove {
		t.Fatal("want the gate to escalate a rework round that changed nothing since its rejected state")
	}
	if len(result.Gate.HardReasons) == 0 {
		t.Fatalf("want an unwaivable hard reason for a zero-delta rework round, got reasons %v", result.Gate.Reasons)
	}
	var found bool
	for _, r := range result.Gate.HardReasons {
		if strings.Contains(r, "changed nothing") {
			found = true
		}
	}
	if !found {
		t.Errorf("want the zero-delta reason among hard reasons, got %v", result.Gate.HardReasons)
	}
	approval, ok, err := protocol.LoadApproval(wt)
	if err != nil || !ok {
		t.Fatalf("LoadApproval: found=%v err=%v", ok, err)
	}
	if approval.Approved {
		t.Errorf("want a persisted not-approved verdict for a zero-delta rework round, got %+v", approval)
	}
}

// TestJudgeOneAllowsReworkThatChangedContent is the false-positive guard for the
// check above: a rework round whose pre-round hash differs from its post-round
// content (any real edit) must clear the zero-delta gate — only a genuine no-op
// escalates, not every rework round.
func TestJudgeOneAllowsReworkThatChangedContent(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{Now: time.Now, Base: "HEAD", Home: home, Policy: &policy}
	plan := &WorkerPlan{Worker: Worker{Task: "rework-real", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}},
	}

	// A pre-round hash no post-round measurement can match: the round
	// demonstrably changed the worktree since this state.
	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, "0000000000000000000000000000000000000000000000000000000000000000", "")
	if !result.Gate.AutoApprove {
		t.Fatalf("want a rework round with changed content to auto-approve, got reasons %v", result.Gate.Reasons)
	}
}

// gitWorktreeWithFixedBaseAndDirtyEdit makes a temp git repo whose base commit
// is also tagged "base-ref" — a name that keeps pointing at that commit even
// after later commits move HEAD, unlike "HEAD" itself. It leaves f.go edited
// but uncommitted, reproducing "an already-fixed-but-uncommitted change"
// sitting in the worktree before a rework round ever dispatches.
func gitWorktreeWithFixedBaseAndDirtyEdit(t *testing.T) string {
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
	run("tag", "base-ref")
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n\nvar Added = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestJudgeOneAllowsReworkThatCommittedPreexistingDirtyContent pins the
// issue-312 false-positive: a rework round whose only job is to commit and
// push content that was already sitting uncommitted in the worktree before
// the round dispatched leaves ContentHash unchanged (same bytes on disk,
// before and after), but a real, distinct commit lands — HEAD moves. That
// must clear the zero-delta gate exactly like a content edit does, since the
// round demonstrably did real, verifiable work (a new commit ships the fix),
// not nothing.
func TestJudgeOneAllowsReworkThatCommittedPreexistingDirtyContent(t *testing.T) {
	wt := gitWorktreeWithFixedBaseAndDirtyEdit(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{Now: time.Now, Base: "base-ref", Home: home, Policy: &policy}
	plan := &WorkerPlan{Worker: Worker{Task: "rework-commit-only", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "true", Result: protocol.ResultPass}},
	}

	_, files, err := MeasureDiff(context.Background(), wt, "base-ref")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	preRound, err := ContentHash(wt, files)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	preRoundHead, err := HeadSHA(context.Background(), wt)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	// The round's entire job: commit the already-edited f.go and "push" it —
	// no further byte edits, so the post-round content hash matches preRound.
	commit := exec.Command("git", "-C", wt, "commit", "-aq", "-m", "commit the fix")
	if out, cerr := commit.CombinedOutput(); cerr != nil {
		t.Fatalf("git commit: %v\n%s", cerr, out)
	}
	postRoundHead, err := HeadSHA(context.Background(), wt)
	if err != nil {
		t.Fatalf("HeadSHA post-commit: %v", err)
	}
	if postRoundHead == preRoundHead {
		t.Fatal("test setup broken: commit did not move HEAD")
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, preRound, preRoundHead)
	if !result.Gate.AutoApprove {
		t.Fatalf("want a rework round that committed pre-existing dirty content to auto-approve, got reasons %v", result.Gate.Reasons)
	}
	for _, r := range result.Gate.HardReasons {
		if strings.Contains(r, "changed nothing") {
			t.Errorf("want no zero-delta hard reason when HEAD moved to a real new commit, got %v", result.Gate.HardReasons)
		}
	}
}

func TestJudgeOneWithNoReviewerLeavesReviewNil(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := reworkTestHome(t, wt)
	policy := DefaultReviewPolicy()
	cfg := &Config{Now: time.Now, Base: "HEAD", Home: home, Policy: &policy} // no Reviewer
	plan := &WorkerPlan{Worker: Worker{Task: "rework-1", Branch: "b", Worktree: wt}}
	status := protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultFail}},
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now(), nil, "", "")
	if result.Gate.AutoApprove {
		t.Fatal("want the gate to escalate on a failing test")
	}
	if result.Review != nil {
		t.Errorf("want no reviewer verdict when no Reviewer is configured, got %+v", result.Review)
	}
	approval, found, err := protocol.LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if approval.Approved || approval.Source != "gate" {
		t.Errorf("want a persisted not-approved gate verdict, got %+v", approval)
	}
}
