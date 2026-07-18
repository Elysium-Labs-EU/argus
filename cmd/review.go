package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"codeberg.org/Elysium_Labs/argus/internal/supervisor"
	"codeberg.org/Elysium_Labs/argus/internal/ui"
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

			reviewer := supervisor.NewCLIReviewer(reviewModel)
			var res supervisor.ReviewResult
			err = ui.WithSpinner("claude reviewing...", func() error {
				var rerr error
				res, rerr = reviewer.Review(ctx, &supervisor.ReviewRequest{
					Task:    task,
					Reasons: reasons,
					Diff:    diff,
				})
				return rerr
			})
			if err != nil {
				return err
			}

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
			return nil
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
