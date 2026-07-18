package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
	"codeberg.org/Elysium_Labs/argus/internal/ui"
)

// renderPlan prints what a real run would do, without doing it. This is the
// dry-run surface: worktrees to create, the settings each worker gets, and the
// brief that will be injected.
func renderPlan(out io.Writer, base, launcher string, plans []WorkerPlan) {
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
		_, _ = fmt.Fprintf(out, "  worktree: %s\n", p.Worktree)
		_, _ = fmt.Fprintf(out, "  pane:     %s\n", pane)
		_, _ = fmt.Fprintf(out, "  spawn:    %s\n", ui.TextCommand.Render(spawnCommand(p.Worktree, launcher)))

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
		} else if phase == protocol.PhaseBlocked {
			mark = ui.LabelError.Render("✗")
		}

		wall := elapsed(st.started, cfg.Now())
		passed, total := testCounts(&st.status)

		_, _ = fmt.Fprintf(out, "%s %s  [%s]\n", mark, ui.TextBold.Render(st.plan.Task), phase)
		_, _ = fmt.Fprintf(out, "    diff: %d file(s) +%d/-%d   tests: %d/%d passed   wall: %s\n",
			st.status.DiffStat.Files, st.status.DiffStat.Insertions, st.status.DiffStat.Deletions,
			passed, total, wall)

		tokenLine := renderTokens(cfg.Home, sessionByPane[st.paneID])
		_, _ = fmt.Fprintf(out, "    tokens: %s\n", tokenLine)

		if st.hasFile {
			renderVerdict(out, Assess(&st.status, cfg.Policy))
			renderReview(out, st)
		}
		if st.hasFile && st.status.PRURL != "" {
			_, _ = fmt.Fprintf(out, "    pr: %s\n", st.status.PRURL)
		}
	}
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

func renderTokens(home, sessionID string) string {
	usage, known, err := TokensForSession(home, sessionID)
	if err != nil || !known {
		return ui.TextMuted.Render("unknown")
	}
	return fmt.Sprintf("%d total (in %d, out %d, cache-create %d, cache-read %d)",
		usage.Total(), usage.Input, usage.Output, usage.CacheCreation, usage.CacheRead)
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
	out := prefix
	for _, r := range text {
		out += string(r)
		if r == '\n' {
			out += prefix
		}
	}
	return out
}
