package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

func TestDefaultPRTitle(t *testing.T) {
	if got := defaultPRTitle(144, "fix-x"); got != "fix: fix-x (#144)" {
		t.Errorf("default+issue title: got %q", got)
	}
	if got := defaultPRTitle(0, "b"); got != "fix: b" {
		t.Errorf("default without issue: got %q", got)
	}
	if got := closesLine(144); !strings.Contains(got, "Closes #144") {
		t.Errorf("closesLine: got %q", got)
	}
	if got := closesLine(0); got != "" {
		t.Errorf("no issue should have no closes line: got %q", got)
	}
}

// withStdinInteractive forces isStdinInteractive's return value for the
// duration of one test, restoring the original on cleanup — the TTY-prompt
// branch of enforceTitleLength/resolvePRTitle is otherwise unreachable under
// `go test`, where go-isatty always reports false.
func withStdinInteractive(t *testing.T, interactive bool) {
	t.Helper()
	original := isStdinInteractive
	isStdinInteractive = func() bool { return interactive }
	t.Cleanup(func() { isStdinInteractive = original })
}

func TestResolvePRTitleExplicitFlagWinsEvenIfTooLong(t *testing.T) {
	long := strings.Repeat("x", 100)
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: t.TempDir(), title: long}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != long {
		t.Errorf("--title should win untouched, even over 72 chars: got %q", got)
	}
}

func TestResolvePRTitleUsesWorkerStatusTitle(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: "feat: add retry backoff"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{issues: map[int]forge.Issue{7: {Title: "should not be used"}}}
	got, err := resolvePRTitle(context.Background(), f, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt, issue: 7}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != "feat: add retry backoff" {
		t.Errorf("want the worker-reported title, got %q", got)
	}
}

func TestResolvePRTitleFallsBackToFetchedIssueTitle(t *testing.T) {
	wt := t.TempDir() // no status.json written: worker omitted title
	f := &fakeForge{issues: map[int]forge.Issue{7: {Title: "the real issue title"}}}
	got, err := resolvePRTitle(context.Background(), f, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt, issue: 7}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != "the real issue title" {
		t.Errorf("want the fetched issue title, got %q", got)
	}
}

func TestResolvePRTitleFallsBackToDefaultWhenNothingElseAvailable(t *testing.T) {
	wt := t.TempDir()
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt, issue: 21}, "o", "r", "fix-x")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != "fix: fix-x (#21)" {
		t.Errorf("want the branch/issue default, got %q", got)
	}
}

func TestResolvePRTitleTooLongHeadlessErrors(t *testing.T) {
	withStdinInteractive(t, false)
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: strings.Repeat("x", 80)}); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt}, "o", "r", "branch")
	if err == nil {
		t.Fatal("want an error for a too-long title with no TTY attached")
	}
	if !strings.Contains(err.Error(), "80") {
		t.Errorf("error should name the offending length: %v", err)
	}
}

func TestResolvePRTitleTooLongTTYEnterAutoTruncates(t *testing.T) {
	withStdinInteractive(t, true)
	wt := t.TempDir()
	long := strings.Repeat("x", 80)
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: long}); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("\n")), &bytes.Buffer{},
		&shipArgs{worktree: wt}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != long[:prTitleMaxLen] {
		t.Errorf("want auto-truncated to %d chars, got %q (%d chars)", prTitleMaxLen, got, len(got))
	}
}

func TestResolvePRTitleTooLongTTYAcceptsShortenedLine(t *testing.T) {
	withStdinInteractive(t, true)
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: strings.Repeat("x", 80)}); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("feat: shortened\n")), &bytes.Buffer{},
		&shipArgs{worktree: wt}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != "feat: shortened" {
		t.Errorf("want the operator's shortened title, got %q", got)
	}
}

