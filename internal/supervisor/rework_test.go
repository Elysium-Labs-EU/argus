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

func TestReworkBriefIncludesFindingsRoundAndTask(t *testing.T) {
	brief := ReworkBrief("fix the widget", "feat-x", []string{"nil check missing in foo.go", "no test for bar"}, 2, 3)

	for _, want := range []string{
		"branch feat-x",
		"rework round 2/3",
		"fix the widget",
		"nil check missing in foo.go",
		"no test for bar",
		"awaiting_review",
		protocol.WriterBrief,
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("ReworkBrief missing %q in:\n%s", want, brief)
		}
	}
}

func TestReworkBriefOmitsOriginalTaskSectionWhenEmpty(t *testing.T) {
	brief := ReworkBrief("", "feat-x", []string{"finding"}, 1, 3)
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
		Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
	}

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now())
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

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now())
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

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now())
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

	result := JudgeOne(context.Background(), cfg, plan, &status, "pane-1", time.Now())
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
