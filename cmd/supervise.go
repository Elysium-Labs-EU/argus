package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"codeberg.org/Elysium_Labs/argus/internal/forge"
	"codeberg.org/Elysium_Labs/argus/internal/herdr"
	"codeberg.org/Elysium_Labs/argus/internal/supervisor"
	"codeberg.org/Elysium_Labs/argus/internal/ui"
)

func newSuperviseCmd() *cobra.Command {
	var (
		panes        []string
		branches     []string
		tasks        []string
		repo         string
		base         string
		launcher     string
		sharedGlobs  []string
		osGlobs      []string
		reviewGlobs  []string
		reviewModel  string
		review       bool
		maxDiffLines int
		interval     time.Duration
		timeout      time.Duration
		issues       []int
		dryRun       bool
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
			if len(panes) == 0 && len(branches) == 0 && len(tasks) == 0 {
				return &ui.UserError{
					Err:  fmt.Errorf("no workers given"),
					Hint: "argus supervise --tasks x,y --branches feat-x,feat-y [--repo <path>]",
				}
			}

			if repo == "" && len(panes) == 0 {
				wd, werr := os.Getwd()
				if werr != nil {
					return fmt.Errorf("resolving working directory: %w", werr)
				}
				repo = wd
			}

			// --issues turns issue numbers into worker briefs by fetching each
			// issue's title and body from the repo's forge, so the operator never
			// hand-writes a task string. Generated tasks/branches append to any
			// given explicitly.
			if len(issues) > 0 {
				fetched, brs, ierr := tasksFromIssues(cmd.Context(), repo, issues)
				if ierr != nil {
					return ierr
				}
				tasks = append(tasks, fetched...)
				if len(branches) == 0 {
					branches = brs
				}
			}

			client := herdr.New()
			workers, err := buildWorkers(cmd.Context(), client, &workerInput{
				panes:    panes,
				branches: branches,
				tasks:    tasks,
				repo:     repo,
			})
			if err != nil {
				return err
			}

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
				Base:     base,
				Home:     home,
				Launcher: launcher,
				Interval: interval,
				Timeout:  timeout,
				Policy: &supervisor.ReviewPolicy{
					MaxDiffLines:      maxDiffLines,
					SharedGlobs:       sharedGlobs,
					OSPathGlobs:       osGlobs,
					AlwaysReviewGlobs: reviewGlobs,
				},
			}
			if review {
				cfg.Reviewer = supervisor.NewCLIReviewer(reviewModel).WithLog(logger)
			}
			return supervisor.Run(cmd.Context(), cfg, workers, dryRun)
		},
	}

	cmd.Flags().IntSliceVar(&issues, "issues", nil, "issue numbers to fetch from the repo's forge and turn into worker briefs (branch defaults to fix-issue-<n>)")
	cmd.Flags().StringSliceVar(&tasks, "tasks", nil, "task/issue per worker (comma-separated); drives worker count in the default mode")
	cmd.Flags().StringSliceVar(&branches, "branches", nil, "branch per worker, paired positionally (default argus-<task-slug>)")
	cmd.Flags().StringSliceVar(&panes, "panes", nil, "reuse these existing herdr panes instead of the worktree's own pane")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root for all workers (default cwd; or each pane's directory in --panes mode)")
	cmd.Flags().StringVar(&base, "base", "origin/main", "base ref new worktrees branch from")
	cmd.Flags().StringVar(&launcher, "launcher", supervisor.DefaultLauncher, "command started in each worker pane after cd into its worktree")
	cmd.Flags().DurationVar(&interval, "interval", 15*time.Second, "how often to poll each worker's status file")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "per-worker wall-clock deadline before argus stops waiting on it (0 = wait indefinitely)")
	cmd.Flags().IntVar(&maxDiffLines, "max-diff-lines", policyDefaults.MaxDiffLines, "review gate: diffs larger than this (insertions+deletions) escalate; 0 disables")
	cmd.Flags().StringSliceVar(&sharedGlobs, "shared-glob", nil, "review gate: path substrings that always require review (shared/prod surface)")
	cmd.Flags().StringSliceVar(&osGlobs, "os-glob", policyDefaults.OSPathGlobs, "review gate: path substrings whose change requires real-world proof")
	cmd.Flags().StringSliceVar(&reviewGlobs, "always-review-glob", policyDefaults.AlwaysReviewGlobs, "review gate: behavior-critical path words that always escalate, even for a small clean diff")
	cmd.Flags().BoolVar(&review, "review", false, "on gate escalation, run a headless claude -p review instead of only surfacing to you")
	cmd.Flags().StringVar(&reviewModel, "review-model", "", "model for --review (default: claude's default)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and exit without creating worktrees or spawning workers")
	return cmd
}

var superviseCmd = newSuperviseCmd()

type workerInput struct {
	repo     string
	panes    []string
	branches []string
	tasks    []string
}

// buildWorkers resolves the paired flag slices into concrete workers. In the
// default mode each worker gets a fresh worktree and runs in the pane herdr opens
// there (PaneID left empty); in --panes mode existing panes are reused and their
// current directory supplies the repo root unless --repo pins one. The worker
// count is driven by --panes if given, else by the longer of --branches/--tasks.
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
// each issue and renders it into a worker brief. It works for Codeberg or GitHub
// without extra flags.
func tasksFromIssues(ctx context.Context, repoPath string, issues []int) (tasks, branches []string, err error) {
	f, owner, name, err := resolveForge(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}
	return issuesToTasks(ctx, f, owner, name, issues)
}

// resolveForge detects the forge host and owner/repo from a repo path's origin
// remote and returns an authenticated client.
func resolveForge(ctx context.Context, repoPath string) (f forge.Forge, owner, name string, err error) {
	remote, err := supervisor.RemoteURL(ctx, repoPath)
	if err != nil {
		return nil, "", "", err
	}
	host, owner, name, err := forge.Detect(remote)
	if err != nil {
		return nil, "", "", err
	}
	token := forge.TokenForHost(host)
	if token == "" {
		return nil, "", "", &ui.UserError{
			Err:  fmt.Errorf("no API token for %s (needed to fetch issues)", host),
			Hint: "set the token env var for this host (e.g. CODEBERG_TOKEN or GITHUB_TOKEN)",
		}
	}
	return forge.New(host, token, nil), owner, name, nil
}

// issuesToTasks renders each issue into a worker brief and a default branch name.
// It takes the forge as a parameter so it is testable without a network.
func issuesToTasks(ctx context.Context, f forge.Forge, owner, name string, issues []int) (tasks, branches []string, err error) {
	for _, n := range issues {
		iss, ferr := f.FetchIssue(ctx, owner, name, n)
		if ferr != nil {
			return nil, nil, fmt.Errorf("fetching issue #%d: %w", n, ferr)
		}
		tasks = append(tasks, fmt.Sprintf(
			"Fix %s/%s issue #%d: %s\n\n%s\n\nAdd a focused test and keep make ci green. Follow the repo STYLE.md. Do NOT git commit or push; argus ships.",
			owner, name, n, iss.Title, iss.Body))
		branches = append(branches, fmt.Sprintf("fix-issue-%d", n))
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
