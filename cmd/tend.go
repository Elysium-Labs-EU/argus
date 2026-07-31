package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newTendCmd() *cobra.Command {
	var (
		worktree          string
		interval          time.Duration
		timeout           time.Duration
		dryRun            bool
		credentialEnv     map[string]string
		forgeKind         string
		owner             string
		forceForeignOwner bool
		ownerStaleAfter   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "tend",
		Short: "Poll a shipped PR's CI checks to a terminal state",
		Long: `Tend is ship's counterpart on the CI side: once a PR is open, it polls the
checks on its head commit until every one has a terminal result, re-stamping
the worktree's ownership lease heartbeat on every tick the same way supervise's
own watch loop does. It reports one of three outcomes: merge-ready (every
check passed), failed (naming the first check that didn't), or an error if
interrupted or --timeout elapses first. It does no dispatch of any kind —
fixing a failing check is a separate, later step.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}
			return runTend(cmd, &tendOpts{
				worktree: worktree, interval: interval, timeout: timeout, dryRun: dryRun,
				credentialEnv: overrides,
				forgeKind:     forgeKind, forgeKindExplicit: cmd.Flags().Changed("forge"),
				owner: ownerFlags{
					owner: owner, forceForeignOwner: forceForeignOwner,
					ownerStaleAfter: ownerStaleAfter, ownerStaleAfterExplicit: cmd.Flags().Changed("owner-stale-after"),
				},
			})
		},
	}

	bindWorktreeFlag(cmd, &worktree, "worktree whose shipped PR to poll")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "CI check poll cadence")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "wall-clock deadline before argus stops polling (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved PR and poll plan without blocking")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	cmd.Flags().StringVar(&forgeKind, "forge", "", "force the forge API shape for a self-hosted host: \"gitlab\" or \"gitea\" (default: auto-detect, which only recognizes github.com/gitlab.com/codeberg.org and refuses every other host). Without this flag, this repo's .argus/config.yml forge key wins, then auto-detect")
	cmd.Flags().StringVar(&owner, "owner", "", ownerFlagHelp)
	cmd.Flags().BoolVar(&forceForeignOwner, "force-foreign-owner", false, forceForeignOwnerFlagHelp)
	cmd.Flags().DurationVar(&ownerStaleAfter, "owner-stale-after", ownership.DefaultStaleAfter, ownerStaleAfterFlagHelp)
	return cmd
}

var tendCmd = newTendCmd()

// tendOpts carries newTendCmd's flag values into runTend, the same split
// rebaseOpts/worktreePruneArgs use to keep the constructor to flag
// registration and the real logic independently testable.
type tendOpts struct {
	credentialEnv     map[string]string
	worktree          string
	forgeKind         string
	owner             ownerFlags
	interval          time.Duration
	timeout           time.Duration
	forgeKindExplicit bool
	dryRun            bool
}

// TendOutcome is tend's terminal result once every check on a PR has reached
// a terminal state: MergeReady when all passed, or FailedCheck naming the
// first that didn't. It is the zero value (used, done=false) while checks are
// still in flight — see evaluateChecks.
type TendOutcome struct {
	PR          forge.PR
	FailedCheck string
	Checks      []forge.Check
	MergeReady  bool
}

// runTend is newTendCmd's RunE body: worktree validation plus resolving and
// dispatching to the forge-facing half, extracted so each stays independently
// testable — resolveTendTarget against real (network-free) git plumbing,
// tendChange against a fake forge.Forge.
func runTend(cmd *cobra.Command, opts *tendOpts) error {
	if opts.worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus tend --worktree <path>"}
	}
	abs, err := supervisor.ResolveWorktree(opts.worktree)
	if err != nil {
		return err
	}
	opts.worktree = abs

	client, branch, owner, repo, err := resolveTendTarget(cmd, opts)
	if err != nil {
		return err
	}
	return tendChange(cmd, client, opts, branch, owner, repo)
}

// resolveTendTarget resolves everything runTend needs to hand off to
// tendChange: the ownership-lease check, the worktree's current branch, its
// forge identity, and a constructed forge.Forge client. Every step here is
// local git plumbing or object construction — forge.New itself makes no
// network call, only OpenPR/FindPR/PRChecks do — so this whole chain is
// testable against a real temp git repo without a fake HTTP server.
func resolveTendTarget(cmd *cobra.Command, opts *tendOpts) (client forge.Forge, branch, owner, repo string, err error) {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if oerr := enforceOwnership(ctx, out, opts.worktree, opts.owner, time.Now()); oerr != nil {
		return nil, "", "", "", oerr
	}

	branch, err = supervisor.CurrentBranch(ctx, opts.worktree)
	if err != nil {
		return nil, "", "", "", err
	}
	host, repoOwner, repoName, err := resolveRepo(ctx, "", opts.worktree)
	if err != nil {
		return nil, "", "", "", err
	}
	kind, err := parseForgeKind(resolveForgeKindValue(opts.forgeKindExplicit, opts.forgeKind, forgeConfigDefault(ctx, out, opts.worktree)))
	if err != nil {
		return nil, "", "", "", err
	}
	client, err = forge.New(host, forge.TokenForHost(host, opts.credentialEnv), nil, kind, issueStatusPageConfigDefault(ctx, opts.worktree))
	if err != nil {
		return nil, "", "", "", err
	}
	return client, branch, repoOwner, repoName, nil
}

// tendChange is runTend's forge-facing half, split out (mirroring
// cmd/ship.go's shipChange) so it is independently testable against a fake
// forge.Forge instead of a real network call.
func tendChange(cmd *cobra.Command, f forge.Forge, opts *tendOpts, branch, owner, repo string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	pr, found, err := f.FindPR(ctx, owner, repo, branch)
	if err != nil {
		return fmt.Errorf("looking up PR for branch %s: %w", branch, err)
	}
	if !found {
		return &ui.UserError{Err: fmt.Errorf("no PR found for branch %q", branch), Hint: "run `argus ship` first"}
	}

	if opts.dryRun {
		_, _ = fmt.Fprintf(out, "%s tend plan (dry run)\n  worktree: %s\n  branch:   %s\n  PR:       %s (#%d)\n  poll:     every %s until every check is terminal\n",
			ui.LabelInfo.Render("i"), opts.worktree, branch, pr.HTMLURL, pr.Number, opts.interval)
		return nil
	}

	logger, closeLog := openRunLog(cmd, "tend")
	defer closeLog()

	outcome, err := pollChecks(ctx, f, logger, opts.worktree, owner, repo, pr, opts.interval, opts.timeout)
	if err != nil {
		return err
	}
	renderTendOutcome(out, &outcome)
	return nil
}

// pollChecks polls f.PRChecks on an interval-sized timer until every check on
// pr reaches a terminal state, re-stamping worktree's ownership lease
// heartbeat on every tick — the same per-tick heartbeat supervise's own watch
// loop performs (see internal/supervisor/loop.go's pollStatus) so a long tend
// run doesn't read as an abandoned worktree. timeout <= 0 disables the
// deadline, mirroring supervise's own --timeout.
func pollChecks(ctx context.Context, f forge.Forge, logger *eventlog.Logger, worktree, owner, repo string, pr forge.PR, interval, timeout time.Duration) (TendOutcome, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		dt := time.NewTimer(timeout)
		defer dt.Stop()
		deadline = dt.C
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return TendOutcome{}, fmt.Errorf("tending PR #%d for %s/%s: %w", pr.Number, owner, repo, ctx.Err())
		case <-deadline:
			return TendOutcome{}, fmt.Errorf("tending PR #%d for %s/%s: timed out after %s waiting for checks to reach a terminal state", pr.Number, owner, repo, timeout)
		case <-timer.C:
			// Best-effort, like supervise's own per-tick heartbeat: a write
			// failure here must not stop the poll loop itself.
			if herr := ownership.Heartbeat(worktree, time.Now()); herr != nil {
				logger.Fail("owner_heartbeat", fmt.Sprintf("pr-%d", pr.Number), herr)
			}
			checks, err := f.PRChecks(ctx, owner, repo, pr.Number)
			if err != nil {
				return TendOutcome{}, fmt.Errorf("polling checks for PR #%d: %w", pr.Number, err)
			}
			if outcome, done := evaluateChecks(pr, checks); done {
				logger.Action("tend", fmt.Sprintf("pr-%d", pr.Number), tendOutcomeLabel(&outcome), outcome.FailedCheck)
				return outcome, nil
			}
			timer.Reset(interval)
		}
	}
}

// evaluateChecks reports the PR's outcome once every check has reached a
// terminal state, and (done=false) while any check is still in flight or the
// host hasn't reported any yet. The first failing check in report order is
// what FailedCheck names — good enough for this MVP slice, which surfaces one
// name for a human/worker to go look at rather than ranking failures.
func evaluateChecks(pr forge.PR, checks []forge.Check) (TendOutcome, bool) {
	if len(checks) == 0 {
		return TendOutcome{}, false
	}
	for _, c := range checks {
		if !c.Terminal() {
			return TendOutcome{}, false
		}
	}
	for _, c := range checks {
		if c.Failed() {
			return TendOutcome{PR: pr, Checks: checks, FailedCheck: c.Name}, true
		}
	}
	return TendOutcome{PR: pr, Checks: checks, MergeReady: true}, true
}

func tendOutcomeLabel(o *TendOutcome) string {
	if o.MergeReady {
		return "merge-ready"
	}
	return "failed:" + o.FailedCheck
}

func renderTendOutcome(out io.Writer, o *TendOutcome) {
	if o.MergeReady {
		_, _ = fmt.Fprintf(out, "%s PR %s merge-ready: every check passed\n", ui.LabelSuccess.Render("✓"), o.PR.HTMLURL)
		return
	}
	_, _ = fmt.Fprintf(out, "%s PR %s failed: check %q did not pass\n", ui.LabelError.Render("✗"), o.PR.HTMLURL, o.FailedCheck)
}
