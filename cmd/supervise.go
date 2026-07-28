package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/credential"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/jira"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newSuperviseCmd() *cobra.Command {
	var (
		panes                 []string
		branches              []string
		labels                []string
		tasks                 []string
		tasksFile             string
		repo                  string
		base                  string
		launcher              string
		proofRequiredPaths    []string
		alwaysReviewPaths     []string
		reviewModel           string
		reviewEffort          string
		review                bool
		maxDiffLines          int
		verifyCmd             string
		interval              time.Duration
		timeout               time.Duration
		reviewConcurrency     int
		issues                []int
		jiraIssues            []string
		jiraAssignOnSpawn     bool
		jiraTransitionOnSpawn string
		dryRun                bool
		noCredProxy           bool
		attach                bool
		workspace             string
		worktrees             []string
		workerRuntime         string
		workerPlacement       string
		forgeKind             string
		allow                 []string
		credentialEnv         map[string]string
	)
	policyDefaults := supervisor.DefaultReviewPolicy()

	cmd := &cobra.Command{
		Use:   "supervise",
		Short: "Supervise herdr worker panes through to review",
		Long: `Supervise runs the deterministic half of multi-pane supervision: it
discovers the given herdr panes, enforces one worktree per worker, spawns each
worker in auto mode with a scoped permission file, and tracks each worker's typed
status.json rather than its scrollback. Judgment calls (diff review, blocked
decisions) are surfaced to you; no LLM sits in this loop.

Workers are defined by paired --tasks and --branches; argus creates a worktree per
worker and runs it in the pane herdr opens there. Pass --panes only to reuse
existing panes instead. --repo sets the repo (default: the current directory, or
each pane's directory in --panes mode).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := herdr.New()

			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}

			// --attach observes workers that are already running in their worktrees
			// instead of spawning any: no worktree is created and no agent started,
			// argus just watches their typed status and reports. Everything else
			// (tasks/branches/issues, worktree creation) belongs to the spawn path.
			var workers []supervisor.Worker
			if attach {
				// --attach watches a worktree argus did not create, so it has no
				// idea what the worker actually branched from. Silently falling
				// back to the spawn-mode default (origin/main) measures the diff
				// against the wrong ref whenever the real base differs (e.g. a
				// stacked branch) — wrong gate/review output with no indication
				// anything was off. Fail fast instead: require the operator to
				// state the real base explicitly.
				if !cmd.Flags().Changed("base") {
					return &ui.UserError{
						Err:  fmt.Errorf("--attach requires --base"),
						Hint: "argus supervise --attach --worktrees <path> --base <real-base-branch>",
					}
				}
				workers, err = attachWorkers(cmd.Context(), client, workspace, worktrees)
			} else {
				workers, err = spawnWorkers(cmd.Context(), client, &workerInput{
					panes: panes, branches: branches, labels: labels, tasks: tasks, tasksFile: tasksFile, repo: repo,
				}, issues, jiraIssues, overrides, jiraSpawnOpts{assignToCaller: jiraAssignOnSpawn, transition: jiraTransitionOnSpawn},
					forgeKind, cmd.Flags().Changed("forge"))
			}
			if err != nil {
				return err
			}

			// The main repo checkout's own .argus/config.yml (see
			// internal/repoconfig) — read from the first resolved worker's
			// RepoRoot, since a single supervise invocation targets one repo
			// in practice. --attach already required an explicit --base above
			// and never writes worker settings (see supervisor.Attach), so it
			// has no RepoRoot and needs none here.
			var repoRoot string
			if len(workers) > 0 {
				repoRoot = workers[0].RepoRoot
			}
			var rc repoconfig.Config
			if repoRoot != "" {
				rc, err = repoconfig.Load(repoconfig.Path(repoRoot))
				if err != nil {
					return &ui.UserError{Err: fmt.Errorf("loading %s: %w", repoconfig.Path(repoRoot), err)}
				}
			}
			applyRepoWorktreeDir(workers, rc.WorktreeDir)

			resolvedBase := resolveSuperviseBase(cmd.Context(), cmd.Flags().Changed("base"), base, repoRoot, &rc)
			policy := resolveGatePolicy(gateFlags{
				maxDiffLines:          maxDiffLines,
				proofRequiredPaths:    proofRequiredPaths,
				alwaysReviewPaths:     alwaysReviewPaths,
				maxDiffLinesExplicit:  cmd.Flags().Changed("max-diff-lines"),
				proofRequiredExplicit: cmd.Flags().Changed("proof-required-path"),
				alwaysReviewExplicit:  cmd.Flags().Changed("always-review-path"),
			}, &rc)
			verifyCommand := resolveVerifyCommand(cmd.Flags().Changed("verify-cmd"), verifyCmd, &rc)

			return runSupervision(cmd, client, workers, &superviseOpts{
				attach: attach, dryRun: dryRun, noCredProxy: noCredProxy,
				base: resolvedBase, launcher: resolveLauncher(cmd.Flags().Changed("launcher"), launcher, &rc), workerRuntime: workerRuntime,
				interval: interval, timeout: timeout,
				review: review, reviewModel: reviewModel, reviewEffort: resolveReviewEffort(cmd.Flags().Changed("review-effort"), reviewEffort, &rc), reviewConcurrency: reviewConcurrency,
				policy: policy, verifyCommand: verifyCommand,
				allow: allow, repoAllow: rc.Allow, credentialEnv: overrides, repoExplicit: repo != "",
				workerPlacement: resolveWorkerPlacement(cmd.Flags().Changed("worker-placement"), workerPlacement, &rc),
				reviewNote:      rc.ReviewNote,
			})
		},
	}

	cmd.Flags().IntSliceVar(&issues, "issues", nil, "issue numbers to fetch from the repo's forge and turn into worker briefs (branch defaults to <repo>-fix-issue-<n>)")
	cmd.Flags().StringSliceVar(&jiraIssues, "jira-issues", nil, "Jira issue keys (e.g. PROJ-123) to fetch and turn into worker briefs (branch defaults to <repo>-fix-<key>); requires JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN, or a JSON config file (see jira.Config) at $JIRA_CONFIG_FILE or ~/.argus/jira.json")
	cmd.Flags().BoolVar(&jiraAssignOnSpawn, "jira-assign-on-spawn", false, "with --jira-issues: assign each issue to the caller (the account owning the configured Jira credentials) at spawn time, before the worker starts")
	cmd.Flags().StringVar(&jiraTransitionOnSpawn, "jira-transition-on-spawn", "", "with --jira-issues: transition name or ID to move each issue to at spawn time (e.g. \"In Progress\"); unset skips this step")
	cmd.Flags().StringSliceVar(&tasks, "tasks", nil, "task/issue per worker (comma-separated); drives worker count in the default mode")
	cmd.Flags().StringVar(&tasksFile, "tasks-file", "", "path to a file with one task per line, appended after --tasks; unlike --tasks this is not CSV-parsed, so commas and quotes in a free-text brief are safe")
	cmd.Flags().StringSliceVar(&branches, "branches", nil, "branch per worker, paired positionally (default argus-<task-slug>)")
	cmd.Flags().StringSliceVar(&labels, "labels", nil, "herdr workspace label per worker, paired positionally (default: derived from --tasks, falling back to the branch)")
	cmd.Flags().StringSliceVar(&panes, "panes", nil, "reuse these existing herdr panes instead of the worktree's own pane")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root for all workers (default cwd; or each pane's directory in --panes mode)")
	cmd.Flags().StringVar(&base, "base", "origin/main", "base ref new worktrees branch from; required with --attach (no default applies — argus does not know what an attached worktree actually branched from). Without --base, this repo's .argus/config.yml base_branch wins, then the detected origin/HEAD, then this default")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in each worker pane after cd into its worktree. Without this flag, this repo's .argus/config.yml launcher wins, then this default")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "how often to poll each worker's status file")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "per-worker wall-clock deadline before argus stops waiting on it (0 = wait indefinitely)")
	cmd.Flags().IntVar(&maxDiffLines, "max-diff-lines", policyDefaults.MaxDiffLines, "review gate: diffs larger than this (insertions+deletions) escalate; 0 disables. Without this flag, this repo's .argus/config.yml max_diff_lines wins, then this default")
	cmd.Flags().StringSliceVar(&proofRequiredPaths, "proof-required-path", policyDefaults.ProofRequiredPaths, "review gate: a touched path matching one of these (whole word, or path substring if it contains /) needs real-world proof. Without this flag, this repo's .argus/config.yml proof_required_paths wins, then this default")
	cmd.Flags().StringSliceVar(&alwaysReviewPaths, "always-review-path", policyDefaults.AlwaysReviewPaths, "review gate: a touched path matching one of these (whole word, or path substring if it contains /) always escalates, even for a small clean diff. Without this flag, this repo's .argus/config.yml always_review_paths wins, then this default")
	cmd.Flags().StringVar(&verifyCmd, "verify-cmd", "", "review gate: shell command re-run in a worker's worktree once it reaches a terminal phase (e.g. this repo's own lint/build/pre-commit); a non-zero exit is an unwaivable escalation. Empty (default) runs nothing — today's behavior. Without this flag, this repo's .argus/config.yml verify_command wins, then this default")
	cmd.Flags().BoolVar(&review, "review", false, "on gate escalation, run a headless claude -p review instead of only surfacing to you")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for --review (default: claude's default)")
	cmd.Flags().StringVar(&reviewEffort, "review-effort", "", "reasoning effort for --review (low, medium, high, xhigh, max; default: claude's default). Without this flag, this repo's .argus/config.yml review_effort wins, then this default")
	cmd.Flags().IntVar(&reviewConcurrency, "review-concurrency", 0, "max concurrent claude -p --review calls when the gate escalates several workers at once (0 = supervisor.defaultReviewConcurrency)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and exit without creating worktrees or spawning workers")
	cmd.Flags().BoolVar(&noCredProxy, "no-cred-proxy", false, "do not front worker API traffic with the credential proxy; workers inherit the host's real ANTHROPIC_API_KEY")
	cmd.Flags().BoolVar(&attach, "attach", false, "watch workers already running in their worktrees (no spawn); pair with --workspace or --worktrees")
	cmd.Flags().StringVar(&workspace, "workspace", "", "with --attach: attach to every herdr pane in this workspace id, using each pane's directory as a worktree")
	cmd.Flags().StringSliceVar(&worktrees, "worktrees", nil, "with --attach: explicit worktree paths to watch (comma-separated)")
	cmd.Flags().StringVar(&workerRuntime, "worker-runtime", "", "isolate each worker with the argus-runtime-<name> adapter on PATH (see docs/worker-runtime-protocol.md); default none runs unwrapped as today")
	cmd.Flags().StringVar(&workerPlacement, "worker-placement", workerPlacementWorkspace, "where a spawned worker's pane lands: workspace (default, each worker its own top-level herdr workspace) | tab (nest into HERDR_WORKSPACE_ID as a tab, even with --repo passed explicitly) | pane (not yet supported). Without this flag, this repo's .argus/config.yml worker_placement wins, then this default")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "extra Claude Code permission patterns appended to every worker's generated allowlist, on top of this repo's .argus/config.yml allow list if any (e.g. --allow \"Bash(task *)\",\"Bash(npm *)\" for a one-off run)")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	cmd.Flags().StringVar(&forgeKind, "forge", "", "force the forge API shape for a self-hosted host when fetching --issues: \"gitlab\" or \"gitea\" (default: auto-detect, which only recognizes github.com/gitlab.com/codeberg.org and refuses every other host). Without this flag, this repo's .argus/config.yml forge key wins, then auto-detect")
	return cmd
}

// superviseOpts bundles the flags runSupervision needs once workers are already
// resolved (attach vs spawn is decided before it is called), so the constructor's
// RunE can pass them through without runSupervision growing a 15-argument
// signature.
type superviseOpts struct {
	credentialEnv     map[string]string
	reviewModel       string
	reviewEffort      string
	base              string
	launcher          string
	workerRuntime     string
	workerPlacement   string
	reviewNote        string
	verifyCommand     string
	policy            *supervisor.ReviewPolicy
	allow             []string
	repoAllow         []string
	interval          time.Duration
	timeout           time.Duration
	reviewConcurrency int
	attach            bool
	dryRun            bool
	noCredProxy       bool
	review            bool
	repoExplicit      bool
}

// --worker-placement values. workerPlacementPane is accepted so the flag's
// error message can name it explicitly rather than lumping it in with a
// genuinely unknown value — it needs a "worktree open"-equivalent that
// targets an already-split pane, which herdr does not expose today (see
// docs on WorktreeSpec).
const (
	workerPlacementWorkspace = "workspace"
	workerPlacementTab       = "tab"
	workerPlacementPane      = "pane"
)

// parentWorkspace resolves supervisor.Config.ParentWorkspace: nesting a
// worker's worktree pane into the operator's own herdr workspace as a tab
// instead of opening its own new top-level one.
//
// placement "tab" forces nesting outright, even with --repo passed
// explicitly — WorktreeCreate drops --cwd for a nesting call (see
// herdr.Client.WorktreeCreate), so the --workspace/--cwd exclusivity that
// used to force this choice no longer applies. It still requires
// HERDR_WORKSPACE_ID: with no enclosing workspace there is nothing to nest
// into, and silently falling back to a new top-level workspace would make an
// explicit ask silently no-op.
//
// placement "workspace" (including the flag's default, "") keeps the
// original auto-detect behavior: nesting only when --repo was left to
// default, since that was the one case it was ever reachable in before this
// flag existed, and changing that default's behavior out from under existing
// callers is out of scope here.
func parentWorkspace(placement string, repoExplicit bool) (string, error) {
	ws := os.Getenv("HERDR_WORKSPACE_ID")
	switch placement {
	case workerPlacementTab:
		if ws == "" {
			return "", &ui.UserError{
				Err:  fmt.Errorf("--worker-placement tab requires HERDR_WORKSPACE_ID"),
				Hint: "run argus supervise from inside a herdr pane, or drop --worker-placement tab",
			}
		}
		return ws, nil
	case workerPlacementPane:
		return "", &ui.UserError{
			Err:  fmt.Errorf("--worker-placement pane is not implemented yet"),
			Hint: "use --worker-placement tab, or the workspace default; a pane-per-worker mode needs herdr-side support to target an already-split pane",
		}
	case workerPlacementWorkspace, "":
		if repoExplicit {
			return "", nil
		}
		return ws, nil
	default:
		return "", &ui.UserError{
			Err:  fmt.Errorf("unknown --worker-placement %q", placement),
			Hint: "workspace, tab, or pane",
		}
	}
}

// resolveSuperviseBase applies --base > this repo's .argus/config.yml
// base_branch > detected origin/HEAD > the flag's own default ("origin/main"),
// threading the bare branch name repoconfig/DetectDefaultBase both return
// into the "origin/<branch>" ref convention herdr.WorktreeSpec.Base expects
// (unlike rebase/ship's own --base, which is a bare branch name — see
// supervisor.ResolveBase). explicit is cmd.Flags().Changed("base"): an
// operator-passed flag always wins outright, matching ResolveBase's own
// precedence for the same three sources everywhere else they're read. rc is
// a pointer solely to avoid copying the struct at the call site.
func resolveSuperviseBase(ctx context.Context, explicit bool, flagValue, repoRoot string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.BaseBranch != "" {
		return "origin/" + rc.BaseBranch
	}
	if repoRoot != "" {
		if detected, err := supervisor.DetectDefaultBase(ctx, repoRoot); err == nil && detected != "" {
			return "origin/" + detected
		}
	}
	return flagValue
}

// resolveWorkerPlacement applies --worker-placement > this repo's
// .argus/config.yml worker_placement > the flag's own default ("workspace"),
// the same explicit-flag-wins precedence resolveSuperviseBase uses. explicit
// is cmd.Flags().Changed("worker-placement"). The value is validated once,
// downstream in parentWorkspace, so a bad config value fails the same way a
// bad flag value does rather than needing a second check here. rc is a
// pointer solely to avoid copying the struct at the call site.
func resolveWorkerPlacement(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.WorkerPlacement != "" {
		return rc.WorkerPlacement
	}
	return flagValue
}

// resolveReviewEffort applies --review-effort > this repo's .argus/config.yml
// review_effort > the flag's own default (""), the same explicit-flag-wins
// precedence resolveWorkerPlacement uses. explicit is
// cmd.Flags().Changed("review-effort"). rc is a pointer solely to avoid
// copying the struct at the call site.
func resolveReviewEffort(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.ReviewEffort != "" {
		return rc.ReviewEffort
	}
	return flagValue
}

// resolveLauncher applies --launcher > this repo's .argus/config.yml
// launcher > the flag's own default (supervisor.DefaultLauncher), the same
// explicit-flag-wins precedence resolveReviewEffort uses. explicit is
// cmd.Flags().Changed("launcher"). rc is a pointer solely to avoid copying
// the struct at the call site.
func resolveLauncher(explicit bool, flagValue string, rc *repoconfig.Config) string {
	if explicit {
		return flagValue
	}
	if rc.Launcher != "" {
		return rc.Launcher
	}
	return flagValue
}

// applyRepoWorktreeDir sets WorktreeDir on every worker that doesn't already
// carry an explicit Worktree, so BuildPlan's default-worktree derivation
// (internal/supervisor.WorktreePath, only consulted when Worktree is empty)
// honors this repo's .argus/config.yml worktree_dir instead of always
// falling back to .claude/worktrees. --attach's workers already set Worktree
// explicitly (the existing directory being observed), so they pass through
// unchanged.
func applyRepoWorktreeDir(workers []supervisor.Worker, worktreeDir string) {
	for i := range workers {
		if workers[i].Worktree == "" {
			workers[i].WorktreeDir = worktreeDir
		}
	}
}

// runSupervision builds the *supervisor.Config for an already-resolved worker
// set, then either hands off to supervisor.Attach (--attach: no isolation is
// argus's to manage) or starts the credential proxy for a live spawn and hands
// off to supervisor.Run. Split out of newSuperviseCmd's RunE so the constructor
// stays flag registration plus a thin dispatcher, and so this half — the
// credential-proxy and reviewer wiring — is independently testable.
func runSupervision(cmd *cobra.Command, client herdr.Client, workers []supervisor.Worker, o *superviseOpts) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home dir: %w", err)
	}

	logger, closeLog := openRunLog(cmd, "supervise")
	defer closeLog()

	parentWS, err := parentWorkspace(o.workerPlacement, o.repoExplicit)
	if err != nil {
		return err
	}

	cfg := &supervisor.Config{
		Out:               cmd.OutOrStdout(),
		Now:               time.Now,
		Client:            client,
		Log:               logger,
		Base:              o.base,
		Home:              home,
		Launcher:          o.launcher,
		ParentWorkspace:   parentWS,
		ScrubEnv:          append(forge.StandardTokenVars(), credential.ScrubVars(o.credentialEnv)...),
		Interval:          o.interval,
		Timeout:           o.timeout,
		ReviewConcurrency: o.reviewConcurrency,
		WorkerRuntime:     o.workerRuntime,
		RepoAllow:         o.repoAllow,
		ExtraAllow:        o.allow,
		Policy:            o.policy,
		ReviewNote:        o.reviewNote,
		VerifyCommand:     o.verifyCommand,
	}
	if o.review {
		cfg.Reviewer = supervisor.NewCLIReviewer(o.reviewModel, o.reviewEffort).WithLog(logger)
	}

	// --attach only watches workers that are already running; it never calls
	// execute() (the only place cfg.Broker.WorkerEnv is used or a runtime
	// adapter is invoked), so it needs no credential proxy and applies no
	// isolation of its own. Warn so the operator knows an attached worker's
	// isolation is whatever it was started with, not argus-managed, then
	// return before the credproxy block starts one for nothing.
	if o.attach {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s --attach does not manage isolation: an attached worker keeps whatever credential proxy and runtime adapter (if any) it was started with\n",
			ui.LabelWarning.Render("○"))
		return supervisor.Attach(cmd.Context(), cfg, workers)
	}

	// Front the workers' API traffic with a credential proxy so a worker is
	// not handed the real key in its own environment. It runs only for a
	// live spawn (a dry run spawns nothing) and only when some known agent
	// key actually resolves (see startCredentialProxy/credproxy.Registry) —
	// when none does (e.g. subscription/OAuth, no key to swap), the proxy
	// stays off. For an unwrapped worker (no runtime adapter) that is still
	// fine — it reaches the host's own credentials (e.g. ~/.claude) directly
	// and gets no credential isolation, but it works. An isolated worker has
	// no such fallback; see the check below. --no-cred-proxy opts out
	// entirely.
	if !o.dryRun && !o.noCredProxy {
		proxy, extraScrub, cleanup, err := startCredentialProxy(logger, o.credentialEnv)
		if err != nil {
			return err
		}
		defer cleanup()
		if proxy != nil {
			cfg.Broker = proxy
			cfg.ScrubEnv = append(cfg.ScrubEnv, extraScrub...)
		}
	}

	// A runtime adapter's whole point is that the worker's filesystem does not
	// contain ~/.claude (see docs/worker-runtime-protocol.md), so unlike the
	// unwrapped path above, an isolated worker has no fallback credential
	// source: if cfg.Broker is still nil here (no known agent key resolved —
	// see credproxy.Registry/startCredentialProxy — or --no-cred-proxy), the
	// worker gets nothing at all and fails deep inside the container with a
	// bare "Not logged in". Fail fast at the one place that knows
	// both facts at once, instead of letting that surprise happen mid-run.
	if !o.dryRun && cfg.Broker == nil && o.workerRuntime != "" && o.workerRuntime != "none" {
		return &ui.UserError{
			Err:  fmt.Errorf("--worker-runtime %s has no credential path: no known agent key resolved (e.g. ANTHROPIC_API_KEY is unset, or --no-cred-proxy was passed), and an isolated worker cannot reach the host's own credentials", o.workerRuntime),
			Hint: fmt.Sprintf("export ANTHROPIC_API_KEY=... (or the key for whichever agent --launcher runs) to bridge credentials via credproxy, or drop --worker-runtime %s to run unwrapped (shares the host's credentials directly)", o.workerRuntime),
		}
	}

	return supervisor.Run(cmd.Context(), cfg, workers, o.dryRun)
}

// attachWorkers resolves --attach targets into workers without creating anything.
// Targets come from explicit --worktrees paths and/or every pane in --workspace
// (each pane's cwd is a worktree). Each worktree's branch is read from git and its
// task from the typed status file, falling back to the branch name, so the report
// is labeled without the operator restating what the worker already recorded.
func attachWorkers(ctx context.Context, client herdr.Client, workspace string, worktrees []string) ([]supervisor.Worker, error) {
	type target struct{ worktree, paneID string }
	var targets []target
	for _, wt := range worktrees {
		// --worktrees is --worktree-shaped like --repo/rebase's/review's/ship's
		// flags, so it gets the same resolution even though an audit for
		// relative-path bugs (an unresolved relative path silently breaking a
		// downstream `cd` or git -C call) found this particular path
		// unreachable today (Attach skips execute() entirely) — fixed
		// defensively anyway, since that class of bug shouldn't need a live
		// repro to be worth closing off.
		abs, err := supervisor.ResolveWorktree(wt)
		if err != nil {
			return nil, fmt.Errorf("resolving --worktrees entry %q: %w", wt, err)
		}
		targets = append(targets, target{worktree: abs})
	}
	if workspace != "" {
		panes, err := client.PaneList(ctx)
		if err != nil {
			return nil, err
		}
		for i := range panes {
			if panes[i].WorkspaceID == workspace && panes[i].Cwd != "" {
				targets = append(targets, target{worktree: panes[i].Cwd, paneID: panes[i].PaneID})
			}
		}
	}
	if len(targets) == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("no attach targets"),
			Hint: "argus supervise --attach --workspace <id>   (or --attach --worktrees p1,p2)",
		}
	}

	workers := make([]supervisor.Worker, 0, len(targets))
	for _, t := range targets {
		branch, err := supervisor.CurrentBranch(ctx, t.worktree)
		if err != nil {
			return nil, &ui.UserError{
				Err:  fmt.Errorf("resolving branch for %s: %w", t.worktree, err),
				Hint: "an --attach worktree must be a git checkout that already exists",
			}
		}
		task := branch
		if s, lerr := protocol.Load(protocol.StatusPath(t.worktree)); lerr == nil && s.Task != "" {
			task = s.Task
		}
		workers = append(workers, supervisor.Worker{Task: task, Branch: branch, Worktree: t.worktree, PaneID: t.paneID})
	}
	return workers, nil
}

var superviseCmd = newSuperviseCmd()

type workerInput struct {
	repo      string
	tasksFile string
	panes     []string
	branches  []string
	labels    []string
	tasks     []string
}

// jiraSpawnOpts holds the optional pre-spawn Jira behavior --jira-issues can
// trigger before a worker starts: claiming the ticket for whoever is running
// this (assignToCaller) and/or moving it into an in-progress-shaped status
// (transition) — mirroring ship's post-ship --jira-assignee/--jira-transition
// but on the other end of the worker lifecycle (see jiraIssuesToTasks).
type jiraSpawnOpts struct {
	transition     string
	assignToCaller bool
}

// spawnWorkers resolves the spawn-mode inputs into workers: it requires at least
// one worker source, defaults --repo to the working directory, folds any --issues
// and --jira-issues into tasks/branches by fetching them from the forge or Jira,
// then pairs the slices. It is the non-attach half of supervise, kept out of RunE
// so each mode reads flat. forgeKindFlag/forgeKindExplicit are --forge's raw
// value and whether it was actually passed, needed only for --issues (the one
// forge.New call this path makes, to fetch issue bodies before any worker
// exists).
func spawnWorkers(ctx context.Context, client herdr.Client, in *workerInput, issues []int, jiraIssues []string, credentialOverrides map[string]string, jiraSpawn jiraSpawnOpts, forgeKindFlag string, forgeKindExplicit bool) ([]supervisor.Worker, error) {
	if len(in.panes) == 0 && len(in.branches) == 0 && len(in.tasks) == 0 && in.tasksFile == "" && len(issues) == 0 && len(jiraIssues) == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("no workers given"),
			Hint: "argus supervise --tasks x,y --branches feat-x,feat-y [--repo <path>]  (or --tasks-file path, --issues n,n, --jira-issues KEY,KEY, or --attach --workspace <id>)",
		}
	}
	if err := resolveSpawnRepo(in); err != nil {
		return nil, err
	}
	if in.tasksFile != "" {
		fileTasks, err := loadTasksFile(in.tasksFile)
		if err != nil {
			return nil, err
		}
		in.tasks = append(in.tasks, fileTasks...)
	}
	if err := foldIssueSources(ctx, in, issues, jiraIssues, credentialOverrides, jiraSpawn, forgeKindFlag, forgeKindExplicit); err != nil {
		return nil, err
	}
	return buildWorkers(ctx, client, in)
}

// resolveSpawnRepo defaults in.repo to the working directory (only needed
// when the spawn isn't --panes-attach, which carries its own repo per pane)
// and resolves whatever repo ends up set to an absolute worktree path.
func resolveSpawnRepo(in *workerInput) error {
	if in.repo == "" && len(in.panes) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolving working directory: %w", err)
		}
		in.repo = wd
	}
	if in.repo != "" {
		abs, err := supervisor.ResolveWorktree(in.repo)
		if err != nil {
			return err
		}
		in.repo = abs
	}
	return nil
}

// loadTasksFile reads --tasks-file into one task per line. --tasks goes through
// pflag's CSV parsing, which chokes on commas and unescaped quotes in free-text
// prose (`bare " in non-quoted-field`); a tasks file sidesteps that entirely by
// only ever splitting on newlines, so a multi-sentence brief with punctuation
// survives untouched as long as it stays on one line.
func loadTasksFile(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-supplied --tasks-file, a deliberate local file read, not remote input
	if err != nil {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("reading --tasks-file %s: %w", path, err),
			Hint: "pass a path to a file with one task per line",
		}
	}
	var tasks []string
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		tasks = append(tasks, line)
	}
	if len(tasks) == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("--tasks-file %s has no non-empty lines", path),
			Hint: "each line becomes one worker's task",
		}
	}
	return tasks, nil
}

