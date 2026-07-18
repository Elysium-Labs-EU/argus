// Package supervisor is the deterministic core of argus: it discovers herdr
// panes, enforces one worktree per worker, spawns workers in auto mode, and
// learns each worker's state by reading its typed status file — never by
// scraping terminal scrollback. No LLM sits in this loop; the three judgment
// points (permission prompts, diff correctness, test adequacy) are surfaced to
// the human in this cut, not automated.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	ScrubEnv []string // env vars withheld from each worker (e.g. forge tokens it never needs)
	Interval time.Duration
	Timeout  time.Duration // per-worker wall-clock deadline; 0 = wait indefinitely
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

// taskLabel is a short, log-friendly identifier for a worker task: the first
// "#<n>" issue reference when present, otherwise the first line trimmed to 60
// characters. It keeps the run log and `argus stats` keyed by something readable
// instead of the entire multi-line brief.
func taskLabel(task string) string {
	if i := strings.IndexByte(task, '#'); i >= 0 {
		j := i + 1
		for j < len(task) && task[j] >= '0' && task[j] <= '9' {
			j++
		}
		if j > i+1 {
			return task[i:j]
		}
	}
	line := task
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	line = strings.TrimSpace(line)
	if len(line) > 60 {
		line = strings.TrimSpace(line[:60])
	}
	return line
}

// DefaultLauncher is the agent argus starts in each worker pane.
const DefaultLauncher = "claude --permission-mode auto"

// initialPrompt is the one-line prompt argus passes to the launcher. It points the
// worker at its brief file rather than pasting a multi-line brief into the TUI —
// a real agent would submit that at the first newline.
const initialPrompt = "Read .claude/argus/brief.md and follow it exactly; it is your task brief."

// SpawnCommand is the shell line argus runs in a worker's pane: cd into the
// worktree, then start the launcher with the initial prompt as its argument. The
// worktree path is single-quoted because it is data (a path that may contain
// spaces, and whose final segment is a branch name): interpolating it raw let a
// branch like feat$(cmd) inject into the pane's shell. launcher is argus-owned
// config (DefaultLauncher or --launcher) and is left unquoted so a smoke test can
// pass a multi-word command.
//
// scrubEnv names environment variables to withhold from the launcher via `env
// -u`, so a secret the pane inherited from the host (a forge token the worker
// never needs) is not present in the worker agent's environment or any child it
// spawns. The names are argus-owned identifiers, not user data, so they are
// emitted unquoted. An empty list yields the plain command unchanged.
func SpawnCommand(worktree, launcher string, scrubEnv []string) string {
	if launcher == "" {
		launcher = DefaultLauncher
	}
	var prefix strings.Builder
	if len(scrubEnv) > 0 {
		prefix.WriteString("env")
		for _, v := range scrubEnv {
			prefix.WriteString(" -u ")
			prefix.WriteString(v)
		}
		prefix.WriteByte(' ')
	}
	return fmt.Sprintf("cd %s && %s%s %q", shellQuote(worktree), prefix.String(), launcher, initialPrompt)
}

// shellQuote wraps s in single quotes for POSIX shells, escaping any embedded
// single quote, so the whole string is treated as one literal argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		renderPlan(cfg.Out, cfg.Base, cfg.Launcher, cfg.ScrubEnv, plans)
		return nil
	}

	states, err := execute(ctx, cfg, plans)
	if err != nil {
		return err
	}
	return superviseStates(ctx, cfg, states)
}

// superviseStates runs the observe→judge→report tail shared by a fresh spawn
// (Run) and an attach to already-running workers (Attach): watch each worker's
// typed status until a terminal phase, measure its real diff from git, gate or
// escalate, and print the report. No terminal scrollback is read in any step.
func superviseStates(ctx context.Context, cfg *Config, states []*workerState) error {
	watch(ctx, cfg, states)
	reconcile(ctx, cfg, states)
	reviewEscalations(ctx, cfg, states)
	renderReport(ctx, cfg, states)
	return nil
}

