package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

// osExit is os.Exit, indirected so tests can stub the exit-2 block path.
var osExit = os.Exit

func newWorkerCheckToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "check-tool",
		Short:  "PreToolUse hook entrypoint: blocks a Bash command the worktree's current phase denies",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerCheckTool(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr())
		},
	}
	return cmd
}

// preToolUseInput is the subset of Claude Code's PreToolUse hook stdin JSON
// this command needs. CWD is a worker's worktree root, doubling as the
// worktree path protocol.StatusPath needs. ToolName distinguishes a Bash
// call (gated on its literal command line) from an Edit/Write call (gated
// purely on whether the current phase allows mutation at all — see
// supervisor.PhaseAllowsMutation) — checkToolHooks registers this same
// command for all three tool names.
type preToolUseInput struct {
	CWD       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// runWorkerCheckTool is newWorkerCheckToolCmd's RunE body, split out for
// direct testing. On a block it writes the reason to stderr and calls
// osExit(2) itself, Claude Code's PreToolUse block contract, rather than
// returning an error (which would exit 1 through main.go's own rendering
// instead of raw stderr).
//
// Malformed input fails open — nothing to enforce. A worktree with no
// status.json yet resolves as protocol.PhasePlanning (see loadCurrentPhase):
// a worker's very first actions are planning, not an ungoverned blind spot
// with nothing to enforce, the way Phase("") used to be. A repo's own
// .argus/config.yml is resolved from the main checkout (via supervisor.
// RepoRoot), never the worktree itself — a worker editing its own worktree's
// tracked copy has no effect here, the same way it has no effect on ship/
// rework/review's own config reads. Failing to resolve it (no git repo,
// no config file) also fails open: no project policy on top of the floor,
// not a hard error.
func runWorkerCheckTool(ctx context.Context, stdin io.Reader, stderr io.Writer) error {
	var in preToolUseInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil //nolint:nilerr // malformed hook payload fails open — nothing to enforce, not a real failure
	}
	if in.CWD == "" {
		return nil
	}

	cur, err := loadCurrentPhase(in.CWD)
	if err != nil {
		return nil //nolint:nilerr // status.json exists but is unreadable/corrupt — nothing this hook can diagnose, fails open
	}

	if in.ToolName == "Edit" || in.ToolName == "Write" {
		if supervisor.PhaseAllowsMutation(cur.Phase) {
			return nil
		}
		_, _ = fmt.Fprintln(stderr, mutationDenyReason(cur.Phase))
		osExit(2)
		return nil
	}

	if in.ToolInput.Command == "" {
		return nil
	}

	cfg := loadProjectConfig(ctx, in.CWD)
	// A worktree WriteSettings never provisioned (e.g. --attach) resolves to
	// a zero repoconfig.Config — the same "no extra flags" fail-open stance
	// loadProjectConfig takes for an unresolvable repo config.
	extraAllow, _ := protocol.LoadExtraAllow(in.CWD)
	if cur.Phase == protocol.PhaseRebase {
		// This hook is deny-only: it either exits 2 to block a call, or
		// exits 0 to defer to --permission-mode dontAsk's own decision — it
		// can never force-allow something dontAsk's static settings.local.json
		// doesn't already permit. provisionWorktree bakes RebasePhaseAllow's
		// git fetch/merge grant into that static file unconditionally, for
		// every worker regardless of phase (see ResolvedAllowSet's doc
		// comment), which is what makes these commands reachable at all.
		// Recomputing the identical grant live here, from this worktree's
		// own recorded Base and its repo's configured verify command, is
		// what narrows enforcement back down to only the rebase phase: below,
		// evaluateToolGate blocks any command absent from `allowed` — without
		// this branch, git fetch/merge would fall through as "not in the
		// resolved allow set" and exit 2 even while the worker is actually
		// reporting rebase; every other phase deliberately omits this branch,
		// so the exact same commands the static file broadly permits still
		// get blocked here, with argus's own message rather than dontAsk's
		// generic one.
		extraAllow = append(slices.Clone(extraAllow), supervisor.RebasePhaseAllow(cur.Base, cfg.ShipVerifyCommand, cfg.GateVerifyCommand)...)
	}

	denied := protocol.ResolvedDenyForPhase(cur.Phase, cfg.Phases)
	allowed := supervisor.ResolvedAllowForPhase(cur.Phase, in.CWD, cfg.Phases, cfg.Allow, extraAllow)
	reason, blocked := evaluateToolGate(in.ToolInput.Command, cur.Phase, denied, allowed)
	if !blocked {
		return nil
	}
	_, _ = fmt.Fprintln(stderr, reason)
	osExit(2)
	return nil
}

