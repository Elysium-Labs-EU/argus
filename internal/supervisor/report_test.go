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

	logRunSummary(cfg, states)

	if strings.Contains(buf.String(), `"outcome":"1/1 approved"`) {
		t.Errorf("run_summary must not count a worker as approved when its persisted verdict says otherwise, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"outcome":"0/1 approved"`) {
		t.Errorf("run_summary should report 0/1 approved for a rejected-but-reviewed worker, got %s", buf.String())
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
