package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

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

	var projectPhases protocol.PhaseConfig
	if repoRoot, err := supervisor.RepoRoot(ctx, in.CWD); err == nil {
		if cfg, err := repoconfig.Load(repoconfig.Path(repoRoot)); err == nil {
			projectPhases = cfg.Phases
		}
	}

	denied := protocol.ResolvedDenyForPhase(cur.Phase, projectPhases)
	reason, blocked := evaluateToolGate(in.ToolInput.Command, cur.Phase, denied)
	if !blocked {
		return nil
	}
	_, _ = fmt.Fprintln(stderr, reason)
	osExit(2)
	return nil
}

// evaluateToolGate is runWorkerCheckTool's pure decision: given the literal
// Bash command a worker is about to run, its currently reported phase, and
// the already-resolved denied-prefix list for that phase (see
// protocol.ResolvedDenyForPhase), whether to block it and why.
func evaluateToolGate(cmdStr string, phase protocol.Phase, denied []string) (reason string, blocked bool) {
	matched, ok := protocol.MatchesDeniedCommand(cmdStr, denied)
	if !ok {
		return "", false
	}
	if slices.Contains(protocol.AlwaysDeniedCommands, matched) {
		return fmt.Sprintf(
			"argus: %q is denied — argus's own supervisor commands (ship/rework/review/supervise) are for the supervising session only, never a worker's own self-invocation",
			matched,
		), true
	}
	if phase == protocol.PhasePlanning && slices.Contains(protocol.AskGatedCommands, matched) {
		return fmt.Sprintf(
			"argus: %q is denied during phase %q — report a plan first (`argus worker report <worktree> planning`) before committing or pushing",
			matched, phase,
		), true
	}
	return fmt.Sprintf("argus: %q is denied during phase %q", matched, phase), true
}