func TestResolvePRTitleTooLongTTYShortenedStillTooLongErrors(t *testing.T) {
	withStdinInteractive(t, true)
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: strings.Repeat("x", 80)}); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader(strings.Repeat("y", 80)+"\n")), &bytes.Buffer{},
		&shipArgs{worktree: wt}, "o", "r", "branch")
	if err == nil {
		t.Fatal("want an error when the operator's shortened line is still too long")
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
	if err := checkApproved(context.Background(), t.TempDir(), "HEAD", false); err == nil {
		t.Fatal("want error shipping a worktree argus never cleared")
	}
}

func TestCheckApprovedRefusesNotApproved(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: false, Source: "review", Summary: "missing UPDATE path"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "HEAD", false); err == nil {
		t.Fatal("want error shipping a change argus did not approve")
	}
}

func TestCheckApprovedAllowsApproved(t *testing.T) {
	wt := gitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, files, err := supervisor.MeasureDiff(context.Background(), wt, "HEAD")
	if err != nil {
		t.Fatalf("MeasureDiff: %v", err)
	}
	hash, err := supervisor.ContentHash(wt, files)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate", ContentHash: hash}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "HEAD", false); err != nil {
		t.Fatalf("approved change should ship: %v", err)
	}
}

func TestCheckApprovedRefusesWhenWorktreeChangedSinceApproval(t *testing.T) {
	wt := gitRepo(t)
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate", ContentHash: "stale-hash-from-a-smaller-diff"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "HEAD", false); err == nil {
		t.Fatal("want error shipping a worktree that changed since the verdict was recorded")
	}
	if err := checkApproved(context.Background(), wt, "HEAD", true); err != nil {
		t.Fatalf("--force should bypass the content-hash check: %v", err)
	}
}

func TestCheckApprovedForceBypassesEverything(t *testing.T) {
	// No verdict, but --force overrides.
	if err := checkApproved(context.Background(), t.TempDir(), "HEAD", true); err != nil {
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

// TestRunShipDryRunRefusesAmbiguousSelfHostedGitLabHost pins the fix for
// issue #242: a --dry-run against a host that looks like self-hosted GitLab
// must fail with a clear error instead of printing a clean plan that a real
// ship could never actually honor.
func TestRunShipDryRunRefusesAmbiguousSelfHostedGitLabHost(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@gitlab.corp.example.com:acme/widget.git"})

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", issue: 21, force: true, dryRun: true})
	if err == nil {
		t.Fatal("want an error for a self-hosted-GitLab-shaped host with no --forge override")
	}
	if !strings.Contains(err.Error(), "--forge gitlab") {
		t.Errorf("error should point at the --forge gitlab escape hatch, got %q", err.Error())
	}
	if strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("dry-run should not print a plan it can't back up: %q", buf.String())
	}
}

// TestRunShipDryRunAcceptsExplicitForgeKindForSelfHostedGitLab pins the other
// half of issue #242: --forge gitlab lets a self-hosted GitLab host through.
func TestRunShipDryRunAcceptsExplicitForgeKindForSelfHostedGitLab(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@gitlab.corp.example.com:acme/widget.git"})

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", issue: 21, force: true, dryRun: true, forgeKind: "gitlab"})
	if err != nil {
		t.Fatalf("dry-run with --forge gitlab should not error: %v", err)
	}
	if !strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("dry-run output missing plan header: %q", buf.String())
	}
}

func TestRunShipRejectsUnknownForgeKind(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

	cmd := newShipCmd()
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", force: true, dryRun: true, forgeKind: "bogus"})
	if err == nil {
		t.Fatal("want an error for an unrecognized --forge value")
	}
}

func TestRunShipRequiresWorktree(t *testing.T) {
	cmd := newShipCmd()
	err := runShip(cmd, &shipArgs{})
	if err == nil {
		t.Fatal("want error when no worktree given")
	}
}

