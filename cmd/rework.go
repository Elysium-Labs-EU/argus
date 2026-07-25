package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newReworkCmd() *cobra.Command {
	var (
		worktree           string
		base               string
		task               string
		findings           []string
		launcher           string
		workerRuntime      string
		reviewModel        string
		proofRequiredPaths []string
		alwaysReviewPaths  []string
		credentialEnv      map[string]string
		interval           time.Duration
		maxRounds          int
		maxDiffLines       int
		dryRun             bool
		noCredProxy        bool
	)
	policyDefaults := supervisor.DefaultReviewPolicy()

	cmd := &cobra.Command{
		Use:   "rework",
		Short: "Re-dispatch a worktree's worker to address a request-changes verdict, looping gate/review until it clears",
		Long: `Rework is the first-class continuation of a request-changes verdict: it
re-dispatches the worktree's own worker (in place, same branch) with the
findings from the last review as its next brief, waits for it to report back,
then re-runs the deterministic gate and — on escalation — the reviewer, exactly
as the main supervise loop does. The resulting verdict is persisted to the
worktree so ship sees it, closing the gap where a manual "argus review" verdict
was never saved anywhere ship could check. On a further request-changes it
loops back into the same cycle, up to --max-rounds, then stops and surfaces the
outcome instead of retrying forever.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}
			logger, closeLog := openRunLog(cmd, "rework")
			defer closeLog()
			return runRework(cmd, herdr.New(), newReviewer(reviewModel, logger), logger, &reworkOpts{
				worktree: worktree, base: base, task: task, findings: findings,
				launcher: launcher, workerRuntime: workerRuntime, interval: interval,
				maxRounds: maxRounds, dryRun: dryRun, noCredProxy: noCredProxy, credentialEnv: overrides,
				gate: gateFlags{
					maxDiffLines:          maxDiffLines,
					proofRequiredPaths:    proofRequiredPaths,
					alwaysReviewPaths:     alwaysReviewPaths,
					maxDiffLinesExplicit:  cmd.Flags().Changed("max-diff-lines"),
					proofRequiredExplicit: cmd.Flags().Changed("proof-required-path"),
					alwaysReviewExplicit:  cmd.Flags().Changed("always-review-path"),
				},
			})
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree whose worker to re-dispatch")
	cmd.Flags().StringVar(&base, "base", "origin/main", "base ref to diff against for the gate/review")
	cmd.Flags().StringVar(&task, "task", "", "task/issue the change addresses (default: the worker's last reported task, else the branch name)")
	cmd.Flags().StringSliceVar(&findings, "findings", nil, "findings to hand the worker for round 1 (default: the worktree's last recorded request-changes verdict)")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in the worker pane if it has no live agent")
	cmd.Flags().StringVar(&workerRuntime, "worker-runtime", "", "isolate the rework worker with the argus-runtime-<name> adapter on PATH (see docs/worker-runtime-protocol.md); default none runs unwrapped as today")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "status poll cadence")
	cmd.Flags().IntVar(&maxRounds, "max-rounds", supervisor.DefaultMaxReworkRounds, "give up and escalate after this many request-changes rounds")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for the review (default: claude's default)")
	cmd.Flags().IntVar(&maxDiffLines, "max-diff-lines", policyDefaults.MaxDiffLines, "review gate: diffs larger than this (insertions+deletions) escalate; 0 disables. Without this flag, this repo's .argus/config.yml max_diff_lines wins, then this default")
	cmd.Flags().StringSliceVar(&proofRequiredPaths, "proof-required-path", policyDefaults.ProofRequiredPaths, "review gate: a touched path matching one of these (whole word, or path substring if it contains /) needs real-world proof. Without this flag, this repo's .argus/config.yml proof_required_paths wins, then this default")
	cmd.Flags().StringSliceVar(&alwaysReviewPaths, "always-review-path", policyDefaults.AlwaysReviewPaths, "review gate: a touched path matching one of these (whole word, or path substring if it contains /) always escalates, even for a small clean diff. Without this flag, this repo's .argus/config.yml always_review_paths wins, then this default")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without dispatching a worker")
	cmd.Flags().BoolVar(&noCredProxy, "no-cred-proxy", false, "do not front the rework worker's API traffic with the credential proxy; it inherits the host's real ANTHROPIC_API_KEY")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	return cmd
}

var reworkCmd = newReworkCmd()

// reworkOpts carries newReworkCmd's flag values into runRework, mirroring
// rebaseOpts's split of constructor-flag-registration from RunE logic.
type reworkOpts struct {
	credentialEnv    map[string]string
	workerRuntime    string
	worktree         string
	base             string
	task             string
	launcher         string
	findings         []string
	gate             gateFlags
	interval         time.Duration
	maxRounds        int
	livenessTimeout  time.Duration // internal knob, mirrors rebaseOpts; zero = package default
	livenessInterval time.Duration
	dryRun           bool
	noCredProxy      bool
}

// dispatchTarget builds dispatchIntoPane's input from a reworkOpts, mirroring
// rebaseOpts.dispatchTarget — the two commands share the same pane-reuse-vs-
// spawn dispatch primitive.
func (o *reworkOpts) dispatchTarget() *dispatchTarget {
	return &dispatchTarget{
		worktree: o.worktree, launcher: o.launcher, workerRuntime: o.workerRuntime,
		noCredProxy: o.noCredProxy, credentialEnv: o.credentialEnv,
		livenessTimeout: o.livenessTimeout, livenessInterval: o.livenessInterval,
	}
}

// runRework is newReworkCmd's RunE body. It resolves round 1's findings from
// the worktree's last recorded verdict (or --findings), then loops
// dispatch-and-judge rounds until the worker is approved, a human decision is
// needed (blocked, or the reviewer abstains), or --max-rounds is exhausted.
func runRework(cmd *cobra.Command, client herdr.Client, reviewer supervisor.Reviewer, logger *eventlog.Logger, opts *reworkOpts) error {
	if opts.worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus rework --worktree <path>"}
	}
	// See supervisor.ResolveWorktree: every command taking --worktree resolves
	// it to an absolute path before it reaches git plumbing, protocol.Load, or
	// a pane's `cd` (argus issue #98).
	abs, err := supervisor.ResolveWorktree(opts.worktree)
	if err != nil {
		return err
	}
	opts.worktree = abs
	if opts.maxRounds <= 0 {
		opts.maxRounds = supervisor.DefaultMaxReworkRounds
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	findings, err := startingFindings(opts.worktree, opts.findings)
	if err != nil {
		return err
	}
	if findings == nil {
		_, _ = fmt.Fprintf(out, "%s %s already has an approving argus verdict — nothing to rework\n", ui.LabelSuccess.Render("✓"), opts.worktree)
		return nil
	}

	branch, err := supervisor.CurrentBranch(ctx, opts.worktree)
	if err != nil {
		return err
	}
	task := opts.task
	if task == "" {
		task = taskFor(opts.worktree, branch)
	}

	if opts.dryRun {
		renderReworkPlan(out, opts, branch, findings)
		return nil
	}

	repoRoot, err := supervisor.RepoRoot(ctx, opts.worktree)
	if err != nil {
		return fmt.Errorf("resolving repo root for %s: %w", opts.worktree, err)
	}
	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return &ui.UserError{Err: fmt.Errorf("loading %s: %w", repoconfig.Path(repoRoot), err)}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home dir: %w", err)
	}
	cfg := &supervisor.Config{
		Now:      time.Now,
		Log:      logger,
		Policy:   resolveGatePolicy(opts.gate, &rc),
		Home:     home,
		Base:     opts.base,
		Reviewer: reviewer,
	}

	for round := 1; round <= opts.maxRounds; round++ {
		outcome, rerr := runReworkRound(ctx, out, logger, client, cfg, repoRoot, branch, task, findings, round, opts)
		if rerr != nil {
			return rerr
		}
		if outcome.stop {
			return nil
		}
		findings = outcome.findings
	}
	return nil
}

// reworkRoundOutcome is what one dispatch-and-judge round decided: stop is
// true once the loop has a terminal answer (approved, blocked, no reviewer
// verdict, needs-human, or rounds exhausted); otherwise findings carries the
// next round's brief.
type reworkRoundOutcome struct {
	findings []string
	stop     bool
}

// runReworkRound dispatches one round, judges the result, renders it, and
// decides whether runRework's loop should stop or continue — split out of
// runRework so this branching (blocked/approved/no-reviewer/needs-human/
// rounds-exhausted) doesn't inflate runRework's own cyclomatic complexity.
func runReworkRound(ctx context.Context, out io.Writer, logger *eventlog.Logger, client herdr.Client, cfg *supervisor.Config, repoRoot, branch, task string, findings []string, round int, opts *reworkOpts) (reworkRoundOutcome, error) {
	// Snapshotted before dispatchReworkRound, which invalidates the worktree's
	// verdict.json before every round (see InvalidateStatus) — reading it any
	// later would always see it gone, permanently defeating gateVerdict's
	// under-report subtraction from round 2 onward.
	var prior *protocol.Approval
	if approval, found, aerr := protocol.LoadApproval(opts.worktree); aerr != nil {
		return reworkRoundOutcome{}, aerr
	} else if found {
		prior = &approval
	}

	status, paneID, dispatchedAt, derr := dispatchReworkRound(ctx, logger, client, repoRoot, branch, task, findings, round, opts)
	if derr != nil {
		return reworkRoundOutcome{}, derr
	}
	if status.Phase == protocol.PhaseBlocked {
		_, _ = fmt.Fprintf(out, "\n%s round %d/%d: worker blocked: %s\n", ui.LabelError.Render("✗"), round, opts.maxRounds, status.BlockedReason)
		return reworkRoundOutcome{stop: true}, nil
	}

	plan := &supervisor.WorkerPlan{Worker: supervisor.Worker{Task: task, Branch: branch, Worktree: opts.worktree}}
	result := supervisor.JudgeOne(ctx, cfg, plan, &status, paneID, dispatchedAt, prior)
	approved := result.Gate.AutoApprove || (result.Review != nil && result.Review.Decision == "approve")
	renderReworkRound(out, round, opts.maxRounds, &result, approved)

	if approved {
		return reworkRoundOutcome{stop: true}, nil
	}
	if result.Review == nil {
		_, _ = fmt.Fprintf(out, "%s escalating: no reviewer verdict available (%v)\n", ui.LabelWarning.Render("!"), result.ReviewErr)
		return reworkRoundOutcome{stop: true}, nil
	}
	if result.Review.Decision == "needs-human" {
		_, _ = fmt.Fprintf(out, "%s escalating: reviewer could not judge (needs-human)\n", ui.LabelWarning.Render("!"))
		return reworkRoundOutcome{stop: true}, nil
	}
	if round == opts.maxRounds {
		_, _ = fmt.Fprintf(out, "%s rework rounds exhausted (%d/%d), still not approved — escalating to the supervisor\n", ui.LabelWarning.Render("!"), round, opts.maxRounds)
		return reworkRoundOutcome{stop: true}, nil
	}
	next := result.Review.Findings
	if len(next) == 0 {
		next = []string{result.Review.Summary}
	}
	return reworkRoundOutcome{findings: next}, nil
}

// startingFindings resolves round 1's findings: an explicit --findings
// override always wins, so rework is usable straight after a plain `argus
// review` call (which never persists a verdict.json at all — see
// cmd/review.go's own "this verdict is not saved" warning). Otherwise the
// worktree's last recorded verdict supplies them. A nil, nil return means the
// last verdict already approved — nothing to rework. No verdict at all is an
// error: rework needs to know what to fix.
func startingFindings(worktree string, explicit []string) ([]string, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}
	approval, found, err := protocol.LoadApproval(worktree)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("no argus verdict for %s", worktree),
			Hint: "run `argus review --worktree <path>` first and pass its findings via --findings, or `argus supervise --attach --review` to record one automatically",
		}
	}
	if approval.Approved {
		return nil, nil
	}
	if len(approval.Reasons) > 0 {
		return approval.Reasons, nil
	}
	return []string{approval.Summary}, nil
}

