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
	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// Worker is one supervised task. PaneID, when set, names an existing pane to run
// the worker in; when empty, argus splits a new pane. Worktree, when empty, is
// derived from RepoRoot and Branch. Label, when empty, is derived by BuildPlan
// from Task (falling back to Branch) — it is the only string herdr shows for
// this worker's workspace in its own UI, and a caller may want it distinct from
// the (often slugged, sometimes truncated) Branch.
type Worker struct {
	Task     string
	Branch   string
	Label    string
	RepoRoot string
	Worktree string
	// WorktreeDir overrides where BuildPlan derives a missing Worktree from
	// (see WorktreePath) — set from the repo's own .argus/config.yml
	// worktree_dir (internal/repoconfig) so a repo with its own worktree
	// convention (e.g. sibling directories next to the checkout) doesn't end
	// up with a second, uncoordinated set of worktrees under argus's default
	// .claude/worktrees/. Empty keeps that default.
	WorktreeDir string
	PaneID      string
}

// CredentialBroker mints per-worker credentials so a worker never holds a real
// API key. WorkerEnv registers the worker (identified by agent label and its
// branch) and returns the environment assignments — a phantom sentinel plus a
// proxied base URL — that route the worker's API traffic through argus. Revoke
// cuts that same worker's access off once its verdict is recorded (see
// recordApproval) — the gate has ruled either way, and nothing about a pane
// staying open afterward should keep its credential live. A nil Broker means
// no proxy: workers inherit the host's real credentials, which is the prior
// behavior.
type CredentialBroker interface {
	WorkerEnv(agent, branch string) []string
	Revoke(agent string) int
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
	// OwnerID and OwnerLabel identify this supervise invocation for the
	// ownership lease (see internal/ownership) prepareWorktree writes into
	// every worktree it creates. Resolved once by cmd/supervise.go — via
	// ownership.ResolveOwnerID and ownership.DefaultOwnerLabel — before any
	// worker is spawned, so every worker this one invocation spawns shares
	// the same lease identity instead of each worktree claiming a fresh
	// generated id of its own.
	OwnerID    string
	OwnerLabel string
	// ReviewNote is the repo's own .argus/config.yml review_note (see
	// internal/repoconfig), forwarded verbatim to each ReviewRequest so
	// reviewOne's prompt carries it. Empty means no repo-specific review
	// criteria, today's behavior.
	ReviewNote string
	// VerifyCommand, when set, is a shell command the gate runs in a
	// worker's worktree (via RunVerifyCommand) once it reaches a terminal
	// phase, mirroring the same bar `argus ship` enforces (a repo's own
	// lint/build/pre-commit) so a clean verdict can't be undone by a hook
	// failure ship only discovers at `git commit` time. Empty means no
	// command is configured — the gate's prior behavior, unchanged.
	VerifyCommand string
	// WorktreeSetupCmd is the repo's own .argus/config.yml worktree_setup_cmd
	// (see internal/repoconfig), run once by prepareWorktree right after
	// herdr's WorktreeCreate succeeds and before the worker's agent is
	// spawned (see RunWorktreeSetupCmd). Empty means no command is
	// configured — a bare `git worktree add` with no bootstrap step, the
	// prior behavior.
	WorktreeSetupCmd string
	// ParentWorkspace, when set, is the herdr workspace id every spawned
	// worker's worktree pane nests into as a tab instead of staying in its own
	// new top-level workspace (see prepareWorktree's use of herdr.Client.PaneMove).
	// Empty means today's behavior: each worker keeps its own top-level workspace.
	ParentWorkspace string
	// WorkerRuntime names a worker-runtime adapter (see
	// docs/worker-runtime-protocol.md): argus execs argus-runtime-<name> to
	// isolate the worker instead of running it directly in the host shell.
	// Empty (or "none") means today's unwrapped behavior — the default, so
	// existing installs are unaffected until an operator opts in.
	WorkerRuntime string
	ScrubEnv      []string // env vars withheld from each worker (e.g. forge tokens it never needs)
	// RepoAllow is the repo's own .argus/config.yml allow list (see
	// internal/repoconfig), the base permission list settingsFor inserts
	// after its fixed structural entries — replacing the hardcoded Go/make
	// literal argus used to assume for every repo regardless of toolchain.
	RepoAllow []string
	// ExtraAllow appends operator-supplied CLI --allow patterns (e.g.
	// "Bash(task *)", "Bash(npm *)") to every worker's generated allowlist, on
	// top of RepoAllow, for a one-off run.
	ExtraAllow []string
	Interval   time.Duration
	Timeout    time.Duration // per-worker wall-clock deadline; 0 = wait indefinitely
	// ReviewConcurrency bounds how many LLM --review calls run at once. 0 uses
	// defaultReviewConcurrency. Gate checks (the deterministic, free/local half
	// of judgment) are never bound by this — only the `claude -p` calls are,
	// since those are the expensive, slow step a batch of escalations could
	// otherwise pile up unbounded.
	ReviewConcurrency int
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

// defaultWorktreeDir is BuildPlan's worktree location when a worker sets
// neither Worktree nor WorktreeDir — the original hardcoded convention,
// still the default for repos that never set .argus/config.yml worktree_dir.
const defaultWorktreeDir = ".claude/worktrees"

// WorktreePath resolves a worker's worktree directory from repoRoot, the
// repo's configured worktree_dir (empty means defaultWorktreeDir), and
// branch. dir containing ".." (or any other relative climb) is joined under
// repoRoot rather than resolved against the process cwd, so a repo whose own
// convention is a sibling directory next to the checkout (dir "..") lands
// there deterministically regardless of where argus itself runs from. An
// absolute dir is used as-is, entirely independent of repoRoot.
func WorktreePath(repoRoot, dir, branch string) string {
	if dir == "" {
		dir = defaultWorktreeDir
	}
	if filepath.IsAbs(dir) {
		return filepath.Join(dir, branch)
	}
	return filepath.Join(repoRoot, dir, branch)
}

// BuildPlan resolves each worker into a concrete plan. Missing worktree paths are
// derived by WorktreePath from RepoRoot, WorktreeDir, and Branch; each brief is
// the task text plus the shared status-writing contract so writer and reader
// can't drift. repoAllow and extraAllow are forwarded to settingsFor so every
// worker's allowlist reflects the same repo-config and operator-supplied
// extension the dry-run preview shows.
func BuildPlan(workers []Worker, base string, repoAllow, extraAllow []string) []WorkerPlan {
	plans := make([]WorkerPlan, len(workers))
	for i := range workers {
		w := workers[i]
		if w.Worktree == "" {
			w.Worktree = WorktreePath(w.RepoRoot, w.WorktreeDir, w.Branch)
		}
		if w.Label == "" {
			// Fold in the task text so the herdr-visible label carries actual
			// information about what the worker is doing, not just its (often
			// slugged) branch name — taskLabel is the same short identifier
			// already used for run-log/eventlog entries, so a worker's label
			// matches what an operator sees for it elsewhere. Task is only
			// ever "" for a hand-built Worker (e.g. in tests); real callers
			// (buildWorkers/attachWorkers) always set it, so this fallback is
			// the true no-context case.
			w.Label = taskLabel(w.Task)
			if w.Label == "" {
				w.Label = w.Branch
			}
		}
		plans[i] = WorkerPlan{
			Worker:   w,
			Settings: settingsFor(w.Worktree, repoAllow, extraAllow),
			Brief:    briefFor(&w, base),
		}
	}
	return plans
}

func briefFor(w *Worker, base string) string {
	return fmt.Sprintf(`Task: %s
Branch: %s

Work only inside %s. Never delete, reset, or touch files outside it; another
agent may share the parent repo. Write a todo list before anything else.

Do the work and verify it (build + tests). Do NOT git commit or git push — argus
handles shipping. When the change is complete and tests pass, set your status
phase to "awaiting_review" (not "done"); use "blocked" if you need a decision only
the supervisor can make.

%s`, w.Task, w.Branch, w.Worktree, protocol.WriterBrief(base))
}

// taskLabel is a short, log-friendly identifier for a worker task: the first
// line, trimmed to 60 characters. It keeps the run log, `argus stats`, and the
// herdr-visible worker label keyed by something readable instead of the entire
// multi-line brief.
//
// It deliberately does not special-case a leading "#<n>" issue reference into
// a bare label like "#143" — issuesToTasks renders --issues tasks as "Fix
// owner/repo issue #143: <title>", so the first line already carries the
// issue number *and* the repo and title that distinguish it from same-numbered
// issues in other repos when several workers run side by side.
func taskLabel(task string) string {
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

// usesDefaultLauncher reports whether launcher actually starts a Claude Code
// session — the only launcher that writes a ~/.claude/projects transcript for
// HasPlanEvidence to grep. An empty launcher resolves to DefaultLauncher the
// same way SpawnCommand and LaunchViaRuntime resolve it; anything else is a
// --launcher override (e.g. a non-Claude-Code agent) for which no such
// transcript can ever exist.
func usesDefaultLauncher(launcher string) bool {
	return launcher == "" || launcher == DefaultLauncher
}

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

// ResolveWorktree makes path absolute against argus's own working directory,
// wrapping any error with context. Every command that accepts a --worktree
// (or --repo/--worktrees) flag must pass it through here before handing it to
// a git -C call, a pane's `cd`, or protocol.Load/LoadApproval — anything that
// interprets the path against a cwd that may not be argus's own. A path given
// relative to argus's own cwd breaks the moment it is used against a pane
// whose cwd differs, or is reused against a worktree already rooted
// elsewhere; resolving once, immediately after the flag's own empty-string
// check, means every downstream call agrees on the same absolute path. This
// exact bug — a caller resolving a relative path against the wrong cwd — has
// recurred multiple times, each time independently reported and fixed in a
// different caller that rolled its own inline filepath.Abs instead of
// reaching for this helper; any new caller should use ResolveWorktree instead
// of repeating that mistake.
//
// path == "" resolves to argus's own cwd (filepath.Abs's own behavior) rather
// than erroring: every caller already has its own "no worktree given"
// ui.UserError check before it ever calls this, so ResolveWorktree does not
// need to special-case the empty string itself.
func ResolveWorktree(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving worktree path %q: %w", path, err)
	}
	return abs, nil
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
		launcher = defaultAgent.DefaultLauncher()
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
		launcher = defaultAgent.DefaultLauncher()
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
// worker, watches and judges each one's status independently until it reaches a
// terminal phase or ctx is canceled, then prints a metrics report.
func Run(ctx context.Context, cfg *Config, workers []Worker, dryRun bool) error {
	plans := BuildPlan(workers, cfg.Base, cfg.RepoAllow, cfg.ExtraAllow)

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
// (Run) and an attach to already-running workers (Attach): each worker is
// watched, measured, and gated/reviewed independently the moment IT reaches a
// terminal phase — not after the whole batch does — so a worker
// that finishes early is judged early instead of waiting on its slowest
// sibling. Only the final report is a full-batch barrier.
func superviseStates(ctx context.Context, cfg *Config, states []*workerState) error {
	judgeEach(ctx, cfg, states)
	renderReport(ctx, cfg, states)
	return nil
}

// defaultReviewConcurrency bounds concurrent LLM --review calls when
// cfg.ReviewConcurrency is unset (0).
const defaultReviewConcurrency = 4

// judgeEach drives one goroutine per worker through watch→reconcile→gate/review,
// so N workers' judgments proceed independently instead of every worker
// waiting for the batch's slowest one to reach a terminal phase before any of
// them is gated. reconcile and reviewEscalations are called with
// a single-element slice per worker — their own batch-shaped loops are what
// existing tests already exercise; running one per goroutine gets the same
// per-worker checks without waiting on siblings.
//
// sem is threaded into reviewEscalations rather than acquired here around the
// whole call: only the LLM `--review` call inside it is expensive and slow,
// so only that call may wait on a slot. Acquiring sem before reviewEscalations
// would make every worker's gate check — meant to be free/local — queue
// behind whichever other workers currently hold a review slot: a fleet larger
// than ReviewConcurrency (default 4) could then go a long time with no
// gate/verdict event at all, even though several workers had already reached
// a terminal phase.
func judgeEach(ctx context.Context, cfg *Config, states []*workerState) {
	n := cfg.ReviewConcurrency
	if n <= 0 {
		n = defaultReviewConcurrency
	}
	sem := make(chan struct{}, n)

	// Mirrors recordApproval's own cfg.Now-or-time.Now fallback: judgeEach is
	// exercised directly by tests whose Config leaves Now unset, and pollStatus
	// needs a clock for its per-tick owner-lease heartbeat (see
	// ownership.Heartbeat below).
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}

	var wg sync.WaitGroup
	for _, st := range states {
		wg.Add(1)
		go func(st *workerState) {
			defer wg.Done()
			pollStatus(ctx, cfg.Client, cfg.Interval, cfg.Timeout, cfg.Log, st, now)

			one := []*workerState{st}
			reconcile(ctx, cfg, one)
			reviewEscalations(ctx, cfg, one, sem)
		}(st)
	}
	wg.Wait()
}

// Attach supervises workers that are already running in their worktrees: it
// creates no worktree and spawns no agent, it only watches each worker's typed
// status file and takes it through the same gate and report as a fresh run. This
// is how an operator brings a worker argus did not launch — one started by hand,
// or grinding on an existing PR branch — under the same deterministic observation
// instead of eyeballing its pane scrollback.
func Attach(ctx context.Context, cfg *Config, workers []Worker) error {
	plans := BuildPlan(workers, cfg.Base, cfg.RepoAllow, cfg.ExtraAllow)
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

// reconcile computes each worker's real diff from git, and independently checks
// its session transcript for real plan/todo evidence, so the gate and report use
// ground truth rather than the worker's self-report. A measurement failure is
// recorded (and later surfaced) rather than silently trusting status.json.
//
// Steps run in fixed order: measureReconcileDiffs before captureReviewDiffs,
// since the later test/verify steps can leave worktree side effects the
// reviewer must never see.
func reconcile(ctx context.Context, cfg *Config, states []*workerState) {
	measureReconcileDiffs(ctx, cfg, states)
	captureReviewDiffs(ctx, cfg, states)
	verifyClaimedTests(ctx, states)
	runConfiguredVerifyCommand(ctx, cfg, states)

	// Non-default --launcher never produces a Claude Code transcript, so
	// checkPlanEvidence would only ever report false — leave plan evidence
	// not-applicable instead of failed.
	if !usesDefaultLauncher(cfg.Launcher) {
		return
	}
	checkPlanEvidence(cfg, states)
}

// measureReconcileDiffs measures each worker's real diff from git. priorMeasured
// is never read back from disk here — only JudgeOne (a rework round) populates
// it, by passing its own pre-dispatch snapshot straight into workerState. A
// plain --attach re-run of an already-judged worktree has no such snapshot, so
// gateVerdict always judges it against the full cumulative diff, not a stale
// baseline left over from that worktree's last verdict.json — the earlier
// disk-read fallback here made a plain re-attach silently reuse whatever an
// unrelated prior round had measured, which could clear (or hide) an
// under-report verdict without the underlying diff or self-report actually
// having changed.
func measureReconcileDiffs(ctx context.Context, cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
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
		// Only a rework round supplies a pre-round hash to compare against, so
		// hashing every worker's touched files in the main loop would be pure
		// wasted I/O — compute the post-round hash only where gateVerdict's
		// zero-delta check can actually fire. A hash error leaves it "", which
		// disables that one check rather than blocking the round.
		if st.priorContentHash != "" {
			if h, herr := ContentHash(st.plan.Worktree, files); herr == nil {
				st.contentHash = h
			}
			if sha, sherr := HeadSHA(ctx, st.plan.Worktree); sherr == nil {
				st.headSHA = sha
			}
		}
	}
}

// captureReviewDiffs must run before verifyClaimedTests/runConfiguredVerifyCommand:
// those execute worker-supplied commands that can dirty the worktree
// (coverage files, regenerated lockfiles), and the reviewer must never see
// that noise.
func captureReviewDiffs(ctx context.Context, cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		if st.status.Phase != protocol.PhaseAwaitingReview && st.status.Phase != protocol.PhaseDone {
			continue
		}
		st.reviewDiff, st.reviewDiffErr = DiffFor(ctx, st.plan.Worktree, cfg.Base)
	}
}

// verifyClaimedTests reproduces every claimed test pass via git worktree +
// shell rather than a launcher-specific transcript convention, so it applies
// regardless of --launcher.
func verifyClaimedTests(ctx context.Context, states []*workerState) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		if st.status.Phase != protocol.PhaseAwaitingReview && st.status.Phase != protocol.PhaseDone {
			continue
		}
		st.testMismatches = VerifyTests(ctx, st.plan.Worktree, st.status.Tests, testVerifyTimeout)
	}
}