// initShipGitRepoAt inits a one-commit git repo at dir with a fake GitHub
// origin remote, so resolveRepo's forge detection has something real to
// parse without needing network access.
func initShipGitRepoAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("remote", "add", "origin", "git@github.com:acme/widget.git")
}

// TestShipUsesAbsoluteWorktree is the direct regression test for argus issue
// #98: a relative --worktree fed through the real cobra command (not just
// runShip called directly) must reach currentBranch — the first supervisor
// call runShip makes — as an absolute path, in every common relative form an
// operator might pass. --force and --dry-run keep the test from needing a
// real forge token or push. Mirrors TestRebaseSpawnLineUsesAbsoluteWorktree
// (cmd/rebase_test.go, issue #96).
func TestShipUsesAbsoluteWorktree(t *testing.T) {
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
			initShipGitRepoAt(t, repoDir)
			t.Chdir(cwd)

			var captured string
			original := currentBranch
			currentBranch = func(_ context.Context, worktree string) (string, error) {
				captured = worktree
				return "feat-x", nil
			}
			t.Cleanup(func() { currentBranch = original })

			cmd := newShipCmd()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetContext(context.Background())
			cmd.SetArgs([]string{"--worktree", rel, "--base", "main", "--force", "--dry-run"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("cmd.Execute: %v", err)
			}

			if !filepath.IsAbs(captured) {
				t.Errorf("currentBranch received worktree %q, want an absolute path", captured)
			}
			wantAbs, err := filepath.Abs(repoDir)
			if err != nil {
				t.Fatalf("filepath.Abs(%q): %v", repoDir, err)
			}
			if captured != wantAbs {
				t.Errorf("currentBranch received worktree %q, want %q", captured, wantAbs)
			}
		})
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

// TestRunShipOmittedBaseUsesRepoConfig pins issue #161/#160: with baseIsDefault
// set (the real CLI path when --base is left unset), runShip resolves this
// repo's .argus/config.yml base_branch instead of trusting the flag's
// literal "main" default.
func TestRunShipOmittedBaseUsesRepoConfig(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{BaseBranch: "develop"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", baseIsDefault: true, force: true, dryRun: true})
	if err != nil {
		t.Fatalf("dry-run ship should not error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "-> develop") {
		t.Errorf("dry-run plan should target the repo-config base branch, got: %q", out)
	}
}

// TestRunShipDryRunUsesRepoConfigForgeDefault pins the new .argus/config.yml
// forge key (issue #256): with no --forge flag, a self-hosted host argus
// would otherwise refuse (see TestRunShipDryRunRefusesAmbiguousSelfHostedGitLabHost)
// succeeds once the repo's own config supplies the forge kind.
func TestRunShipDryRunUsesRepoConfigForgeDefault(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{Forge: "gitea"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", issue: 21, force: true, dryRun: true})
	if err != nil {
		t.Fatalf("dry-run ship should use the repo config's forge default, got: %v", err)
	}
	if !strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("dry-run output missing plan header: %q", buf.String())
	}
}

// TestRunShipExplicitForgeFlagOverridesRepoConfig pins the other half of
// issue #256: an operator-passed --forge always wins over the repo's
// configured default, even when they disagree.
func TestRunShipExplicitForgeFlagOverridesRepoConfig(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{Forge: "gitea"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{
		worktree: wt, base: "main", issue: 21, force: true, dryRun: true,
		forgeKind: "bogus", forgeKindExplicit: true,
	})
	if err == nil {
		t.Fatal("want an error: explicit --forge bogus should override the repo config's forge:gitea and fail to parse")
	}
}

func TestRunShipFailsWithoutForgeToken(t *testing.T) {
	// codeberg.org is on New's auto-detect allowlist (see issue #242's rework:
	// an arbitrary unlisted host now fails forge-shape validation before ever
	// reaching the token check, which isn't what this test is pinning).
	t.Setenv("CODEBERG_TOKEN", "")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

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
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus
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

	// A lifecycle record lets `argus worktree prune` find this PR later
	// without re-deriving it from the branch name (see issue #101).
	lc, found, lerr := protocol.LoadLifecycle(wt)
	if lerr != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, lerr)
	}
	if lc.State != protocol.LifecycleShipped || lc.PRNumber != 99 || lc.PRURL != "https://fake/pull/99" {
		t.Errorf("unexpected lifecycle record: %+v", lc)
	}
}

