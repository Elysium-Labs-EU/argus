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
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
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
		forgeKind     string
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
				forgeKind: forgeKind, forgeKindExplicit: cmd.Flags().Changed("forge"),
			})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose worktrees to inspect")
	cmd.Flags().StringVar(&branch, "branch", "", "prune only the worktree for this branch")
	cmd.Flags().BoolVar(&merged, "merged", false, "sweep every worktree under the repo, not just one branch")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan (which worktrees, which check failed/passed) without deleting anything")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	cmd.Flags().StringVar(&forgeKind, "forge", "", "force the forge API shape for a self-hosted host: \"gitlab\" or \"gitea\" (default: auto-detect, which only recognizes github.com/gitlab.com/codeberg.org and refuses every other host). Without this flag, this repo's .argus/config.yml forge key wins, then auto-detect")
	addDebugFlag(cmd)
	return cmd
}

// worktreePruneArgs holds newWorktreePruneCmd's flag values so runWorktreePrune
// can be tested directly, without going through cobra flag parsing.
type worktreePruneArgs struct {
	credentialEnv  map[string]string
	repo, branch   string
	forgeKind      string
	merged, dryRun bool
	// forgeKindExplicit is true only when --forge was actually passed,
	// mirroring shipArgs.forgeKindExplicit's explicit-flag-wins precedence
	// over this repo's .argus/config.yml forge key.
	forgeKindExplicit bool
}

// pruneTargets is what resolvePruneTargets resolves before runWorktreePrune
// hands off to prunePlan: the repo/forge identity and the candidate worktree
// list, independent of --dry-run.
type pruneTargets struct {
	client   forge.Forge
	repoRoot string
	owner    string
	name     string
	entries  []supervisor.WorktreeEntry
}

// resolvePruneTargets runs runWorktreePrune's resolution steps — repo root,
// candidate worktrees, repo/forge identity — so runWorktreePrune itself only
// has to branch on the outcome. Split out because this is the bulk of
// runWorktreePrune's own decision points; isolating them here keeps both
// functions independently testable and each under the CRAP gate.
func resolvePruneTargets(ctx context.Context, out io.Writer, a *worktreePruneArgs) (*pruneTargets, error) {
	resolvedRepo, err := supervisor.ResolveWorktree(a.repo)
	if err != nil {
		return nil, err
	}
	repoRoot, err := supervisor.RepoRoot(ctx, resolvedRepo)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root for %s: %w", resolvedRepo, err)
	}

	entries, err := supervisor.ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if a.branch != "" {
		entries = filterByBranch(entries, a.branch)
		if len(entries) == 0 {
			return nil, &ui.UserError{Err: fmt.Errorf("no worktree found for branch %q under %s", a.branch, repoRoot)}
		}
	}

	owner, name, client, err := resolvePruneForgeClient(ctx, out, repoRoot, a)
	if err != nil {
		return nil, err
	}
	return &pruneTargets{repoRoot: repoRoot, owner: owner, name: name, entries: entries, client: client}, nil
}

// resolvePruneForgeClient resolves the repo/forge identity for repoRoot and
// builds the forge.Forge client prunePlan needs to check PR merge state.
// Split out of resolvePruneTargets so each half of the resolution stays small
// and independently testable.
func resolvePruneForgeClient(ctx context.Context, out io.Writer, repoRoot string, a *worktreePruneArgs) (owner, name string, client forge.Forge, err error) {
	host, owner, name, err := resolveRepo(ctx, "", repoRoot)
	if err != nil {
		return "", "", nil, err
	}
	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return "", "", nil, fmt.Errorf("loading %s: %w", repoconfig.Path(repoRoot), err)
	}
	warnDeprecatedConfigKeys(out, &rc)
	kind, err := parseForgeKind(resolveForgeKindValue(a.forgeKindExplicit, a.forgeKind, rc.Forge))
	if err != nil {
		return "", "", nil, err
	}
	client, err = forge.New(host, forge.TokenForHost(host, a.credentialEnv), nil, kind, rc.StatusPage)
	if err != nil {
		return "", "", nil, err
	}
	return owner, name, client, nil
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
	targets, err := resolvePruneTargets(ctx, cmd.OutOrStdout(), a)
	if err != nil {
		return err
	}

	logger, closeLog := openRunLog(cmd, "worktree_prune")
	defer closeLog()

	prunePlan(cmd, ctx, logger, targets.client, herdr.New(), targets.owner, targets.name, targets.repoRoot, targets.entries, a.dryRun)
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
