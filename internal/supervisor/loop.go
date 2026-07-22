// Package supervisor is the deterministic core of argus: it discovers herdr
// panes, enforces one worktree per worker, spawns workers in auto mode, and
// learns each worker's state by reading its typed status file — never by
// scraping terminal scrollback. No LLM sits in this loop; the three judgment
// points (permission prompts, diff correctness, test adequacy) are surfaced to
// the human in this cut, not automated.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
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

// CredentialBroker mints per-worker credentials so a worker never holds a real
// API key. WorkerEnv registers the worker (identified by agent label and its
// branch) and returns the environment assignments — a phantom sentinel plus a
// proxied base URL — that route the worker's API traffic through argus. A nil
// Broker means no proxy: workers inherit the host's real credentials, which is
// the prior behavior.
type CredentialBroker interface {
	WorkerEnv(agent, branch string) []string
}

// Config carries the dependencies and knobs for a supervise run. Everything
// effectful (herdr, the clock, the home dir, output) enters here so the loop
// stays testable.
type Config struct {
	Reviewer Reviewer
	Out      io.Writer
	Broker   CredentialBroker
	Now      func() time.Time
	Log      *eventlog.Logger
	Policy   *ReviewPolicy
	Client   herdr.Client
	Base     string
	Home     string
	Launcher string
	// WorkerRuntime names a worker-runtime adapter (see
	// docs/worker-runtime-protocol.md): argus execs argus-runtime-<name> to
	// isolate the worker instead of running it directly in the host shell.
	// Empty (or "none") means today's unwrapped behavior — the default, so
	// existing installs are unaffected until an operator opts in.
	WorkerRuntime string
	ScrubEnv      []string // env vars withheld from each worker (e.g. forge tokens it never needs)
	// ExtraAllow appends operator-supplied permission patterns (e.g.
	// "Bash(task *)", "Bash(npm *)") to every worker's generated allowlist, on
	// top of the Go/make defaults settingsFor always includes. This is how a
	// repo whose mandated command runner isn't make (task, npm, etc.) avoids a
	// permission prompt on every invocation.
	ExtraAllow []string
	Interval   time.Duration
	Timeout    time.Duration // per-worker wall-clock deadline; 0 = wait indefinitely
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
// the shared status-writing contract so writer and reader can't drift. extraAllow
// is forwarded to settingsFor so every worker's allowlist reflects the same
// operator-supplied extension the dry-run preview shows.
func BuildPlan(workers []Worker, extraAllow []string) []WorkerPlan {
	plans := make([]WorkerPlan, len(workers))
	for i := range workers {
		w := workers[i]
		if w.Worktree == "" {
			w.Worktree = filepath.Join(w.RepoRoot, ".claude", "worktrees", w.Branch)
		}
		plans[i] = WorkerPlan{
			Worker:   w,
			Settings: settingsFor(w.Worktree, extraAllow),
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

// InitialPrompt is the one-line prompt argus passes to the launcher. It points the
// worker at its brief file rather than pasting a multi-line brief into the TUI —
// a real agent would submit that at the first newline.
const InitialPrompt = "Read .claude/argus/brief.md and follow it exactly; it is your task brief."

// ResolveLauncherPath rewrites launcher's first (whitespace-separated) token
// — the binary name — to its absolute path via a PATH lookup on argus's own
// process, leaving the rest of the command (flags, args) untouched. It falls
// back to returning launcher unchanged when that lookup fails.
//
// A freshly opened worker pane's shell may not have finished initializing its
// own PATH (slow rc-file startup: nvm, fnm, plugin managers, prompt segments
// that shell out, ...) by the moment argus types the launch command into it —
// PaneRun runs right after the pane opens, with no readiness wait. If the
// launcher binary transiently isn't found via the new shell's own PATH, a
// shell with spelling-correction enabled (oh-my-zsh's default) can offer a
// correction and then block forever on an unanswered interactive prompt,
// rather than failing loudly. argus's own PATH is already fully initialized
// by the time this runs, so resolving the binary here and splicing in the
// absolute path sidesteps that race for the one PATH-dependent token in the
// launch line.
func ResolveLauncherPath(launcher string) string {
	fields := strings.Fields(launcher)
	if len(fields) == 0 {
		return launcher
	}
	resolved, err := exec.LookPath(fields[0])
	if err != nil {
		return launcher
	}
	rest := strings.TrimPrefix(strings.TrimLeft(launcher, " \t"), fields[0])
	return resolved + rest
}

// SpawnCommand is the shell line argus runs in a worker's pane: cd into the
// worktree, then start the launcher with the initial prompt as its argument. The
// worktree path is single-quoted because it is data (a path that may contain
// spaces, and whose final segment is a branch name): interpolating it raw let a
// branch like feat$(cmd) inject into the pane's shell. launcher is argus-owned
// config (DefaultLauncher or --launcher) and is left unquoted so a smoke test can
// pass a multi-word command.
//
// scrubEnv names environment variables to withhold from the launcher via `env
// -u`, so a secret the pane inherited from the host (a forge or issue-tracker
// token the worker never needs) is not present in the worker agent's
// environment or any child it spawns. The names are argus-owned identifiers,
// not user data, so they are emitted unquoted.
//
// workerEnv is "KEY=VALUE" assignments placed inline immediately before the
// launcher, so they apply to the launcher process and every child it spawns
// but not to the pane's own shell — this is how a credential broker (see
// internal/credproxy) hands a worker a phantom sentinel in place of a real
// key. Each value is single-quoted for the same reason the worktree is: it is
// data (a URL, a token) and must not be re-parsed by the shell.
//
// scrubEnv and workerEnv are independent and never overlap in practice (one
// withholds names, the other sets different names); either or both may be
// empty, in which case the corresponding prefix is omitted and an all-nil call
// reproduces the plain command unchanged.
func SpawnCommand(worktree, launcher string, scrubEnv, workerEnv []string) string {
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
	}
	for _, kv := range workerEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if prefix.Len() > 0 {
			prefix.WriteByte(' ')
		}
		prefix.WriteString(k)
		prefix.WriteByte('=')
		prefix.WriteString(shellQuote(v))
	}
	if prefix.Len() > 0 {
		prefix.WriteByte(' ')
	}
	return fmt.Sprintf("cd %s && %s%s %q", shellQuote(worktree), prefix.String(), launcher, InitialPrompt)
}

// shellQuote wraps s in single quotes for POSIX shells, escaping any embedded
// single quote, so the whole string is treated as one literal argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// launcherCommand is the inner command a runtime adapter wraps: the launcher
// plus the initial prompt, quoted the same way SpawnCommand quotes it. Unlike
// SpawnCommand it carries no `cd` or env prefix — ARGUS_RUNTIME_WORKTREE and
// ARGUS_RUNTIME_ENV carry that information across the adapter boundary
// instead, since a container backend places the worktree and environment
// differently than a plain host shell does.
func launcherCommand(launcher string) string {
	if launcher == "" {
		launcher = DefaultLauncher
	}
	return fmt.Sprintf("%s %q", launcher, InitialPrompt)
}

// LaunchViaRuntime resolves the argus-runtime-<name> adapter on $PATH (see
// docs/worker-runtime-protocol.md) and execs it to obtain the final shell
// command line argus types into the worker's pane. worktree and env
// ("KEY=VALUE" pairs — typically just the credproxy sentinel and base URL,
// not cfg.ScrubEnv, since an isolated environment never had those secrets to
// begin with) cross the boundary as ARGUS_RUNTIME_WORKTREE and
// ARGUS_RUNTIME_ENV; launcher is turned into ARGUS_RUNTIME_CMD the same way
// SpawnCommand turns it into the tail of its host-shell command line.
//
// A missing adapter, a non-zero exit, or empty output is a hard error naming
// the adapter — there is no silent fallback to running the worker unwrapped,
// since that would defeat the point of configuring a runtime at all. Only a
// real spawn path (execute, or a command like rebase that spawns a single
// worker directly) calls this; renderPlan's dry-run preview must never exec
// an adapter subprocess just to print a preview line.
func LaunchViaRuntime(ctx context.Context, adapterName, worktree, launcher string, env []string) (string, error) {
	bin := "argus-runtime-" + adapterName
	binPath, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("worker runtime adapter %q not found on PATH: %w", bin, err)
	}

	envJSON, err := json.Marshal(envMap(env))
	if err != nil {
		return "", fmt.Errorf("encoding runtime env for adapter %q: %w", bin, err)
	}

	cmd := exec.CommandContext(ctx, binPath) //nolint:gosec // binPath resolved via LookPath against an argus-owned naming convention
	cmd.Env = append(os.Environ(),
		"ARGUS_RUNTIME_WORKTREE="+worktree,
		"ARGUS_RUNTIME_ENV="+string(envJSON),
		"ARGUS_RUNTIME_CMD="+launcherCommand(launcher),
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("worker runtime adapter %q failed: %w", bin, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("worker runtime adapter %q produced no output", bin)
	}
	return line, nil
}

// envMap turns "KEY=VALUE" pairs into a JSON object for ARGUS_RUNTIME_ENV,
// dropping any entry without an "=" the same way SpawnCommand's inline
// injection does.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

// Run is the whole deterministic supervise loop. In dry-run it prints the plan
// and makes no changes. Otherwise it enforces distinct worktrees, spawns each
// worker, watches their status files until every one reaches a terminal phase or
// ctx is canceled, then prints a metrics report.
func Run(ctx context.Context, cfg *Config, workers []Worker, dryRun bool) error {
	plans := BuildPlan(workers, cfg.ExtraAllow)

	if err := EnsureDistinctWorktrees(worktreePaths(plans)); err != nil {
		return err
	}

	if dryRun {
		renderPlan(cfg.Out, cfg.Base, cfg.Launcher, cfg.WorkerRuntime, cfg.ScrubEnv, plans)
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
	plans := BuildPlan(workers, cfg.ExtraAllow)
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
	dispatchedAt  time.Time
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

		// Captured before the worktree is touched, so pollStatus rejects any
		// status.json/verdict.json already sitting in the worktree directory
		// (see InvalidateStatus and issue #50/#75) even if invalidation below
		// races with a stray write.
		dispatchedAt := cfg.Now()

		wt, err := prepareWorktree(ctx, cfg, p)
		if err != nil {
			return fail(i, err)
		}

		paneID, err := resolvePaneID(p, wt)
		if err != nil {
			return fail(i, err)
		}

		// When a broker is configured, the launch line carries this worker's
		// phantom credentials inline, so the agent authenticates through argus
		// and is not handed a real key in its own environment.
		var workerEnv []string
		if cfg.Broker != nil {
			workerEnv = cfg.Broker.WorkerEnv(taskLabel(p.Task), p.Branch)
		}

		spawnLine, err := resolveSpawnLine(ctx, cfg, p, workerEnv)
		if err != nil {
			return fail(i, err)
		}

		if err := cfg.Client.PaneRun(ctx, paneID, spawnLine); err != nil {
			cfg.Log.Fail("spawn", taskLabel(p.Task), err)
			return fail(i, fmt.Errorf("spawning worker for %s: %w", p.Task, err))
		}
		cfg.Log.Action("spawn", taskLabel(p.Task), "ok", paneID)

		states[i] = &workerState{plan: p, paneID: paneID, started: cfg.Now(), dispatchedAt: dispatchedAt}
	}
	return states, nil
}

// prepareWorktree creates one worker's git worktree via herdr and writes its
// settings and brief into it. Split out of execute to keep the worktree/herdr
// side effects independently testable from pane resolution and launch, the
// same way foldIssueSources in cmd/supervise.go isolates one source's
// fetch-and-fold step.
func prepareWorktree(ctx context.Context, cfg *Config, p *WorkerPlan) (herdr.Worktree, error) {
	wt, err := cfg.Client.WorktreeCreate(ctx, &herdr.WorktreeSpec{
		Cwd:    p.RepoRoot,
		Branch: p.Branch,
		Base:   cfg.Base,
		Path:   p.Worktree,
		Label:  p.Branch,
	})
	if err != nil {
		return herdr.Worktree{}, fmt.Errorf("creating worktree for %s: %w", p.Task, err)
	}
	// A worktree directory can carry a leftover status.json/verdict.json from an
	// unrelated prior task (issue #75) — e.g. directory reuse in worktree
	// creation. Without this, the watch loop's first poll can read that stale
	// terminal-phase file and report the worker approved before it has done
	// anything at all. dispatchedAt (captured before this call) is the
	// independent second guard: pollStatus also ignores any status whose
	// UpdatedAt isn't after it.
	if err := InvalidateStatus(p.Worktree); err != nil {
		return herdr.Worktree{}, fmt.Errorf("invalidating stale status before dispatching %s: %w", p.Task, err)
	}
	if err := WriteSettings(p.Worktree, cfg.ExtraAllow); err != nil {
		return herdr.Worktree{}, fmt.Errorf("writing settings for %s: %w", p.Task, err)
	}
	if err := WriteBrief(p.Worktree, p.Brief); err != nil {
		return herdr.Worktree{}, fmt.Errorf("writing brief for %s: %w", p.Task, err)
	}
	return wt, nil
}

// resolvePaneID picks the pane a worker launches in: a caller-supplied pane
// wins, otherwise the pane herdr opened inside the new worktree — no separate
// --panes needed. Neither present is a hard error, since execute has nowhere
// left to run the worker.
func resolvePaneID(p *WorkerPlan, wt herdr.Worktree) (string, error) {
	if p.PaneID != "" {
		return p.PaneID, nil
	}
	if wt.RootPaneID != "" {
		return wt.RootPaneID, nil
	}
	return "", fmt.Errorf("worker %s has no pane and herdr returned no root pane for its worktree", p.Task)
}

// resolveSpawnLine builds the command line argus types into a worker's pane:
// cd + start the agent with a prompt that points it at the brief file (no
// second paste — the brief is on disk, not typed in), or — when a runtime
// adapter is configured — the line argus-runtime-<name> produces for an
// isolated environment instead. "" or "none" is today's unwrapped behavior.
// Only the adapter path gets workerEnv via ARGUS_RUNTIME_ENV — cfg.ScrubEnv
// stays host-shell-only (see docs/worker-runtime-protocol.md), since an
// isolated environment never had those secrets to scrub in the first place.
// Split out of execute so the adapter-selection branch, and its own error
// path, is independently testable — mirroring foldIssueSources's split in
// cmd/supervise.go.
//
// The launcher's binary is resolved to an absolute path (see
// ResolveLauncherPath) only on the plain-host-shell path (SpawnCommand),
// where a newly opened pane's not-yet-initialized shell PATH is the race
// being sidestepped. A container/podman runtime adapter runs the launcher
// inside its own image via its own already-initialized shell PATH, and the
// host's absolute path (e.g. /opt/homebrew/bin/claude) is almost certainly
// wrong inside that container's filesystem, so LaunchViaRuntime gets the
// launcher unresolved and lets the container's PATH find it.
func resolveSpawnLine(ctx context.Context, cfg *Config, p *WorkerPlan, workerEnv []string) (string, error) {
	launcher := cfg.Launcher
	if launcher == "" {
		launcher = DefaultLauncher
	}

	if cfg.WorkerRuntime == "" || cfg.WorkerRuntime == "none" {
		return SpawnCommand(p.Worktree, ResolveLauncherPath(launcher), cfg.ScrubEnv, workerEnv), nil
	}
	line, err := LaunchViaRuntime(ctx, cfg.WorkerRuntime, p.Worktree, launcher, workerEnv)
	if err != nil {
		cfg.Log.Fail("spawn", taskLabel(p.Task), err)
		return "", fmt.Errorf("launching worker for %s via runtime adapter: %w", p.Task, err)
	}
	return line, nil
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
	var loggedStale bool
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
			case err == nil && !st.dispatchedAt.IsZero() && isStale(path, st.dispatchedAt):
				// A status.json whose file mtime is at or before dispatchedAt
				// predates this dispatch — a stale leftover (issue #75), not this
				// worker's report. Judged by mtime, not the worker's self-reported
				// UpdatedAt (issue #90), since InvalidateStatus removes the file
				// before dispatch so any file present afterward was necessarily
				// written by this dispatch regardless of what clock value the
				// worker put inside it. Treated the same as no file at all
				// (mirrors WaitForStatus, issue #50). dispatchedAt is zero for an
				// attach, where no dispatch happened and any existing status is
				// legitimately this worker's own.
				if !loggedStale {
					loggedStale = true
					log.Action("status_stale", st.plan.Task, "discarded", string(s.Phase))
				}
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
