package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// fakeReviewer is a supervisor.Reviewer test double so runReview's branches
// can be driven without shelling out to the real claude CLI.
type fakeReviewer struct {
	err error
	res supervisor.ReviewResult
}

func (f fakeReviewer) Review(_ context.Context, _ *supervisor.ReviewRequest) (supervisor.ReviewResult, error) {
	return f.res, f.err
}

func reviewGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// reviewGitRepo creates a repo with one commit; withChange also leaves an
// uncommitted working-tree edit so `git diff HEAD` is non-empty.
func reviewGitRepo(t *testing.T, withChange bool) string {
	t.Helper()
	dir := t.TempDir()
	reviewGitCmd(t, dir, "init", "-q")
	reviewGitCmd(t, dir, "config", "user.email", "t@t")
	reviewGitCmd(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	reviewGitCmd(t, dir, "add", "f.txt")
	reviewGitCmd(t, dir, "commit", "-q", "-m", "base")
	if withChange {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("changed\n"), 0o644); err != nil {
			t.Fatalf("edit file: %v", err)
		}
	}
	return dir
}

func testCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	return cmd, &buf
}

func TestRunReviewNoWorktree(t *testing.T) {
	cmd, _ := testCmd()
	err := runReview(cmd, "", "HEAD", "task", nil, fakeReviewer{}, nil)
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *ui.UserError, got %v", err)
	}
	if !strings.Contains(uerr.Error(), "no worktree") {
		t.Errorf("unexpected message: %v", uerr)
	}
}

func TestRunReviewDiffError(t *testing.T) {
	cmd, _ := testCmd()
	err := runReview(cmd, t.TempDir(), "HEAD", "task", nil, fakeReviewer{}, nil)
	if err == nil {
		t.Fatal("expected an error diffing a non-git worktree")
	}
}

func TestRunReviewNoDiff(t *testing.T) {
	dir := reviewGitRepo(t, false)
	cmd, _ := testCmd()
	err := runReview(cmd, dir, "HEAD", "task", nil, fakeReviewer{}, nil)
	var uerr *ui.UserError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *ui.UserError, got %v", err)
	}
	if !strings.Contains(uerr.Error(), "no diff") {
		t.Errorf("unexpected message: %v", uerr)
	}
}

func TestRunReviewReviewerError(t *testing.T) {
	dir := reviewGitRepo(t, true)
	cmd, _ := testCmd()
	wantErr := errors.New("reviewer blew up")
	err := runReview(cmd, dir, "HEAD", "task", nil, fakeReviewer{err: wantErr}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

// capturingReviewer is a supervisor.Reviewer test double that records the
// Worktree field of the request it receives, so a test can assert on the
// path runReview actually resolved rather than the raw flag value.
type capturingReviewer struct{ got *string }

func (c capturingReviewer) Review(_ context.Context, req *supervisor.ReviewRequest) (supervisor.ReviewResult, error) {
	*c.got = req.Worktree
	return supervisor.ReviewResult{Decision: "approve", Summary: "ok"}, nil
}

// initReviewGitRepoAt inits a one-commit git repo at dir with an uncommitted
// edit, so `git diff HEAD` against it is non-empty — mirroring reviewGitRepo
// but at a caller-chosen path instead of a fresh t.TempDir(), since the
// relative-worktree regression test below needs repoDir, cwd, and the
// relative path between them to be independently controlled.
func initReviewGitRepoAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	reviewGitCmd(t, dir, "init", "-q")
	reviewGitCmd(t, dir, "config", "user.email", "t@t")
	reviewGitCmd(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	reviewGitCmd(t, dir, "add", "f.txt")
	reviewGitCmd(t, dir, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("edit file: %v", err)
	}
}

// TestReviewUsesAbsoluteWorktree is the direct regression test for argus
// issue #98: a relative --worktree fed through the real cobra command (not
// just runReview called directly) must reach both DiffFor's git -C call and
// the ReviewRequest handed to the reviewer as an absolute path, in every
// common relative form an operator might pass. Mirrors
// TestRebaseSpawnLineUsesAbsoluteWorktree (cmd/rebase_test.go, issue #96).
func TestReviewUsesAbsoluteWorktree(t *testing.T) {
	cases := []struct {
		setup func(t *testing.T, base string) (repoDir, cwd, rel string)
		name  string
	}{
		{
			name: "nested (.claude/worktrees/x)",
			setup: func(_ *testing.T, base string) (string, string, string) {
				return filepath.Join(base, ".claude", "worktrees", "featx"), base, filepath.Join(".claude", "worktrees", "featx")
			},
		},
		{
			name: "dot-slash (./x)",
			setup: func(_ *testing.T, base string) (string, string, string) {
				return filepath.Join(base, "featx"), base, "./featx"
			},
		},
		{
			name: "dot-dot-slash (../x)",
			setup: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				child := filepath.Join(base, "child")
				if err := os.MkdirAll(child, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", child, err)
				}
				return filepath.Join(base, "featx"), child, filepath.Join("..", "featx")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			base := t.TempDir()
			repoDir, cwd, rel := tc.setup(t, base)
			initReviewGitRepoAt(t, repoDir)
			t.Chdir(cwd)

			var captured string
			original := newReviewer
			newReviewer = func(_ string, _ *eventlog.Logger) supervisor.Reviewer {
				return capturingReviewer{got: &captured}
			}
			t.Cleanup(func() { newReviewer = original })

			cmd := newReviewCmd()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			cmd.SetArgs([]string{"--worktree", rel, "--base", "HEAD"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("cmd.Execute: %v", err)
			}

			if !filepath.IsAbs(captured) {
				t.Errorf("reviewer received worktree %q, want an absolute path", captured)
			}
			wantAbs, err := filepath.Abs(repoDir)
			if err != nil {
				t.Fatalf("filepath.Abs(%q): %v", repoDir, err)
			}
			if captured != wantAbs {
				t.Errorf("reviewer received worktree %q, want %q", captured, wantAbs)
			}
		})
	}
}

func TestRunReviewDecisions(t *testing.T) {
	cases := []struct {
		decision string
		want     string
	}{
		{"approve", "approve"},
		{"request-changes", "request-changes"},
		{"needs-human", "needs-human"},
	}
	for _, c := range cases {
		dir := reviewGitRepo(t, true)
		cmd, buf := testCmd()
		reviewer := fakeReviewer{res: supervisor.ReviewResult{
			Decision: c.decision,
			Summary:  "a summary",
			Findings: []string{"finding one"},
		}}
		if err := runReview(cmd, dir, "HEAD", "task", []string{"reason"}, reviewer, nil); err != nil {
			t.Fatalf("decision %q: %v", c.decision, err)
		}
		out := buf.String()
		for _, want := range []string{c.want, "a summary", "finding one"} {
			if !strings.Contains(out, want) {
				t.Errorf("decision %q: output missing %q:\n%s", c.decision, want, out)
			}
		}
	}
}