// foldIssueSources turns --issues and --jira-issues into worker briefs and
// appends them to in.tasks/in.branches, so the operator never hand-writes a
// task string for an issue that already has a title and body. Generated
// branches merge into the positional slots the fetched tasks land at (see
// mergeFetchedBranches), so an explicit --branches shorter than the total
// worker count — e.g. covering earlier --tasks workers only — still leaves
// the issue workers with their normal <repo>-fix-issue-N default instead of
// falling through to defaultBranch's slug of the entire fetched task body.
// Split out of spawnWorkers to keep each source's fetch-and-fold step
// independently testable and readable. forgeKindFlag/forgeKindExplicit only
// matter to the --issues path (see resolveIssueForgeKind).
func foldIssueSources(ctx context.Context, in *workerInput, issues []int, jiraIssues []string, credentialOverrides map[string]string, jiraSpawn jiraSpawnOpts, forgeKindFlag string, forgeKindExplicit bool) error {
	// --issues fetches from the repo's forge (GitHub, GitLab, or Codeberg/Gitea).
	if len(issues) > 0 {
		kind, err := resolveIssueForgeKind(ctx, in.repo, forgeKindExplicit, forgeKindFlag)
		if err != nil {
			return err
		}
		fetched, brs, err := tasksFromIssues(ctx, in.repo, issues, credentialOverrides, kind)
		if err != nil {
			return err
		}
		preCount := len(in.tasks)
		in.tasks = append(in.tasks, fetched...)
		in.branches = mergeFetchedBranches(in.branches, preCount, brs)
	}
	// --jira-issues works the same way but reads from Jira Cloud instead, since
	// Jira is an issue tracker with no git-host concept to resolve from the
	// origin remote.
	if len(jiraIssues) > 0 {
		fetched, brs, err := jiraTasksFromIssues(ctx, in.repo, jiraIssues, jiraSpawn)
		if err != nil {
			return err
		}
		preCount := len(in.tasks)
		in.tasks = append(in.tasks, fetched...)
		in.branches = mergeFetchedBranches(in.branches, preCount, brs)
	}
	return nil
}

