package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func plansWithWorktrees(worktrees ...string) []WorkerPlan {
	plans := make([]WorkerPlan, len(worktrees))
	for i, wt := range worktrees {
		plans[i].Worktree = wt
	}
	return plans
}

func TestPreflightEmptyConfigPasses(t *testing.T) {
	cfg := &Config{}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a", "/repo/.claude/worktrees/b")
	if err := Preflight(context.Background(), cfg, plans); err != nil {
		t.Fatalf("empty config with distinct worktrees should pass, got %v", err)
	}
}

func TestPreflightCatchesBadGateVerifyCommandBeforeAnyWorktree(t *testing.T) {
	cfg := &Config{GateVerifyCommand: "make ci (lefthook pre-commit)"}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a")
	err := Preflight(context.Background(), cfg, plans)
	if err == nil {
		t.Fatal("unparseable gate_verify_command should fail preflight, got nil")
	}
	if !strings.Contains(err.Error(), "gate_verify_command") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestPreflightCatchesBadWorktreeBootstrapCommand(t *testing.T) {
	cfg := &Config{WorktreeBootstrapCommand: "cp ../.env .env && ("}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a")
	err := Preflight(context.Background(), cfg, plans)
	if err == nil {
		t.Fatal("unparseable worktree_bootstrap_command should fail preflight, got nil")
	}
	if !strings.Contains(err.Error(), "worktree_bootstrap_command") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

// TestPreflightSkipsDirectExecCommands pins execArgvOrShell's own choice of
// execution path: a command with no genuine shell feature and a resolvable
// argv[0] never reaches /bin/sh at runtime, so a syntax check against sh
// would be checking a code path this command never takes.
func TestPreflightSkipsDirectExecCommands(t *testing.T) {
	cfg := &Config{GateVerifyCommand: "echo TestFoo|TestBar"}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a")
	if err := Preflight(context.Background(), cfg, plans); err != nil {
		t.Fatalf("a direct-argv-eligible command should skip the shell syntax check, got %v", err)
	}
}

func TestPreflightConsolidatesMultipleProblems(t *testing.T) {
	cfg := &Config{
		GateVerifyCommand:        "make ci (",
		WorktreeBootstrapCommand: "cp .env (",
	}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a", "/repo/.claude/worktrees/a")
	err := Preflight(context.Background(), cfg, plans)
	if err == nil {
		t.Fatal("collision plus two bad commands should fail preflight, got nil")
	}
	var errs PreflightErrors
	if !errors.As(err, &errs) {
		t.Fatalf("want PreflightErrors, got %T", err)
	}
	if len(errs) != 3 {
		t.Fatalf("want 3 consolidated problems (collision + 2 bad commands), got %d: %v", len(errs), errs)
	}
	msg := err.Error()
	for _, want := range []string{"same worktree", "gate_verify_command", "worktree_bootstrap_command"} {
		if !strings.Contains(msg, want) {
			t.Errorf("consolidated message missing %q, got:\n%s", want, msg)
		}
	}
}

func TestPreflightValidShellSyntaxPasses(t *testing.T) {
	cfg := &Config{
		GateVerifyCommand:        "make ci && echo done",
		WorktreeBootstrapCommand: "cp $(git rev-parse --show-toplevel)/.env .env",
	}
	plans := plansWithWorktrees("/repo/.claude/worktrees/a")
	if err := Preflight(context.Background(), cfg, plans); err != nil {
		t.Fatalf("valid shell syntax should pass preflight, got %v", err)
	}
}
