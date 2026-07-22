package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newRebaseCmd() *cobra.Command {
	var (
		worktree      string
		base          string
		launcher      string
		workerRuntime string
		interval      time.Duration
		force         bool
		dryRun        bool
		noCredProxy   bool
		credentialEnv map[string]string
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
			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}
			return runRebase(cmd, herdr.New(), &rebaseOpts{
				worktree:      worktree,
				base:          base,
				launcher:      launcher,
				workerRuntime: workerRuntime,
				interval:      interval,
				force:         force,
				dryRun:        dryRun,
				noCredProxy:   noCredProxy,
				credentialEnv: overrides,
			})
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree whose branch to rebase")
	cmd.Flags().StringVar(&base, "base", "main", "base branch to rebase onto")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in the worker pane")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "status poll cadence")
	cmd.Flags().BoolVar(&force, "force", false, "dispatch a rebase even if no conflict is detected")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "detect and print the plan without dispatching a worker")
	cmd.Flags().StringVar(&workerRuntime, "worker-runtime", "", "isolate the rebase worker with the argus-runtime-<name> adapter on PATH (see docs/worker-runtime-protocol.md); default none runs unwrapped as today")
	cmd.Flags().BoolVar(&noCredProxy, "no-cred-proxy", false, "do not front the rebase worker's API traffic with the credential proxy; it inherits the host's real ANTHROPIC_API_KEY")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	return cmd
}

var rebaseCmd = newRebaseCmd()

// rebaseOpts carries newRebaseCmd's flag values into runRebase. It exists so the
// constructor stays flag-registration boilerplate and the actual RunE logic lives
// in a top-level function go-crap can score (and tests can call) on its own,
// instead of an inline closure whose complexity gets charged to the constructor.
type rebaseOpts struct {
	credentialEnv map[string]string
	worktree      string
	base          string
	launcher      string
	workerRuntime string
	interval      time.Duration
	force         bool
	dryRun        bool
	noCredProxy   bool
}

// runRebase is newRebaseCmd's RunE body. It detects whether the worktree's branch
// conflicts with its base, then either reports the plan (--dry-run / no conflict)
// or dispatches the worktree's own worker to resolve it.
func runRebase(cmd *cobra.Command, client herdr.Client, opts *rebaseOpts) error {
	if opts.worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus rebase --worktree <path>"}
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	branch, conflicts, err := detectRebaseConflict(ctx, opts.worktree, opts.base)
	if err != nil {
		return err
	}

	logger, closeLog := openRunLog(cmd, "rebase")
	defer closeLog()
	logger.Action("conflict_check", branch, fmt.Sprintf("conflicts=%v", conflicts), opts.base)

	if !conflicts && !opts.force {
		_, _ = fmt.Fprintf(out, "%s %s has no conflict with origin/%s — nothing to rebase (use --force to dispatch anyway)\n",
			ui.LabelSuccess.Render("✓"), branch, opts.base)
		return nil
	}

	// Resolved even in a dry run (read-only git plumbing, no side effect) so a
	// broken worktree — one herdr couldn't open a pane for even with this in
	// hand — is caught by --dry-run too, not just by the real dispatch below.
	repoRoot, err := supervisor.RepoRoot(ctx, opts.worktree)
	if err != nil {
		return fmt.Errorf("resolving repo root for %s: %w", opts.worktree, err)
	}

	if opts.dryRun {
		_, _ = fmt.Fprintf(out, "%s rebase plan (dry run)\n  worktree: %s\n  repo:     %s\n  branch:   %s -> origin/%s\n  conflicts: %v\n  action:   dispatch worker to rebase + force-push\n",
			ui.LabelInfo.Render("i"), opts.worktree, repoRoot, branch, opts.base, conflicts)
		return nil
	}

	return dispatchRebaseWorker(ctx, logger, client, out, repoRoot, branch, opts)
}

// detectRebaseConflict resolves the worktree's current branch, refreshes its view
// of origin/base, and reports whether rebasing onto it would conflict.
func detectRebaseConflict(ctx context.Context, worktree, base string) (branch string, conflicts bool, err error) {
	branch, err = supervisor.CurrentBranch(ctx, worktree)
	if err != nil {
		return "", false, err
	}
	if ferr := supervisor.FetchBase(ctx, worktree, base); ferr != nil {
		return "", false, ferr
	}
	conflicts, err = supervisor.ConflictsWith(ctx, worktree, base)
	if err != nil {
		return "", false, err
	}
	return branch, conflicts, nil
}

