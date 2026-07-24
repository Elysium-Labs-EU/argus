package cmd

import (
	"context"
	"fmt"
	"os"
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
		panes             []string
		branches          []string
		labels            []string
		tasks             []string
		tasksFile         string
		repo              string
		base              string
		launcher          string
		sharedGlobs       []string
		osGlobs           []string
		reviewGlobs       []string
		reviewModel       string
		review            bool
		maxDiffLines      int
		interval          time.Duration
		timeout           time.Duration
		reviewConcurrency int
		issues            []int
		jiraIssues        []string
		dryRun            bool
		noCredProxy       bool
		attach            bool
		workspace         string
		worktrees         []string
		workerRuntime     string
		allow             []string
		credentialEnv     map[string]string
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
				}, issues, jiraIssues, overrides)
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

			resolvedBase := resolveSuperviseBase(cmd.Context(), cmd.Flags().Changed("base"), base, repoRoot, rc)

			return runSupervision(cmd, client, workers, &superviseOpts{
				attach: attach, dryRun: dryRun, noCredProxy: noCredProxy,
				base: resolvedBase, launcher: launcher, workerRuntime: workerRuntime,
				interval: interval, timeout: timeout,
				review: review, reviewModel: reviewModel, reviewConcurrency: reviewConcurrency,
				maxDiffLines: maxDiffLines, sharedGlobs: sharedGlobs, osGlobs: osGlobs, reviewGlobs: reviewGlobs,
				allow: allow, repoAllow: rc.Allow, credentialEnv: overrides, repoExplicit: repo != "",
			})
		},
	}

	cmd.Flags().IntSliceVar(&issues, "issues", nil, "issue numbers to fetch from the repo's forge and turn into worker briefs (branch defaults to fix-issue-<n>)")
	cmd.Flags().StringSliceVar(&jiraIssues, "jira-issues", nil, "Jira issue keys (e.g. PROJ-123) to fetch and turn into worker briefs (branch defaults to fix-<key>); requires JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN, or a JSON config file (see jira.Config) at $JIRA_CONFIG_FILE or ~/.argus/jira.json")
	cmd.Flags().StringSliceVar(&tasks, "tasks", nil, "task/issue per worker (comma-separated); drives worker count in the default mode")
	cmd.Flags().StringVar(&tasksFile, "tasks-file", "", "path to a file with one task per line, appended after --tasks; unlike --tasks this is not CSV-parsed, so commas and quotes in a free-text brief are safe")
	cmd.Flags().StringSliceVar(&branches, "branches", nil, "branch per worker, paired positionally (default argus-<task-slug>)")
	cmd.Flags().StringSliceVar(&labels, "labels", nil, "herdr workspace label per worker, paired positionally (default: derived from --tasks, falling back to the branch)")
	cmd.Flags().StringSliceVar(&panes, "panes", nil, "reuse these existing herdr panes instead of the worktree's own pane")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root for all workers (default cwd; or each pane's directory in --panes mode)")
	cmd.Flags().StringVar(&base, "base", "origin/main", "base ref new worktrees branch from; required with --attach (no default applies — argus does not know what an attached worktree actually branched from). Without --base, this repo's .argus/config.yml base_branch wins, then the detected origin/HEAD, then this default")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in each worker pane after cd into its worktree")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "how often to poll each worker's status file")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "per-worker wall-clock deadline before argus stops waiting on it (0 = wait indefinitely)")
	cmd.Flags().IntVar(&maxDiffLines, "max-diff-lines", policyDefaults.MaxDiffLines, "review gate: diffs larger than this (insertions+deletions) escalate; 0 disables")
	cmd.Flags().StringSliceVar(&sharedGlobs, "shared-glob", nil, "review gate: path substrings that always require review (shared/prod surface)")
	cmd.Flags().StringSliceVar(&osGlobs, "os-glob", policyDefaults.OSPathGlobs, "review gate: path substrings whose change requires real-world proof")
	cmd.Flags().StringSliceVar(&reviewGlobs, "always-review-glob", policyDefaults.AlwaysReviewGlobs, "review gate: behavior-critical path words that always escalate, even for a small clean diff")
	cmd.Flags().BoolVar(&review, "review", false, "on gate escalation, run a headless claude -p review instead of only surfacing to you")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for --review (default: claude's default)")
	cmd.Flags().IntVar(&reviewConcurrency, "review-concurrency", 0, "max concurrent claude -p --review calls when the gate escalates several workers at once (0 = supervisor.defaultReviewConcurrency)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and exit without creating worktrees or spawning workers")
	cmd.Flags().BoolVar(&noCredProxy, "no-cred-proxy", false, "do not front worker API traffic with the credential proxy; workers inherit the host's real ANTHROPIC_API_KEY")
	cmd.Flags().BoolVar(&attach, "attach", false, "watch workers already running in their worktrees (no spawn); pair with --workspace or --worktrees")
	cmd.Flags().StringVar(&workspace, "workspace", "", "with --attach: attach to every herdr pane in this workspace id, using each pane's directory as a worktree")
	cmd.Flags().StringSliceVar(&worktrees, "worktrees", nil, "with --attach: explicit worktree paths to watch (comma-separated)")
	cmd.Flags().StringVar(&workerRuntime, "worker-runtime", "", "isolate each worker with the argus-runtime-<name> adapter on PATH (see docs/worker-runtime-protocol.md); default none runs unwrapped as today")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "extra Claude Code permission patterns appended to every worker's generated allowlist, on top of this repo's .argus/config.yml allow list if any (e.g. --allow \"Bash(task *)\",\"Bash(npm *)\" for a one-off run)")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	return cmd
}

