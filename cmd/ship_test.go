package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// testLogger returns a *eventlog.Logger that discards its output, for tests
// that call a function taking a *eventlog.Logger directly without going
// through openRunLog's real file-backed one.
func testLogger(t *testing.T) *eventlog.Logger {
	t.Helper()
	return eventlog.New(io.Discard, "ship", "test", nil)
}

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

// TestResolvePRTitleEnforcesConfiguredPrefix pins issue #303: a worker's
// self-reported status.Title that violates the configured
// title_prefix_template gets corrected — the required prefix mechanically
// prepended — before ship ever opens a PR with it.
func TestResolvePRTitleEnforcesConfiguredPrefix(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: "add retry backoff"}); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt, issue: 42, titlePrefixTemplate: "TICKET-{issue}: "}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if want := "TICKET-#42: add retry backoff"; got != want {
		t.Errorf("want the worker title corrected with the configured prefix, got %q, want %q", got, want)
	}
}

// TestResolvePRTitleAlreadyCorrectPrefixLeftUntouched is the other half of
// #303: a worker who already wrote the right prefix isn't double-prefixed.
func TestResolvePRTitleAlreadyCorrectPrefixLeftUntouched(t *testing.T) {
	wt := t.TempDir()
	want := "TICKET-#42: add retry backoff"
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: want}); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: wt, issue: 42, titlePrefixTemplate: "TICKET-{issue}: "}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if got != want {
		t.Errorf("an already-correct prefix should not be duplicated, got %q, want %q", got, want)
	}
}