// Attach supervises workers that are already running in their worktrees: it
// creates no worktree and spawns no agent, it only watches each worker's typed
// status file and takes it through the same gate and report as a fresh run. This
// is how an operator brings a worker argus did not launch — one started by hand,
// or grinding on an existing PR branch — under the same deterministic observation
// instead of eyeballing its pane scrollback.
func Attach(ctx context.Context, cfg *Config, workers []Worker) error {
	plans := BuildPlan(workers)
	if err := EnsureDistinctWorktrees(worktreePaths(plans)); err != nil {
		return err
	}
	states := make([]*workerState, len(plans))
	for i := range plans {
		states[i] = &workerState{plan: &plans[i], paneID: plans[i].PaneID, started: cfg.Now()}
		cfg.Log.Action("attach", taskLabel(plans[i].Task), "watching", plans[i].Worktree)
	}
	return superviseStates(ctx, cfg, states)
}

// reconcile computes each worker's real diff from git so the gate and report use
// ground truth rather than the worker's self-report. A measurement failure is
// recorded (and later surfaced) rather than silently trusting status.json.
func reconcile(ctx context.Context, cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile {
			continue
		}
		ds, files, err := MeasureDiff(ctx, st.plan.Worktree, cfg.Base)
		if err != nil {
			st.diffErr = err
			cfg.Log.Fail("measure_diff", st.plan.Task, err)
			continue
		}
		st.measured = ds
		st.measuredFiles = files
		st.measuredOK = true
	}
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
		verdict := gateVerdict(st, cfg.Policy)
		if verdict.AutoApprove {
			cfg.Log.Action("gate", st.plan.Task, "auto-approve", "")
			recordApproval(cfg, st, true, "gate", "auto-approved: clean within policy", nil)
			continue
		}
		cfg.Log.Action("gate", st.plan.Task, "escalate", strings.Join(verdict.Reasons, "; "))

		// The gate escalated. Only spend an LLM review when one is configured;
		// otherwise the escalation is surfaced to the human — not approved.
		if cfg.Reviewer == nil {
			recordApproval(cfg, st, false, "gate", "escalated, awaiting human decision", verdict.Reasons)
			continue
		}
		diff, err := DiffFor(ctx, st.plan.Worktree, cfg.Base)
		if err != nil {
			st.reviewErr = err
			cfg.Log.Fail("review", st.plan.Task, err)
			recordApproval(cfg, st, false, "gate", "review could not run: "+err.Error(), verdict.Reasons)
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
			recordApproval(cfg, st, false, "gate", "review errored: "+err.Error(), verdict.Reasons)
			continue
		}
		st.review = &res
		cfg.Log.Action("review", st.plan.Task, res.Decision, res.Summary)
		recordApproval(cfg, st, res.Decision == "approve", "review", res.Summary, res.Findings)
	}
}

// recordApproval writes the worker's disposition to its worktree so ship can
// enforce it, and logs a verdict event for the run log. A write failure is logged
// but does not abort the run — ship will then see "no verdict" and refuse, which
// is the safe default.
func recordApproval(cfg *Config, st *workerState, approved bool, source, summary string, reasons []string) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	a := protocol.Approval{
		Approved:  approved,
		Source:    source,
		Summary:   summary,
		Reasons:   reasons,
		UpdatedAt: now(),
	}
	outcome := "approved"
	if !approved {
		outcome = "not-approved"
	}
	cfg.Log.Action("verdict", st.plan.Task, outcome, summary)
	if err := protocol.WriteApproval(st.plan.Worktree, &a); err != nil {
		cfg.Log.Fail("verdict_write", st.plan.Task, err)
	}
}

func worktreePaths(plans []WorkerPlan) []string {
	paths := make([]string, len(plans))
	for i := range plans {
		paths[i] = plans[i].Worktree
	}
	return paths
}

// workerState tracks one worker across the watch loop. status is what the worker
// reported; measured is the ground truth argus computed from git. The gate trusts
// measured, not status, for diff size and files touched.
type workerState struct {
	started       time.Time
	reviewErr     error
	diffErr       error
	plan          *WorkerPlan
	review        *ReviewResult
	paneID        string
	measuredFiles []string
	status        protocol.Status
	measured      protocol.DiffStat
	hasFile       bool
	measuredOK    bool
}

// effective returns the status the gate should judge: the worker's reported phase,
// tests, and proof, but with diff size and files-touched replaced by argus's own
// measurement when it succeeded. This is the trust boundary — self-report is a
// hint; git is the truth.
func (st *workerState) effective() protocol.Status {
	s := st.status
	if st.measuredOK {
		s.DiffStat = st.measured
		s.FilesTouched = st.measuredFiles
	}
	return s
}

