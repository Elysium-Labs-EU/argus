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

		if st.hasFile && st.status.Phase == protocol.PhaseBlocked {
			renderBlocked(out, &st.status)
		}

		if st.hasFile || st.herdrEscalation != "" {
			v := gateVerdict(st, cfg.Policy)
			renderVerdict(out, &v)
			renderReview(out, st)
			renderProvenance(out, st.plan.Worktree)
		}
		if st.hasFile && st.status.PRURL != "" {
			_, _ = fmt.Fprintf(out, "    pr: %s\n", st.status.PRURL)
		}
	}

	blocked, blockedOnQuestion := blockedCounts(states)
	if blocked > 0 {
		_, _ = fmt.Fprintf(out, "\n%s %d worker(s) blocked (%d on a structured question, answerable via `argus worker answer`)\n",
			ui.LabelWarning.Render("!"), blocked, blockedOnQuestion)
	}

	logRunSummary(cfg, states, blocked, blockedOnQuestion)
}

// renderBlocked prints a blocked worker's reason, distinguishing a
// machine-readable Question (with its Options, so an operator sees exactly
// what `argus worker answer --option N` would pick) from a plain
// BlockedReason string — the two are no longer folded into one opaque line
// the way gateVerdict's reasons list still shows them.
func renderBlocked(out io.Writer, s *protocol.Status) {
	if s.Question != nil && s.Question.Text != "" {
		_, _ = fmt.Fprintf(out, "    blocked on question: %s\n", s.Question.Text)
		for i, opt := range s.Question.Options {
			_, _ = fmt.Fprintf(out, "      %d. %s\n", i+1, opt)
		}
		return
	}
	if s.BlockedReason != "" {
		_, _ = fmt.Fprintf(out, "    blocked: %s\n", s.BlockedReason)
	}
}

// blockedCounts tallies how many workers are sitting at phase blocked, and
// of those, how many carry a structured Question rather than only a
// freeform BlockedReason — the categories `argus supervise`'s report and
// run_summary event both need to surface blocked workers as their own kind
// of outcome, the same way gate escalations are already distinguished from
// clean auto-approves.
func blockedCounts(states []*workerState) (blocked, blockedOnQuestion int) {
	for _, st := range states {
		if !st.hasFile || st.status.Phase != protocol.PhaseBlocked {
			continue
		}
		blocked++
		if st.status.Question != nil {
			blockedOnQuestion++
		}
	}
	return blocked, blockedOnQuestion
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
// escalated, how many argus approved, and how many are sitting blocked (and of
// those, how many on a structured Question rather than only freeform prose).
// It is the per-run row `argus stats` aggregates across runs.
//
// "Approved" is read back from each worker's persisted verdict.json rather
// than recomputed from the gate verdict and review decision here: recordApproval
// (loop.go) is the one place a hard gate reason (e.g. a VerifyTests mismatch)
// forces approved=false even when the reviewer said "approve", and duplicating
// that logic here previously ignored HardReasons entirely — a worker rejected
// by an unwaivable check could still be counted approved as long as the
// reviewer text said so.
//
// blocked/blockedOnQuestion are the caller's own tally (see blockedCounts) —
// passed in rather than recomputed here so the terminal report line and this
// event always agree on the same numbers.
func logRunSummary(cfg *Config, states []*workerState, blocked, blockedOnQuestion int) {
	workers, reported, escalated, approved := 0, 0, 0, 0
	// Provenance counts split "approved" by who cleared it and surface how many
	// workers still need a human read, so the operator can trust the aggregate
	// the same way the per-worker report's provenance line lets them trust each
	// diff (see renderProvenance).
	gateApproved, reviewerApproved, awaitingHuman, reworkBudgetExceeded := 0, 0, 0, 0
	for _, st := range states {
		workers++
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		reported++
		v := gateVerdict(st, cfg.Policy)
		if !v.AutoApprove {
			escalated++
		}
		if a, found, err := protocol.LoadApproval(st.plan.Worktree); err == nil && found {
			if a.Approved {
				approved++
			}
			switch a.Provenance() {
			case protocol.ProvenanceGateApproved:
				gateApproved++
			case protocol.ProvenanceReviewerApproved:
				reviewerApproved++
			case protocol.ProvenanceAwaitingHuman:
				awaitingHuman++
			case protocol.ProvenanceReworkBudgetExceeded:
				reworkBudgetExceeded++
			}
		}
	}
	cfg.Log.Emit(&eventlog.Event{
		Action:  "run_summary",
		Outcome: fmt.Sprintf("%d/%d approved", approved, workers),
		Fields: map[string]any{
			"workers":                workers,
			"reported":               reported,
			"escalated":              escalated,
			"approved":               approved,
			"gate_approved":          gateApproved,
			"reviewer_approved":      reviewerApproved,
			"awaiting_human":         awaitingHuman,
			"rework_budget_exceeded": reworkBudgetExceeded,
			"blocked":                blocked,
			"blocked_on_question":    blockedOnQuestion,
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
func renderVerdict(out io.Writer, v *Verdict) {
	if v.AutoApprove {
		_, _ = fmt.Fprintf(out, "    gate: %s auto-approve (no review needed)\n", ui.LabelSuccess.Render("✓"))
		renderNotes(out, v.Notes)
		return
	}
	_, _ = fmt.Fprintf(out, "    gate: %s needs review\n", ui.LabelWarning.Render("○"))
	for _, r := range v.Reasons {
		_, _ = fmt.Fprintf(out, "      - %s\n", r)
	}
	renderNotes(out, v.Notes)
}

// renderNotes prints the gate's informational call-outs (e.g. an intentional
// test failure it did not escalate on) below the verdict line, so a reviewer
// sees they happened without them reading as an escalation reason.
func renderNotes(out io.Writer, notes []string) {
	for _, n := range notes {
		_, _ = fmt.Fprintf(out, "      · %s\n", n)
	}
}

// renderProvenance prints the one line that closes the verify-once loop: which of
// the three approval sources cleared this worker, and whether the operator still
// needs to hand-read its diff. It reads the persisted verdict.json rather than
// recomputing from the gate verdict here, because that file — written by
// recordApproval — is the only place a reviewer's actual decision and an
// unwaivable hard-reason override are both already folded in; renderVerdict's
// recomputed gate verdict knows neither. A missing verdict (no report reached a
// terminal phase) prints nothing, same as renderReview's no-op.
func renderProvenance(out io.Writer, worktree string) {
	a, found, err := protocol.LoadApproval(worktree)
	if err != nil || !found {
		return
	}
	switch a.Provenance() {
	case protocol.ProvenanceGateApproved:
		_, _ = fmt.Fprintf(out, "    approval: %s gate-auto-approved — verified by the gate, no human read needed before ship\n",
			ui.LabelSuccess.Render("✓"))
	case protocol.ProvenanceReviewerApproved:
		_, _ = fmt.Fprintf(out, "    approval: %s reviewer-approved — verified by the review, no human read needed before ship\n",
			ui.LabelSuccess.Render("✓"))
	case protocol.ProvenanceAwaitingHuman:
		_, _ = fmt.Fprintf(out, "    approval: %s surfaced-awaiting-human — hand-read this diff and decide\n",
			ui.LabelWarning.Render("○"))
	case protocol.ProvenanceReworkBudgetExceeded:
		_, _ = fmt.Fprintf(out, "    approval: %s %s — %s\n",
			ui.LabelError.Render("✗"), protocol.ProvenanceReworkBudgetExceeded, a.Summary)
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
