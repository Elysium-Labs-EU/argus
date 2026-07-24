package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// renderPlan prints what a real run would do, without doing it. This is the
// dry-run surface: worktrees to create, the settings each worker gets, and the
// brief that will be injected.
//
// It always prints the plain SpawnCommand line, never LaunchViaRuntime's
// output — dry-run's contract is "makes no changes," so it must not exec an
// adapter subprocess just to print a preview line. When a runtime adapter is
// configured, it instead appends a note that the real spawn will be wrapped.
func renderPlan(out io.Writer, base, launcher, workerRuntime string, scrubEnv []string, plans []WorkerPlan) {
	_, _ = fmt.Fprintf(out, "%s supervise plan — %d worker(s), base %s\n\n",
		ui.LabelInfo.Render("i"), len(plans), base)

	for i := range plans {
		p := &plans[i]
		pane := p.PaneID
		if pane == "" {
			pane = ui.TextMuted.Render("(new split)")
		}
		_, _ = fmt.Fprintf(out, "%s %s\n", ui.TextBold.Render("•"), ui.TextBold.Render(p.Task))
		_, _ = fmt.Fprintf(out, "  branch:   %s\n", p.Branch)
		_, _ = fmt.Fprintf(out, "  label:    %s\n", p.Label)
		_, _ = fmt.Fprintf(out, "  worktree: %s\n", p.Worktree)
		_, _ = fmt.Fprintf(out, "  pane:     %s\n", pane)
		spawn := SpawnCommand(p.Worktree, launcher, scrubEnv, nil)
		if workerRuntime != "" && workerRuntime != "none" {
			_, _ = fmt.Fprintf(out, "  spawn:    %s %s\n", ui.TextCommand.Render(spawn),
				ui.TextMuted.Render(fmt.Sprintf("(wrapped by runtime adapter: %s)", workerRuntime)))
		} else {
			_, _ = fmt.Fprintf(out, "  spawn:    %s\n", ui.TextCommand.Render(spawn))
		}

		settings, _ := json.MarshalIndent(p.Settings, "    ", "  ")
		_, _ = fmt.Fprintf(out, "  settings.local.json:\n    %s\n", settings)
		_, _ = fmt.Fprintf(out, "  brief:\n%s\n\n", indent(p.Brief, "    "))
	}

	_, _ = fmt.Fprintf(out, "%s dry run — nothing was created or spawned\n", ui.TextMuted.Render("i"))
}

// renderReport prints the terminal metrics table: per worker its final phase,
// diff size, tests, whether it reached success, wall time, and token spend. The
// token total is the black-box measurement a future deterministic-review cut is
// compared against.
func renderReport(ctx context.Context, cfg *Config, states []*workerState) {
	out := cfg.Out
	_, _ = fmt.Fprintf(out, "\n%s supervise report\n\n", ui.LabelInfo.Render("i"))

	panes, err := cfg.Client.PaneList(ctx)
	sessionByPane := map[string]string{}
	if err == nil {
		for i := range panes {
			sessionByPane[panes[i].PaneID] = panes[i].AgentSession.Value
		}
	}

	for _, st := range states {
		phase := protocol.Phase("no report")
		if st.hasFile {
			phase = st.status.Phase
		}
		success := st.hasFile && st.status.Phase == protocol.PhaseDone
		mark := ui.LabelWarning.Render("○")
		if success {
			mark = ui.LabelSuccess.Render("✓")
		} else if phase == protocol.PhaseBlocked || st.herdrEscalation != "" {
			mark = ui.LabelError.Render("✗")
		}

		wall := elapsed(st.started, cfg.Now())
		passed, total := testCounts(&st.status)

		// Show the measured diff (ground truth) when we have it; fall back to the
		// worker's self-report only when the measurement failed. Tests remain
		// self-reported and are labeled as a worker claim.
		diff := st.status.DiffStat
		diffSrc := "reported"
		if st.measuredOK {
			diff = st.measured
			diffSrc = "measured"
		}
		_, _ = fmt.Fprintf(out, "%s %s  [%s]\n", mark, ui.TextBold.Render(st.plan.Task), phase)
		_, _ = fmt.Fprintf(out, "    diff (%s): %d file(s) +%d/-%d   tests (reported): %d/%d passed   wall: %s\n",
			diffSrc, diff.Files, diff.Insertions, diff.Deletions, passed, total, wall)
		if st.diffErr != nil {
			_, _ = fmt.Fprintf(out, "    %s could not measure diff: %v\n", ui.LabelWarning.Render("○"), st.diffErr)
		}

		_, _ = fmt.Fprintf(out, "    tokens: %s\n", reportTokens(cfg, st, sessionByPane[st.paneID]))

		if st.hasFile || st.herdrEscalation != "" {
			renderVerdict(out, gateVerdict(st, cfg.Policy))
			renderReview(out, st)
		}
		if st.hasFile && st.status.PRURL != "" {
			_, _ = fmt.Fprintf(out, "    pr: %s\n", st.status.PRURL)
		}
	}

	logRunSummary(cfg, states)
}