// TestResolvePRTitleEnforcesPrefixEvenOnExplicitTitleOverride pins the other
// requirement from #303: the configured template applies to an explicit
// --title override exactly as much as to a worker-reported one (unlike the
// 72-char rule, which --title is exempt from).
func TestResolvePRTitleEnforcesPrefixEvenOnExplicitTitleOverride(t *testing.T) {
	got, err := resolvePRTitle(context.Background(), nil, bufio.NewReader(strings.NewReader("")), &bytes.Buffer{},
		&shipArgs{worktree: t.TempDir(), title: "add retry backoff", issue: 42, titlePrefixTemplate: "TICKET-{issue}: "}, "o", "r", "branch")
	if err != nil {
		t.Fatalf("resolvePRTitle: %v", err)
	}
	if want := "TICKET-#42: add retry backoff"; got != want {
		t.Errorf("want --title also corrected with the configured prefix, got %q, want %q", got, want)
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

// TestCheckApprovedAllowsApprovedDespiteVerifyArtifactLeftInWorktree pins the
// fix for ship binding ContentHash to the pre-gate-verify file set: a verify
// command (e.g. `make ci`) can leave a new non-ignored artifact after
// approval was recorded. ship must hash the recorded MeasuredFiles set, not a
// fresh re-measure that would pick up the artifact as a superset and refuse
// an otherwise-untouched approval.
func TestCheckApprovedAllowsApprovedDespiteVerifyArtifactLeftInWorktree(t *testing.T) {
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
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate", ContentHash: hash, MeasuredFiles: files}); err != nil {
		t.Fatal(err)
	}

	// A gate-verify-command-style artifact, left after approval, not part of
	// the approved set.
	if err := os.WriteFile(filepath.Join(wt, "coverage.out"), []byte("mode: set\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := checkApproved(context.Background(), wt, "HEAD", false); err != nil {
		t.Fatalf("a verify-command artifact outside the approved set should not block ship: %v", err)
	}
}

// TestCheckApprovedRefusesByteEditToApprovedFileAfterApproval is the
// invariant the above fix must not weaken: a genuine post-approval edit to a
// file that WAS in the approved set must still trip the stale-content guard.
func TestCheckApprovedRefusesByteEditToApprovedFileAfterApproval(t *testing.T) {
	wt := gitRepo(t)
	path := filepath.Join(wt, "f.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o600); err != nil {
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
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate", ContentHash: hash, MeasuredFiles: files}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("package x\n\nvar x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := checkApproved(context.Background(), wt, "HEAD", false); err == nil {
		t.Fatal("want error: an approved file's bytes changed after approval")
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

// TestRunShipDryRunRefusesAmbiguousSelfHostedGitLabHost pins the fix that a
// --dry-run against a host that looks like self-hosted GitLab must fail with
// a clear error instead of printing a clean plan that a real ship could
// never actually honor.
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
// half of that fix: --forge gitlab lets a self-hosted GitLab host through.
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

// TestShipUsesAbsoluteWorktree is a regression test: a relative --worktree
// fed through the real cobra command (not just runShip called directly) must
// reach currentBranch — the first supervisor call runShip makes — as an
// absolute path, in every common relative form an operator might pass.
// --force and --dry-run keep the test from needing a real forge token or
// push. Mirrors TestRebaseSpawnLineUsesAbsoluteWorktree (cmd/rebase_test.go).
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

// TestRunShipOmittedBaseUsesRepoConfig pins the omitted-base-falls-back-to-
// repo-config behavior: with baseIsDefault set (the real CLI path when
// --base is left unset), runShip resolves this repo's .argus/config.yml
// base_branch instead of trusting the flag's literal "main" default.
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

// TestResolveShipContextUsesRepoConfigStatusPageDefault pins issue #300: a
// self-hosted host with no built-in svcstatus entry still gets a status-page
// override from the repo's .argus/config.yml status_page key.
func TestResolveShipContextUsesRepoConfigStatusPageDefault(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{Forge: "gitea", StatusPage: "https://status.example.com"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	var buf bytes.Buffer
	a := &shipArgs{worktree: wt, base: "main", force: true}
	if _, err := resolveShipContext(context.Background(), &buf, a); err != nil {
		t.Fatalf("resolveShipContext: %v", err)
	}
	if a.statusPageURL != "https://status.example.com" {
		t.Errorf("statusPageURL = %q, want the repo config's status_page default", a.statusPageURL)
	}
}

// TestResolveShipContextExplicitStatusPageURLOverridesRepoConfig pins the
// explicit-flag-wins half: --status-page-url always wins over the repo's
// configured default, the same precedence --forge already has.
func TestResolveShipContextExplicitStatusPageURLOverridesRepoConfig(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{Forge: "gitea", StatusPage: "https://status.example.com"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	var buf bytes.Buffer
	a := &shipArgs{
		worktree: wt, base: "main", force: true,
		statusPageURL: "https://status.override.com", statusPageURLExplicit: true,
	}
	if _, err := resolveShipContext(context.Background(), &buf, a); err != nil {
		t.Fatalf("resolveShipContext: %v", err)
	}
	if a.statusPageURL != "https://status.override.com" {
		t.Errorf("statusPageURL = %q, want the explicit --status-page-url flag to win", a.statusPageURL)
	}
}

// TestRunShipDryRunCorrectsWorkerTitleAgainstRepoConfigTemplate is the
// end-to-end regression pin for issue #303: a repo's .argus/config.yml
// title_prefix_template is applied mechanically to a worker-reported title
// that violates it, and the corrected title is what the ship plan (and,
// outside --dry-run, the actual PR) uses — not the worker's raw report.
func TestRunShipDryRunCorrectsWorkerTitleAgainstRepoConfigTemplate(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{TitlePrefixTemplate: "TICKET-{issue}: "}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: "add retry backoff"}); err != nil {
		t.Fatalf("seeding worker status: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", issue: 42, force: true, dryRun: true})
	if err != nil {
		t.Fatalf("dry-run ship should not error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "TICKET-#42: add retry backoff") {
		t.Errorf("dry-run plan should show the worker's title corrected against the repo's configured prefix, got: %q", out)
	}
}

// TestRunShipExplicitTitlePrefixFlagOverridesRepoConfig pins the
// explicit-flag-wins half: --title-prefix-template always wins over the
// repo's configured default, the same precedence --forge already has.
func TestRunShipExplicitTitlePrefixFlagOverridesRepoConfig(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{TitlePrefixTemplate: "TICKET-{issue}: "}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: "add retry backoff"}); err != nil {
		t.Fatalf("seeding worker status: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{
		worktree: wt, base: "main", issue: 42, force: true, dryRun: true,
		titlePrefixTemplate: "OPS-{issue}: ", titlePrefixTemplateExplicit: true,
	})
	if err != nil {
		t.Fatalf("dry-run ship should not error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "OPS-#42: add retry backoff") {
		t.Errorf("dry-run plan should use the explicit --title-prefix-template flag, got: %q", out)
	}
	if strings.Contains(out, "TICKET-") {
		t.Errorf("explicit flag should override the repo config's template entirely, got: %q", out)
	}
}

func TestRunShipFailsWithoutForgeToken(t *testing.T) {
	// codeberg.org is on New's auto-detect allowlist. An arbitrary unlisted
	// host now fails forge-shape validation before ever reaching the token
	// check, which isn't what this test is pinning.
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
	// without re-deriving it from the branch name.
	lc, found, lerr := protocol.LoadLifecycle(wt)
	if lerr != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, lerr)
	}
	if lc.State != protocol.LifecycleShipped || lc.PRNumber != 99 || lc.PRURL != "https://fake/pull/99" {
		t.Errorf("unexpected lifecycle record: %+v", lc)
	}
	if lc.Title != target.prTitle {
		t.Errorf("lifecycle.Title = %q, want the resolved PR title %q (so fleet always has one, even if the worker never self-titled)", lc.Title, target.prTitle)
	}
}

// TestShipChangeWarnsWhenFreshCommitConflicts confirms shipChange's
// post-commit check (warnIfShipConflicts) warns the operator when the commit
// CommitAll just made will conflict with origin/base, rather than only
// finding out once the PR opens DIRTY. Push still succeeds (a new branch
// name has nothing of its own on origin to fast-forward against), so this
// is purely a warning, not a ship failure.
func TestShipChangeWarnsWhenFreshCommitConflicts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	seed := t.TempDir()
	git(seed, "init", "-q", "-b", "main")
	git(seed, "config", "user.email", "t@t")
	git(seed, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-q", "-m", "seed")
	git(seed, "remote", "add", "origin", remote)
	git(seed, "push", "-q", "-u", "origin", "main")

	// wt clones before the sibling's change lands, so its own branch diverges
	// from the same base main is still at right now — matching a worker's
	// worktree, which is created once and never re-synced until this ship
	// (via warnIfShipConflicts's own FetchBase) fetches origin again.
	wt := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, wt).CombinedOutput(); err != nil {
		t.Fatalf("clone wt: %v\n%s", err, out)
	}
	git(wt, "config", "user.email", "t@t")
	git(wt, "config", "user.name", "t")
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/main")

	// A sibling change lands on main after wt already cloned.
	sibling := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, sibling).CombinedOutput(); err != nil {
		t.Fatalf("clone sibling: %v\n%s", err, out)
	}
	git(sibling, "config", "user.email", "t@t")
	git(sibling, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(sibling, "f.txt"), []byte("origin-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(sibling, "add", "-A")
	git(sibling, "commit", "-q", "-m", "origin edits f.txt")
	git(sibling, "push", "-q", "origin", "main")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("branch-change\n"), 0o644); err != nil {
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

	if !strings.Contains(buf.String(), "will conflict with origin/main") {
		t.Errorf("want a post-commit conflict warning, got: %q", buf.String())
	}
	if f.opened == nil {
		t.Error("a conflict warning must not block ship from still opening the PR")
	}
}

// TestWarnIfShipConflictsNoopWhenBaseUnfetchable confirms a worktree whose
// origin can't be fetched (no origin remote configured here) is left silent
// — this check is best-effort and must never itself surface an error, only a
// warning when it can actually determine one.
func TestWarnIfShipConflictsNoopWhenBaseUnfetchable(t *testing.T) {
	wt := gitRepo(t)
	var buf bytes.Buffer
	warnIfShipConflicts(context.Background(), &buf, testLogger(t), wt, "main", "feat-x")
	if buf.String() != "" {
		t.Errorf("want no output when origin/base can't be fetched, got: %q", buf.String())
	}
}

// TestWarnIfShipConflictsNoopWhenClean confirms a worktree whose HEAD
// already matches origin/base produces no warning.
func TestWarnIfShipConflictsNoopWhenClean(t *testing.T) {
	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	seed := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git(seed, "init", "-q", "-b", "main")
	git(seed, "config", "user.email", "t@t")
	git(seed, "config", "user.name", "t")
	git(seed, "commit", "-q", "--allow-empty", "-m", "base")
	git(seed, "remote", "add", "origin", remote)
	git(seed, "push", "-q", "-u", "origin", "main")

	wt := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, wt).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}

	var buf bytes.Buffer
	warnIfShipConflicts(context.Background(), &buf, testLogger(t), wt, "main", "main")
	if buf.String() != "" {
		t.Errorf("want no output for a worktree already matching origin/base, got: %q", buf.String())
	}
}

// TestShipChangeReusesExistingPRInsteadOfDuplicating covers a ship retry
// after a prior run was killed between push succeeding and OpenPR completing:
// CommitAll/Push are no-ops the second time round, but without a FindPR check
// OpenPR would still fire unconditionally and open a second PR for the same
// branch.
func TestShipChangeReusesExistingPRInsteadOfDuplicating(t *testing.T) {
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

	f := &fakeForge{
		findPR:      forge.PR{Number: 42, HTMLURL: "https://fake/pull/42", State: "open"},
		findPRFound: true,
	}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	if err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target); err != nil {
		t.Fatalf("shipChange: %v", err)
	}

	if f.opened != nil {
		t.Errorf("want no new PR opened on retry, got %+v", f.opened)
	}
	if !strings.Contains(buf.String(), "reusing existing PR #42") {
		t.Errorf("want reused-PR output, got: %q", buf.String())
	}

	lc, found, lerr := protocol.LoadLifecycle(wt)
	if lerr != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, lerr)
	}
	if lc.State != protocol.LifecycleShipped || lc.PRNumber != 42 || lc.PRURL != "https://fake/pull/42" {
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

// TestShipChangeSkipsDuplicateJiraNotificationOnRetry covers a ship retry
// after a prior run already posted the Jira post-ship comment: FindPR now
// reports the PR as already existing (see
// TestShipChangeReusesExistingPRInsteadOfDuplicating), but unlike PR
// creation, Jira's Comment call has no forge-side FindPR-equivalent to
// de-dupe against — without protocol.Lifecycle.JiraNotified, shipChange would
// call postShipJira unconditionally on every retry and post a second
// identical comment.
func TestShipChangeSkipsDuplicateJiraNotificationOnRetry(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	w := &fakeJiraWriter{}
	withFakeJiraClient(t, w)

	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	args := &shipArgs{worktree: wt, base: "main", jiraIssue: "PROJ-1"}

	// First ship: no existing PR, so it opens one and notifies Jira.
	first := &fakeForge{}
	if err := shipChange(cmd, first, args, target); err != nil {
		t.Fatalf("first shipChange: %v", err)
	}
	if len(w.comments) != 1 {
		t.Fatalf("after first ship: comments = %v, want exactly one", w.comments)
	}

	// Retry: FindPR now reports the PR opened above as already existing.
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	retry := &fakeForge{
		findPR:      forge.PR{Number: 99, HTMLURL: "https://fake/pull/99", State: "open"},
		findPRFound: true,
	}
	if err := shipChange(cmd, retry, args, target); err != nil {
		t.Fatalf("retry shipChange: %v", err)
	}
	if len(w.comments) != 1 {
		t.Errorf("after retry: comments = %v, want still exactly one (no duplicate)", w.comments)
	}
	if !strings.Contains(buf.String(), "already notified Jira issue PROJ-1") {
		t.Errorf("want an already-notified message on retry, got: %q", buf.String())
	}
}

// TestShipChangeRetriesJiraTransitionAfterCommentNotified covers the gap
// left by JiraNotified gating the whole post-ship block instead of just the
// comment: a first ship where Transition fails but Comment succeeds must
// still retry Transition (idempotent) on the next ship, even though the
// comment is correctly skipped as a duplicate.
func TestShipChangeRetriesJiraTransitionAfterCommentNotified(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	w := &fakeJiraWriter{failOn: "transition"}
	withFakeJiraClient(t, w)

	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	args := &shipArgs{worktree: wt, base: "main", jiraIssue: "PROJ-1", jiraTransition: "In Review"}

	// First ship: Transition fails, Comment still succeeds, so JiraNotified
	// persists true even though the transition never landed.
	first := &fakeForge{}
	if err := shipChange(cmd, first, args, target); err != nil {
		t.Fatalf("first shipChange: %v", err)
	}
	if len(w.transitions) != 0 {
		t.Fatalf("after first ship: transitions = %v, want none (Transition failed)", w.transitions)
	}
	if len(w.comments) != 1 {
		t.Fatalf("after first ship: comments = %v, want exactly one", w.comments)
	}

	// Retry: Transition now succeeds. It must be attempted again despite
	// JiraNotified already being true, while Comment must not duplicate.
	w.failOn = ""
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	retry := &fakeForge{
		findPR:      forge.PR{Number: 99, HTMLURL: "https://fake/pull/99", State: "open"},
		findPRFound: true,
	}
	if err := shipChange(cmd, retry, args, target); err != nil {
		t.Fatalf("retry shipChange: %v", err)
	}
	if len(w.transitions) != 1 || w.transitions[0] != "In Review" {
		t.Errorf("after retry: transitions = %v, want [In Review] (retried)", w.transitions)
	}
	if len(w.comments) != 1 {
		t.Errorf("after retry: comments = %v, want still exactly one (no duplicate)", w.comments)
	}
}

// TestShipChangeFailsWhenShipVerifyCommandFails covers the .argus/config.yml
// ship_lint gate: a failing command must stop shipChange before anything is
// committed or pushed, not just get reported alongside a PR that already
// opened.
func TestShipChangeFailsWhenShipVerifyCommandFails(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipVerifyCommand: "exit 1"}); err != nil {
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

// TestShipChangeRunsPassingShipVerifyCommand is the success-path counterpart:
// a configured ship_lint that exits zero does not block the normal
// commit/push/open-PR flow.
func TestShipChangeRunsPassingShipVerifyCommand(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipVerifyCommand: "true"}); err != nil {
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
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipVerifyCommand: "exit 1"}); err != nil {
		t.Fatalf("seeding ship_lint config: %v", err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main", force: true}, target)
	if err == nil {
		t.Fatal("want ship_lint failure to block ship even with --force")
	}
}

// TestCheckApprovedLoadApprovalErrPropagates covers the LoadApproval-error
// branch: a corrupt verdict.json must fail closed, the same as no verdict at
// all, rather than being silently treated as "not found".
func TestCheckApprovedLoadApprovalErrPropagates(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(protocol.VerdictPath(wt)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol.VerdictPath(wt), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "HEAD", false); err == nil {
		t.Fatal("want error when verdict.json is malformed")
	}
}

// TestCheckApprovedMeasureDiffErrPropagates covers the re-measure step
// failing outright (a base ref that doesn't resolve) rather than merely
// disagreeing with the recorded hash.
func TestCheckApprovedMeasureDiffErrPropagates(t *testing.T) {
	wt := gitRepo(t)
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "origin/does-not-exist", false); err == nil {
		t.Fatal("want error when MeasureDiff's base ref does not resolve")
	}
}