// mergeFetchedBranches merges a fetched issue source's default branches into
// in.branches at the positional slots its tasks were just appended at
// (preCount..preCount+len(fetched)), padding with "" as needed rather than
// only merging when in.branches started out empty. That gate used to mean an
// explicit --branches covering earlier --tasks workers (so len(in.branches)
// != 0) silently dropped the fetched defaults for every --issues/--jira-issues
// worker after it, leaving buildWorkers to fall through to defaultBranch and
// slug the entire fetched task body into a branch name. A slot that already
// holds an explicit branch (from --branches covering that same position) is
// left untouched, so explicit still wins position-for-position.
func mergeFetchedBranches(branches []string, preCount int, fetched []string) []string {
	for len(branches) < preCount+len(fetched) {
		branches = append(branches, "")
	}
	for i, b := range fetched {
		if j := preCount + i; branches[j] == "" {
			branches[j] = b
		}
	}
	return branches
}

// buildWorkers resolves the paired flag slices into concrete workers. In the
// default mode each worker gets a fresh worktree and runs in the pane herdr opens
// there (PaneID left empty); in --panes mode existing panes are reused and their
// current directory supplies the repo root unless --repo pins one. The worker
// count is driven by --panes if given, else by the longer of --branches/--tasks.
// --labels is paired the same way but never drives the count; an empty Worker.Label
// leaves the default (task-derived, falling back to branch) to supervisor.BuildPlan.
func buildWorkers(ctx context.Context, client herdr.Client, in *workerInput) ([]supervisor.Worker, error) {
	n := len(in.panes)
	if n == 0 {
		n = max(len(in.branches), len(in.tasks))
	}
	if n == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("no workers given"),
			Hint: "argus supervise --tasks x,y --branches feat-x,feat-y",
		}
	}

	cwdByPane, err := paneCwds(ctx, client, in.panes)
	if err != nil {
		return nil, err
	}

	workers := make([]supervisor.Worker, 0, n)
	for i := range n {
		pane := at(in.panes, i)

		repoRoot := in.repo
		if repoRoot == "" {
			if pane != "" {
				repoRoot = cwdByPane[pane]
			} else {
				return nil, &ui.UserError{
					Err:  fmt.Errorf("--repo is required when not using --panes"),
					Hint: "argus supervise --repo <path> --tasks x --branches feat-x",
				}
			}
		}

		task := at(in.tasks, i)
		branch := at(in.branches, i)
		if branch != "" && !validBranch(branch) {
			return nil, &ui.UserError{
				Err:  fmt.Errorf("unsafe branch name %q", branch),
				Hint: "branches become worktree paths and shell arguments; use only letters, digits, . _ - /",
			}
		}
		if branch == "" {
			branch = defaultBranch(pane, task, i)
		}
		if task == "" {
			task = defaultTask(pane, i)
		}

		workers = append(workers, supervisor.Worker{
			Task:     task,
			Branch:   branch,
			Label:    at(in.labels, i),
			RepoRoot: repoRoot,
			PaneID:   pane,
		})
	}
	return workers, nil
}