// runConfiguredVerifyCommand mirrors the bar `argus ship`'s `git commit`
// hooks enforce (Config.VerifyCommand), so a failure surfaces here instead
// of at ship time; it runs after captureReviewDiffs for the same reason —
// its own worktree side effects must not leak into the diff already shown
// to the reviewer.
func runConfiguredVerifyCommand(ctx context.Context, cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		if st.status.Phase != protocol.PhaseAwaitingReview && st.status.Phase != protocol.PhaseDone {
			continue
		}
		st.verifyMismatch = RunVerifyCommand(ctx, st.plan.Worktree, cfg.VerifyCommand)
	}
}

// checkPlanEvidence records whether each worker's Claude Code transcript
// shows real plan/todo evidence (see defaultAgent.PlanEvidence).
func checkPlanEvidence(cfg *Config, states []*workerState) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
			continue
		}
		ok, transcripts, err := defaultAgent.PlanEvidence(cfg.Home, st.plan.Worktree)
		if err != nil {
			st.planEvidenceErr = err
			cfg.Log.Fail("plan_evidence", st.plan.Task, err)
			continue
		}
		st.hasPlanEvidence = ok
		st.planEvidenceOK = true
		// Zero transcripts scanned (dir missing/misconfigured home) and one-or-more
		// transcripts scanned with no marker match are different failure modes;
		// recording the count here means a recurrence of the soft "plan evidence
		// unverified" gate flag can be pattern-matched from the run log alone,
		// without re-deriving which case it was.
		if !ok {
			cfg.Log.Emit(&eventlog.Event{
				Action:  "plan_evidence",
				Target:  st.plan.Task,
				Outcome: "not-found",
				Fields:  map[string]any{"transcripts_checked": transcripts},
			})
		}
	}
}

