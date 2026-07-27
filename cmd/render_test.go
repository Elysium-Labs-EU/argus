package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func TestRenderStats(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	s := &eventlog.Stats{
		Runs: 2, Workers: 3,
		GateAutoApprove: 1, GateEscalate: 2,
		Reviews: 2, ReviewReAsks: 1,
		Approved: 2, NotApproved: 1,
		ReviewDecisions: map[string]int{"approve": 1, "request-changes": 1},
		Phases:          map[string]int{"awaiting_review": 2},
		TokensByTask:    map[string]int64{"a": 1000, "b": 3000},
	}
	renderStats(cmd, s)
	out := buf.String()
	for _, want := range []string{"2 run(s)", "escalation rate 67%", "parse-fail rate 50%", "awaiting_review", "3000"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output missing %q:\n%s", want, out)
		}
	}
	// Tokens are sorted high-to-low: b (3000) before a (1000).
	if strings.Index(out, "b: 3000") > strings.Index(out, "a: 1000") {
		t.Errorf("tokens not sorted by spend:\n%s", out)
	}
}

// TestRenderStatsBlockedOnQuestion pins renderStats' split between a
// "blocked" phase count that includes structured questions (the "(N on a
// structured question)" suffix) and one that doesn't (plain count line) —
// the two are rendered by the same loop iteration but only diverge when
// BlockedOnQuestion is actually non-zero for the "blocked" phase key.
func TestRenderStatsBlockedOnQuestion(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	s := &eventlog.Stats{
		Runs: 1, Workers: 3,
		Phases:            map[string]int{"awaiting_review": 1, "blocked": 2},
		BlockedOnQuestion: 1,
	}
	renderStats(cmd, s)
	out := buf.String()
	if !strings.Contains(out, "blocked: 2 (1 on a structured question)") {
		t.Errorf("stats output missing blocked-on-question suffix:\n%s", out)
	}
	if strings.Contains(out, "blocked: 2\n") {
		t.Errorf("plain (unsuffixed) blocked line should not appear when BlockedOnQuestion > 0:\n%s", out)
	}
}

// TestRenderStatsBlockedWithNoQuestions confirms the plain, unsuffixed line
// still renders when workers are blocked but none carried a structured
// Question (BlockedOnQuestion == 0) — the suffix must not appear regardless
// of how many workers are blocked.
func TestRenderStatsBlockedWithNoQuestions(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	s := &eventlog.Stats{
		Runs: 1, Workers: 1,
		Phases: map[string]int{"blocked": 1},
	}
	renderStats(cmd, s)
	out := buf.String()
	if !strings.Contains(out, "blocked: 1\n") {
		t.Errorf("stats output missing plain blocked line:\n%s", out)
	}
	if strings.Contains(out, "structured question") {
		t.Errorf("suffix should not appear with BlockedOnQuestion == 0:\n%s", out)
	}
}

func TestRenderRebaseOutcome(t *testing.T) {
	cases := []struct {
		phase protocol.Phase
		want  string
	}{
		{protocol.PhaseAwaitingReview, "ready"},
		{protocol.PhaseBlocked, "blocked"},
		{protocol.PhaseWorking, "still"},
		{protocol.Phase("weird"), "phase"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		renderRebaseOutcome(&buf, "feat-x", &protocol.Status{Phase: c.phase, BlockedReason: "x"})
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("phase %q: output %q missing %q", c.phase, buf.String(), c.want)
		}
	}
}

func TestRenderReviewResult(t *testing.T) {
	cases := []struct {
		decision string
		want     string
	}{
		{"approve", "✓"},
		{"request-changes", "✗"},
		{"needs-human", "○"},
		{"", "○"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		renderReviewResult(&buf, supervisor.ReviewResult{
			Decision: c.decision,
			Summary:  "a summary",
			Findings: []string{"finding one"},
		})
		out := buf.String()
		if !strings.Contains(out, c.want) {
			t.Errorf("decision %q: output %q missing mark %q", c.decision, out, c.want)
		}
		if !strings.Contains(out, "a summary") || !strings.Contains(out, "finding one") {
			t.Errorf("decision %q: output missing summary/findings:\n%s", c.decision, out)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]int{"b": 1, "a": 2, "c": 3})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys not sorted: %v", got)
	}
}
