package supervisor

import (
	"context"
	"errors"
	"fmt"
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
		{"ssh://git@gitlab.example.com:2222/group/subgroup/project.git", "group/subgroup", "project"},
		{"https://gitlab.example.com:8443/group/subgroup/project.git", "group/subgroup", "project"},
		{"git@gitlab.com:group/subgroup/deeper/project.git", "group/subgroup/deeper", "project"},
		{"https://codeberg.org/Elysium_Labs/eos/", "Elysium_Labs", "eos"},
		{"git@codeberg.org:Owner/Repo.git/", "Owner", "Repo"},
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

// TestParseOwnerRepoRejectsSingleSegment covers a URL that survives scheme
// normalization but still resolves to only one path segment, so the
// len(parts) < 2 guard — not the earlier "no slash at all" case above — is
// what actually rejects it.
func TestParseOwnerRepoRejectsSingleSegment(t *testing.T) {
	_, _, err := parseOwnerRepo("https://codeberg.org/onlyrepo")
	if err == nil {
		t.Fatal("want error for a URL with no owner segment")
	}
	if !strings.Contains(err.Error(), "onlyrepo") {
		t.Errorf("error %q does not name the unparseable remote", err.Error())
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
	write(".argus-report-body.json", `{"phase":"awaiting_review"}`)

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
	if strings.Contains(files, ".argus-report-body.json") {
		t.Errorf("worker report-body scratch file leaked into the commit; files=%q", files)
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

// TestGitTranslatesBadRevision covers the third quotedAfter pattern
// translateGitFailure recognizes: "fatal: bad revision '<ref>'", the shape
// git uses for `git log <bad> --` rather than the "ambiguous argument" shape
// covered above.
func TestGitTranslatesBadRevision(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, err := git(context.Background(), worktree, "log", "nonexistent-base-ref", "--")
	if err == nil {
		t.Fatal("want an error for a bad revision")
	}
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("want *ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(uerr.Error(), "nonexistent-base-ref") {
		t.Errorf("error %q does not name the bad ref", uerr.Error())
	}
}

// TestGitTranslatesNotAValidObjectName covers the fourth pattern
// translateGitFailure recognizes: "fatal: Not a valid object name <ref>",
// unquoted and with no trailing punctuation to cut on unlike the other three
// — the shape `git merge-base <bad> HEAD` uses (see ResolveEffectiveDiffBase,
// which MeasureDiff/DiffFor now route through before ever reaching a plain
// `git diff`).
func TestGitTranslatesNotAValidObjectName(t *testing.T) {
	worktree, _ := initGitRepo(t)
	_, err := git(context.Background(), worktree, "merge-base", "nonexistent-base-ref", "HEAD")
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

// initPlainRepo builds a minimal, remote-less git repo for tests that only
// need CommitAll's staging machinery, not a real origin.
func initPlainRepo(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	return wt
}

// useFakeGitFailing puts a "git" shim ahead of the real one on PATH for the
// rest of the test. The shim fails only the single invocation whose
// arguments (after the leading "-C <dir>" every git() call adds) join into
// failArgs; every other invocation execs the real git untouched. No real
// repo state can selectively fail just `git reset` or just `git diff
// --cached` on demand, so this is the only way to exercise CommitAll's
// individual error-return branches.
func useFakeGitFailing(t *testing.T, failArgs string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
dir="$2"
shift 2
if [ "$*" = %q ]; then
  echo "fatal: synthetic test failure" >&2
  exit 1
fi
exec %q -C "$dir" "$@"
`, failArgs, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCommitAllAddFails(t *testing.T) {
	wt := initPlainRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitFailing(t, "add -A")
	if err := CommitAll(context.Background(), wt, "msg"); err == nil {
		t.Fatal("want error when git add -A fails")
	}
}

func TestCommitAllResetControlPlaneFails(t *testing.T) {
	wt := initPlainRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitFailing(t, "reset -q -- .claude/argus .claude/settings.local.json .argus-report-body.json")
	if err := CommitAll(context.Background(), wt, "msg"); err == nil {
		t.Fatal("want error when unstaging the control plane fails")
	}
}

func TestCommitAllDiffCachedFails(t *testing.T) {
	wt := initPlainRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitFailing(t, "diff --cached --name-only")
	if err := CommitAll(context.Background(), wt, "msg"); err == nil {
		t.Fatal("want error when checking staged files fails")
	}
}

func TestCommitAllCommitFails(t *testing.T) {
	wt := initPlainRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitFailing(t, "commit -m msg")
	err := CommitAll(context.Background(), wt, "msg")
	if err == nil {
		t.Fatal("want error when the final commit fails")
	}
	if errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("want the underlying commit error, not ErrNothingToCommit: %v", err)
	}
}

func TestRepoRootNonGitDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := RepoRoot(context.Background(), dir); err == nil {
		t.Fatal("want error for a non-git directory")
	}
}

// newLinkedWorktree builds a main repo plus a real linked worktree off it,
// for tests that need VerifyLinkedWorktree's --is-inside-work-tree check to
// pass so a later rev-parse call can be selectively broken.
func newLinkedWorktree(t *testing.T) string {
	t.Helper()
	main := t.TempDir()
	run := func(args ...string) {
		t.Helper()
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
	return linked
}

func TestVerifyLinkedWorktreeWrapsGitDirError(t *testing.T) {
	linked := newLinkedWorktree(t)
	useFakeGitFailing(t, "rev-parse --git-dir")
	err := VerifyLinkedWorktree(context.Background(), linked)
	if err == nil {
		t.Fatal("want error when resolving --git-dir fails")
	}
	if !strings.Contains(err.Error(), "resolving git dir for") {
		t.Errorf("error %q missing the wrap context", err.Error())
	}
}

func TestVerifyLinkedWorktreeWrapsGitCommonDirError(t *testing.T) {
	linked := newLinkedWorktree(t)
	useFakeGitFailing(t, "rev-parse --git-common-dir")
	err := VerifyLinkedWorktree(context.Background(), linked)
	if err == nil {
		t.Fatal("want error when resolving --git-common-dir fails")
	}
	if !strings.Contains(err.Error(), "resolving git common dir for") {
		t.Errorf("error %q missing the wrap context", err.Error())
	}
}

func TestRemoteURLReturnsRawOriginURL(t *testing.T) {
	worktree, _ := initGitRepo(t)
	want, err := exec.Command("git", "-C", worktree, "config", "--get", "remote.origin.url").CombinedOutput()
	if err != nil {
		t.Fatalf("git config: %v\n%s", err, want)
	}
	got, err := RemoteURL(context.Background(), worktree)
	if err != nil {
		t.Fatalf("RemoteURL: %v", err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Errorf("RemoteURL = %q, want %q", got, strings.TrimSpace(string(want)))
	}
}

func TestRemoteURLNoOriginErrors(t *testing.T) {
	wt := initPlainRepo(t)
	if _, err := RemoteURL(context.Background(), wt); err == nil {
		t.Fatal("want error when no origin remote is configured")
	}
}

func TestRemoteOwnerRepoNoOriginErrors(t *testing.T) {
	wt := initPlainRepo(t)
	if _, _, err := RemoteOwnerRepo(context.Background(), wt); err == nil {
		t.Fatal("want error when no origin remote is configured")
	}
}

func TestRemoteOwnerRepoParsesOrigin(t *testing.T) {
	worktree, _ := initGitRepo(t)
	// Point origin at a recognizable owner/repo URL so parseOwnerRepo's success
	// path is exercised end-to-end through the real git remote, not just unit-tested.
	if out, err := exec.Command("git", "-C", worktree, "remote", "set-url", "origin",
		"git@codeberg.org:Elysium_Labs/argus.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote set-url: %v\n%s", err, out)
	}
	owner, repo, err := RemoteOwnerRepo(context.Background(), worktree)
	if err != nil {
		t.Fatalf("RemoteOwnerRepo: %v", err)
	}
	if owner != "Elysium_Labs" || repo != "argus" {
		t.Errorf("RemoteOwnerRepo = %s/%s, want Elysium_Labs/argus", owner, repo)
	}
}