// reviewEscalations runs the LLM reviewer on exactly the workers the deterministic
// gate could not clear — the risky minority. No reviewer configured (or a clean
// gate verdict) means no call, so the LLM cost tracks the escalation rate, not the
// worker count. sem bounds only the actual reviewer call (see reviewOne); every
// other branch here — auto-approve, no-reviewer-configured — is free/local and
// runs unconditionally so a worker's gate verdict never waits on sem. sem may be
// nil (tests exercising this function directly, outside judgeEach's concurrency
// bound, pass no limiter).
func reviewEscalations(ctx context.Context, cfg *Config, states []*workerState, sem chan struct{}) {
	for _, st := range states {
		if !st.hasFile && st.herdrEscalation == "" {
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
		reviewOne(ctx, cfg, st, &verdict, sem)
	}
}

// priorFindings returns the Reasons from a previously recorded, non-approved
// verdict for worktree, or nil if none exists (first review, or the prior
// round already approved). recordApproval overwrites verdict.json with each
// review's outcome, so this must be read before the new verdict is recorded.
func priorFindings(worktree string) []string {
	prior, found, err := protocol.LoadApproval(worktree)
	if err != nil || !found || prior.Approved {
		return nil
	}
	return prior.Reasons
}

// reviewOne runs the LLM review for one escalated worker, acquiring sem only
// around this call — the expensive, slow step — so a sibling worker's gate
// check never waits on it. sem == nil means unbounded (no limiter configured).
//
// It uses st.reviewDiff/reviewDiffErr, captured by reconcile before
// VerifyTests ran, rather than fetching the diff itself here — by now
// VerifyTests may already have left its own side effects in the worktree,
// and a fresh DiffFor call here would show the reviewer those instead of
// only the worker's actual change.
func reviewOne(ctx context.Context, cfg *Config, st *workerState, verdict *Verdict, sem chan struct{}) {
	if sem != nil {
		sem <- struct{}{}
		defer func() { <-sem }()
	}

	if st.reviewDiffErr != nil {
		st.reviewErr = st.reviewDiffErr
		cfg.Log.Fail("review", st.plan.Task, st.reviewDiffErr)
		recordApproval(cfg, st, false, "gate", "review could not run: "+st.reviewDiffErr.Error(), verdict.Reasons)
		return
	}
	res, err := cfg.Reviewer.Review(ctx, &ReviewRequest{
		Task:          st.plan.Task,
		Branch:        st.plan.Branch,
		Worktree:      st.plan.Worktree,
		Diff:          st.reviewDiff,
		Reasons:       verdict.Reasons,
		HardReasons:   verdict.HardReasons,
		PriorFindings: priorFindings(st.plan.Worktree),
		ReviewNote:    cfg.ReviewNote,
	})
	if err != nil {
		st.reviewErr = err
		cfg.Log.Fail("review", st.plan.Task, err)
		recordApproval(cfg, st, false, "gate", "review errored: "+err.Error(), verdict.Reasons)
		return
	}
	st.review = &res
	cfg.Log.Action("review", st.plan.Task, res.Decision, res.Summary)

	// A hard reason (unmeasurable diff, material under-report, zero files
	// changed despite a claimed terminal phase) is not a factor for the
	// reviewer to weigh — it is evidence status.json can't be trusted for this
	// change, so no reviewer verdict, including "approve", can waive it. Record
	// the reviewer's findings for a human to read, but never auto-ship past it.
	approved := res.Decision == "approve"
	summary := res.Summary
	if len(verdict.HardReasons) > 0 {
		approved = false
		summary = fmt.Sprintf("reviewer said %q (%s), but a hard gate check is unwaivable: %s",
			res.Decision, res.Summary, strings.Join(verdict.HardReasons, "; "))
	}
	recordApproval(cfg, st, approved, "review", summary, res.Findings)
}

// recordApproval writes the worker's disposition to its worktree so ship can
// enforce it, and logs a verdict event for the run log. A write failure is logged
// but does not abort the run — ship will then see "no verdict" and refuse, which
// is the safe default. It also stamps argus's own measured diff so a later
// round can judge only what changed since this verdict — see priorMeasured.
//
// Both outcomes are terminal from the gate's point of view — approved heads
// toward ship, rejected/escalated-without-a-reviewer is abandoned pending a
// human — so this is the one place every worker's judgment is finalized, and
// therefore the right place to cut its credential-proxy access: nothing else
// in this process learns a worker's fate is decided.
func recordApproval(cfg *Config, st *workerState, approved bool, source, summary string, reasons []string) {
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	a := protocol.Approval{
		Approved:     approved,
		Source:       source,
		Summary:      summary,
		Reasons:      reasons,
		MeasuredDiff: st.measured,
		UpdatedAt:    now(),
	}
	// An approval with no binding is exactly the gap this exists to close, so a
	// hash failure must not silently ship as an approved verdict.
	if approved {
		hash, err := ContentHash(st.plan.Worktree, st.measuredFiles)
		if err != nil {
			cfg.Log.Fail("content_hash", st.plan.Task, err)
			a.Approved = false
			a.Summary = "could not bind verdict to worktree content: " + err.Error()
		} else {
			a.ContentHash = hash
		}
	}
	outcome := "approved"
	if !a.Approved {
		outcome = "not-approved"
	}
	cfg.Log.Action("verdict", st.plan.Task, outcome, a.Summary)
	if err := protocol.WriteApproval(st.plan.Worktree, &a); err != nil {
		cfg.Log.Fail("verdict_write", st.plan.Task, err)
	}
	if cfg.Broker != nil {
		cfg.Broker.Revoke(taskLabel(st.plan.Task))
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
// measured, not status, for diff size and files touched. planEvidenceErr and
// planEvidenceOK mirror diffErr/measuredOK for the transcript-based plan-evidence
// check: planEvidenceOK true means HasPlanEvidence ran without error and
// hasPlanEvidence holds its result; planEvidenceErr set means the check itself
// could not run. Both stay at their zero value — not-applicable, not failed —
// for a worker started with a non-default launcher, which can never have a
// Claude Code transcript to check.
type workerState struct {
	measuredFiles []string
	// testMismatches holds a reason string for each worker-claimed test pass
	// that VerifyTests reproduced as a failure or could not reproduce at all
	// (see reconcile) — nil means either no terminal-phase check ran yet or
	// every claimed pass reproduced clean.
	testMismatches []string
	// verifyMismatch holds the RunVerifyCommand failure reason once the repo's
	// configured verify command (Config.VerifyCommand) has been re-run against
	// a terminal-phase worker — empty means either no command is configured,
	// it has not run yet, or it ran clean.
	verifyMismatch string
	// reviewDiff is the diff reviewOne sends the LLM reviewer, fetched by
	// reconcile before VerifyTests runs so a test command's own side effects
	// can never leak into what the reviewer is shown. reviewDiffErr set means
	// that fetch failed; reviewOne must not fall back to fetching a fresh
	// (possibly test-polluted) diff itself.
	reviewDiff      string
	reviewDiffErr   error
	started         time.Time
	dispatchedAt    time.Time
	reviewErr       error
	diffErr         error
	planEvidenceErr error
	paneID          string
	// herdrEscalation is set once herdrStuckElapsed (below) crosses
	// herdrStuckThreshold: the reason the gate must escalate this worker even
	// though status.json (hasFile) may never have been written at all. Empty
	// means herdr hasn't observed this pane stuck long enough to distrust the
	// worker's silence.
	herdrEscalation string
	// herdrErr dedupes a repeated herdr pane-list failure the same way
	// pollStatus's lastErr dedupes a repeated status-file read failure.
	herdrErr string
	// contentHash / priorContentHash carry a rework round's own delta test:
	// contentHash digests the round's post-dispatch touched-file bytes (set by
	// measureReconcileDiffs only when priorContentHash is populated, since
	// nothing else consumes it), priorContentHash the same digest of the
	// pre-round state the prior verdict already rejected. Both empty outside a
	// rework round (JudgeOne is the only populator), which disables the check.
	// Equal terminal hashes alone do NOT mean the round addressed nothing: a
	// worker can commit content that was already sitting in the worktree
	// uncommitted before the round started (e.g. a fix applied but never
	// landed), leaving the on-disk bytes ContentHash reads unchanged while a
	// genuinely new HEAD commit ships it — headSHA/priorHeadSHA is the git-level
	// signal that catches that case; see gateVerdict.
	contentHash      string
	priorContentHash string
	// headSHA / priorHeadSHA are the worktree's HEAD commit before and after a
	// rework round, populated the same way and under the same gating as
	// contentHash above. A changed HEAD proves a real, distinct commit landed
	// during the round even when priorContentHash still matches contentHash.
	headSHA      string
	priorHeadSHA string
	plan         *WorkerPlan
	review       *ReviewResult
	status       protocol.Status
	measured     protocol.DiffStat
	// priorMeasured is a rework round's pre-dispatch verdict measurement,
	// set only by JudgeOne (see priorMeasuredOK); gateVerdict subtracts it from
	// measured so that round is judged on its own delta, not the cumulative
	// diff since base. Zero outside a rework round, same as priorContentHash.
	priorMeasured protocol.DiffStat
	// herdrStuckElapsed accumulates poll ticks (in interval-sized steps, not
	// wall-clock reads) while herdr's own agent_status for this pane reports
	// blocked or done; it resets to zero the moment that stops being true.
	herdrStuckElapsed time.Duration
	// herdrNudged is set once checkHerdrStuck has sent its one AgentPrompt
	// reminder for the pane's current stuck streak, so a still-stuck pane
	// escalates on the next threshold crossing instead of nudging forever.
	// It resets alongside herdrStuckElapsed the moment the pane stops
	// looking stuck, so a worker that gets stuck again later is nudged
	// again.
	herdrNudged     bool
	hasFile         bool
	measuredOK      bool
	planEvidenceOK  bool
	hasPlanEvidence bool
	priorMeasuredOK bool
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
		for j := range i {
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
		// (see InvalidateStatus) even if invalidation below
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

		if err = ensureFreshPane(ctx, cfg.Client, paneID, taskLabel(p.Task)); err != nil {
			cfg.Log.Fail("spawn", taskLabel(p.Task), err)
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
		Label:  p.Label,
	})
	if err != nil {
		return herdr.Worktree{}, fmt.Errorf("creating worktree for %s: %w", p.Task, err)
	}
	// WorktreeCreate always opens its own new top-level workspace — it has no
	// nesting flag (see its doc comment). Nesting into cfg.ParentWorkspace is
	// PaneMove's job, moving the just-created root pane into a new tab there;
	// herdr closes the now-empty top-level workspace WorktreeCreate opened as
	// part of that move. Every later use of wt.RootPaneID (pane registry,
	// resolvePaneID) must see the moved pane's id, not the original one.
	if cfg.ParentWorkspace != "" && wt.RootPaneID != "" {
		moved, err := cfg.Client.PaneMove(ctx, wt.RootPaneID, cfg.ParentWorkspace)
		if err != nil {
			return herdr.Worktree{}, fmt.Errorf("nesting worktree pane for %s into workspace %s: %w", p.Task, cfg.ParentWorkspace, err)
		}
		wt.RootPaneID = moved.PaneID
	}
	// Runs immediately after `git worktree add` succeeds (whether or not
	// nesting above ran) and before any of argus's own scaffolding below, so
	// a repo's bootstrap step (e.g. copying in gitignored per-developer local
	// config) sees the worktree exactly as `git worktree add` left it. A
	// failure here fails worktree creation the same way a WorktreeCreate
	// error already does — execute never reaches PaneRun for this worker.
	if err := RunWorktreeSetupCmd(ctx, p.Worktree, cfg.WorktreeSetupCmd); err != nil {
		return herdr.Worktree{}, fmt.Errorf("running worktree_setup_cmd for %s: %w", p.Task, err)
	}
	// A worktree directory can carry a leftover status.json/verdict.json from an
	// unrelated prior task — e.g. directory reuse in worktree
	// creation. Without this, the watch loop's first poll can read that stale
	// terminal-phase file and report the worker approved before it has done
	// anything at all. dispatchedAt (captured before this call) is the
	// independent second guard: pollStatus also ignores any status whose
	// UpdatedAt isn't after it.
	if err := InvalidateStatus(p.Worktree); err != nil {
		return herdr.Worktree{}, fmt.Errorf("invalidating stale status before dispatching %s: %w", p.Task, err)
	}
	// Records the base this worktree actually branched from — cfg.Base is a
	// ref (e.g. "origin/main"), while protocol.Status.Base and every
	// consumer of it (ResolveBase, used by ship/rebase) use the bare branch
	// name convention, so the "origin/" prefix is stripped here, once. A
	// worker's own `argus worker report` never sets this field and carries
	// it forward unchanged (see runWorkerReport), so ship can read back what
	// base was actually used instead of re-defaulting to the literal "main".
	baseBranch := strings.TrimPrefix(cfg.Base, "origin/")
	if err := protocol.Write(protocol.StatusPath(p.Worktree), &protocol.Status{Base: baseBranch}); err != nil {
		return herdr.Worktree{}, fmt.Errorf("recording base branch for %s: %w", p.Task, err)
	}
	if err := WriteSettings(p.Worktree, cfg.RepoAllow, cfg.ExtraAllow); err != nil {
		return herdr.Worktree{}, fmt.Errorf("writing settings for %s: %w", p.Task, err)
	}
	if err := WriteBrief(p.Worktree, p.Brief); err != nil {
		return herdr.Worktree{}, fmt.Errorf("writing brief for %s: %w", p.Task, err)
	}
	// Claims this worktree for cfg.OwnerID before any worker touches it, so a
	// second, unrelated argus/herdr session's rework/rebase/ship/worker-answer
	// call refuses by default instead of silently racing this one (see
	// internal/ownership). cfg.OwnerID/OwnerLabel are resolved once per
	// supervise invocation, not here, so every worker this run spawns shares
	// one lease identity. now mirrors recordApproval's own cfg.Now-or-time.Now
	// fallback, since prepareWorktree is exercised directly by tests whose
	// Config leaves Now unset.
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	if err := ownership.Spawn(p.Worktree, cfg.OwnerID, cfg.OwnerLabel, now()); err != nil {
		return herdr.Worktree{}, fmt.Errorf("writing owner lease for %s: %w", p.Task, err)
	}
	// Recorded in the repo-root pane registry (not the worktree's own
	// lifecycle.json) so `argus worktree prune` can later close the pane
	// herdr opened for this worktree — and its workspace, if left empty —
	// even after the worktree directory itself is gone, e.g. deleted by hand
	// rather than through prune. wt.RootPaneID is empty only if herdr's reply
	// omitted it, in which case there's nothing to record or later close.
	if wt.RootPaneID != "" {
		reg, err := protocol.LoadPaneRegistry(p.RepoRoot)
		if err != nil {
			return herdr.Worktree{}, fmt.Errorf("loading pane registry for %s: %w", p.Task, err)
		}
		reg.Panes[p.Worktree] = wt.RootPaneID
		if cfg.ParentWorkspace != "" {
			if reg.Nested == nil {
				reg.Nested = map[string]bool{}
			}
			reg.Nested[p.Worktree] = true
		}
		if err := protocol.WritePaneRegistry(p.RepoRoot, reg); err != nil {
			return herdr.Worktree{}, fmt.Errorf("recording spawned pane for %s: %w", p.Task, err)
		}
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

// ensureFreshPane confirms paneID has no live agent session before argus types
// a launch command into it. This is the root-cause fix: PaneRun's
// own doc comment already says it "is the wrong call for a pane that already
// has a live agent session running... the agent's own input box would receive
// the literal shell text as a chat message instead of a command a shell
// executes" — but execute previously never checked, so a stale live session
// sitting in the resolved pane (most plausibly a reused --panes value, since a
// pane herdr just opened for a brand-new worktree should always be a bare
// shell) would silently swallow the "cd <worktree> && claude ..." line as a
// chat message into its own, unrelated conversation. That old session then
// carries on with its own prior task and can write its own plausible-looking
// status.json/verdict.json into the new worktree, indistinguishable at a
// glance from a worker that actually ran there — exactly the failure a real
// incident showed ("verbatim content from an unrelated, real session from the
// day before"). AgentGet is the only way to know pane occupancy in advance, so
// it must run unconditionally on every spawn, not just when a caller-supplied
// pane makes the hazard obvious.
//
// An earlier fix added a gate that escalates a *terminal-phase* worker
// reporting zero measured file changes — a useful backstop, but it only fires
// after the fact, only for terminal phases, and not at all if the stolen
// session happens to leave some incidental file change behind. Refusing to
// spawn into an occupied pane prevents the attachment itself rather than
// hoping to catch one of its symptoms later.
func ensureFreshPane(ctx context.Context, client herdr.Client, paneID, task string) error {
	agent, ok, err := client.AgentGet(ctx, paneID)
	if err != nil {
		return fmt.Errorf("checking pane %s is free to spawn %s: %w", paneID, task, err)
	}
	if ok {
		cwd := agent.Cwd
		if cwd == "" {
			cwd = "unknown"
		}
		return fmt.Errorf(
			"pane %s already has a live agent session (session %s, cwd %s) — refusing to spawn %s there: "+
				"typing the launch command in would deliver it as a chat message into that unrelated "+
				"session instead of starting a fresh one; if that cwd points into a worktree "+
				"you already removed, close the pane and retry",
			paneID, agent.AgentSession.Value, cwd, task,
		)
	}
	return nil
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
		launcher = defaultAgent.DefaultLauncher()
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

// herdrStuckThreshold bounds how long a worker's pane may sit at a herdr
// agent_status of "blocked" or "done" — detected externally by herdr from
// the pane's actual terminal state — before pollStatus stops trusting the
// worker's silence and reports it stuck instead. Neither state can ever
// resolve into a status.json write: a pane sitting on an unanswered
// interactive prompt ("blocked") or whose agent turn already ended ("done")
// has no path left to reach argus's own self-reported phases at all.
const herdrStuckThreshold = 2 * time.Minute

// herdrNudgeMessage is the one reminder checkHerdrStuck sends into a "done"
// pane before escalating: the mismatch it targets (a worker whose chat
// claims completion but never called `argus worker report`) is entirely
// mechanical — the worker already knows what to report, it just didn't run
// the command — so a single re-prompt resolves it without waiting on a
// human to notice the gate's warning.
const herdrNudgeMessage = "Your pane looks idle but status.json hasn't reached a terminal phase. If your work is actually finished, run `argus worker report <phase>` now with the JSON body your brief describes — that call, not this chat, is what tells argus you're ready for review."

// herdrNudgeTimeout bounds AgentPrompt's own --wait --until working call
// (see checkHerdrStuck): sized well under herdrStuckThreshold so a nudge
// that never lands still escalates promptly instead of silently consuming
// most of the next stuck window before anyone finds out it failed.
const herdrNudgeTimeout = 30 * time.Second

// herdrStuck reports whether herdr's own agent_status value for a pane means
// its agent is not going to advance status.json on its own. This is
// deliberately distinct from protocol.PhaseBlocked, which the worker itself
// writes into status.json when it wants a human decision — a worker that is
// externally blocked (stuck on a permission prompt) or done (its process
// exited) can never reach that self-reported state, since it requires the
// worker to still be running and able to write the file.
func herdrStuck(agentStatus string) bool {
	return agentStatus == "blocked" || agentStatus == "done"
}

// findPane returns the pane in panes matching paneID, if any.
func findPane(panes []herdr.Pane, paneID string) (herdr.Pane, bool) {
	for i := range panes {
		if panes[i].PaneID == paneID {
			return panes[i], true
		}
	}
	return herdr.Pane{}, false
}

// checkHerdrStuck cross-references herdr's live agent_status for st's pane
// against status.json, and reports whether pollStatus should stop waiting on
// a self-report that will never come. st.paneID == "" (an attach with no
// resolvable pane) skips the check entirely, since there is nothing to ask
// herdr about. A herdr pane-list failure is logged once (like pollStatus's
// own status_unreadable dedupe) and otherwise ignored — a transport error
// says nothing about the worker's real state, so it must not itself count as
// evidence of being stuck.
//
// A pane stuck at "done" gets exactly one AgentPrompt reminder (see
// herdrNudgeMessage) before this escalates — its agent turn already ended
// cleanly, so the likeliest cause is a worker that claimed completion in
// chat and forgot the mandatory report call, which a text nudge can fix. A
// pane stuck at "blocked" is waiting on an unanswered permission prompt that
// no text nudge resolves, so it escalates immediately, as before.
func checkHerdrStuck(ctx context.Context, client herdr.Client, log *eventlog.Logger, st *workerState, tick time.Duration) bool {
	if st.paneID == "" {
		return false
	}
	panes, err := client.PaneList(ctx)
	if err != nil {
		if err.Error() != st.herdrErr {
			st.herdrErr = err.Error()
			if log != nil {
				log.Fail("herdr_status_unreadable", st.plan.Task, err)
			}
		}
		return false
	}
	st.herdrErr = ""

	pane, found := findPane(panes, st.paneID)
	if !found || !herdrStuck(pane.AgentStatus) {
		st.herdrStuckElapsed = 0
		st.herdrNudged = false
		return false
	}

	st.herdrStuckElapsed += tick
	if st.herdrStuckElapsed < herdrStuckThreshold {
		return false
	}

	if pane.AgentStatus == "done" && !st.herdrNudged {
		st.herdrNudged = true
		if err := client.AgentPrompt(ctx, st.paneID, herdrNudgeMessage, herdrNudgeTimeout); err == nil {
			st.herdrStuckElapsed = 0
			if log != nil {
				log.Action("herdr_nudge", st.plan.Task, pane.AgentStatus, "reminded worker to run argus worker report")
			}
			return false
		} else if log != nil {
			log.Fail("herdr_nudge", st.plan.Task, err)
		}
		// The nudge itself didn't land — fall through and escalate, since
		// there's no working channel left to recover through.
	}

	st.herdrEscalation = fmt.Sprintf(
		"herdr reports pane %s agent_status=%q for over %s, but status.json is still at phase %q — "+
			"the worker may be stuck on an unanswered prompt or ended without ever writing a terminal status",
		st.paneID, pane.AgentStatus, herdrStuckThreshold, st.status.Phase)
	if log != nil {
		log.Action("herdr_stuck", st.plan.Task, pane.AgentStatus, st.herdrEscalation)
	}
	return true
}

// herdrWaitStates are the herdr pane-lifecycle states waitForWake blocks on
// via `herdr agent wait` — herdr's own generic vocabulary, not argus's
// status.json phases. A pane reaching one of these doesn't mean the worker
// reached a terminal argus phase; it only means it's worth re-reading
// status.json to find out.
var herdrWaitStates = []string{"idle", "blocked", "done"}

// herdrWaitInstantThreshold bounds how quickly an AgentWait call can return a
// match before waitForWake treats it as "the pane was already sitting in one
// of herdrWaitStates when we asked" rather than "it just transitioned while
// we were waiting". herdr's wait is level-triggered: calling it against a pane
// already idle/blocked/done returns right away every time, indistinguishable
// on its face from a fresh transition. The two cases need different
// responses — a fresh transition (this call actually blocked for a while)
// means something just happened, so status.json is worth reading right away;
// an already-matching pane whose status.json read moments ago was still
// non-terminal means nothing is progressing, so repeating the same call
// immediately would just spin.
const herdrWaitInstantThreshold = 50 * time.Millisecond

// herdrWaitBackoff floors the gap before the next AgentWait call once one
// resolves within herdrWaitInstantThreshold — see herdrWaitInstantThreshold.
// Without this floor a pane parked in a non-terminal herdrWaitStates state (a
// worker that idled without finishing, or crashed and left its pane "done")
// would spin waitForWake in a tight subprocess-spawning loop instead of
// waiting for something to actually change.
const herdrWaitBackoff = 2 * time.Second

// waitForWake decides how long pollStatus's fallback timer should wait before
// its next tick, called once per tick after that tick's status.json read
// already found a non-terminal phase (a terminal read returns from pollStatus
// before waitForWake is ever reached). A worker with a resolvable pane
// (st.paneID set) blocks on herdr's own `agent wait` — a real,
// server-mediated wait on pane lifecycle state, not a client-side sleep — so
// a worker that finishes is noticed close to the moment herdr sees its pane
// go idle rather than up to a full interval later: the call
// having taken herdrWaitInstantThreshold or longer means the pane transitioned
// during this call, most plausibly right around the same status.json write,
// so the next tick fires immediately. A call that resolved faster than that
// means the pane was already sitting in a matching state before this tick's
// (already non-terminal) status read, so the next attempt is floored at
// herdrWaitBackoff — see herdrWaitBackoff. A wait that timed out (herdr's own
// --timeout, sized to interval) already spent about interval waiting, so the
// next tick fires immediately too. A worker with no pane (--attach
// --worktrees, with nothing herdr can watch) keeps the plain interval
// unchanged — the pre-existing timer poll is the only option there. A herdr
// error that isn't a timeout is logged once (mirroring pollStatus's own
// status_unreadable dedupe) and falls back to the plain interval until herdr
// wait starts working again.
func waitForWake(ctx context.Context, client herdr.Client, log *eventlog.Logger, st *workerState, interval time.Duration, lastWaitErr *string) time.Duration {
	if st.paneID == "" {
		return interval
	}

	started := time.Now()
	_, err := client.AgentWait(ctx, st.paneID, herdrWaitStates, interval)
	if ctx.Err() != nil {
		return 0
	}
	switch {
	case err == nil:
		if time.Since(started) < herdrWaitInstantThreshold {
			return herdrWaitBackoff
		}
		return 0
	case errors.Is(err, herdr.ErrWaitTimeout):
		return 0
	default:
		if err.Error() != *lastWaitErr {
			*lastWaitErr = err.Error()
			if log != nil {
				log.Fail("herdr_wait_unreadable", st.plan.Task, err)
			}
		}
		return interval
	}
}

// pollStatus polls one worker's status file until it reaches a terminal phase,
// its deadline passes, or ctx is canceled. A timer (not time.After) drives the
// poll so we don't leak a timer per tick. The deadline is what stops a hung or
// dead worker from blocking its caller forever. client cross-checks herdr's own
// agent_status for st.paneID on every tick alongside status.json (see
// checkHerdrStuck), since a pane herdr reports blocked or done can never write
// status.json to reflect that itself. Between ticks, waitForWake blocks on
// herdr's own pane-lifecycle wait rather than sleeping blind for interval, so
// a pane-backed worker's terminal status is noticed close to the moment herdr
// observes its pane go idle, not up to a full interval later.
func pollStatus(ctx context.Context, client herdr.Client, interval, timeout time.Duration, log *eventlog.Logger, st *workerState, now func() time.Time) {
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
	var lastWaitErr string
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
			// Advances this worktree's owner lease heartbeat on every tick
			// supervise is actively tracking it (see internal/ownership) —
			// a no-op if it never spawned one (e.g. --attach against a
			// worktree with no lease). Best-effort: a write failure here
			// must not stop the watch loop itself.
			if herr := ownership.Heartbeat(st.plan.Worktree, now()); herr != nil {
				log.Fail("owner_heartbeat", st.plan.Task, herr)
			}
			s, err := protocol.Load(path)
			switch {
			case err == nil && !st.dispatchedAt.IsZero() && isStale(path, st.dispatchedAt):
				// A status.json whose file mtime is strictly before
				// dispatchedAt predates this dispatch — a stale leftover,
				// not this worker's report. Judged by mtime, not
				// the worker's self-reported
				// UpdatedAt, since a worker's clock can lie or lag and
				// InvalidateStatus removes the file
				// before dispatch so any file present afterward was necessarily
				// written by this dispatch regardless of what clock value the
				// worker put inside it. Treated the same as no file at all
				// (mirrors WaitForStatus). dispatchedAt is zero for an
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
			if checkHerdrStuck(ctx, client, log, st, interval) {
				return
			}
			timer.Reset(waitForWake(ctx, client, log, st, interval, &lastWaitErr))
		}
	}
}
