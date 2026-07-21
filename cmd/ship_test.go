package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestPrTitleForAndClosesLine(t *testing.T) {
	if got := prTitleFor("", 144, "fix-x"); got != "fix: fix-x (#144)" {
		t.Errorf("default+issue title: got %q", got)
	}
	if got := prTitleFor("feat: real", 0, "b"); got != "feat: real" {
		t.Errorf("explicit title: got %q", got)
	}
	if got := closesLine(144); !strings.Contains(got, "Closes #144") {
		t.Errorf("closesLine: got %q", got)
	}
	if got := closesLine(0); got != "" {
		t.Errorf("no issue should have no closes line: got %q", got)
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	owner, name, ok := splitOwnerRepo("Elysium_Labs/argus")
	if !ok || owner != "Elysium_Labs" || name != "argus" {
		t.Errorf("got %s/%s ok=%v", owner, name, ok)
	}
	for _, bad := range []string{"noslash", "/leading", "trailing/"} {
		if _, _, ok := splitOwnerRepo(bad); ok {
			t.Errorf("splitOwnerRepo(%q) should fail", bad)
		}
	}
}

func TestResolveRepoDetectsHostAndOverride(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@github.com:acme/widget.git"})

	host, owner, name, err := resolveRepo(context.Background(), "", wt)
	if err != nil || host != "github.com" || owner != "acme" || name != "widget" {
		t.Errorf("detect: got host=%s %s/%s err=%v", host, owner, name, err)
	}
	// Override changes owner/name but the host still comes from the remote.
	host2, o2, n2, err := resolveRepo(context.Background(), "Other/Repo", wt)
	if err != nil || host2 != "github.com" || o2 != "Other" || n2 != "Repo" {
		t.Errorf("override: got host=%s %s/%s err=%v", host2, o2, n2, err)
	}
}

func TestCheckApprovedRefusesWithoutVerdict(t *testing.T) {
	// No verdict.json at all: ship must refuse unless forced.
	if err := checkApproved(t.TempDir(), false); err == nil {
		t.Fatal("want error shipping a worktree argus never cleared")
	}
}

func TestCheckApprovedRefusesNotApproved(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: false, Source: "review", Summary: "missing UPDATE path"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(wt, false); err == nil {
		t.Fatal("want error shipping a change argus did not approve")
	}
}

func TestCheckApprovedAllowsApproved(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(wt, false); err != nil {
		t.Fatalf("approved change should ship: %v", err)
	}
}

func TestCheckApprovedForceBypassesEverything(t *testing.T) {
	// No verdict, but --force overrides.
	if err := checkApproved(t.TempDir(), true); err != nil {
		t.Fatalf("--force should bypass the verdict check: %v", err)
	}
}

func TestShipCmdHelpDocumentsGitLab(t *testing.T) {
	long := newShipCmd().Long
	if !strings.Contains(long, "GitLab") {
		t.Errorf("ship --help should document GitLab as a supported forge, got: %q", long)
	}
	if !strings.Contains(long, "GITLAB_TOKEN") {
		t.Errorf("ship --help should document GITLAB_TOKEN, got: %q", long)
	}
}

func TestRunShipRequiresWorktree(t *testing.T) {
	cmd := newShipCmd()
	err := runShip(cmd, &shipArgs{})
	if err == nil {
		t.Fatal("want error when no worktree given")
	}
}

func TestRunShipDryRunPrintsPlanWithoutShipping(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", issue: 21, force: true, dryRun: true})
	if err != nil {
		t.Fatalf("dry-run ship should not error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ship plan (dry run)") {
		t.Errorf("dry-run output missing plan header: %q", out)
	}
	if !strings.Contains(out, "acme/widget") {
		t.Errorf("dry-run output missing resolved repo: %q", out)
	}
	if !strings.Contains(out, "Closes #21") {
		t.Errorf("dry-run output missing issue-derived commit message: %q", out)
	}
}

func TestRunShipFailsWithoutForgeToken(t *testing.T) {
	// example.test isn't github/codeberg/gitlab, so TokenForHost falls back to
	// EXAMPLE_TEST_TOKEN then FORGE_TOKEN; clear both so ship has no token to use.
	t.Setenv("EXAMPLE_TEST_TOKEN", "")
	t.Setenv("FORGE_TOKEN", "")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@example.test:acme/widget.git"})

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", force: true})
	if err == nil {
		t.Fatal("want error shipping to a host with no configured token")
	}
	if !strings.Contains(err.Error(), "no API token") {
		t.Errorf("want no-token error, got: %v", err)
	}
}

func TestShipChangeCommitsPushesAndOpensPR(t *testing.T) {
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	wt := gitRepo(t,
		[]string{"checkout", "-q", "-b", "feat-x"},
		[]string{"remote", "add", "origin", remote},
	)
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	if err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target); err != nil {
		t.Fatalf("shipChange: %v", err)
	}

	if f.opened == nil {
		t.Fatal("want a PR opened")
	}
	if f.opened.Head != "feat-x" || f.opened.Base != "main" || f.opened.Title != "fix: feat-x" {
		t.Errorf("PR request: %+v", f.opened)
	}
	if !strings.Contains(buf.String(), "opened PR #99") {
		t.Errorf("want ship success output, got: %q", buf.String())
	}

	// The bare remote now has the pushed branch with the committed change.
	branchOut, err := exec.Command("git", "-C", remote, "branch", "--list", "feat-x").CombinedOutput()
	if err != nil || !strings.Contains(string(branchOut), "feat-x") {
		t.Errorf("branch not pushed to remote: %q err %v", branchOut, err)
	}
}

func TestShipChangeReturnsErrorWhenNothingToCommit(t *testing.T) {
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	wt := gitRepo(t, []string{"remote", "add", "origin", remote})

	cmd := newShipCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	f := &fakeForge{}
	target := &shipTarget{branch: "main", prTitle: "fix: x", commitMsg: "fix: x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error shipping a worktree with nothing to commit")
	}
	if f.opened != nil {
		t.Error("no PR should be opened when there is nothing to ship")
	}
}

var _ forge.Forge = (*fakeForge)(nil)
