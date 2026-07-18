// Package supervisor is the deterministic core of argus: it discovers herdr
// panes, enforces one worktree per worker, spawns workers in auto mode, and
// learns each worker's state by reading its typed status file — never by
// scraping terminal scrollback. No LLM sits in this loop; the three judgment
// points (permission prompts, diff correctness, test adequacy) are surfaced to
// the human in this cut, not automated.
package supervisor

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codeberg.org/Elysium_Labs/argus/internal/eventlog"
	"codeberg.org/Elysium_Labs/argus/internal/herdr"
	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

// Worker is one supervised task. PaneID, when set, names an existing pane to run
// the worker in; when empty, argus splits a new pane. Worktree, when empty, is
// derived from RepoRoot and Branch.
type Worker struct {
	Task     string
	Branch   string
	RepoRoot string
	Worktree string
	PaneID   string
}

// Config carries the dependencies and knobs for a supervise run. Everything
// effectful (herdr, the clock, the home dir, output) enters here so the loop
// stays testable.
type Config struct {
	Out      io.Writer
	Now      func() time.Time
	Policy   *ReviewPolicy
	Reviewer Reviewer
	Log      *eventlog.Logger
	Client   herdr.Client
	Base     string
	Home     string
	Launcher string
	Interval time.Duration
}

// WorkerPlan is the fully-resolved intent for one worker: the concrete worktree
// path, the permission settings that will be written, and the brief that will be
// injected. It is pure data — BuildPlan computes it without side effects so the
// dry-run can print exactly what a real run would do.
type WorkerPlan struct {
	Worker
	Brief    string
	Settings permissionSettings
}

// BuildPlan resolves each worker into a concrete plan. Missing worktree paths are
// derived as <repo>/.claude/worktrees/<branch>; each brief is the task text plus
// the shared status-writing contract so writer and reader can't drift.
func BuildPlan(workers []Worker) []WorkerPlan {
	plans := make([]WorkerPlan, len(workers))
	for i := range workers {
		w := workers[i]
		if w.Worktree == "" {
			w.Worktree = filepath.Join(w.RepoRoot, ".claude", "worktrees", w.Branch)
		}
		plans[i] = WorkerPlan{
			Worker:   w,
			Settings: settingsFor(w.Worktree),
			Brief:    briefFor(&w),
		}
	}
	return plans
}

func briefFor(w *Worker) string {
	return fmt.Sprintf(`Task: %s
Branch: %s

Work only inside %s. Never delete, reset, or touch files outside it; another
agent may share the parent repo. Write a todo list before anything else.

Do the work and verify it (build + tests). Do NOT git commit or git push — argus
handles shipping. When the change is complete and tests pass, set your status
phase to "awaiting_review" (not "done"); use "blocked" if you need a decision only
the supervisor can make.

%s`, w.Task, w.Branch, w.Worktree, protocol.WriterBrief)
}

// DefaultLauncher is the agent argus starts in each worker pane.
const DefaultLauncher = "claude --permission-mode auto"

// initialPrompt is the one-line prompt argus passes to the launcher. It points the
// worker at its brief file rather than pasting a multi-line brief into the TUI —
// a real agent would submit that at the first newline.
const initialPrompt = "Read .claude/argus/brief.md and follow it exactly; it is your task brief."

// spawnCommand is the shell line argus runs in a worker's pane: cd into the
// worktree, then start the launcher with the initial prompt as its argument.
// launcher is configurable so a smoke test can point argus at a cheap shell
// instead of a full agent (the shell simply ignores the prompt argument).
func spawnCommand(worktree, launcher string) string {
	if launcher == "" {
		launcher = DefaultLauncher
	}
	return fmt.Sprintf("cd %s && %s %q", worktree, launcher, initialPrompt)
}

// Run is the whole deterministic supervise loop. In dry-run it prints the plan
// and makes no changes. Otherwise it enforces distinct worktrees, spawns each
// worker, watches their status files until every one reaches a terminal phase or
// ctx is canceled, then prints a metrics report.
func Run(ctx context.Context, cfg *Config, workers []Worker, dryRun bool) error {
	plans := BuildPlan(workers)

	if err := EnsureDistinctWorktrees(worktreePaths(plans)); err != nil {
		return err
	}

	if dryRun {
		renderPlan(cfg.Out, cfg.Base, cfg.Launcher, plans)
		return nil
	}

	states, err := execute(ctx, cfg, plans)
	if err != nil {
		return err
	}
	watch(ctx, cfg, states)
	reviewEscalations(ctx, cfg, states)
	renderReport(ctx, cfg, states)
	return nil
}

