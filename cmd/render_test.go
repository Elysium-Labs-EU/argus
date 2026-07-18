package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"codeberg.org/Elysium_Labs/argus/internal/eventlog"
	"codeberg.org/Elysium_Labs/argus/internal/protocol"
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

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]int{"b": 1, "a": 2, "c": 3})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys not sorted: %v", got)
	}
}
