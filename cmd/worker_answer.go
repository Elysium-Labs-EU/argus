package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newWorkerAnswerCmd() *cobra.Command {
	var (
		option            int
		owner             string
		forceForeignOwner bool
		ownerStaleAfter   time.Duration
	)

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
			of := ownerFlags{
				owner: owner, forceForeignOwner: forceForeignOwner,
				ownerStaleAfter: ownerStaleAfter, ownerStaleAfterExplicit: cmd.Flags().Changed("owner-stale-after"),
			}
			return runWorkerAnswer(cmd, herdr.New(), logger, args[0], text, option, of, time.Now)
		},
	}

	cmd.Flags().IntVar(&option, "option", 0, "1-indexed choice from the worker's reported question options, instead of free-form TEXT")
	cmd.Flags().StringVar(&owner, "owner", "", ownerFlagHelp)
	cmd.Flags().BoolVar(&forceForeignOwner, "force-foreign-owner", false, forceForeignOwnerFlagHelp)
	cmd.Flags().DurationVar(&ownerStaleAfter, "owner-stale-after", ownership.DefaultStaleAfter, ownerStaleAfterFlagHelp)
	return cmd
}

// runWorkerAnswer is newWorkerAnswerCmd's RunE body: it validates the target
// worker is actually blocked, resolves the answer text, persists it as a
// durable Question/Answer pair on status.json, then delivers it into the
// worker's live pane. Split out of the RunE closure so it is directly
// testable without cobra flag parsing, mirroring runWorkerReport.
func runWorkerAnswer(cmd *cobra.Command, client herdr.Client, logger *eventlog.Logger, worktree, text string, option int, of ownerFlags, now func() time.Time) error {
	if worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus worker answer <worktree> <text>"}
	}
	abs, err := supervisor.ResolveWorktree(worktree)
	if err != nil {
		return err
	}
	worktree = abs
	if oerr := enforceOwnership(cmd.Context(), cmd.OutOrStdout(), worktree, of, now()); oerr != nil {
		return oerr
	}

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
	if err := deliverPaneMessage(ctx, logger, client, wt.RootPaneID, worktree, "answer", message); err != nil {
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