func TestShipChangeReturnsErrorWhenNothingToCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus
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

// fakeJiraWriter is a jiraIssueWriter stub for tests: it records every
// Transition/Comment/Assign call, and can be made to fail one of them by
// name via failOn.
type fakeJiraWriter struct {
	failOn      string
	transitions []string
	comments    []string
	assignees   []string
}

func (f *fakeJiraWriter) Transition(_ context.Context, _, idOrName string) error {
	if f.failOn == "transition" {
		return errors.New("boom transition")
	}
	f.transitions = append(f.transitions, idOrName)
	return nil
}

func (f *fakeJiraWriter) Comment(_ context.Context, _, body string) error {
	if f.failOn == "comment" {
		return errors.New("boom comment")
	}
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeJiraWriter) Assign(_ context.Context, _, accountID string) error {
	if f.failOn == "assign" {
		return errors.New("boom assign")
	}
	f.assignees = append(f.assignees, accountID)
	return nil
}

// withFakeJiraClient points newJiraClient at w for the duration of one test,
// restoring the original (jira.NewFromEnv) on cleanup.
func withFakeJiraClient(t *testing.T, w jiraIssueWriter) {
	t.Helper()
	original := newJiraClient
	newJiraClient = func() (jiraIssueWriter, error) { return w, nil }
	t.Cleanup(func() { newJiraClient = original })
}

func shipChangeTestSetup(t *testing.T) (worktree string, cmd *cobra.Command, buf *bytes.Buffer) {
	t.Helper()
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

	c := newShipCmd()
	var b bytes.Buffer
	c.SetOut(&b)
	c.SetErr(&b)
	c.SetContext(context.Background())
	return wt, c, &b
}

// TestShipChangeSkipsJiraHookWhenJiraIssueUnset covers the default-off gate:
// no --jira-issue means postShipJira never runs, even if newJiraClient is
// stubbed to succeed.
func TestShipChangeSkipsJiraHookWhenJiraIssueUnset(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	w := &fakeJiraWriter{}
	withFakeJiraClient(t, w)

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	if err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target); err != nil {
		t.Fatalf("shipChange: %v", err)
	}
	if len(w.comments) != 0 {
		t.Errorf("want no Jira calls without --jira-issue, got comments %v", w.comments)
	}
}

// TestShipChangeRunsJiraPostShipHook covers the full hook: transition,
// assign, and a comment linking the opened PR, all issued once --jira-issue
// (plus --jira-transition/--jira-assignee) are set.
func TestShipChangeRunsJiraPostShipHook(t *testing.T) {
	wt, cmd, buf := shipChangeTestSetup(t)
	w := &fakeJiraWriter{}
	withFakeJiraClient(t, w)

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	args := &shipArgs{
		worktree: wt, base: "main",
		jiraIssue: "PROJ-1", jiraTransition: "In Review", jiraAssignee: "acc-123",
	}
	if err := shipChange(cmd, f, args, target); err != nil {
		t.Fatalf("shipChange: %v", err)
	}

	if len(w.transitions) != 1 || w.transitions[0] != "In Review" {
		t.Errorf("transitions = %v, want [In Review]", w.transitions)
	}
	if len(w.assignees) != 1 || w.assignees[0] != "acc-123" {
		t.Errorf("assignees = %v, want [acc-123]", w.assignees)
	}
	if len(w.comments) != 1 || !strings.Contains(w.comments[0], "https://fake/pull/99") {
		t.Errorf("comments = %v, want one linking the opened PR", w.comments)
	}
	if strings.Contains(buf.String(), "jira post-ship") {
		t.Errorf("no jira warning expected on success, got: %q", buf.String())
	}
}

