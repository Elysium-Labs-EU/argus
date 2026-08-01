package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		url, owner, repo string
	}{
		{"git@codeberg.org:Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"git@codeberg.org:Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"ssh://git@codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
	}
	for _, tc := range cases {
		owner, repo, err := parseOwnerRepo(tc.url)
		if err != nil {
			t.Errorf("%s: %v", tc.url, err)
			continue
		}
		if owner != tc.owner || repo != tc.repo {
			t.Errorf("%s: got %s/%s want %s/%s", tc.url, owner, repo, tc.owner, tc.repo)
		}
	}
}

func TestParseOwnerRepoRejectsGarbage(t *testing.T) {
	if _, _, err := parseOwnerRepo("not-a-url"); err == nil {
		t.Fatal("want error for unparseable remote")
	}
}

func TestCommitAllExcludesControlPlane(t *testing.T) {
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")

	// A real code change plus the argus control-plane files a worker's worktree
	// carries. Only the code change may be committed.
	write := func(rel, content string) {
		full := filepath.Join(wt, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write(".claude/argus/status.json", `{"phase":"awaiting_review"}`)
	write(".claude/argus/brief.md", "do the thing")
	write(".claude/settings.local.json", "{}")

	if err := CommitAll(context.Background(), wt, "feat: real change"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}

	files, err := git(context.Background(), wt, "show", "--name-only", "--pretty=format:", "HEAD")
	if err != nil {
		t.Fatalf("listing committed files: %v", err)
	}
	if !strings.Contains(files, "main.go") {
		t.Errorf("real change not committed; files=%q", files)
	}
	if strings.Contains(files, ".claude") {
		t.Errorf("control-plane files leaked into the commit; files=%q", files)
	}
}

// TestRepoRootPlainRepoReturnsItsOwnAbsolutePath is the regression test for a
// real bug argus worktree prune's --repo (pointed straight at a main repo,
// unlike ship/rebase which only ever pass an already-linked worker worktree)
// exposed: `git rev-parse --git-common-dir` answers with a bare ".git" for a
// plain, non-linked repo — relative to worktree, not to argus's own cwd — and
// filepath.Dir(".git") alone resolves that relative to argus's own process
// cwd instead, silently pointing RepoRoot at a wrong, unrelated location.
func TestRepoRootPlainRepoReturnsItsOwnAbsolutePath(t *testing.T) {
	wt := t.TempDir()
	if out, err := exec.Command("git", "-C", wt, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	got, err := RepoRoot(context.Background(), wt)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	wantAbs, err := filepath.Abs(wt)
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir() can return a path through a symlink (e.g. macOS /var ->
	// /private/var); resolve both sides before comparing.
	wantReal, err := filepath.EvalSymlinks(wantAbs)
	if err != nil {
		t.Fatal(err)
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("RepoRoot returned an unresolvable path %q: %v", got, err)
	}
	if gotReal != wantReal {
		t.Errorf("RepoRoot(%s) = %s, want %s", wt, got, wt)
	}
}

// TestCommitAllRespectsNativePreCommitHook pins the core contract: CommitAll
// must never pass --no-verify/-n to `git commit`, so a repo's own
// .git/hooks/pre-commit still runs — and still blocks the commit on a
// non-zero exit — exactly as it would for a human running `git commit` by
// hand. If this test starts failing, someone added --no-verify to
// CommitAll's git invocation.
func TestCommitAllRespectsNativePreCommitHook(t *testing.T) {
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")

	hook := filepath.Join(wt, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing pre-commit hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CommitAll(context.Background(), wt, "feat: real change"); err == nil {
		t.Fatal("want CommitAll to fail when the repo's own pre-commit hook rejects the commit")
	}
}

// TestRepoRootLinkedWorktreeReturnsMainRepo confirms the already-covered
// production path (ship/rebase calling RepoRoot on a linked worker worktree)
// still resolves to the main repo, not the linked worktree itself.
func TestRepoRootLinkedWorktreeReturnsMainRepo(t *testing.T) {
	main := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "seed")
	run("branch", "feat-x")

	linked := filepath.Join(t.TempDir(), "feat-x")
	run("worktree", "add", "-q", linked, "feat-x")

	got, err := RepoRoot(context.Background(), linked)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	wantReal, err := filepath.EvalSymlinks(main)
	if err != nil {
		t.Fatal(err)
	}
	gotReal, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("RepoRoot returned an unresolvable path %q: %v", got, err)
	}
	if gotReal != wantReal {
		t.Errorf("RepoRoot(%s) = %s, want the main repo %s", linked, got, main)
	}
}

func TestVerifyLinkedWorktreeAcceptsLinkedWorktree(t *testing.T) {
	main := t.TempDir()
	run := func(dir string, args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	run(main, "config", "user.email", "t@t")
	run(main, "config", "user.name", "t")
	run(main, "commit", "-q", "--allow-empty", "-m", "seed")
	run(main, "branch", "feat-x")

	linked := filepath.Join(t.TempDir(), "feat-x")
	run(main, "worktree", "add", "-q", linked, "feat-x")

	if err := VerifyLinkedWorktree(context.Background(), linked); err != nil {
		t.Errorf("VerifyLinkedWorktree(%s) = %v, want nil for a real linked worktree", linked, err)
	}
}

func TestVerifyLinkedWorktreeRejectsMainCheckout(t *testing.T) {
	main := t.TempDir()
	if out, err := exec.Command("git", "-C", main, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	err := VerifyLinkedWorktree(context.Background(), main)
	if !errors.Is(err, ErrNotLinkedWorktree) {
		t.Errorf("VerifyLinkedWorktree(%s) = %v, want ErrNotLinkedWorktree for the main checkout", main, err)
	}
}

func TestVerifyLinkedWorktreeRejectsNonGitDirectory(t *testing.T) {
	dir := t.TempDir()

	err := VerifyLinkedWorktree(context.Background(), dir)
	if !errors.Is(err, ErrNotLinkedWorktree) {
		t.Errorf("VerifyLinkedWorktree(%s) = %v, want ErrNotLinkedWorktree for a non-git directory", dir, err)
	}
}

func TestCommitAllControlPlaneOnlyIsNothingToCommit(t *testing.T) {
	wt := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "argus"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus", "status.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The only change is argus's own control plane → nothing worth shipping.
	if err := CommitAll(context.Background(), wt, "m"); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("want ErrNothingToCommit, got %v", err)
	}
}

// TestGitTranslatesBadWorktreePath is the regression test for issue #393's
// core symptom: a --worktree git can't cd into used to surface as the raw
// "git rev-parse: fatal: cannot change to '<path>': No such file or
// directory". git() must instead return a *ui.UserError naming the bad path.
func TestGitTranslatesBadWorktreePath(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := git(context.Background(), bad, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("want an error for a nonexistent worktree path")
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want *ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(uerr.Error(), bad) {
		t.Errorf("error %q does not name the bad path %q", uerr.Error(), bad)
	}
	if strings.Contains(uerr.Error(), "fatal:") || strings.Contains(uerr.Error(), "exit status") {
		t.Errorf("error %q leaks raw git internals", uerr.Error())
	}
}

// TestGitTranslatesBadRemoteRef is the regression test for rebase's
// "git fetch: fatal: couldn't find remote ref <base>" leak on a nonexistent
// --base.
func TestGitTranslatesBadRemoteRef(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, err := git(context.Background(), worktree, "fetch", "origin", "nonexistent-base-ref")
	if err == nil {
		t.Fatal("want an error for a nonexistent remote ref")
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want *ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(uerr.Error(), "nonexistent-base-ref") {
		t.Errorf("error %q does not name the bad ref", uerr.Error())
	}
}

// TestGitTranslatesAmbiguousRef covers a bad ref caught locally (e.g. `git
// diff <bad-base>`) rather than by a remote fetch — git's own message shape
// ("ambiguous argument") differs from "couldn't find remote ref", so this is
// a distinct pattern translateGitFailure must also recognize.
func TestGitTranslatesAmbiguousRef(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, err := git(context.Background(), worktree, "diff", "nonexistent-base-ref")
	if err == nil {
		t.Fatal("want an error for a nonexistent ref")
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want *ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(uerr.Error(), "nonexistent-base-ref") {
		t.Errorf("error %q does not name the bad ref", uerr.Error())
	}
}