func execute(ctx context.Context, cfg *Config, plans []WorkerPlan) ([]*workerState, error) {
	states := make([]*workerState, len(plans))

	// If spawning aborts partway, the workers already launched are live agents in
	// their panes. Report them as orphaned so the operator can stop or reuse them,
	// rather than leaving them running invisibly.
	fail := func(i int, err error) ([]*workerState, error) {
		for j := 0; j < i; j++ {
			if states[j] != nil {
				cfg.Log.Action("orphaned", taskLabel(states[j].plan.Task), "spawn-aborted", states[j].paneID)
			}
		}
		return nil, err
	}

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
			return fail(i, fmt.Errorf("creating worktree for %s: %w", p.Task, err))
		}
		if err := WriteSettings(p.Worktree); err != nil {
			return fail(i, fmt.Errorf("writing settings for %s: %w", p.Task, err))
		}
		if err := WriteBrief(p.Worktree, p.Brief); err != nil {
			return fail(i, fmt.Errorf("writing brief for %s: %w", p.Task, err))
		}

		// Prefer a caller-supplied pane; otherwise run the worker in the pane
		// herdr opened inside the new worktree — no separate --panes needed.
		paneID := p.PaneID
		if paneID == "" {
			paneID = wt.RootPaneID
		}
		if paneID == "" {
			return fail(i, fmt.Errorf("worker %s has no pane and herdr returned no root pane for its worktree", p.Task))
		}

		// One launch: cd + start the agent with a prompt that points it at the
		// brief file. No second paste — the brief is on disk, not typed in.
		if err := cfg.Client.PaneRun(ctx, paneID, SpawnCommand(p.Worktree, cfg.Launcher, cfg.ScrubEnv)); err != nil {
			cfg.Log.Fail("spawn", taskLabel(p.Task), err)
			return fail(i, fmt.Errorf("spawning worker for %s: %w", p.Task, err))
		}
		cfg.Log.Action("spawn", taskLabel(p.Task), "ok", paneID)

		states[i] = &workerState{plan: p, paneID: paneID, started: cfg.Now()}
	}
	return states, nil
}

// watch polls every worker's status file until it reaches a terminal phase, its
// deadline passes, or ctx is canceled. One goroutine per worker; a timer (not
// time.After) drives the poll so we don't leak a timer per tick. The deadline is
// what stops a hung or dead worker from blocking the whole run forever.
func watch(ctx context.Context, cfg *Config, states []*workerState) {
	var wg sync.WaitGroup
	for _, st := range states {
		wg.Add(1)
		go func(st *workerState) {
			defer wg.Done()
			pollStatus(ctx, cfg.Interval, cfg.Timeout, cfg.Log, st)
		}(st)
	}
	wg.Wait()
}

func pollStatus(ctx context.Context, interval, timeout time.Duration, log *eventlog.Logger, st *workerState) {
	path := protocol.StatusPath(st.plan.Worktree)

	// A per-worker wall-clock deadline: without it a worker that dies in a
	// non-terminal phase (crash, exit, never writes awaiting_review) would hang
	// watch/wg.Wait indefinitely. 0 disables the deadline.
	var deadline <-chan time.Time
	if timeout > 0 {
		dt := time.NewTimer(timeout)
		defer dt.Stop()
		deadline = dt.C
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			log.Action("watch", st.plan.Task, "canceled", string(st.status.Phase))
			return
		case <-deadline:
			log.Action("watch", st.plan.Task, "timeout", string(st.status.Phase))
			return
		case <-timer.C:
			s, err := protocol.Load(path)
			switch {
			case err == nil:
				st.status = s
				st.hasFile = true
				lastErr = ""
				if protocol.IsTerminal(s.Phase) {
					log.Action("phase", st.plan.Task, string(s.Phase), s.BlockedReason)
					return
				}
			case !errors.Is(err, os.ErrNotExist) && err.Error() != lastErr:
				// A malformed (not merely absent) status file was silently
				// swallowed before, so a lying/broken writer looked identical to
				// one that never started. Log it once per distinct error.
				lastErr = err.Error()
				log.Fail("status_unreadable", st.plan.Task, err)
			}
			timer.Reset(interval)
		}
	}
}
