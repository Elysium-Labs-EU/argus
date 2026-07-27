package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newWorkerAnswerCmd() *cobra.Command {
	var option int

	cmd := &cobra.Command{
		Use:   "answer <worktree> [text]",
		Short: "Deliver a supervisor's answer to a worker blocked on a question",
		Long: `Answer is the resolution half of a blocked worker's structured question: it
records the question/answer pair into the worktree's status.json (a durable
trace argus itself can read back — see protocol.Question/protocol.Answer),
then delivers the answer as a chat message into the worker's live pane so it
can resume from it instead of a human free-typing into herdr by hand.

<worktree> must currently be reporting phase "blocked". The answer is either
a free-form TEXT argument or --option N picking one of the choices the
worker listed in its own reported question (1-indexed) — not both.

This does not itself change the worker's reported phase: the worker still
reports its next phase (working, self_test, ...) once it acts on the answer,
the same as any other report.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := ""
			if len(args) > 1 {
				text = args[1]
			}
			logger, closeLog := openRunLog(cmd, "worker-answer")
			defer closeLog()
			return runWorkerAnswer(cmd, herdr.New(), logger, args[0], text, option, time.Now)
		},
	}

	cmd.Flags().IntVar(&option, "option", 0, "1-indexed choice from the worker's reported question options, instead of free-form TEXT")
	return cmd
}

// runWorkerAnswer is newWorkerAnswerCmd's RunE body: it validates the target
// worker is actually blocked, resolves the answer text, persists it as a
// durable Question/Answer pair on status.json, then delivers it into the
// worker's live pane. Split out of the RunE closure so it is directly
// testable without cobra flag parsing, mirroring runWorkerReport.
func runWorkerAnswer(cmd *cobra.Command, client herdr.Client, logger *eventlog.Logger, worktree, text string, option int, now func() time.Time) error {
	if worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus worker answer <worktree> <text>"}
	}
	abs, err := supervisor.ResolveWorktree(worktree)
	if err != nil {
		return err
	}
	worktree = abs

	cur, err := protocol.Load(protocol.StatusPath(worktree))
	if err != nil {
		return &ui.UserError{
			Err:  fmt.Errorf("loading status for %s: %w", worktree, err),
			Hint: "the worker must have reported at least once before it can be answered",
		}
	}
	if cur.Phase != protocol.PhaseBlocked {
		return &ui.UserError{
			Err:  fmt.Errorf("%s is not blocked (phase %q)", worktree, cur.Phase),
			Hint: "worker answer only applies to a worker currently reporting phase \"blocked\"",
		}
	}

	answerText, err := resolveAnswerText(cur.Question, text, option)
	if err != nil {
		return err
	}

	cur.Answer = &protocol.Answer{Text: answerText, Option: option, AnsweredAt: now()}
	cur.UpdatedAt = now()
	if werr := protocol.Write(protocol.StatusPath(worktree), &cur); werr != nil {
		return fmt.Errorf("recording answer for %s: %w", worktree, werr)
	}

	ctx := cmd.Context()
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return fmt.Errorf("answer recorded, but resolving repo root for %s to deliver it: %w", worktree, err)
	}
	wt, err := client.WorktreeOpen(ctx, repoRoot, worktree)
	if err != nil {
		return fmt.Errorf("answer recorded, but could not open %s's pane to deliver it: %w", worktree, err)
	}
	if wt.RootPaneID == "" {
		return fmt.Errorf("answer recorded, but herdr opened no pane for %s to deliver it to", worktree)
	}

	message := supervisor.AnswerMessage(cur.Question, cur.BlockedReason, answerText)
	if err := deliverAnswerToPane(ctx, logger, client, wt.RootPaneID, worktree, message); err != nil {
		return fmt.Errorf("answer recorded, but delivery failed: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s answer recorded and delivered to %s\n", ui.LabelSuccess.Render("✓"), worktree)
	return nil
}

// resolveAnswerText turns a worker answer command's TEXT/--option input into
// the answer string to record and deliver, validating that exactly one of
// the two was given and that --option, when given, indexes a real choice on
// the worker's own reported question.
func resolveAnswerText(q *protocol.Question, text string, option int) (string, error) {
	if option != 0 && text != "" {
		return "", &ui.UserError{
			Err:  fmt.Errorf("both a TEXT argument and --option were given"),
			Hint: "pass either free-form text or --option N, not both",
		}
	}
	if option != 0 {
		if q == nil || len(q.Options) == 0 {
			return "", &ui.UserError{Err: fmt.Errorf("worker's question has no options to choose --option %d from", option)}
		}
		if option < 1 || option > len(q.Options) {
			return "", &ui.UserError{
				Err:  fmt.Errorf("--option %d out of range", option),
				Hint: fmt.Sprintf("worker's question has %d option(s), pass 1..%d", len(q.Options), len(q.Options)),
			}
		}
		return q.Options[option-1], nil
	}
	if text == "" {
		return "", &ui.UserError{
			Err:  fmt.Errorf("no answer given"),
			Hint: "argus worker answer <worktree> <text>, or --option N",
		}
	}
	return text, nil
}

// deliverAnswerToPane submits message into paneID's live agent so a blocked
// worker can resume from a supervisor's answer without a human free-typing
// into herdr by hand. Unlike dispatchIntoPane, there is no spawn-a-fresh-
// agent fallback: a pane with no live agent means the original worker's
// session is gone, and starting a brand-new one with no context beyond this
// one message would not actually resume the blocked task — the operator
// needs `argus rework`/`argus rebase` instead, which re-dispatch with a full
// brief.
func deliverAnswerToPane(ctx context.Context, logger *eventlog.Logger, client herdr.Client, paneID, worktree, message string) error {
	_, live, err := client.AgentGet(ctx, paneID)
	if err != nil {
		return fmt.Errorf("checking whether pane %s has a live agent: %w", paneID, err)
	}
	if !live {
		return fmt.Errorf("pane %s has no live agent — the worker's session is gone", paneID)
	}

	timeout := defaultLivenessTimeout
	perr := client.AgentPrompt(ctx, paneID, message, timeout)
	if perr == nil {
		logger.Action("answer", worktree, "delivered", paneID)
		return nil
	}
	if !errors.Is(perr, herdr.ErrAgentPromptStalled) {
		return fmt.Errorf("delivering answer to pane %s: %w", paneID, perr)
	}

	logger.Action("answer", worktree, "prompt-stalled-fallback-pane-run", paneID)
	if rerr := client.PaneRun(ctx, paneID, message); rerr != nil {
		return fmt.Errorf("delivering answer to pane %s: %w (pane-run fallback also failed: %w)", paneID, perr, rerr)
	}
	if kerr := client.PaneSendKeys(ctx, paneID, "enter"); kerr != nil {
		return fmt.Errorf("delivering answer to pane %s: %w (pane-run fallback's submit keystroke failed: %w)", paneID, perr, kerr)
	}
	if _, werr := client.AgentWait(ctx, paneID, []string{"working"}, timeout); werr != nil {
		return fmt.Errorf("delivering answer to pane %s: %w (pane-run fallback sent, but agent never started working: %w)", paneID, perr, werr)
	}
	logger.Action("answer", worktree, "delivered-via-fallback", paneID)
	return nil
}