// paneCwds resolves the current directory of each named pane from herdr. It only
// hits herdr when --panes were given, so the default (auto-pane) mode makes no
// pane-list call.
func paneCwds(ctx context.Context, client herdr.Client, panes []string) (map[string]string, error) {
	if len(panes) == 0 {
		return map[string]string{}, nil
	}
	live, err := client.PaneList(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]string, len(live))
	for i := range live {
		byID[live[i].PaneID] = live[i].Cwd
	}
	for _, p := range panes {
		if _, ok := byID[p]; !ok {
			return nil, &ui.UserError{Err: fmt.Errorf("pane %s not found", p), Hint: "herdr pane list"}
		}
	}
	return byID, nil
}

func defaultBranch(pane, task string, i int) string {
	switch {
	case pane != "":
		return "argus-" + strings.ReplaceAll(pane, ":", "-")
	case task != "":
		return "argus-" + slug(task)
	default:
		return fmt.Sprintf("argus-worker-%d", i+1)
	}
}

func defaultTask(pane string, i int) string {
	if pane != "" {
		return pane
	}
	return fmt.Sprintf("worker-%d", i+1)
}

// slug turns a task label into a branch-safe fragment, collapsing runs of
// separators into a single dash.
func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// tasksFromIssues resolves the repo's forge from its origin remote, then fetches
// each issue and renders it into a worker brief. It works for GitHub, GitLab, and
// Codeberg/Gitea-family hosts without extra flags, plus a self-hosted GitLab or
// Gitea/Forgejo host when kind says which shape it is.
func tasksFromIssues(ctx context.Context, repoPath string, issues []int, credentialOverrides map[string]string, kind forge.Kind) (tasks, branches []string, err error) {
	f, owner, name, err := resolveForge(ctx, repoPath, credentialOverrides, kind)
	if err != nil {
		return nil, nil, err
	}
	return issuesToTasks(ctx, f, owner, name, repoPath, issues)
}

