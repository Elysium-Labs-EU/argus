package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newReviewCmd() *cobra.Command {
	var (
		worktree    string
		base        string
		task        string
		reasons     []string
		reviewModel string
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a headless claude -p review of a worktree's diff",
		Long: `Review diffs a worktree against a base ref and asks a headless claude -p for a
verdict (approve / request-changes / needs-human). It is the manual counterpart
to supervise --review: the same scoped, one-shot review argus runs when its
deterministic gate escalates, pointed at any worktree on demand.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, closeLog := openRunLog(cmd, "review")
			defer closeLog()

			reviewer := supervisor.NewCLIReviewer(reviewModel).WithLog(logger)
			return runReview(cmd, worktree, base, task, reasons, reviewer, logger)
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree whose diff to review")
	cmd.Flags().StringVar(&base, "base", "origin/main", "base ref to diff against")
	cmd.Flags().StringVar(&task, "task", "", "task/issue the change addresses (context for the reviewer)")
	cmd.Flags().StringSliceVar(&reasons, "reasons", nil, "why this needs review (context for the reviewer)")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for the review (default: claude's default)")
	return cmd
}

var reviewCmd = newReviewCmd()

// runReview is newReviewCmd's RunE body, pulled out so tests can drive it
// directly with a fake supervisor.Reviewer instead of shelling out to claude.
func runReview(cmd *cobra.Command, worktree, base, task string, reasons []string, reviewer supervisor.Reviewer, logger *eventlog.Logger) error {
	if worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus review --worktree <path>"}
	}
	ctx := cmd.Context()
	diff, err := supervisor.DiffFor(ctx, worktree, base)
	if err != nil {
		return err
	}
	if diff == "" {
		return &ui.UserError{Err: fmt.Errorf("no diff between worktree and %s", base), Hint: "check --base"}
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "%s reviewing %s vs %s...\n", ui.LabelInfo.Render("i"), worktree, base)

	var res supervisor.ReviewResult
	err = ui.WithSpinner("claude reviewing...", func() error {
		var rerr error
		res, rerr = reviewer.Review(ctx, &supervisor.ReviewRequest{
			Task:     task,
			Worktree: worktree,
			Reasons:  reasons,
			Diff:     diff,
		})
		return rerr
	})
	if err != nil {
		logger.Fail("review", task, err)
		return err
	}
	logger.Action("review", task, res.Decision, res.Summary)

	renderReviewResult(out, res)
	return nil
}

// renderReviewResult prints a reviewer's verdict with a decision-colored mark.
func renderReviewResult(out io.Writer, res supervisor.ReviewResult) {
	mark := ui.LabelWarning.Render("○")
	switch res.Decision {
	case "approve":
		mark = ui.LabelSuccess.Render("✓")
	case "request-changes":
		mark = ui.LabelError.Render("✗")
	}
	_, _ = fmt.Fprintf(out, "\n%s %s — %s\n", mark, ui.TextBold.Render(res.Decision), res.Summary)
	for _, f := range res.Findings {
		_, _ = fmt.Fprintf(out, "  · %s\n", f)
	}
	_, _ = fmt.Fprintf(out, "\n%s this verdict is not saved, %s will not see it. Run %s against this worktree to ship.\n",
		ui.LabelWarning.Render("!"), ui.TextBold.Render("ship"), ui.TextBold.Render("supervise --attach --review"))
}
