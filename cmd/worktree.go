package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage a worktree's post-ship lifecycle",
	}
	cmd.AddCommand(worktreePruneCmd)
	return cmd
}

var worktreeCmd = newWorktreeCmd()

// worktreePruneCmd is a package-level var (like shipCmd, rebaseCmd, ...) so
// skill_lint_test.go can cross-check its flags against SKILL.md directly,
// the same way it already does for every other documented subcommand.
var worktreePruneCmd = newWorktreePruneCmd()

func newWorktreePruneCmd() *cobra.Command {
	var (
		repo          string
		branch        string
		merged        bool
		dryRun        bool
		credentialEnv map[string]string
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Clean up worktrees whose PR has merged",
		Long: `Prune is ship's counterpart at the other end of a worktree's life: once the PR
it opened has merged, the worktree that produced it is dead weight. For each
candidate worktree, prune deterministically checks (no LLM) whether the forge
reports its PR merged, and whether the working directory is otherwise safe to
remove — no uncommitted changes, no unpushed commits, no stash entries. This
is the same safe-majority-auto, risky-minority-escalate split as the review
gate: a safe worktree is cleaned automatically (a recoverable relocation,
never a raw rm), and anything else is reported with the specific reason and
left alone. Use --branch to target one worktree or --merged to sweep every
worktree under the repo.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}
			return runWorktreePrune(cmd, &worktreePruneArgs{
				repo: repo, branch: branch, merged: merged, dryRun: dryRun, credentialEnv: overrides,
			})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose worktrees to inspect")
	cmd.Flags().StringVar(&branch, "branch", "", "prune only the worktree for this branch")
	cmd.Flags().BoolVar(&merged, "merged", false, "sweep every worktree under the repo, not just one branch")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan (which worktrees, which check failed/passed) without deleting anything")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	return cmd
}

// worktreePruneArgs holds newWorktreePruneCmd's flag values so runWorktreePrune
// can be tested directly, without going through cobra flag parsing.
type worktreePruneArgs struct {
	credentialEnv  map[string]string
	repo, branch   string
	merged, dryRun bool
}

// runWorktreePrune is newWorktreePruneCmd's RunE body, extracted so the
// decision logic is independently testable.
func runWorktreePrune(cmd *cobra.Command, a *worktreePruneArgs) error {
	if a.branch == "" && !a.merged {
		return &ui.UserError{Err: fmt.Errorf("no target given"), Hint: "argus worktree prune --branch <name>, or --merged to sweep every worktree"}
	}
	if a.branch != "" && a.merged {
		return &ui.UserError{Err: fmt.Errorf("--branch and --merged are mutually exclusive")}
	}

	ctx := cmd.Context()
	resolvedRepo, err := supervisor.ResolveWorktree(a.repo)
	if err != nil {
		return err
	}
	repoRoot, err := supervisor.RepoRoot(ctx, resolvedRepo)
	if err != nil {
		return fmt.Errorf("resolving repo root for %s: %w", resolvedRepo, err)
	}

	entries, err := supervisor.ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		return err
	}
	if a.branch != "" {
		entries = filterByBranch(entries, a.branch)
		if len(entries) == 0 {
			return &ui.UserError{Err: fmt.Errorf("no worktree found for branch %q under %s", a.branch, repoRoot)}
		}
	}

	host, owner, name, err := resolveRepo(ctx, "", repoRoot)
	if err != nil {
		return err
	}
	f := forge.New(host, forge.TokenForHost(host, a.credentialEnv), nil)

	logger, closeLog := openRunLog(cmd, "worktree_prune")
	defer closeLog()

	prunePlan(cmd, ctx, logger, f, herdr.New(), owner, name, repoRoot, entries, a.dryRun)
	return nil
}

func filterByBranch(entries []supervisor.WorktreeEntry, branch string) []supervisor.WorktreeEntry {
	for _, e := range entries {
		if e.Branch == branch {
			return []supervisor.WorktreeEntry{e}
		}
	}
	return nil
}

// prunePlan evaluates every entry and, outside --dry-run, cleans the safe
// ones — split out of runWorktreePrune so the per-entry loop is independently
// testable against a fake forge.Forge without cobra or a real git repo. A
// per-candidate failure (forge lookup, clean) is reported and logged, not
// returned: one worktree argus can't evaluate must never abort a --merged
// sweep of the rest.
func prunePlan(cmd *cobra.Command, ctx context.Context, logger *eventlog.Logger, f forge.Forge, client herdr.Client, owner, name, repoRoot string, entries []supervisor.WorktreeEntry, dryRun bool) {
	out := cmd.OutOrStdout()
	var cleaned, unsafe int
	for _, e := range entries {
		if e.Branch == "" {
			continue // bare or detached-HEAD worktree: not a per-worker worktree, not a candidate
		}
		c, err := supervisor.EvaluateCandidate(ctx, f, owner, name, repoRoot, e.Path, e.Branch, e.Prunable, dryRun)
		if err != nil {
			logger.Fail("evaluate", e.Branch, err)
			_, _ = fmt.Fprintf(out, "%s %s: %v\n", ui.LabelError.Render("✗"), e.Branch, err)
			continue
		}
		renderCandidate(out, c)
		if !c.SafeToClean {
			unsafe++
			continue
		}
		if dryRun {
			if c.PaneID != "" {
				_, _ = fmt.Fprintf(out, "  (dry run) would relocate %s, remove its worktree registration, and close herdr pane %s\n", c.Path, c.PaneID)
			} else {
				_, _ = fmt.Fprintf(out, "  (dry run) would relocate %s and remove its worktree registration\n", c.Path)
			}
			cleaned++
			continue
		}
		dest, paneWarning, cerr := supervisor.CleanWorktree(ctx, repoRoot, client, c)
		if cerr != nil {
			logger.Fail("clean", e.Branch, cerr)
			_, _ = fmt.Fprintf(out, "%s cleaning %s: %v\n", ui.LabelError.Render("✗"), e.Branch, cerr)
			continue
		}
		logger.Action("clean", e.Branch, "ok", dest)
		if dest != "" {
			_, _ = fmt.Fprintf(out, "  %s relocated to %s, worktree registration removed\n", ui.LabelSuccess.Render("✓"), dest)
		} else {
			_, _ = fmt.Fprintf(out, "  %s worktree registration removed (directory was already gone)\n", ui.LabelSuccess.Render("✓"))
		}
		if paneWarning != "" {
			logger.Fail("close_pane", e.Branch, fmt.Errorf("%s", paneWarning))
			_, _ = fmt.Fprintf(out, "  %s %s\n", ui.LabelWarning.Render("!"), paneWarning)
		}
		cleaned++
	}
	_, _ = fmt.Fprintf(out, "%s %d cleaned, %d left in place\n", ui.LabelInfo.Render("i"), cleaned, unsafe)
}

func renderCandidate(out io.Writer, c *supervisor.PruneCandidate) {
	if c.SafeToClean {
		_, _ = fmt.Fprintf(out, "%s %s safe to clean (PR %s merged)\n", ui.LabelSuccess.Render("✓"), c.Branch, c.PRURL)
		return
	}
	reasons := "unknown"
	if len(c.Reasons) > 0 {
		reasons = strings.Join(c.Reasons, "; ")
	}
	_, _ = fmt.Fprintf(out, "%s %s not safe: %s\n", ui.LabelWarning.Render("○"), c.Branch, reasons)
}
