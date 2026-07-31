package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newReviewCmd() *cobra.Command {
	var (
		worktree     string
		base         string
		task         string
		reasons      []string
		reviewModel  string
		reviewEffort string
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

			reviewer := newReviewer(reviewModel, reviewEffort, logger)
			return runReview(cmd, worktree, base, task, reasons, reviewer, logger)
		},
	}

	bindWorktreeFlag(cmd, &worktree, "worktree whose diff to review")
	bindBaseFlag(cmd, &base, "origin/main", "base ref to diff against")
	cmd.Flags().StringVar(&task, "task", "", "task/issue the change addresses (context for the reviewer)")
	cmd.Flags().StringSliceVar(&reasons, "reasons", nil, "why this needs review (context for the reviewer)")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for the review (default: claude's default)")
	cmd.Flags().StringVar(&reviewEffort, "review-effort", "", "reasoning effort for the review (low, medium, high, xhigh, max; default: claude's default)")
	return cmd
}

var reviewCmd = newReviewCmd()

// newReviewer builds the reviewer newReviewCmd's RunE hands runReview. It is a
// var, not a plain call, so a test driving the command through cmd.SetArgs +
// cmd.Execute (rather than calling runReview directly) can substitute a fake
// Reviewer without shelling out to the real claude CLI.
var newReviewer = func(model, effort string, logger *eventlog.Logger) supervisor.Reviewer {
	return supervisor.NewCLIReviewer(model, effort).WithLog(logger)
}

// runReview is newReviewCmd's RunE body, pulled out so tests can drive it
// directly with a fake supervisor.Reviewer instead of shelling out to claude.
func runReview(cmd *cobra.Command, worktree, base, task string, reasons []string, reviewer supervisor.Reviewer, logger *eventlog.Logger) error {
	if worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus review --worktree <path>"}
	}
	// See supervisor.ResolveWorktree: a --worktree given relative to argus's
	// own cwd must be resolved before it reaches DiffFor's git -C call or the
	// ReviewRequest handed to the reviewer, so every downstream use agrees on
	// the same absolute path.
	resolved, err := supervisor.ResolveWorktree(worktree)
	if err != nil {
		return err
	}
	worktree = resolved
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
			Task:          task,
			Worktree:      worktree,
			Reasons:       reasons,
			Diff:          diff,
			PriorFindings: priorFindings(worktree),
			ReviewNote:    repoReviewNote(ctx, worktree),
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

// repoReviewNote reads this worktree's repo's optional .argus/config.yml
// review_note (see internal/repoconfig), best-effort: an unresolvable repo
// root or unreadable config just means no repo-specific criteria to append,
// not a hard failure of a manual one-off review.
func repoReviewNote(ctx context.Context, worktree string) string {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return ""
	}
	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return ""
	}
	return rc.ReviewNote
}

// priorFindings returns the Reasons from a previously recorded, non-approved
// verdict for worktree, or nil if none exists (first review, or the prior
// round already approved).
func priorFindings(worktree string) []string {
	prior, found, err := protocol.LoadApproval(worktree)
	if err != nil || !found || prior.Approved {
		return nil
	}
	return prior.Reasons
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
