package cmd

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func gitRepo(t *testing.T, extra ...[]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "base")
	for _, cmd := range extra {
		run(cmd...)
	}
	return dir
}

func TestReviewCmdNoDiff(t *testing.T) {
	// A clean repo has no diff against HEAD, so review reports nothing to review.
	wt := gitRepo(t)
	cmd := newReviewCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "HEAD"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("review of a clean worktree should error with no-diff")
	}
}

func TestShipCmdDryRun(t *testing.T) {
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "fix-x"},
		[]string{"remote", "add", "origin", "git@codeberg.org:Elysium_Labs/eos.git"},
	)
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--issue", "42", "--dry-run", "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ship dry-run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Elysium_Labs/eos") || !strings.Contains(out, "fix-x") {
		t.Errorf("ship dry-run should print the plan:\n%s", out)
	}
}

func TestExecuteRunsRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rootCmd.SetArgs([]string{"stats"})
	if err := Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestReviewCmdRequiresWorktree(t *testing.T) {
	cmd := newReviewCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("review with no --worktree should error")
	}
}

func TestShipCmdRequiresWorktree(t *testing.T) {
	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("ship with no --worktree should error")
	}
}

func TestSuperviseCmdRequiresWorkers(t *testing.T) {
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("supervise with no workers should error")
	}
}

func TestSuperviseDryRunPrintsPlan(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run", "--repo", "/repo", "--tasks", "t", "--branches", "feat-t"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run supervise: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "feat-t") {
		t.Errorf("dry-run should print the plan:\n%s", out)
	}
}

func TestStatsCmdEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.argus/runs yet
	cmd := newStatsCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(buf.String(), "no run logs") {
		t.Errorf("empty stats should say so:\n%s", buf.String())
	}
}