// resolveIssueForgeKind applies --forge > this repo's .argus/config.yml forge
// key > auto-detect, for the one forge.New call --issues makes to fetch issue
// bodies before any worker exists. Unlike the rest of supervise's repo config
// (loaded later, in newSuperviseCmd's RunE, from the first resolved worker's
// RepoRoot), this needs to run earlier, so it resolves its own repo root and
// config here — best-effort, matching supervisor.ResolveBase's own lookup: a
// repo path that doesn't resolve, or has no config file, just falls through
// to auto-detect.
func resolveIssueForgeKind(ctx context.Context, repoPath string, explicit bool, flagValue string) (forge.Kind, error) {
	var configValue string
	if repoRoot, err := supervisor.RepoRoot(ctx, repoPath); err == nil {
		if rc, err := repoconfig.Load(repoconfig.Path(repoRoot)); err == nil {
			configValue = rc.Forge
		}
	}
	return parseForgeKind(resolveForgeKindValue(explicit, flagValue, configValue))
}

// repoBriefNote reads this repo's optional .argus/config.yml brief_note (see
// internal/repoconfig), best-effort: a missing or unreadable config file just
// means no note to append, not a hard failure of task generation.
func repoBriefNote(repoPath string) string {
	rc, err := repoconfig.Load(repoconfig.Path(repoPath))
	if err != nil {
		return ""
	}
	return rc.BriefNote
}

