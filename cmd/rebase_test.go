package cmd

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestRebaseDryRunNoConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus

	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// A bare origin with a main branch, and a worktree on a feature branch ahead
	// of main — no conflict, so rebase --dry-run reports nothing to do.
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	seed := t.TempDir()
	git(seed, "init", "-q")
	git(seed, "config", "user.email", "t@t")
	git(seed, "config", "user.name", "t")
	git(seed, "checkout", "-q", "-b", "main")
	git(seed, "commit", "-q", "--allow-empty", "-m", "base")
	git(seed, "remote", "add", "origin", remote)
	git(seed, "push", "-q", "-u", "origin", "main")

	wt := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, wt).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	git(wt, "config", "user.email", "t@t")
	git(wt, "config", "user.name", "t")
	// Base the feature branch on origin/main so it shares history with it (the
	// bare remote's default HEAD isn't main, so clone leaves HEAD unborn).
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/main")
	git(wt, "commit", "-q", "--allow-empty", "-m", "work")

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rebase dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "no conflict") {
		t.Errorf("expected a no-conflict message:\n%s", buf.String())
	}
}