// dispatchRebaseWorker opens the worktree in herdr, hands its root pane the rebase
// brief, gets the worker running there, and waits for it to reach a terminal
// status. repoRoot is the worktree's main repo (see supervisor.RepoRoot) — herdr's
// WorktreeOpen needs it as --cwd to confirm the calling context is inside a git
// work tree.
func dispatchRebaseWorker(ctx context.Context, logger *eventlog.Logger, client herdr.Client, out io.Writer, repoRoot, branch string, opts *rebaseOpts) error {
	// Captured before anything else touches the worktree, so WaitForStatus
	// rejects a status.json left over from before this dispatch (see
	// InvalidateStatus and issue #50) even if invalidation below races with a
	// stray write.
	dispatchedAt := time.Now()

	wt, err := client.WorktreeOpen(ctx, repoRoot, opts.worktree)
	if err != nil {
		return err
	}
	if wt.RootPaneID == "" {
		return &ui.UserError{Err: fmt.Errorf("herdr opened no pane for %s", opts.worktree)}
	}
	if ierr := supervisor.InvalidateStatus(opts.worktree); ierr != nil {
		return fmt.Errorf("invalidating stale status before rebase dispatch: %w", ierr)
	}
	if werr := supervisor.WriteBrief(opts.worktree, supervisor.RebaseBrief(branch, opts.base)); werr != nil {
		return werr
	}

	if err := dispatchIntoPane(ctx, logger, client, wt.RootPaneID, branch, opts); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s dispatched rebase worker in pane %s; waiting...\n", ui.LabelInfo.Render("i"), wt.RootPaneID)
	status, seen := supervisor.WaitForStatus(ctx, opts.worktree, opts.interval, dispatchedAt)
	if !seen {
		logger.Action("rebase", branch, "no-status", "")
		return fmt.Errorf("worker wrote no status before the deadline")
	}
	logger.Action("rebase", branch, string(status.Phase), status.BlockedReason)
	renderRebaseOutcome(out, branch, &status)
	return nil
}

// dispatchIntoPane gets a worker acting on the freshly written rebase brief inside
// paneID. rebase targets a worktree an earlier task already ran a worker in (see
// RebaseBrief: "it already has full context"), so paneID very often still holds
// that worker's live, idle Claude Code session — not a bare shell. Typing a shell
// command line into that pane (PaneRun) would land as a chat message in the
// agent's own input box, not a command a shell executes, which is why the
// dispatch used to silently no-op (argus issue #88): the pane's scrollback never
// showed the brief being read at all. So this checks with herdr first: a live
// agent gets re-tasked in place via AgentPrompt, using the same one-line pointer
// at the brief a fresh spawn would pass as its initial prompt; only a genuinely
// bare pane (no agent herdr recognizes) falls back to spawning a new one, exactly
// as supervisor.execute does for a freshly created worktree.
func dispatchIntoPane(ctx context.Context, logger *eventlog.Logger, client herdr.Client, paneID, branch string, opts *rebaseOpts) error {
	_, live, err := client.AgentGet(ctx, paneID)
	if err != nil {
		return fmt.Errorf("checking whether %s already has a live agent: %w", paneID, err)
	}
	if live {
		logger.Action("rebase_dispatch", branch, "reuse-live-agent", paneID)
		return client.AgentPrompt(ctx, paneID, supervisor.InitialPrompt)
	}

	logger.Action("rebase_dispatch", branch, "spawn-new-agent", paneID)
	spawnLine, cleanup, err := buildRebaseSpawnLine(ctx, logger, opts.worktree, branch, opts.launcher, opts.workerRuntime, opts.noCredProxy, opts.credentialEnv)
	defer cleanup()
	if err != nil {
		return err
	}
	return client.PaneRun(ctx, paneID, spawnLine)
}

// buildRebaseSpawnLine resolves the shell command line argus types into a
// rebase worker's pane. It fronts the worker's API traffic with the same
// generalized credproxy wiring cmd/supervise.go gives spawn-mode workers (see
// startCredentialProxy) — this path used to pass workerEnv: nil
// unconditionally, so a rebase-dispatched worker never got a sentinel even
// when spawn-mode workers did — then wraps the result via a runtime adapter
// when one is configured. cleanup shuts down any credproxy this call started
// and must be deferred by the caller regardless of the returned error.
func buildRebaseSpawnLine(ctx context.Context, logger *eventlog.Logger, worktree, branch, launcher, workerRuntime string, noCredProxy bool, credentialEnv map[string]string) (spawnLine string, cleanup func(), err error) {
	cleanup = func() {}
	scrubEnv := forge.StandardTokenVars()

	var workerEnv []string
	if !noCredProxy {
		proxy, extraScrub, pcleanup, perr := startCredentialProxy(logger, credentialEnv) //nolint:contextcheck // startCredentialProxy's own proxy.Start() takes no context; it only binds a loopback listener
		cleanup = pcleanup
		if perr != nil {
			return "", cleanup, perr
		}
		if proxy != nil {
			workerEnv = proxy.WorkerEnv(branch, branch)
			scrubEnv = append(scrubEnv, extraScrub...)
		}
	}

	// See supervisor.ResolveLauncherPath: a freshly opened pane's shell may not
	// have finished initializing its own PATH yet, so resolve the launcher
	// binary to an absolute path here (argus's own PATH is already ready)
	// rather than risk it racing against the new shell's startup.
	launcher = supervisor.ResolveLauncherPath(launcher)

	spawnLine = supervisor.SpawnCommand(worktree, launcher, scrubEnv, workerEnv)
	if workerRuntime != "" && workerRuntime != "none" {
		line, rerr := supervisor.LaunchViaRuntime(ctx, workerRuntime, worktree, launcher, workerEnv)
		if rerr != nil {
			return "", cleanup, fmt.Errorf("launching rebase worker via runtime adapter: %w", rerr)
		}
		spawnLine = line
	}
	return spawnLine, cleanup, nil
}

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