// TestCheckApprovedContentHashErrPropagates covers ContentHash itself
// failing (not just disagreeing): a tracked path replaced on disk by a
// directory still shows up in MeasureDiff's file list (git only compares
// blobs, not on-disk file types), but ContentHash's os.ReadFile on it fails.
func TestCheckApprovedContentHashErrPropagates(t *testing.T) {
	wt := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", wt}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(wt, "sub"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	if err := os.Remove(filepath.Join(wt, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", "inner.txt"), []byte("inner\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(context.Background(), wt, "HEAD", false); err == nil {
		t.Fatal("want error when a tracked diff path is now a directory on disk")
	}
}

// TestEnforceShipGateRepoRootErrRefusesGate covers a worktree outside any
// git repo: the gate must refuse (return an error), not silently skip
// enforcement and let shipChange proceed to commit.
func TestEnforceShipGateRepoRootErrRefusesGate(t *testing.T) {
	if err := enforceShipGate(context.Background(), io.Discard, t.TempDir(), false, ""); err == nil {
		t.Fatal("want the gate to refuse a worktree outside any git repo")
	}
}

// TestEnforceShipGateRepoConfigLoadErrRefusesGate covers a malformed
// .argus/config.yml: the gate must refuse rather than silently proceeding
// as if the repo had no config at all.
func TestEnforceShipGateRepoConfigLoadErrRefusesGate(t *testing.T) {
	wt := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(wt, ".argus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoconfig.Path(wt), []byte("not a valid config line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := enforceShipGate(context.Background(), io.Discard, wt, false, "")
	if err == nil {
		t.Fatal("want the gate to refuse when the repo's config.yml is malformed")
	}
	if !strings.Contains(err.Error(), "loading") {
		t.Errorf("error should name the config load failure, got: %v", err)
	}
}

// TestResolveRepoRemoteURLErrPropagates covers a worktree with no origin
// remote at all: forge/owner/name can never be derived, so resolveRepo must
// fail rather than return zero-value host/owner/name.
func TestResolveRepoRemoteURLErrPropagates(t *testing.T) {
	wt := gitRepo(t) // no origin remote added
	if _, _, _, err := resolveRepo(context.Background(), "", wt); err == nil {
		t.Fatal("want error resolving a repo with no origin remote")
	}
}

// TestResolveRepoRejectsMalformedOverride covers --repo given in a shape
// that isn't owner/name — must fail with a UserError, not silently split on
// the wrong character or fall through with an empty owner/name.
func TestResolveRepoRejectsMalformedOverride(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@github.com:acme/widget.git"})
	_, _, _, err := resolveRepo(context.Background(), "not-owner-slash-name", wt)
	if err == nil {
		t.Fatal("want error for a --repo value that isn't owner/name")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Errorf("want a UserError, got %T: %v", err, err)
	}
}

// TestResolveShipContextCurrentBranchErrPropagates covers the
// currentBranch-fails branch: resolveShipContext must surface it rather
// than proceeding with a zero-value branch.
func TestResolveShipContextCurrentBranchErrPropagates(t *testing.T) {
	original := currentBranch
	currentBranch = func(context.Context, string) (string, error) {
		return "", errors.New("boom")
	}
	t.Cleanup(func() { currentBranch = original })

	wt := gitRepo(t)
	_, err := resolveShipContext(context.Background(), io.Discard, &shipArgs{worktree: wt, base: "main"})
	if err == nil {
		t.Fatal("want error when currentBranch fails")
	}
}

// TestResolveShipContextCheckApprovedErrPropagates covers a worktree with no
// argus verdict and no --force: resolveShipContext must refuse before ever
// resolving the repo/forge.
func TestResolveShipContextCheckApprovedErrPropagates(t *testing.T) {
	wt := gitRepo(t)
	_, err := resolveShipContext(context.Background(), io.Discard, &shipArgs{worktree: wt, base: "main"})
	if err == nil {
		t.Fatal("want error when the worktree has no argus verdict and --force is not set")
	}
}

// TestResolveShipContextResolveRepoErrPropagates covers resolveRepo failing
// (no origin remote) once the approval gate has been cleared via --force.
func TestResolveShipContextResolveRepoErrPropagates(t *testing.T) {
	wt := gitRepo(t) // no origin remote added
	_, err := resolveShipContext(context.Background(), io.Discard, &shipArgs{worktree: wt, base: "main", force: true})
	if err == nil {
		t.Fatal("want error when the worktree's origin remote can't be resolved")
	}
}

// TestRunShipRealPathTitleTooLongHeadlessErrors covers runShip's non-dry-run
// path far enough to reach resolvePRTitle: a real token and a valid forge
// let it past the earlier checks, and a too-long worker-reported title with
// no TTY attached then fails resolvePRTitle itself.
func TestRunShipRealPathTitleTooLongHeadlessErrors(t *testing.T) {
	withStdinInteractive(t, false)
	t.Setenv("CODEBERG_TOKEN", "dummy-token")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: strings.Repeat("x", 80)}); err != nil {
		t.Fatal(err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", force: true})
	if err == nil {
		t.Fatal("want error when the real (non-dry-run) path hits a too-long title with no TTY")
	}
	if !strings.Contains(err.Error(), "80") {
		t.Errorf("error should name the offending length, got: %v", err)
	}
}

// TestNewShipCmdRunERejectsUnreadableCredentialConfig covers RunE's own
// resolveCredentialOverrides-error branch, driven through the real cobra
// command (not runShip directly) so RunE's own wiring is exercised: pointing
// ARGUS_CONFIG_FILE at a directory makes config.Load fail with something
// other than "not exist".
func TestNewShipCmdRunERejectsUnreadableCredentialConfig(t *testing.T) {
	t.Setenv("ARGUS_CONFIG_FILE", t.TempDir())
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main", "--force", "--dry-run"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("want error when the persisted credential config can't be read")
	}
}