// taskFor falls back from an explicit --task to the worker's own last
// reported task, then to the branch name — the same fallback chain
// attachWorkers uses in cmd/supervise.go.
func taskFor(worktree, branch string) string {
	if s, err := protocol.Load(protocol.StatusPath(worktree)); err == nil && s.Task != "" {
		return s.Task
	}
	return branch
}

// dispatchReworkRound writes one round's brief, re-dispatches the worktree's
// worker (reusing dispatchIntoPane's live-agent-vs-spawn logic), and waits for
// its next terminal status.
func dispatchReworkRound(ctx context.Context, logger *eventlog.Logger, client herdr.Client, repoRoot, branch, task string, findings []string, round int, opts *reworkOpts) (protocol.Status, string, time.Time, error) {
	// Captured before the worktree is touched, so WaitForStatus rejects any
	// status.json left over from a prior round or dispatch (see
	// InvalidateStatus and issue #50) even if invalidation below races with a
	// stray write.
	dispatchedAt := time.Now()

	wt, err := client.WorktreeOpen(ctx, repoRoot, opts.worktree)
	if err != nil {
		return protocol.Status{}, "", dispatchedAt, err
	}
	if wt.RootPaneID == "" {
		return protocol.Status{}, "", dispatchedAt, &ui.UserError{Err: fmt.Errorf("herdr opened no pane for %s", opts.worktree)}
	}
	if ierr := supervisor.InvalidateStatus(opts.worktree); ierr != nil {
		return protocol.Status{}, "", dispatchedAt, fmt.Errorf("invalidating stale status before rework dispatch: %w", ierr)
	}
	brief := supervisor.ReworkBrief(task, branch, findings, round, opts.maxRounds)
	if werr := supervisor.WriteBrief(opts.worktree, brief); werr != nil {
		return protocol.Status{}, "", dispatchedAt, werr
	}

	if err := dispatchIntoPane(ctx, logger, client, wt.RootPaneID, branch, opts.dispatchTarget()); err != nil {
		return protocol.Status{}, "", dispatchedAt, err
	}

	status, seen := supervisor.WaitForStatus(ctx, opts.worktree, opts.interval, dispatchedAt)
	if !seen {
		return protocol.Status{}, "", dispatchedAt, fmt.Errorf("worker wrote no status before the deadline (round %d/%d)", round, opts.maxRounds)
	}
	logger.Action("rework", branch, string(status.Phase), status.BlockedReason)
	return status, wt.RootPaneID, dispatchedAt, nil
}