// TestShipChangeWarnsButSucceedsWhenJiraHookFails covers the best-effort
// contract: a Jira post-ship failure is surfaced as a warning but does not
// fail the ship, which already succeeded (PR opened, branch pushed) by the
// time the hook runs.
func TestShipChangeWarnsButSucceedsWhenJiraHookFails(t *testing.T) {
	wt, cmd, buf := shipChangeTestSetup(t)
	w := &fakeJiraWriter{failOn: "comment"}
	withFakeJiraClient(t, w)

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	args := &shipArgs{worktree: wt, base: "main", jiraIssue: "PROJ-1"}
	if err := shipChange(cmd, f, args, target); err != nil {
		t.Fatalf("shipChange should still succeed when the jira hook fails: %v", err)
	}
	if !strings.Contains(buf.String(), "jira post-ship for PROJ-1") {
		t.Errorf("want a jira post-ship warning in output, got: %q", buf.String())
	}
}

// TestShipChangeFailsWhenShipLintCommandFails covers the .argus/config.yml
// ship_lint gate: a failing command must stop shipChange before anything is
// committed or pushed, not just get reported alongside a PR that already
// opened.
func TestShipChangeFailsWhenShipLintCommandFails(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipLint: "exit 1"}); err != nil {
		t.Fatalf("seeding ship_lint config: %v", err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error when ship_lint fails")
	}
	if !strings.Contains(err.Error(), "ship_lint") {
		t.Errorf("error should name ship_lint, got: %v", err)
	}
	if f.opened != nil {
		t.Error("no PR should be opened when ship_lint fails")
	}
	out, cerr := exec.Command("git", "-C", wt, "log", "--oneline").CombinedOutput()
	if cerr != nil {
		t.Fatalf("git log: %v", cerr)
	}
	if strings.Contains(string(out), "fix: feat-x") {
		t.Errorf("no commit should have happened when ship_lint failed: %s", out)
	}
}

// TestShipChangeRunsPassingShipLintCommand is the success-path counterpart:
// a configured ship_lint that exits zero does not block the normal
// commit/push/open-PR flow.
func TestShipChangeRunsPassingShipLintCommand(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipLint: "true"}); err != nil {
		t.Fatalf("seeding ship_lint config: %v", err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	if err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target); err != nil {
		t.Fatalf("shipChange with a passing ship_lint: %v", err)
	}
	if f.opened == nil {
		t.Fatal("want a PR opened")
	}
}

// TestShipChangeFailsWhenConfiguredHookToolMissing covers EnforceHooks wired
// into shipChange: a lefthook.yml present in the worktree with no lefthook
// binary on PATH must fail ship rather than silently committing unchecked —
// a configured-but-unenforced hook is a gap that must be loud at ship time,
// not discovered later at CI.
func TestShipChangeFailsWhenConfiguredHookToolMissing(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := os.WriteFile(filepath.Join(wt, "lefthook.yml"), []byte("pre-commit:\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error: lefthook.yml present but lefthook not on PATH")
	}
	if f.opened != nil {
		t.Error("no PR should be opened when the configured hook tool is missing")
	}
}

// TestShipChangeGateRunsEvenWithForce locks the requirement that --force only
// bypasses the argus-approval check (checkApproved), never the hook/lint
// gate: forcing a ship past a missing verdict must not also let a broken
// ship_lint command through, or --force would become a second, wider
// --no-verify.
func TestShipChangeGateRunsEvenWithForce(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipLint: "exit 1"}); err != nil {
		t.Fatalf("seeding ship_lint config: %v", err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main", force: true}, target)
	if err == nil {
		t.Fatal("want ship_lint failure to block ship even with --force")
	}
}

var _ forge.Forge = (*fakeForge)(nil)
