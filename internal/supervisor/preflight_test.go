package supervisor

import (
	"context"
	"errors"
	"path/filepath"
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

func plansWithRepoRoot(repoRoot string) []WorkerPlan {
	return []WorkerPlan{{Worker: Worker{RepoRoot: repoRoot, Worktree: filepath.Join(repoRoot, ".claude/worktrees/a")}}}
}

// TestPreflightRejectsNonexistentRepo pins the --dry-run pre-flight check:
// dry-run must fail the same way a real run eventually would (at `git
// worktree add`), not print a confident-looking plan for a --repo that was
// never there.
func TestPreflightRejectsNonexistentRepo(t *testing.T) {
	cfg := &Config{}
	plans := plansWithRepoRoot(filepath.Join(t.TempDir(), "does-not-exist"))
	err := Preflight(context.Background(), cfg, plans)
	if err == nil {
		t.Fatal("nonexistent --repo should fail preflight, got nil")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error should name --repo, got: %v", err)
	}
}

func TestPreflightRejectsNonGitRepo(t *testing.T) {
	cfg := &Config{}
	plans := plansWithRepoRoot(t.TempDir())
	err := Preflight(context.Background(), cfg, plans)
	if err == nil {
		t.Fatal("--repo pointing at a non-git directory should fail preflight, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error should say the directory is not a git repository, got: %v", err)
	}
}

func TestPreflightAcceptsValidGitRepo(t *testing.T) {
	cfg := &Config{}
	plans := plansWithRepoRoot(gitInitDir(t))
	if err := Preflight(context.Background(), cfg, plans); err != nil {
		t.Fatalf("a real git repo should pass preflight, got %v", err)
	}
}

// TestPreflightChecksRepoRootOnceAcrossWorkers pins distinctRepoRoots'
// dedup: several workers sharing one bad --repo should surface as one
// consolidated problem, not one per worker.
func TestPreflightChecksRepoRootOnceAcrossWorkers(t *testing.T) {
	cfg := &Config{}
	bad := filepath.Join(t.TempDir(), "does-not-exist")
	plans := append(plansWithRepoRoot(bad), plansWithRepoRoot(bad)...)
	plans[1].Worktree = filepath.Join(bad, ".claude/worktrees/b")
	err := Preflight(context.Background(), cfg, plans)
	var errs PreflightErrors
	if !errors.As(err, &errs) {
		t.Fatalf("want PreflightErrors, got %T (%v)", err, err)
	}
	if len(errs) != 1 {
		t.Fatalf("want 1 consolidated repo problem for 2 workers sharing one bad --repo, got %d: %v", len(errs), errs)
	}
}