func renderReworkPlan(out io.Writer, opts *reworkOpts, branch string, findings []string) {
	_, _ = fmt.Fprintf(out, "%s rework plan (dry run)\n  worktree:   %s\n  branch:     %s\n  base:       %s\n  max-rounds: %d\n  findings:\n",
		ui.LabelInfo.Render("i"), opts.worktree, branch, opts.base, opts.maxRounds)
	for _, f := range findings {
		_, _ = fmt.Fprintf(out, "    - %s\n", f)
	}
}

// renderReworkRound prints one round's gate and (when it ran) reviewer
// verdict, mirroring internal/supervisor/report.go's renderVerdict/renderReview
// for the full supervise loop.
func renderReworkRound(out io.Writer, round, maxRounds int, result *supervisor.JudgeResult, approved bool) {
	mark := ui.LabelWarning.Render("○")
	if approved {
		mark = ui.LabelSuccess.Render("✓")
	}
	_, _ = fmt.Fprintf(out, "\n%s rework round %d/%d\n", mark, round, maxRounds)

	if result.Gate.AutoApprove {
		_, _ = fmt.Fprintf(out, "  gate: %s auto-approve\n", ui.LabelSuccess.Render("✓"))
	} else {
		_, _ = fmt.Fprintf(out, "  gate: %s escalated\n", ui.LabelWarning.Render("○"))
		for _, r := range result.Gate.Reasons {
			_, _ = fmt.Fprintf(out, "    - %s\n", r)
		}
	}

	if result.ReviewErr != nil {
		_, _ = fmt.Fprintf(out, "  review: %s error: %v\n", ui.LabelError.Render("✗"), result.ReviewErr)
	}
	if result.Review != nil {
		revMark := ui.LabelWarning.Render("○")
		switch result.Review.Decision {
		case "approve":
			revMark = ui.LabelSuccess.Render("✓")
		case "request-changes":
			revMark = ui.LabelError.Render("✗")
		}
		_, _ = fmt.Fprintf(out, "  review: %s %s — %s\n", revMark, result.Review.Decision, result.Review.Summary)
		for _, f := range result.Review.Findings {
			_, _ = fmt.Fprintf(out, "    · %s\n", f)
		}
	}
}