// reportTokens formats a worker's token spend for the terminal and, when the
// spend is known, logs a tokens event carrying the components and session id so
// `argus stats` can total tokens per task from the run log alone.
func reportTokens(cfg *Config, st *workerState, sessionID string) string {
	usage, known, err := TokensForSession(cfg.Home, sessionID)
	if err != nil || !known {
		return ui.TextMuted.Render("unknown")
	}
	cfg.Log.Emit(&eventlog.Event{
		Action: "tokens",
		Target: taskLabel(st.plan.Task),
		Fields: map[string]any{
			"total":          usage.Total(),
			"input":          usage.Input,
			"output":         usage.Output,
			"cache_creation": usage.CacheCreation,
			"cache_read":     usage.CacheRead,
			"session":        sessionID,
		},
	})
	return fmt.Sprintf("%d total (in %d, out %d, cache-create %d, cache-read %d)",
		usage.Total(), usage.Input, usage.Output, usage.CacheCreation, usage.CacheRead)
}

// logRunSummary records one run-level event: how many workers, how many the gate
// escalated, and how many argus approved. It is the per-run row `argus stats`
// aggregates across runs.
func logRunSummary(cfg *Config, states []*workerState) {
	workers, reported, escalated, approved := 0, 0, 0, 0
	for _, st := range states {
		workers++
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		reported++
		v := gateVerdict(st, cfg.Policy)
		if v.AutoApprove || (st.review != nil && st.review.Decision == "approve") {
			approved++
		}
		if !v.AutoApprove {
			escalated++
		}
	}
	cfg.Log.Emit(&eventlog.Event{
		Action:  "run_summary",
		Outcome: fmt.Sprintf("%d/%d approved", approved, workers),
		Fields: map[string]any{
			"workers":   workers,
			"reported":  reported,
			"escalated": escalated,
			"approved":  approved,
		},
	})
}

// renderReview prints the LLM reviewer's verdict when the gate escalated and a
// reviewer ran. Nothing prints for auto-approved workers (no reviewer call) or
// when no reviewer was configured.
func renderReview(out io.Writer, st *workerState) {
	if st.reviewErr != nil {
		_, _ = fmt.Fprintf(out, "    review: %s reviewer error: %v\n", ui.LabelError.Render("✗"), st.reviewErr)
		return
	}
	if st.review == nil {
		return
	}
	mark := ui.LabelWarning.Render("○")
	switch st.review.Decision {
	case "approve":
		mark = ui.LabelSuccess.Render("✓")
	case "request-changes":
		mark = ui.LabelError.Render("✗")
	}
	_, _ = fmt.Fprintf(out, "    review: %s %s — %s\n", mark, st.review.Decision, st.review.Summary)
	for _, f := range st.review.Findings {
		_, _ = fmt.Fprintf(out, "      · %s\n", f)
	}
}

// renderVerdict prints the gate's decision: a one-line auto-approve, or the list
// of reasons the change needs review. This is where the deterministic gate hands
// off to a human (and, in Milestone B, to claude -p).
func renderVerdict(out io.Writer, v Verdict) {
	if v.AutoApprove {
		_, _ = fmt.Fprintf(out, "    gate: %s auto-approve (no review needed)\n", ui.LabelSuccess.Render("✓"))
		return
	}
	_, _ = fmt.Fprintf(out, "    gate: %s needs review\n", ui.LabelWarning.Render("○"))
	for _, r := range v.Reasons {
		_, _ = fmt.Fprintf(out, "      - %s\n", r)
	}
}

func testCounts(s *protocol.Status) (passed, total int) {
	for i := range s.Tests {
		total++
		if s.Tests[i].Result == protocol.ResultPass {
			passed++
		}
	}
	return passed, total
}

func indent(text, prefix string) string {
	var out strings.Builder
	out.WriteString(prefix)
	for _, r := range text {
		out.WriteRune(r)
		if r == '\n' {
			out.WriteString(prefix)
		}
	}
	return out.String()
}