// superviseOpts bundles the flags runSupervision needs once workers are already
// resolved (attach vs spawn is decided before it is called), so the constructor's
// RunE can pass them through without runSupervision growing a 15-argument
// signature.
type superviseOpts struct {
	credentialEnv     map[string]string
	reviewModel       string
	base              string
	launcher          string
	workerRuntime     string
	osGlobs           []string
	sharedGlobs       []string
	allow             []string
	repoAllow         []string
	reviewGlobs       []string
	interval          time.Duration
	timeout           time.Duration
	maxDiffLines      int
	reviewConcurrency int
	attach            bool
	dryRun            bool
	noCredProxy       bool
	review            bool
	repoExplicit      bool
}

// parentWorkspace resolves supervisor.Config.ParentWorkspace: nesting a
// worker's worktree pane into the operator's own workspace only when --repo
// was left to default, since an explicit --repo must be passed to herdr as
// --cwd, and herdr's worktree-create rejects --workspace and --cwd together.
func parentWorkspace(repoExplicit bool) string {
	if repoExplicit {
		return ""
	}
	return os.Getenv("HERDR_WORKSPACE_ID")
}

// resolveSuperviseBase applies --base > this repo's .argus/config.yml
// base_branch > detected origin/HEAD > the flag's own default ("origin/main"),
// threading the bare branch name repoconfig/DetectDefaultBase both return
// into the "origin/<branch>" ref convention herdr.WorktreeSpec.Base expects
// (unlike rebase/ship's own --base, which is a bare branch name — see
// supervisor.ResolveBase). explicit is cmd.Flags().Changed("base"): an
// operator-passed flag always wins outright, matching ResolveBase's own
// precedence for the same three sources everywhere else they're read.
func resolveSuperviseBase(ctx context.Context, explicit bool, flagValue, repoRoot string, rc repoconfig.Config) string {
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

	cfg := &supervisor.Config{
		Out:      cmd.OutOrStdout(),
		Now:      time.Now,
		Client:   client,
		Log:      logger,
		Base:     o.base,
		Home:     home,
		Launcher: o.launcher,
		// HERDR_WORKSPACE_ID is herdr's own env var naming the pane argus itself
		// is running in, when it is running inside a herdr pane at all (e.g.
		// headless/CI invocations have no such pane and see it unset). Reading
		// it here nests every worker this invocation spawns as a tab in the
		// operator's own workspace instead of a disconnected new one, with no
		// flag required for the common interactive case.
		//
		// herdr's own worktree-create usage documents --workspace and --cwd as
		// mutually exclusive. WorktreeCreate always sends --cwd (the repo a
		// worker's worktree derives from), so an explicit --repo — which names
		// that repo directly — must win outright rather than being layered on
		// top of workspace auto-detection; nesting stays a same-repo-only
		// convenience for the case where --repo was left to default.
		ParentWorkspace:   parentWorkspace(o.repoExplicit),
		ScrubEnv:          append(forge.StandardTokenVars(), credential.ScrubVars(o.credentialEnv)...),
		Interval:          o.interval,
		Timeout:           o.timeout,
		ReviewConcurrency: o.reviewConcurrency,
		WorkerRuntime:     o.workerRuntime,
		RepoAllow:         o.repoAllow,
		ExtraAllow:        o.allow,
		Policy: &supervisor.ReviewPolicy{
			MaxDiffLines:      o.maxDiffLines,
			SharedGlobs:       o.sharedGlobs,
			OSPathGlobs:       o.osGlobs,
			AlwaysReviewGlobs: o.reviewGlobs,
		},
	}
	if o.review {
		cfg.Reviewer = supervisor.NewCLIReviewer(o.reviewModel).WithLog(logger)
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
	// bare "Not logged in" (issue #57). Fail fast at the one place that knows
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
		// flags, so it gets the same resolution even though a repo-wide sweep
		// for issue #96 found this particular path unreachable from that bug
		// today (Attach skips execute() entirely) — fixed defensively anyway,
		// since this is exactly the class of bug that shouldn't need a live
		// repro to be worth closing off (argus issue #98).
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

// spawnWorkers resolves the spawn-mode inputs into workers: it requires at least
// one worker source, defaults --repo to the working directory, folds any --issues
// and --jira-issues into tasks/branches by fetching them from the forge or Jira,
// then pairs the slices. It is the non-attach half of supervise, kept out of RunE
// so each mode reads flat.
func spawnWorkers(ctx context.Context, client herdr.Client, in *workerInput, issues []int, jiraIssues []string, credentialOverrides map[string]string) ([]supervisor.Worker, error) {
	if len(in.panes) == 0 && len(in.branches) == 0 && len(in.tasks) == 0 && in.tasksFile == "" && len(issues) == 0 && len(jiraIssues) == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("no workers given"),
			Hint: "argus supervise --tasks x,y --branches feat-x,feat-y [--repo <path>]  (or --tasks-file path, --issues n,n, --jira-issues KEY,KEY, or --attach --workspace <id>)",
		}
	}
	if in.repo == "" && len(in.panes) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving working directory: %w", err)
		}
		in.repo = wd
	}
	if in.repo != "" {
		abs, err := supervisor.ResolveWorktree(in.repo)
		if err != nil {
			return nil, err
		}
		in.repo = abs
	}
	if in.tasksFile != "" {
		fileTasks, err := loadTasksFile(in.tasksFile)
		if err != nil {
			return nil, err
		}
		in.tasks = append(in.tasks, fileTasks...)
	}
	if err := foldIssueSources(ctx, in, issues, jiraIssues, credentialOverrides); err != nil {
		return nil, err
	}
	return buildWorkers(ctx, client, in)
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
// branches only fill in.branches when it is still empty, so explicit
// --branches always wins. Split out of spawnWorkers to keep each source's
// fetch-and-fold step independently testable and readable.
func foldIssueSources(ctx context.Context, in *workerInput, issues []int, jiraIssues []string, credentialOverrides map[string]string) error {
	// --issues fetches from the repo's forge (GitHub, GitLab, or Codeberg/Gitea).
	if len(issues) > 0 {
		fetched, brs, err := tasksFromIssues(ctx, in.repo, issues, credentialOverrides)
		if err != nil {
			return err
		}
		in.tasks = append(in.tasks, fetched...)
		if len(in.branches) == 0 {
			in.branches = brs
		}
	}
	// --jira-issues works the same way but reads from Jira Cloud instead, since
	// Jira is an issue tracker with no git-host concept to resolve from the
	// origin remote.
	if len(jiraIssues) > 0 {
		fetched, brs, err := jiraTasksFromIssues(ctx, in.repo, jiraIssues)
		if err != nil {
			return err
		}
		in.tasks = append(in.tasks, fetched...)
		if len(in.branches) == 0 {
			in.branches = brs
		}
	}
	return nil
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
// Codeberg/Gitea-family hosts without extra flags.
func tasksFromIssues(ctx context.Context, repoPath string, issues []int, credentialOverrides map[string]string) (tasks, branches []string, err error) {
	f, owner, name, err := resolveForge(ctx, repoPath, credentialOverrides)
	if err != nil {
		return nil, nil, err
	}
	return issuesToTasks(ctx, f, owner, name, repoPath, issues)
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

// fixedBriefTail appends argus's own non-negotiable ship-pipeline invariant
// after an optional repo-supplied brief_note. Unlike brief_note (toolchain
// flavor a repo owner opts into via config, e.g. "keep make ci green"), "don't
// commit, argus ships" is argus's own pipeline contract — ship phase owns
// commit/push, the worker must not — so it always applies, not something a
// repo owner can disable.
func fixedBriefTail(briefNote string) string {
	const fixed = "Do NOT git commit or push; argus ships."
	if briefNote == "" {
		return fixed
	}
	return briefNote + " " + fixed
}

// resolveForge detects the forge host and owner/repo from a repo path's origin
// remote and returns an authenticated client. credentialOverrides maps a forge
// host to an alternate env var name that takes priority over argus's built-in
// token var list (see internal/credential and --credential-env); it may be nil.
func resolveForge(ctx context.Context, repoPath string, credentialOverrides map[string]string) (f forge.Forge, owner, name string, err error) {
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
	return forge.New(host, token, nil), owner, name, nil
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
		branches = append(branches, fmt.Sprintf("fix-issue-%d", n))
	}
	return tasks, branches, nil
}

// jiraIssueFetcher is the subset of *jira.Client that jiraIssuesToTasks needs,
// so it is testable without a network.
type jiraIssueFetcher interface {
	FetchIssue(ctx context.Context, key string) (forge.Issue, error)
}

// jiraTasksFromIssues builds a Jira client from JIRA_BASE_URL, JIRA_EMAIL, and
// JIRA_API_TOKEN and fetches each key. Unlike tasksFromIssues this does not go
// through internal/forge or the origin remote: Jira is an issue tracker, not a
// git host, so there is no owner/repo or PR concept to resolve.
func jiraTasksFromIssues(ctx context.Context, repoPath string, keys []string) (tasks, branches []string, err error) {
	c, err := jira.NewFromEnv(nil)
	if err != nil {
		return nil, nil, &ui.UserError{
			Err:  err,
			Hint: "set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN, or write them to a JSON config file at $JIRA_CONFIG_FILE or ~/.argus/jira.json, to fetch --jira-issues",
		}
	}
	return jiraIssuesToTasks(ctx, c, repoPath, keys)
}

// jiraIssuesToTasks renders each Jira issue into a worker brief and a default
// branch name, mirroring issuesToTasks for the git-forge issue pipeline.
// repoPath resolves this repo's optional brief_note the same way
// issuesToTasks does — Jira is only the issue tracker here, the tasks it
// produces still run against this same repo checkout.
func jiraIssuesToTasks(ctx context.Context, c jiraIssueFetcher, repoPath string, keys []string) (tasks, branches []string, err error) {
	tail := fixedBriefTail(repoBriefNote(repoPath))
	for _, key := range keys {
		iss, ferr := c.FetchIssue(ctx, key)
		if ferr != nil {
			return nil, nil, fmt.Errorf("fetching jira issue %s: %w", key, ferr)
		}
		tasks = append(tasks, fmt.Sprintf(
			"Fix Jira issue %s: %s\n\n%s\n\n%s",
			key, iss.Title, iss.Body, tail))
		branches = append(branches, fmt.Sprintf("fix-%s", strings.ToLower(key)))
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
