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

	"github.com/spf13/cobra"

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
