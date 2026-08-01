package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestRenderReportShowsVerdictAndReview(t *testing.T) {
	var buf bytes.Buffer
	cfg := &Config{
		Out:    &buf,
		Now:    func() time.Time { return time.Date(2026, 7, 18, 0, 5, 0, 0, time.UTC) },
		Client: herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) { return []byte(twoPaneList), nil }),
		Home:   t.TempDir(),
	}

	start := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	states := []*workerState{
		{ // clean → auto-approve, no reviewer call
			plan: &WorkerPlan{Worker: Worker{Task: "clean-a"}}, paneID: "1-2", hasFile: true, started: start,
			planEvidenceOK: true, hasPlanEvidence: true,
			status: protocol.Status{
				Phase:        protocol.PhaseAwaitingReview,
				Tests:        []protocol.TestRun{{Cmd: "make test", Result: protocol.ResultPass}},
				FilesTouched: []string{"cmd/x.go"},
				DiffStat:     protocol.DiffStat{Insertions: 40, Deletions: 5},
			},
		},
		{ // escalated and reviewed → request-changes with a finding
			plan: &WorkerPlan{Worker: Worker{Task: "risky-b"}}, paneID: "1-3", hasFile: true, started: start,
			status: protocol.Status{Phase: protocol.PhaseAwaitingReview, DiffStat: protocol.DiffStat{Insertions: 600, Deletions: 50}},
			review: &ReviewResult{Decision: "request-changes", Summary: "off-by-one", Findings: []string{"loop bound wrong"}},
		},
		{ // blocked with a reviewer error
			plan: &WorkerPlan{Worker: Worker{Task: "blocked-c"}}, hasFile: true, started: start,
			status:    protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "needs decision"},
			reviewErr: errors.New("reviewer boom"),
		},
	}

	renderReport(context.Background(), cfg, states)
	out := buf.String()

	for _, want := range []string{
		"clean-a", "auto-approve",
		"risky-b", "needs review", "request-changes", "off-by-one", "loop bound wrong",
		"blocked-c", "reviewer error",
		"tokens:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

// TestLogRunSummaryCountsPersistedVerdictNotReviewDecision is the regression
// for a run summary that said "2/2 approved" right after both workers'
// persisted verdict.json had approved:false: a hard gate reason (VerifyTests
// mismatch) forces recordApproval to write approved=false even when the
// reviewer said "approve" (loop.go's reviewOne), so the summary must read
// that persisted disposition back rather than recompute its own from
// st.review.Decision, which knows nothing about HardReasons.
func TestLogRunSummaryCountsPersistedVerdictNotReviewDecision(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{
		Approved: false,
		Source:   "review",
		Summary:  `reviewer said "approve", but a hard gate check is unwaivable: worker claimed "make ci" passed, but re-running it failed`,
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cfg := &Config{Log: eventlog.New(&buf, "supervise", "test-run", nil)}
	states := []*workerState{{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "worker-a", Worktree: wt}},
		status:  protocol.Status{Phase: protocol.PhaseAwaitingReview},
		review:  &ReviewResult{Decision: "approve", Summary: "looked fine to me"},
	}}

	logRunSummary(cfg, states, 0, 0)

	if strings.Contains(buf.String(), `"outcome":"1/1 approved"`) {
		t.Errorf("run_summary must not count a worker as approved when its persisted verdict says otherwise, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"outcome":"0/1 approved"`) {
		t.Errorf("run_summary should report 0/1 approved for a rejected-but-reviewed worker, got %s", buf.String())
	}
}

// TestRenderReportShowsApprovalProvenance pins the signal issue #285 asks for:
// the report must tell an operator, per worker, which of the three sources
// cleared it — so an auto- or reviewer-approved diff is never re-read by hand,
// and only a surfaced-awaiting-human one is. The provenance is read from each
// worktree's persisted verdict.json (recordApproval's output), the only place a
// reviewer decision and a hard-reason override are already folded in.
func TestRenderReportShowsApprovalProvenance(t *testing.T) {
	gateWT := t.TempDir()
	reviewWT := t.TempDir()
	humanWT := t.TempDir()
	writeVerdict := func(wt string, approved bool, source string) {
		t.Helper()
		if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: approved, Source: source}); err != nil {
			t.Fatal(err)
		}
	}
	writeVerdict(gateWT, true, "gate")
	writeVerdict(reviewWT, true, "review")
	writeVerdict(humanWT, false, "review") // reviewer request-changes → needs a human

	var buf bytes.Buffer
	cfg := &Config{
		Out:    &buf,
		Now:    func() time.Time { return time.Date(2026, 7, 18, 0, 5, 0, 0, time.UTC) },
		Client: herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) { return []byte(`{"result":{"panes":[]}}`), nil }),
		Home:   t.TempDir(),
		Log:    eventlog.New(&bytes.Buffer{}, "supervise", "test-run", nil),
	}
	start := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	awaiting := func(task, wt string) *workerState {
		return &workerState{
			plan: &WorkerPlan{Worker: Worker{Task: task, Worktree: wt}}, hasFile: true, started: start,
			status: protocol.Status{Phase: protocol.PhaseAwaitingReview},
		}
	}
	states := []*workerState{
		awaiting("gate-a", gateWT),
		awaiting("review-b", reviewWT),
		awaiting("human-c", humanWT),
	}

	renderReport(context.Background(), cfg, states)
	out := buf.String()

	for _, want := range []string{
		"gate-auto-approved — verified by the gate, no human read needed",
		"reviewer-approved — verified by the review, no human read needed",
		"surfaced-awaiting-human — hand-read this diff and decide",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing provenance %q\n---\n%s", want, out)
		}
	}
}

