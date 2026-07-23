package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