// TestPostShipJiraNewClientErrWarnsAndReturnsAlreadyNotified covers
// newJiraClient itself failing (e.g. missing Jira env config): postShipJira
// must warn and return alreadyNotified unchanged rather than panicking on a
// nil client.
func TestPostShipJiraNewClientErrWarnsAndReturnsAlreadyNotified(t *testing.T) {
	original := newJiraClient
	newJiraClient = func() (jiraIssueWriter, error) { return nil, errors.New("no JIRA_BASE_URL") }
	t.Cleanup(func() { newJiraClient = original })

	var buf bytes.Buffer
	a := &shipArgs{jiraIssue: "PROJ-1"}
	got := postShipJira(context.Background(), &buf, testLogger(t), a, forge.PR{HTMLURL: "https://fake/pull/1"}, false)
	if got != false {
		t.Errorf("want alreadyNotified(false) passed through unchanged, got %v", got)
	}
	if !strings.Contains(buf.String(), "jira post-ship for PROJ-1") {
		t.Errorf("want a jira post-ship warning in output, got: %q", buf.String())
	}
}

// TestPostShipJiraAssignErrWarnsButStillComments covers the assign-fails
// branch specifically: unlike Transition, a failed Assign must not skip the
// Comment call that links the PR back to the issue.
func TestPostShipJiraAssignErrWarnsButStillComments(t *testing.T) {
	w := &fakeJiraWriter{failOn: "assign"}
	original := newJiraClient
	newJiraClient = func() (jiraIssueWriter, error) { return w, nil }
	t.Cleanup(func() { newJiraClient = original })

	var buf bytes.Buffer
	a := &shipArgs{jiraIssue: "PROJ-1", jiraAssignee: "acc-123"}
	got := postShipJira(context.Background(), &buf, testLogger(t), a, forge.PR{HTMLURL: "https://fake/pull/1"}, false)
	if got != true {
		t.Errorf("want postShipJira to still succeed via the comment, got %v", got)
	}
	if len(w.assignees) != 0 {
		t.Errorf("want no recorded assignee since Assign failed, got %v", w.assignees)
	}
	if len(w.comments) != 1 {
		t.Errorf("want the comment still posted despite the assign failure, got %v", w.comments)
	}
	if !strings.Contains(buf.String(), "jira post-ship for PROJ-1") {
		t.Errorf("want a jira post-ship warning in output, got: %q", buf.String())
	}
}