// loadCurrentPhase resolves worktree's current Phase for the check-tool
// gate. A real status.json is trusted as-is. A worktree with no status.json
// at all has never had a worker report — which is exactly what "planning"
// means (a fresh worker's first actions, before its first report, already
// are planning; see internal/protocol/transition.go) — so it resolves the
// same as an explicit planning report rather than as the ungoverned
// Phase("") blind spot that used to fail open unconditionally here. Any
// other Load failure (a corrupt or unreadable file) is returned as an error
// instead: there is nothing about that case this function can respond to
// beyond letting the caller's own fail-open stance handle it.
func loadCurrentPhase(worktree string) (protocol.Status, error) {
	cur, err := protocol.Load(protocol.StatusPath(worktree))
	if err == nil {
		return cur, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Status{Phase: protocol.PhasePlanning}, nil
	}
	return protocol.Status{}, err
}

// mutationDenyReason renders the block message for an Edit/Write call
// during a phase that doesn't allow mutation (see
// supervisor.PhaseAllowsMutation) — the Edit/Write counterpart to
// denyReason's Bash-command messages.
func mutationDenyReason(phase protocol.Phase) string {
	return fmt.Sprintf(
		"argus: file edits are denied during phase %q — only working, self_test, and rebase allow mutating tracked files.\n"+
			"If you genuinely need to edit a file here, report `blocked` and explain why.",
		phase,
	)
}

// loadProjectConfig resolves a repo's own .argus/config.yml from the
// trusted main checkout for worktree — never the worktree itself, so a
// worker editing its own tracked copy has no effect here (same trust
// boundary ship/rework/review's own config reads already enforce). An
// unresolvable repo root or config file fails open to a zero Config: no
// project policy layered on top of the structural floor and deny floor, not
// a hard error — the same stance runWorkerCheckTool already took before
// this was split out.
func loadProjectConfig(ctx context.Context, worktree string) repoconfig.Config {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return repoconfig.Config{}
	}
	cfg, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return repoconfig.Config{}
	}
	return cfg
}

// evaluateToolGate is runWorkerCheckTool's pure decision: given the literal
// Bash command a worker is about to run, its currently reported phase, the
// already-resolved denied-prefix list for that phase (see
// protocol.ResolvedDenyForPhase), and the already-resolved allow-entry list
// for that same phase (see supervisor.ResolvedAllowForPhase), whether to
// block it and why. Deny is checked first and wins outright — it is the
// unremovable floor. Only once a command clears deny does allow-scoping
// apply: dontAsk's own static settings.local.json is the union of every
// phase's own resolved allow (it can't itself vary by phase), so this second
// check is what actually narrows a live call back down to what the worker's
// *current* phase permits — without it, a command legitimately allowed only
// in "working" would also silently work during "planning".
func evaluateToolGate(cmdStr string, phase protocol.Phase, denied, allowed []string) (reason string, blocked bool) {
	trimmed := strings.TrimSpace(cmdStr)
	if matched, ok := protocol.MatchesDeniedCommand(trimmed, denied); ok {
		return denyReason(matched, phase), true
	}
	if !supervisor.AllowCoversCommand(allowed, trimmed) {
		return fmt.Sprintf(
			"argus: %q is not in the resolved allow set for phase %q.\n"+
				"Allowed commands here: %s.\n"+
				"If you genuinely need this command, report `blocked` with it named — do not edit .argus/config.yml, it has no effect from inside a worktree.",
			trimmed, phase, supervisor.AllowSetBrief(allowed),
		), true
	}
	return "", false
}

// denyReason renders the block message for a command matched against denied
// (see protocol.ResolvedDenyForPhase): matched names which of three
// distinct, non-overlapping families it fell into, since a single generic
// wording would misdescribe the other two —
//   - AlwaysDeniedCommands (argus's own supervisor commands, plus the
//     PostToolUse-hook-only record-plan): always denied, every phase, for a
//     reason unrelated to git.
//   - AskGatedCommands (git commit/push): also always denied, every phase,
//     but specifically because a worker never commits or pushes at all.
//   - anything else: a repo's own phases.<name>.deny addition — scoped to
//     the one phase it was configured for, not "every phase", and not about
//     commit/push at all (e.g. "npm publish").
func denyReason(matched string, phase protocol.Phase) string {
	switch {
	case matched == "argus worker record-plan":
		return fmt.Sprintf(
			"argus: %q is denied — it only ever runs as a PostToolUse hook Claude Code itself fires on a real TodoWrite/TaskCreate/TaskUpdate call, never something a worker's own turn invokes directly",
			matched,
		)
	case slices.Contains(protocol.AlwaysDeniedCommands, matched):
		return fmt.Sprintf(
			"argus: %q is denied — argus's own supervisor commands (ship/rework/review/supervise) are for the supervising session only, never a worker's own self-invocation",
			matched,
		)
	case slices.Contains(protocol.AskGatedCommands, matched):
		return fmt.Sprintf("argus: %q is denied in every phase — a worker never commits or pushes; argus ship does that once a verdict exists", matched)
	default:
		return fmt.Sprintf("argus: %q is denied during phase %q", matched, phase)
	}
}
