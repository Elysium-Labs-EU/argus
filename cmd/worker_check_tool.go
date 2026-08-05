package cmd

import (
	"context"
	"encoding/json"
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
// worktree path protocol.StatusPath needs.
type preToolUseInput struct {
	CWD       string `json:"cwd"`
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
// Malformed input and a worktree with no status.json yet both fail open —
// nothing to enforce before a worker's first report. A repo's own
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
	if in.ToolInput.Command == "" || in.CWD == "" {
		return nil
	}

	cur, err := protocol.Load(protocol.StatusPath(in.CWD))
	if err != nil {
		return nil //nolint:nilerr // no status.json yet (worker hasn't reported) fails open — nothing to enforce
	}

	projectPhases, baseAllow := loadProjectPolicy(ctx, in.CWD)
	// A worktree WriteSettings never provisioned (e.g. --attach) or a read
	// failure both resolve to nil — the same "no extra flags" fail-open
	// stance loadProjectPolicy takes for an unresolvable repo config.
	extraAllow, _ := protocol.LoadExtraAllow(in.CWD)

	denied := protocol.ResolvedDenyForPhase(cur.Phase, projectPhases)
	allowed := supervisor.ResolvedAllowForPhase(cur.Phase, projectPhases, baseAllow, extraAllow)
	reason, blocked := evaluateToolGate(in.ToolInput.Command, cur.Phase, denied, allowed)
	if !blocked {
		return nil
	}
	_, _ = fmt.Fprintln(stderr, reason)
	osExit(2)
	return nil
}

// loadProjectPolicy resolves a repo's own .argus/config.yml phases policy
// and top-level allow list from the trusted main checkout for worktree —
// never the worktree itself, so a worker editing its own tracked copy has no
// effect here (same trust boundary ship/rework/review's own config reads
// already enforce). An unresolvable repo root or config file fails open to
// zero values: no project policy layered on top of the structural floor and
// deny floor, not a hard error — the same stance runWorkerCheckTool already
// took before this was split out.
func loadProjectPolicy(ctx context.Context, worktree string) (protocol.PhaseConfig, []string) {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return nil, nil
	}
	cfg, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return nil, nil
	}
	return cfg.Phases, cfg.Allow
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
//   - AlwaysDeniedCommands (argus's own supervisor commands): always denied,
//     every phase, for a reason unrelated to git.
//   - AskGatedCommands (git commit/push): also always denied, every phase,
//     but specifically because a worker never commits or pushes at all.
//   - anything else: a repo's own phases.<name>.deny addition — scoped to
//     the one phase it was configured for, not "every phase", and not about
//     commit/push at all (e.g. "npm publish").
func denyReason(matched string, phase protocol.Phase) string {
	switch {
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