// reviewEscalations runs the LLM reviewer on exactly the workers the deterministic
// gate could not clear — the risky minority. No reviewer configured (or a clean
// gate verdict) means no call, so the LLM cost tracks the escalation rate, not the
// worker count.
func reviewEscalations(ctx context.Context, cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		verdict := Assess(&st.status, cfg.Policy)
		if verdict.AutoApprove {
			cfg.Log.Action("gate", st.plan.Task, "auto-approve", "")
			continue
		}
		cfg.Log.Action("gate", st.plan.Task, "escalate", strings.Join(verdict.Reasons, "; "))

		// The gate escalated. Only spend an LLM review when one is configured;
		// otherwise the escalation is surfaced to the human in the report.
		if cfg.Reviewer == nil {
			continue
		}
		diff, err := DiffFor(ctx, st.plan.Worktree, cfg.Base)
		if err != nil {
			st.reviewErr = err
			cfg.Log.Fail("review", st.plan.Task, err)
			continue
		}
		res, err := cfg.Reviewer.Review(ctx, &ReviewRequest{
			Task:     st.plan.Task,
			Branch:   st.plan.Branch,
			Worktree: st.plan.Worktree,
			Diff:     diff,
			Reasons:  verdict.Reasons,
		})
		if err != nil {
			st.reviewErr = err
			cfg.Log.Fail("review", st.plan.Task, err)
			continue
		}
		st.review = &res
		cfg.Log.Action("review", st.plan.Task, res.Decision, res.Summary)
	}
}

func worktreePaths(plans []WorkerPlan) []string {
	paths := make([]string, len(plans))
	for i := range plans {
		paths[i] = plans[i].Worktree
	}
	return paths
}

// workerState tracks one worker across the watch loop.
type workerState struct {
	started   time.Time
	plan      *WorkerPlan
	review    *ReviewResult
	reviewErr error
	paneID    string
	status    protocol.Status
	hasFile   bool
}

func execute(ctx context.Context, cfg *Config, plans []WorkerPlan) ([]*workerState, error) {
	states := make([]*workerState, len(plans))
	for i := range plans {
		p := &plans[i]

		wt, err := cfg.Client.WorktreeCreate(ctx, &herdr.WorktreeSpec{
			Cwd:    p.RepoRoot,
			Branch: p.Branch,
			Base:   cfg.Base,
			Path:   p.Worktree,
			Label:  p.Branch,
		})
		if err != nil {
			return nil, fmt.Errorf("creating worktree for %s: %w", p.Task, err)
		}
		if err := WriteSettings(p.Worktree); err != nil {
			return nil, fmt.Errorf("writing settings for %s: %w", p.Task, err)
		}
		if err := WriteBrief(p.Worktree, p.Brief); err != nil {
			return nil, fmt.Errorf("writing brief for %s: %w", p.Task, err)
		}

		// Prefer a caller-supplied pane; otherwise run the worker in the pane
		// herdr opened inside the new worktree — no separate --panes needed.
		paneID := p.PaneID
		if paneID == "" {
			paneID = wt.RootPaneID
		}
		if paneID == "" {
			return nil, fmt.Errorf("worker %s has no pane and herdr returned no root pane for its worktree", p.Task)
		}

		// One launch: cd + start the agent with a prompt that points it at the
		// brief file. No second paste — the brief is on disk, not typed in.
		if err := cfg.Client.PaneRun(ctx, paneID, spawnCommand(p.Worktree, cfg.Launcher)); err != nil {
			cfg.Log.Fail("spawn", p.Task, err)
			return nil, fmt.Errorf("spawning worker for %s: %w", p.Task, err)
		}
		cfg.Log.Action("spawn", p.Task, "ok", paneID)

		states[i] = &workerState{plan: p, paneID: paneID, started: cfg.Now()}
	}
	return states, nil
}

// watch polls every worker's status file until it reaches a terminal phase or
// ctx is canceled. One goroutine per worker; a timer (not time.After) drives
// the poll so we don't leak a timer per tick.
func watch(ctx context.Context, cfg *Config, states []*workerState) {
	var wg sync.WaitGroup
	for _, st := range states {
		wg.Add(1)
		go func(st *workerState) {
			defer wg.Done()
			pollStatus(ctx, cfg.Interval, cfg.Log, st)
		}(st)
	}
	wg.Wait()
}

func pollStatus(ctx context.Context, interval time.Duration, log *eventlog.Logger, st *workerState) {
	path := protocol.StatusPath(st.plan.Worktree)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Action("watch", st.plan.Task, "canceled", string(st.status.Phase))
			return
		case <-timer.C:
			if s, err := protocol.Load(path); err == nil {
				st.status = s
				st.hasFile = true
				if protocol.IsTerminal(s.Phase) {
					log.Action("phase", st.plan.Task, string(s.Phase), s.BlockedReason)
					return
				}
			}
			timer.Reset(interval)
		}
	}
}