// fixedBriefTail appends argus's own non-negotiable ship-pipeline invariants
// after an optional repo-supplied brief_note. Unlike brief_note (toolchain
// flavor a repo owner opts into via config, e.g. "keep make ci green"), these
// are argus's own pipeline contract — ship phase owns commit/push, and the
// under-report gate compares the worker's self-reported diff_stat against
// MeasureDiff's ground truth (internal/supervisor/measure.go) which counts
// untracked new files as added lines — so they always apply, not something a
// repo owner can disable.
//
// The lint/build/pre-commit sentence below closes the other half of the same
// gap a configured verify_command closes on the gate side (see
// resolveVerifyCommand): `argus ship`'s `git commit` runs whatever hooks the
// target repo has wired up (e.g. lefthook running golangci-lint), so a diff
// that never ran them locally can earn a clean gate verdict and still fail
// at commit time. It stays deliberately toolchain-agnostic — argus has no
// opinion on what "the repo's own lint/build" means for a given repo, only
// that a worker should run whatever that is before calling itself done.
func fixedBriefTail(briefNote string) string {
	const fixed = "Do NOT git commit or push; argus ships. " +
		"When reporting your diff size, count untracked new files too " +
		"(e.g. `git diff --stat <base>` plus every new file's own line count) " +
		"— a plain `git diff` alone misses files you just created, and argus's " +
		"own measurement does not. Before reporting a terminal phase " +
		"(awaiting_review or blocked), run this repo's own lint/build/pre-commit " +
		"checks, if any (e.g. a Makefile/Taskfile target, lefthook/husky hooks, " +
		"golangci-lint), and fix anything they flag — argus ship's `git commit` " +
		"runs the same pre-commit hooks, so a failure you don't catch here " +
		"surfaces there instead."
	if briefNote == "" {
		return fixed
	}
	return briefNote + " " + fixed
}