func TestTitlePrefixIssueRefPrefersJiraIssueOverPlainIssue(t *testing.T) {
	got := titlePrefixIssueRef(&shipArgs{jiraIssue: "PROJ-9", issue: 21})
	if got != "PROJ-9" {
		t.Errorf("want the Jira key to win, got %q", got)
	}
}

func TestTitlePrefixIssueRefEmptyWhenNeitherSet(t *testing.T) {
	got := titlePrefixIssueRef(&shipArgs{})
	if got != "" {
		t.Errorf("want empty when neither --jira-issue nor --issue is set, got %q", got)
	}
}

// TestWritePRChangeSectionHappyPath covers the successful MeasureDiff path:
// a real diff against a resolvable base produces the "## Change" summary
// line plus a bullet per changed file, instead of the silently-omitted
// section MeasureDiff's error path leaves (see TestBuildPRBodyReport).
func TestWritePRChangeSectionHappyPath(t *testing.T) {
	wt := gitRepo(t)
	sha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if out, err := exec.Command("git", "-C", wt, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(string(sha))).CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wt, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", wt, "commit", "-q", "-m", "add f").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	var b strings.Builder
	writePRChangeSection(context.Background(), &b, wt, "main")
	got := b.String()
	if !strings.Contains(got, "## Change") {
		t.Errorf("want a Change section header, got: %q", got)
	}
	if !strings.Contains(got, "1 file(s), +1/-0") {
		t.Errorf("want the file/line summary, got: %q", got)
	}
	if !strings.Contains(got, "- `f.go`") {
		t.Errorf("want a per-file bullet, got: %q", got)
	}
}

