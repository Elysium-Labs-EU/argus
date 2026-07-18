package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

// initGitRepo builds a tiny real git repo with an origin/<base> remote so the
// merge-tree conflict check runs against actual git plumbing.
func initGitRepo(t *testing.T) (worktree, base string) {
	t.Helper()
	base = "main"
	origin := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	// Bare origin with a main branch holding one file.
	run(origin, "init", "-q", "--bare", "-b", base, ".")
	seed := t.TempDir()
	run(seed, "init", "-q", "-b", base, ".")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(seed, "add", "-A")
	run(seed, "commit", "-q", "-m", "seed")
	run(seed, "remote", "add", "origin", origin)
	run(seed, "push", "-q", "origin", base)

	// Worktree clone; its branch will diverge from origin/main.
	worktree = t.TempDir()
	run(filepath.Dir(worktree), "clone", "-q", origin, filepath.Base(worktree))
	return worktree, base
}

func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func TestConflictsWithDetectsCleanAndConflicting(t *testing.T) {
	ctx := context.Background()

	// Clean case: branch edits a different line region than origin.
	wt, base := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("independent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, wt, "add", "-A")
	gitDo(t, wt, "commit", "-q", "-m", "add independent file")
	if err := FetchBase(ctx, wt, base); err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	conflicts, err := ConflictsWith(ctx, wt, base)
	if err != nil {
		t.Fatalf("ConflictsWith(clean): %v", err)
	}
	if conflicts {
		t.Errorf("independent change should not conflict")
	}

	// Conflicting case: origin and the branch edit the same line differently.
	wt2, base2 := initGitRepo(t)
	origin := mustRemote(t, wt2)
	// Advance origin/main to change f.txt line1.
	other := t.TempDir()
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, filepath.Base(other))
	if werr := os.WriteFile(filepath.Join(other, "f.txt"), []byte("origin-change\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitDo(t, other, "add", "-A")
	gitDo(t, other, "commit", "-q", "-m", "origin edits line1")
	gitDo(t, other, "push", "-q", "origin", base2)
	// Branch edits the same line differently.
	if werr := os.WriteFile(filepath.Join(wt2, "f.txt"), []byte("branch-change\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	gitDo(t, wt2, "add", "-A")
	gitDo(t, wt2, "commit", "-q", "-m", "branch edits line1")
	if ferr := FetchBase(ctx, wt2, base2); ferr != nil {
		t.Fatalf("FetchBase: %v", ferr)
	}
	conflicts, err = ConflictsWith(ctx, wt2, base2)
	if err != nil {
		t.Fatalf("ConflictsWith(conflict): %v", err)
	}
	if !conflicts {
		t.Errorf("same-line divergent edits should conflict")
	}
}

func mustRemote(t *testing.T, worktree string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", worktree, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote get-url: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestRebaseBriefCarriesRebaseSteps(t *testing.T) {
	b := RebaseBrief("feat-x", "main")
	for _, want := range []string{"feat-x", "git rebase origin/main", "--force-with-lease", protocol.WriterBrief} {
		if !strings.Contains(b, want) {
			t.Errorf("rebase brief missing %q", want)
		}
	}
}
