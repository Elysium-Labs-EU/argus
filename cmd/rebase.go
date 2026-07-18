package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"codeberg.org/Elysium_Labs/argus/internal/forge"
	"codeberg.org/Elysium_Labs/argus/internal/herdr"
	"codeberg.org/Elysium_Labs/argus/internal/protocol"
	"codeberg.org/Elysium_Labs/argus/internal/supervisor"
	"codeberg.org/Elysium_Labs/argus/internal/ui"
)

func newRebaseCmd() *cobra.Command {
	var (
		worktree string
		base     string
		launcher string
		interval time.Duration
		force    bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "rebase",
		Short: "Dispatch a worker to rebase a conflicting branch onto its base",
		Long: `Rebase handles the post-merge conflict handoff: when a sibling PR merges first
and leaves a worktree's branch conflicting, argus fetches the base, confirms the
conflict, and dispatches the worktree's own worker to rebase, resolve, re-verify,
and force-push. argus does the deterministic parts (detect, dispatch, wait); the
conflict resolution itself needs the worker.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if worktree == "" {
				return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus rebase --worktree <path>"}
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			branch, err := supervisor.CurrentBranch(ctx, worktree)
			if err != nil {
				return err
			}
			if ferr := supervisor.FetchBase(ctx, worktree, base); ferr != nil {
				return ferr
			}
			conflicts, err := supervisor.ConflictsWith(ctx, worktree, base)
			if err != nil {
				return err
			}

			logger, closeLog := openRunLog(cmd, "rebase")
			defer closeLog()
			logger.Action("conflict_check", branch, fmt.Sprintf("conflicts=%v", conflicts), base)

			if !conflicts && !force {
				_, _ = fmt.Fprintf(out, "%s %s has no conflict with origin/%s — nothing to rebase (use --force to dispatch anyway)\n",
					ui.LabelSuccess.Render("✓"), branch, base)
				return nil
			}

			if dryRun {
				_, _ = fmt.Fprintf(out, "%s rebase plan (dry run)\n  worktree: %s\n  branch:   %s -> origin/%s\n  conflicts: %v\n  action:   dispatch worker to rebase + force-push\n",
					ui.LabelInfo.Render("i"), worktree, branch, base, conflicts)
				return nil
			}

			client := herdr.New()
			wt, err := client.WorktreeOpen(ctx, worktree)
			if err != nil {
				return err
			}
			if wt.RootPaneID == "" {
				return &ui.UserError{Err: fmt.Errorf("herdr opened no pane for %s", worktree)}
			}
			if err := supervisor.WriteBrief(worktree, supervisor.RebaseBrief(branch, base)); err != nil {
				return err
			}
			if err := client.PaneRun(ctx, wt.RootPaneID, supervisor.SpawnCommand(worktree, launcher, forge.StandardTokenVars())); err != nil {
				return err
			}

			_, _ = fmt.Fprintf(out, "%s dispatched rebase worker in pane %s; waiting...\n", ui.LabelInfo.Render("i"), wt.RootPaneID)
			status, seen := supervisor.WaitForStatus(ctx, worktree, interval)
			if !seen {
				logger.Action("rebase", branch, "no-status", "")
				return fmt.Errorf("worker wrote no status before the deadline")
			}
			logger.Action("rebase", branch, string(status.Phase), status.BlockedReason)
			renderRebaseOutcome(out, branch, &status)
			return nil
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree whose branch to rebase")
	cmd.Flags().StringVar(&base, "base", "main", "base branch to rebase onto")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in the worker pane")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "status poll cadence")
	cmd.Flags().BoolVar(&force, "force", false, "dispatch a rebase even if no conflict is detected")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "detect and print the plan without dispatching a worker")
	return cmd
}

var rebaseCmd = newRebaseCmd()

func renderRebaseOutcome(out io.Writer, branch string, status *protocol.Status) {
	switch status.Phase {
	case protocol.PhaseAwaitingReview, protocol.PhaseDone:
		_, _ = fmt.Fprintf(out, "%s %s rebased and ready (%s)\n", ui.LabelSuccess.Render("✓"), branch, status.Phase)
	case protocol.PhaseBlocked:
		_, _ = fmt.Fprintf(out, "%s %s rebase blocked: %s\n", ui.LabelError.Render("✗"), branch, status.BlockedReason)
	case protocol.PhasePlanning, protocol.PhaseWorking, protocol.PhaseSelfTest:
		_, _ = fmt.Fprintf(out, "%s %s rebase still %s\n", ui.LabelWarning.Render("○"), branch, status.Phase)
	default:
		_, _ = fmt.Fprintf(out, "%s %s rebase phase %s\n", ui.LabelWarning.Render("○"), branch, status.Phase)
	}
}