// resolveForge detects the forge host and owner/repo from a repo path's origin
// remote and returns an authenticated client. credentialOverrides maps a forge
// host to an alternate env var name that takes priority over argus's built-in
// token var list (see internal/credential and --credential-env); it may be nil.
// kind picks the API shape for a self-hosted host outside forge.New's
// auto-detected allowlist (see resolveIssueForgeKind).
func resolveForge(ctx context.Context, repoPath string, credentialOverrides map[string]string, kind forge.Kind) (f forge.Forge, owner, name string, err error) {
	remote, err := supervisor.RemoteURL(ctx, repoPath)
	if err != nil {
		return nil, "", "", err
	}
	host, owner, name, err := forge.Detect(remote)
	if err != nil {
		return nil, "", "", err
	}
	token := forge.TokenForHost(host, credentialOverrides)
	if token == "" {
		return nil, "", "", &ui.UserError{
			Err:  fmt.Errorf("no API token for %s (needed to fetch issues)", host),
			Hint: "set the token env var for this host (e.g. CODEBERG_TOKEN, GITHUB_TOKEN, or GITLAB_TOKEN), or run `gh auth login` / `glab auth login`",
		}
	}
	f, err = forge.New(host, token, nil, kind)
	if err != nil {
		return nil, "", "", err
	}
	return f, owner, name, nil
}