// TestWritePRVerificationSectionIncludesRealWorldProof covers the
// RealWorldProof-non-empty branch: a worker's proof line must appear in the
// PR body even with no test entries at all.
func TestWritePRVerificationSectionIncludesRealWorldProof(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{RealWorldProof: "curl'd the live endpoint, got 200"}); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writePRVerificationSection(&b, wt)
	if got := b.String(); !strings.Contains(got, "Real-world proof: curl'd the live endpoint, got 200") {
		t.Errorf("want the real-world proof line, got: %q", got)
	}
}

// TestConfigDefaultsEmptyOutsideRepo covers forgeConfigDefault/
// statusPageConfigDefault/titlePrefixTemplateConfigDefault's RepoRoot-error
// branch: a worktree outside any git repo has no config to offer, so each
// must return "" rather than propagating the error (they are all
// best-effort, per their own doc comments).
func TestConfigDefaultsEmptyOutsideRepo(t *testing.T) {
	wt := t.TempDir()
	if got := forgeConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("forgeConfigDefault outside a repo = %q, want \"\"", got)
	}
	if got := statusPageConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("statusPageConfigDefault outside a repo = %q, want \"\"", got)
	}
	if got := titlePrefixTemplateConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("titlePrefixTemplateConfigDefault outside a repo = %q, want \"\"", got)
	}
}

