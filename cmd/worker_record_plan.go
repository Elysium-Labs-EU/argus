package cmd

import (
	"encoding/json"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func newWorkerRecordPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "record-plan",
		Short:  "PostToolUse hook entrypoint: records a TodoWrite/TaskCreate/TaskUpdate call as live plan evidence",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkerRecordPlan(cmd.InOrStdin(), time.Now, supervisor.AppendPlanLog)
		},
	}
	return cmd
}

// postToolUseInput is the subset of Claude Code's PostToolUse hook stdin JSON
// this command needs. CWD ties the call back to a worktree the same way
// preToolUseInput's CWD does for check-tool; ToolName records which of the
// two registered matchers (see recordPlanHooks) actually fired.
type postToolUseInput struct {
	CWD      string `json:"cwd"`
	ToolName string `json:"tool_name"`
}

// runWorkerRecordPlan is newWorkerRecordPlanCmd's RunE body, split out for
// direct testing. Unlike runWorkerCheckTool, this hook is PostToolUse and
// purely observational: it fires only after the tool call it's registered
// for already ran, so there is nothing left to block. It always returns nil —
// a malformed payload, an empty cwd, or a failed append (disk full,
// permission denied) can never surface as a worker-visible failure, mirroring
// runWorkerCheckTool's own fail-open stance on malformed input, just with no
// blocking branch to mirror at all. appendPlanLog is indirected
// (supervisor.AppendPlanLog in production) so tests can observe what this
// recorded, or force the append itself to fail, without touching a real
// worktree.
func runWorkerRecordPlan(stdin io.Reader, now func() time.Time, appendPlanLog func(worktree, toolName string, ts time.Time) error) error {
	var in postToolUseInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil //nolint:nilerr // malformed hook payload fails open — observe-only, nothing to enforce
	}
	if in.CWD == "" {
		return nil
	}
	_ = appendPlanLog(in.CWD, in.ToolName, now())
	return nil
}
