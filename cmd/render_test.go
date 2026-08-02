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
	renderStats(cmd, s, -1)
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
	renderStats(cmd, s, -1)
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
	renderStats(cmd, s, -1)
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

// TestRenderStatsCapsTaskList pins that a positive taskLimit shows only the
// top-N (by spend) tasks and appends an omitted-count footer naming both
// how many were dropped and the full total.
func TestRenderStatsCapsTaskList(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	s := &eventlog.Stats{
		Runs: 1, Workers: 1,
		TokensByTask: map[string]int64{"a": 300, "b": 200, "c": 100},
	}
	renderStats(cmd, s, 2)
	out := buf.String()
	if !strings.Contains(out, "a: 300") || !strings.Contains(out, "b: 200") {
		t.Errorf("expected the two highest-spend tasks in output:\n%s", out)
	}
	if strings.Contains(out, "c: 100") {
		t.Errorf("task beyond the limit should be omitted, not printed:\n%s", out)
	}
	if !strings.Contains(out, "1 more task(s) omitted (--limit 3 or --all to see them)") {
		t.Errorf("expected an omitted-count footer naming the full total:\n%s", out)
	}
}

// TestRenderStatsNegativeLimitShowsEverything pins that a negative taskLimit
// (the --all sentinel) prints every task with no footer.
func TestRenderStatsNegativeLimitShowsEverything(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	s := &eventlog.Stats{
		Runs: 1, Workers: 1,
		TokensByTask: map[string]int64{"a": 300, "b": 200, "c": 100},
	}
	renderStats(cmd, s, -1)
	out := buf.String()
	for _, want := range []string{"a: 300", "b: 200", "c: 100"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected every task in output, missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "omitted") {
		t.Errorf("no footer expected when every task is shown:\n%s", out)
	}
}

// TestTruncateTaskLabel pins the ellipsis-truncation contract: bounded width,
// single line, and an unambiguous "…" marker on cut so a task's own colon
// can never be mistaken for the truncation point.
func TestTruncateTaskLabel(t *testing.T) {
	cases := []struct {
		name  string
		task  string
		want  string
		width int
	}{
		{"under width passes through", "short task", "short task", 60},
		{"exact width passes through unmarked", strings.Repeat("x", 10), strings.Repeat("x", 10), 10},
		{"over width gets ellipsis", strings.Repeat("x", 11), strings.Repeat("x", 10) + "…", 10},
		{"first line only", "line one\nline two", "line one", 60},
		{"trims surrounding whitespace", "  padded  ", "padded", 60},
		{
			"task text containing a colon still gets an unambiguous marker",
			"Fix Elysium-Labs-EU/argus issue #161: Add a minimal declarative thing",
			"Fix Elysium-Labs-EU/argus issue #161: Ad" + "…",
			40,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateTaskLabel(c.task, c.width)
			if got != c.want {
				t.Errorf("truncateTaskLabel(%q, %d) = %q, want %q", c.task, c.width, got, c.want)
			}
		})
	}
}