// issuesToTasks renders each issue into a worker brief and a default branch
// name. It takes the forge as a parameter so it is testable without a
// network. repoPath resolves this repo's optional brief_note (see
// repoBriefNote) — argus itself supplies no toolchain-flavored text of its
// own when no config is present, only the fixed "don't commit" invariant.
func issuesToTasks(ctx context.Context, f forge.Forge, owner, name, repoPath string, issues []int) (tasks, branches []string, err error) {
	tail := fixedBriefTail(repoBriefNote(repoPath))
	for _, n := range issues {
		iss, ferr := f.FetchIssue(ctx, owner, name, n)
		if ferr != nil {
			return nil, nil, fmt.Errorf("fetching issue #%d: %w", n, ferr)
		}
		tasks = append(tasks, fmt.Sprintf(
			"Fix %s/%s issue #%d: %s\n\n%s\n\n%s",
			owner, name, n, iss.Title, iss.Body, tail))
		branches = append(branches, fmt.Sprintf("%s-fix-issue-%d", name, n))
	}
	return tasks, branches, nil
}

// jiraIssueFetcher is the subset of *jira.Client that jiraIssuesToTasks needs
// to build a brief, so it is testable without a network.
type jiraIssueFetcher interface {
	FetchIssue(ctx context.Context, key string) (forge.Issue, error)
}

// jiraSpawnClient extends jiraIssueFetcher with the write calls
// jiraIssuesToTasks needs for its optional pre-spawn assign/transition step
// (see jiraSpawnOpts) — kept separate from jiraIssueFetcher so a test
// exercising only the brief-building path can stub the smaller interface.
type jiraSpawnClient interface {
	jiraIssueFetcher
	Assign(ctx context.Context, key, accountID string) error
	Transition(ctx context.Context, key, idOrName string) error
	Myself(ctx context.Context) (string, error)
}

// jiraTasksFromIssues builds a Jira client from JIRA_BASE_URL, JIRA_EMAIL, and
// JIRA_API_TOKEN and fetches each key. Unlike tasksFromIssues this does not go
// through internal/forge or the origin remote: Jira is an issue tracker, not a
// git host, so there is no owner/repo or PR concept to resolve.
func jiraTasksFromIssues(ctx context.Context, repoPath string, keys []string, jiraSpawn jiraSpawnOpts) (tasks, branches []string, err error) {
	c, err := jira.NewFromEnv(nil)
	if err != nil {
		return nil, nil, &ui.UserError{
			Err:  err,
			Hint: "set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN, or write them to a JSON config file at $JIRA_CONFIG_FILE or ~/.argus/jira.json, to fetch --jira-issues",
		}
	}
	return jiraIssuesToTasks(ctx, c, repoPath, repoBranchPrefix(ctx, repoPath), keys, jiraSpawn)
}

// repoBranchPrefix names the git repo at repoPath for branch prefixing, so
// issue-driven spawns across multiple repos in the same org don't collide on
// a bare "fix-issue-42". It tries the origin remote first (matching the repo
// name a human would use, e.g. "argus" from Elysium-Labs-EU/argus) and falls
// back to the checkout's directory name when there is no remote to read —
// Jira has no owner/repo of its own to resolve this from.
func repoBranchPrefix(ctx context.Context, repoPath string) string {
	if remote, err := supervisor.RemoteURL(ctx, repoPath); err == nil {
		if _, _, name, err := forge.Detect(remote); err == nil && name != "" {
			return name
		}
	}
	return filepath.Base(repoPath)
}

// jiraIssuesToTasks renders each Jira issue into a worker brief and a default
// branch name, mirroring issuesToTasks for the git-forge issue pipeline. It
// also runs the optional pre-spawn Jira hook (jiraSpawn) before a worker for
// that issue starts, so the ticket shows a claimed signal on the board for
// its whole worker lifetime rather than only after ship's post-ship hook
// runs (see postShipJira in cmd/ship.go). Unlike that post-ship hook, a
// failure here aborts the spawn instead of degrading to a warning — the PR
// doesn't exist yet, so there is still something to protect: an operator
// finding out the claim never took before workers pile onto an
// apparently-unclaimed ticket. repoPath resolves this repo's optional
// brief_note the same way issuesToTasks does.
func jiraIssuesToTasks(ctx context.Context, c jiraSpawnClient, repoPath, branchPrefix string, keys []string, jiraSpawn jiraSpawnOpts) (tasks, branches []string, err error) {
	tail := fixedBriefTail(repoBriefNote(repoPath))

	var callerAccountID string
	if jiraSpawn.assignToCaller {
		callerAccountID, err = c.Myself(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving caller's Jira account for --jira-assign-on-spawn: %w", err)
		}
	}

	for _, key := range keys {
		iss, ferr := c.FetchIssue(ctx, key)
		if ferr != nil {
			return nil, nil, fmt.Errorf("fetching jira issue %s: %w", key, ferr)
		}
		if jiraSpawn.assignToCaller {
			if aerr := c.Assign(ctx, key, callerAccountID); aerr != nil {
				return nil, nil, fmt.Errorf("assigning jira issue %s to caller: %w", key, aerr)
			}
		}
		if jiraSpawn.transition != "" {
			if terr := c.Transition(ctx, key, jiraSpawn.transition); terr != nil {
				return nil, nil, fmt.Errorf("transitioning jira issue %s to %q: %w", key, jiraSpawn.transition, terr)
			}
		}
		tasks = append(tasks, fmt.Sprintf(
			"Fix Jira issue %s: %s\n\n%s\n\n%s",
			key, iss.Title, iss.Body, tail))
		branches = append(branches, fmt.Sprintf("%s-fix-%s", branchPrefix, strings.ToLower(key)))
	}
	return tasks, branches, nil
}

// validBranch accepts only branch names safe to embed in a worktree path and a
// shell command: letters, digits, and . _ - /, with no leading dash. It rejects
// spaces and shell metacharacters (e.g. feat$(cmd), a b) before they ever reach a
// filesystem path or the pane's shell.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") {
		return false
	}
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/':
		default:
			return false
		}
	}
	return true
}