// TestConfigDefaultsEmptyOnMalformedRepoConfig covers the same three
// functions' repoconfig.Load-error branch: a malformed .argus/config.yml
// must not propagate as an error either — each falls through to "" so a
// broken config degrades to no-default rather than blocking every ship
// command that touches this worktree.
func TestConfigDefaultsEmptyOnMalformedRepoConfig(t *testing.T) {
	wt := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(wt, ".argus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoconfig.Path(wt), []byte("not a valid config line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := forgeConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("forgeConfigDefault with a malformed config = %q, want \"\"", got)
	}
	if got := statusPageConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("statusPageConfigDefault with a malformed config = %q, want \"\"", got)
	}
	if got := titlePrefixTemplateConfigDefault(context.Background(), io.Discard, wt); got != "" {
		t.Errorf("titlePrefixTemplateConfigDefault with a malformed config = %q, want \"\"", got)
	}
}

// TestRunShipDryRunTitleTooLongHeadlessErrors covers the dry-run path's own
// resolvePRTitle call failing (distinct from the real-path call tested by
// TestRunShipRealPathTitleTooLongHeadlessErrors): a dry-run must still
// refuse to preview an over-length title rather than silently truncating.
func TestRunShipDryRunTitleTooLongHeadlessErrors(t *testing.T) {
	withStdinInteractive(t, false)
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{Title: strings.Repeat("x", 80)}); err != nil {
		t.Fatal(err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", force: true, dryRun: true})
	if err == nil {
		t.Fatal("want error when the dry-run path hits a too-long title with no TTY")
	}
	if strings.Contains(buf.String(), "ship plan (dry run)") {
		t.Errorf("dry-run should not print a plan when the title is rejected: %q", buf.String())
	}
}

// TestRunShipRealPathReachesShipChange covers runShip's own success-path
// tail (building shipTarget and calling shipChange) once token/forge/title
// resolution all succeed. A failing ship_verify_command makes shipChange
// itself fail fast, deterministically and without touching the network,
// while still proving runShip's own lines ran.
func TestRunShipRealPathReachesShipChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus
	t.Setenv("CODEBERG_TOKEN", "dummy-token")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@codeberg.org:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipVerifyCommand: "exit 1"}); err != nil {
		t.Fatalf("seeding ship_verify_command config: %v", err)
	}

	cmd := newShipCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := runShip(cmd, &shipArgs{worktree: wt, base: "main", force: true})
	if err == nil {
		t.Fatal("want the ship_verify_command failure to surface through runShip's real (non-dry-run) path")
	}
	if !strings.Contains(err.Error(), "ship_lint") {
		t.Errorf("error should name ship_lint (from enforceShipGate, reached via shipChange), got: %v", err)
	}
}

// TestShipChangeCommitFailurePropagates covers CommitAll failing for a
// reason other than ErrNothingToCommit (a rejecting pre-commit hook): unlike
// the nothing-to-commit case, this must stop shipChange before any push.
func TestShipChangeCommitFailurePropagates(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	hookDir := filepath.Join(wt, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error when the pre-commit hook rejects the commit")
	}
	if f.opened != nil {
		t.Error("no PR should be opened when the commit itself fails")
	}
}

// TestShipChangeFindPRErrPropagates covers the existing-PR check itself
// failing (a forge API error), distinct from FindPR simply reporting none
// found: shipChange must not fall through to opening a possibly-duplicate
// PR when it couldn't even check.
func TestShipChangeFindPRErrPropagates(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	f := &fakeForge{findPRErr: errors.New("forge unavailable")}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error when FindPR itself fails")
	}
	if !strings.Contains(err.Error(), "checking for an existing PR") {
		t.Errorf("error should name the FindPR step, got: %v", err)
	}
	if f.opened != nil {
		t.Error("no PR should be opened when FindPR errored")
	}
}

// TestShipChangeOpenPRErrPropagates covers OpenPR itself failing after a
// successful commit/push/FindPR-not-found: the branch is already live on
// origin at this point, but the PR never gets created and shipChange must
// report that rather than silently succeeding.
func TestShipChangeOpenPRErrPropagates(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	f := &fakeForge{openErr: errors.New("forge rejected the PR")}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target)
	if err == nil {
		t.Fatal("want error when OpenPR fails")
	}
}