// TestLogRunSummaryEmitsProvenanceCounts pins that the run_summary event splits
// approvals by source and counts workers still awaiting a human, so `argus
// stats` (and any operator reading the run log) can trust the aggregate the same
// way the per-worker report lets them trust each diff.
func TestLogRunSummaryEmitsProvenanceCounts(t *testing.T) {
	gateWT := t.TempDir()
	humanWT := t.TempDir()
	if err := protocol.WriteApproval(gateWT, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteApproval(humanWT, &protocol.Approval{Approved: false, Source: "gate"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cfg := &Config{Log: eventlog.New(&buf, "supervise", "test-run", nil)}
	states := []*workerState{
		{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "gate-a", Worktree: gateWT}}, status: protocol.Status{Phase: protocol.PhaseAwaitingReview}},
		{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "human-b", Worktree: humanWT}}, status: protocol.Status{Phase: protocol.PhaseBlocked}},
	}
	logRunSummary(cfg, states, 1, 0)

	out := buf.String()
	for _, want := range []string{`"gate_approved":1`, `"reviewer_approved":0`, `"awaiting_human":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("run_summary missing %q: %s", want, out)
		}
	}
}

// TestRenderReportDistinguishesQuestionFromGenericBlocked pins the per-worker
// display split: a blocked worker carrying a structured Question shows its
// text and options, while one with only a freeform BlockedReason shows that
// string instead — the report must not fold the two into one opaque line.
func TestRenderReportDistinguishesQuestionFromGenericBlocked(t *testing.T) {
	var buf bytes.Buffer
	cfg := &Config{
		Out:    &buf,
		Now:    func() time.Time { return time.Date(2026, 7, 18, 0, 5, 0, 0, time.UTC) },
		Client: herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) { return []byte(`{"result":{"panes":[]}}`), nil }),
		Home:   t.TempDir(),
	}
	start := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	states := []*workerState{
		{
			plan: &WorkerPlan{Worker: Worker{Task: "question-a"}}, hasFile: true, started: start,
			status: protocol.Status{
				Phase:         protocol.PhaseBlocked,
				BlockedReason: "needs a decision",
				Question:      &protocol.Question{Text: "wait or cherry-pick?", Options: []string{"wait", "cherry-pick"}},
			},
		},
		{
			plan: &WorkerPlan{Worker: Worker{Task: "generic-b"}}, hasFile: true, started: start,
			status: protocol.Status{Phase: protocol.PhaseBlocked, BlockedReason: "no forge configured"},
		},
	}

	renderReport(context.Background(), cfg, states)
	out := buf.String()

	for _, want := range []string{
		"blocked on question: wait or cherry-pick?",
		"1. wait",
		"2. cherry-pick",
		"blocked: no forge configured",
		"2 worker(s) blocked (1 on a structured question",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

func TestBlockedCountsOnlyCountsBlockedWithAStatusFile(t *testing.T) {
	states := []*workerState{
		{hasFile: true, status: protocol.Status{Phase: protocol.PhaseBlocked, Question: &protocol.Question{Text: "q"}}},
		{hasFile: true, status: protocol.Status{Phase: protocol.PhaseBlocked}},
		{hasFile: true, status: protocol.Status{Phase: protocol.PhaseAwaitingReview}},
		{hasFile: false, status: protocol.Status{Phase: protocol.PhaseBlocked}}, // no file: not a real report
	}
	blocked, onQuestion := blockedCounts(states)
	if blocked != 2 {
		t.Errorf("blocked = %d, want 2", blocked)
	}
	if onQuestion != 1 {
		t.Errorf("blockedOnQuestion = %d, want 1", onQuestion)
	}
}

// TestLogRunSummaryEmitsBlockedFields pins that the caller-supplied blocked
// counts (see blockedCounts) land verbatim on the run_summary event, so
// `argus stats` can aggregate "blocked on a question" across many runs.
func TestLogRunSummaryEmitsBlockedFields(t *testing.T) {
	var buf bytes.Buffer
	cfg := &Config{Log: eventlog.New(&buf, "supervise", "test-run", nil)}
	logRunSummary(cfg, nil, 3, 2)

	out := buf.String()
	if !strings.Contains(out, `"blocked":3`) {
		t.Errorf("run_summary missing blocked count: %s", out)
	}
	if !strings.Contains(out, `"blocked_on_question":2`) {
		t.Errorf("run_summary missing blocked_on_question count: %s", out)
	}
}

func TestLauncherModelEffort(t *testing.T) {
	cases := []struct {
		launcher, model, effort string
	}{
		{"claude --permission-mode auto", "", ""},
		{"claude --model opus --permission-mode auto", "opus", ""},
		{"claude --effort high --permission-mode auto", "", "high"},
		{"claude --model=opus --effort=high", "opus", "high"},
		{"claude --model opus --effort high", "opus", "high"},
	}
	for _, c := range cases {
		model, effort := launcherModelEffort(c.launcher)
		if model != c.model || effort != c.effort {
			t.Errorf("launcherModelEffort(%q) = (%q, %q), want (%q, %q)", c.launcher, model, effort, c.model, c.effort)
		}
	}
}

func TestRenderReviewDecisionMarks(t *testing.T) {
	cases := map[string]string{
		"approve":         "approve",
		"request-changes": "request-changes",
		"needs-human":     "needs-human",
	}
	for decision, want := range cases {
		var buf bytes.Buffer
		renderReview(&buf, &workerState{review: &ReviewResult{Decision: decision, Summary: "s"}})
		if !strings.Contains(buf.String(), want) {
			t.Errorf("decision %q not rendered: %q", decision, buf.String())
		}
	}

	// No reviewer ran → nothing printed.
	var empty bytes.Buffer
	renderReview(&empty, &workerState{})
	if empty.Len() != 0 {
		t.Errorf("no-review state should print nothing, got %q", empty.String())
	}
}
