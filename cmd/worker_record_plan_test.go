package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func TestRunWorkerRecordPlanAppendsOnValidInput(t *testing.T) {
	var gotWorktree, gotTool string
	var gotTS time.Time
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	stdin := strings.NewReader(`{"cwd":"/repo/wt","tool_name":"TodoWrite"}`)
	err := runWorkerRecordPlan(stdin, func() time.Time { return stamp }, func(worktree, toolName string, ts time.Time) error {
		gotWorktree, gotTool, gotTS = worktree, toolName, ts
		return nil
	})
	if err != nil {
		t.Fatalf("runWorkerRecordPlan: %v", err)
	}
	if gotWorktree != "/repo/wt" {
		t.Errorf("worktree = %q, want /repo/wt", gotWorktree)
	}
	if gotTool != "TodoWrite" {
		t.Errorf("toolName = %q, want TodoWrite", gotTool)
	}
	if !gotTS.Equal(stamp) {
		t.Errorf("ts = %v, want the injected clock's %v", gotTS, stamp)
	}
}

func TestRunWorkerRecordPlanMalformedStdinFailsOpen(t *testing.T) {
	called := false
	stdin := strings.NewReader("not json")
	err := runWorkerRecordPlan(stdin, time.Now, func(string, string, time.Time) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("runWorkerRecordPlan: %v, want nil (fails open on malformed input)", err)
	}
	if called {
		t.Error("appendPlanLog was called for malformed stdin, want it never invoked")
	}
}

func TestRunWorkerRecordPlanEmptyCWDFailsOpen(t *testing.T) {
	called := false
	stdin := strings.NewReader(`{"cwd":"","tool_name":"TodoWrite"}`)
	err := runWorkerRecordPlan(stdin, time.Now, func(string, string, time.Time) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("runWorkerRecordPlan: %v, want nil", err)
	}
	if called {
		t.Error("appendPlanLog was called for an empty cwd, want it never invoked")
	}
}

func TestRunWorkerRecordPlanNeverBlocksOnAppendFailure(t *testing.T) {
	stdin := strings.NewReader(`{"cwd":"/repo/wt","tool_name":"TaskUpdate"}`)
	err := runWorkerRecordPlan(stdin, time.Now, func(string, string, time.Time) error {
		return errors.New("disk full")
	})
	if err != nil {
		t.Fatalf("runWorkerRecordPlan: %v, want nil — a failed append must never surface as a worker-visible failure", err)
	}
}

// TestWorkerRecordPlanCmdWiresRealAppendPlanLog drives the actual cobra
// command (the exact wiring `argus worker record-plan` runs as a PostToolUse
// hook), proving RunE's closure — real supervisor.AppendPlanLog and time.Now,
// not the injected test doubles the unit tests above use — actually persists
// a record a later HasFreshPlanEvidence call can see.
func TestWorkerRecordPlanCmdWiresRealAppendPlanLog(t *testing.T) {
	wt := t.TempDir()
	cmd := newWorkerRecordPlanCmd()
	cmd.SetIn(strings.NewReader(fmt.Sprintf(`{"cwd":%q,"tool_name":"TodoWrite"}`, wt)))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd.Execute: %v", err)
	}

	fresh, logExists, err := supervisor.HasFreshPlanEvidence(wt)
	if err != nil {
		t.Fatalf("HasFreshPlanEvidence: %v", err)
	}
	if !logExists || !fresh {
		t.Errorf("fresh=%v logExists=%v, want true,true — the real command wiring must have appended a record", fresh, logExists)
	}
}

// TestWorkerRecordPlanCmdRejectsPositionalArgs pins Args: cobra.NoArgs end to
// end, through cmd.Execute rather than calling cmd.Args directly.
func TestWorkerRecordPlanCmdRejectsPositionalArgs(t *testing.T) {
	cmd := newWorkerRecordPlanCmd()
	cmd.SetArgs([]string{"unexpected"})
	cmd.SetIn(bytes.NewReader(nil))
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error for an unexpected positional arg, got nil")
	}
}

func TestNewWorkerRecordPlanCmdIsHiddenAndTakesNoArgs(t *testing.T) {
	cmd := newWorkerRecordPlanCmd()
	if !cmd.Hidden {
		t.Error("record-plan command must be Hidden — it's a hook entrypoint, not a worker-facing command")
	}
	if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
		t.Error("want an error for an unexpected positional arg, got nil")
	}
}