// TestShipChangeWarnsOnLifecycleWriteFailure covers the best-effort
// WriteLifecycle-fails-after-a-successful-PR branch: shipChange must still
// report success (the PR really did open) while printing a warning, not
// fail the whole ship over a bookkeeping write.
func TestShipChangeWarnsOnLifecycleWriteFailure(t *testing.T) {
	wt, cmd, buf := shipChangeTestSetup(t)
	// A plain file where .claude/argus should be a directory makes every
	// protocol write under it (including WriteLifecycle) fail.
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus"), []byte("blocking"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	if err := shipChange(cmd, f, &shipArgs{worktree: wt, base: "main"}, target); err != nil {
		t.Fatalf("a lifecycle write failure must not fail the ship: %v", err)
	}
	if f.opened == nil {
		t.Fatal("want the PR still opened despite the lifecycle write failure")
	}
	if !strings.Contains(buf.String(), "recording worktree lifecycle") {
		t.Errorf("want a lifecycle-write warning in output, got: %q", buf.String())
	}
}

// TestShipChangeWarnsOnLifecycleWriteFailureAfterJiraNotify covers the
// second WriteLifecycle call — the one recording JiraNotified after a
// successful postShipJira — failing the same best-effort way as the first.
// Unlike the first failure, this one is only logged to the run log, not
// printed to out, so the observable proof here is that postShipJira still
// ran to completion (the comment landed) despite the write around it
// failing both times.
func TestShipChangeWarnsOnLifecycleWriteFailureAfterJiraNotify(t *testing.T) {
	wt, cmd, buf := shipChangeTestSetup(t)
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".claude", "argus"), []byte("blocking"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &fakeJiraWriter{}
	withFakeJiraClient(t, w)

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	args := &shipArgs{worktree: wt, base: "main", jiraIssue: "PROJ-1"}
	if err := shipChange(cmd, f, args, target); err != nil {
		t.Fatalf("a lifecycle write failure must not fail the ship: %v", err)
	}
	if f.opened == nil {
		t.Fatal("want the PR still opened despite the lifecycle write failure")
	}
	if len(w.comments) != 1 {
		t.Errorf("want postShipJira to still run to completion despite both lifecycle writes failing, comments = %v", w.comments)
	}
	if !strings.Contains(buf.String(), "recording worktree lifecycle") {
		t.Errorf("want at least the first lifecycle-write warning in output, got: %q", buf.String())
	}
}

// TestNewJiraClientDefaultWiringErrorsWithNoConfig covers the production
// default of newJiraClient (jira.NewFromEnv) actually running, instead of
// always being stubbed out by withFakeJiraClient: with no JIRA_* env vars
// and no config file reachable, it must return an error rather than a nil
// client that would panic postShipJira.
func TestNewJiraClientDefaultWiringErrorsWithNoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))

	if _, err := newJiraClient(); err == nil {
		t.Fatal("want an error from the default Jira client wiring with no Jira config present")
	}
}

// TestResolveRepoDetectErrPropagates covers forge.Detect itself failing on
// an origin remote that doesn't parse as any recognized URL shape (RemoteURL
// succeeds, but the value it returns is unusable).
func TestResolveRepoDetectErrPropagates(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "not-a-recognizable-remote-url"})
	if _, _, _, err := resolveRepo(context.Background(), "", wt); err == nil {
		t.Fatal("want error when the origin remote doesn't parse as any known forge URL shape")
	}
}

// TestIsStdinInteractiveDefaultWiring covers isStdinInteractive's own
// production closure (isatty.IsTerminal) actually running, instead of
// always being overridden by withStdinInteractive.
func TestIsStdinInteractiveDefaultWiring(t *testing.T) {
	_ = isStdinInteractive()
}

var _ forge.Forge = (*fakeForge)(nil)

// TestShipChangeShipVerifyCommandFlagOverridesConfig pins the new
// --ship-verify-command flag's explicit-flag-wins precedence: a repo config
// with a failing ship_verify_command must not block ship when an explicit
// flag override supplies a passing command instead.
func TestShipChangeShipVerifyCommandFlagOverridesConfig(t *testing.T) {
	wt, cmd, _ := shipChangeTestSetup(t)
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{ShipVerifyCommand: "exit 1"}); err != nil {
		t.Fatalf("seeding ship_verify_command config: %v", err)
	}

	f := &fakeForge{}
	target := &shipTarget{host: "fake", owner: "acme", name: "widget", branch: "feat-x", prTitle: "fix: feat-x", commitMsg: "fix: feat-x"}
	err := shipChange(cmd, f, &shipArgs{
		worktree: wt, base: "main", force: true,
		shipVerifyCmd: "true", shipVerifyCmdExplicit: true,
	}, target)
	if err != nil {
		t.Fatalf("want the explicit --ship-verify-command flag (a passing command) to win over the failing repo config, got: %v", err)
	}
}
