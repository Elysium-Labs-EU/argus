package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// initGitDir creates a minimal git repo (with a commit, so HEAD resolves) for
// tests that only need a real branch/worktree, not a full remote setup.
func initGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("checkout", "-q", "-b", "feat-x")
	run("commit", "-q", "--allow-empty", "-m", "work")
	return dir
}

// initGitDirAt is initGitDir with a caller-chosen path instead of a fresh
// t.TempDir(), and its own "origin" remote pointing at itself (so FetchBase
// succeeds against branch itself, the TestRunRebaseDryRunForcesPastNoConflict
// trick) — needed to test --worktree resolution against a specific relative
// path/cwd combination, which requires the repo living at an exact,
// predictable location. Every caller works against the same "feat-x" branch;
// "main" also exists (pointing at the same commit, never checked out). The
// trailing fetch populates the local origin/* remote-tracking refs
// CommitsAheadOfBase reads — in production those come from detectRebaseConflict's
// FetchBase plus the worker's own `git fetch origin <base>` step (RebaseBrief),
// neither of which a test calling dispatchRebaseWorker directly goes through.
func initGitDirAt(t *testing.T, dir string) {
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
	run("checkout", "-q", "-b", "feat-x")
	run("commit", "-q", "--allow-empty", "-m", "work")
	run("branch", "main")
	run("remote", "add", "origin", dir)
	run("fetch", "-q", "origin")
}

// fakeRebaseClient routes "herdr worktree ..." and "herdr pane ..." calls to
// caller-supplied outcomes, so dispatchRebaseWorker's herdr interactions can be
// tested without a real herdr binary. Its "agent get" reply reports no live
// agent (herdr.ErrAgentNotFound) — a bare shell pane, the scenario every
// existing test here models — until a "pane run" call succeeds, at which point
// it starts reporting live: this models a spawn that actually comes up, so
// dispatchIntoPane's post-spawn liveness poll (waitForAgentLive, argus issue
// #96) sees a live agent on its very first check and every test here keeps
// behaving as it did before that poll existed. paneErr exercises the PaneRun
// failure path instead, in which case no agent ever spawns.
// TestDispatchRebaseWorkerReusesLiveAgent below covers the reuse-live-agent
// branch (no spawn, no liveness poll at all) with its own fake.
func fakeRebaseClient(paneID string, worktreeErr, paneErr error) herdr.Client {
	var mu sync.Mutex
	var spawned bool
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			if worktreeErr != nil {
				return nil, worktreeErr
			}
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, paneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, paneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			if paneErr != nil {
				return nil, paneErr
			}
			mu.Lock()
			spawned = true
			mu.Unlock()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

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

// setupNoConflictOriginBehind builds a bare origin, pushes a published
// feat-x branch to it, then adds one more local-only commit on top —
// "rebase already happened locally but the push never landed", with no
// textual conflict against base since it's a clean fast-forward.
func setupNoConflictOriginBehind(t *testing.T) (worktree string) {
	t.Helper()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

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
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/main")
	git(wt, "commit", "-q", "--allow-empty", "-m", "work")
	git(wt, "push", "-q", "-u", "origin", "feat-x") // publish it, as if an earlier `argus rebase` run had pushed successfully

	git(wt, "commit", "-q", "--allow-empty", "-m", "rebased locally, push never landed")
	return wt
}

// TestRunRebaseNoConflictOriginBehindPushesDirectly confirms a branch that's
// already rebased locally (no textual conflict — ConflictsWith reports
// false) but whose origin ref is behind must still get pushed, not be waved
// off as "nothing to rebase".
func TestRunRebaseNoConflictOriginBehindPushesDirectly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := setupNoConflictOriginBehind(t)

	localHead, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main"})
	if rerr := cmd.Execute(); rerr != nil {
		t.Fatalf("rebase: %v\noutput:\n%s", rerr, buf.String())
	}
	if !strings.Contains(buf.String(), "pushed to origin") {
		t.Errorf("expected a direct-push success message, got:\n%s", buf.String())
	}

	remoteHead, err := exec.Command("git", "-C", wt, "ls-remote", "origin", "refs/heads/feat-x").Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if !strings.HasPrefix(string(remoteHead), strings.TrimSpace(string(localHead))) {
		t.Errorf("origin/feat-x = %q, want it to now equal local HEAD %q", remoteHead, localHead)
	}
}

// TestRunRebaseNoConflictOriginBehindDryRunDoesNotPush confirms --dry-run
// reports the push plan without performing it.
func TestRunRebaseNoConflictOriginBehindDryRunDoesNotPush(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := setupNoConflictOriginBehind(t)

	before, err := exec.Command("git", "-C", wt, "ls-remote", "origin", "refs/heads/feat-x").Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main", "--dry-run"})
	if rerr := cmd.Execute(); rerr != nil {
		t.Fatalf("rebase --dry-run: %v\noutput:\n%s", rerr, buf.String())
	}
	if !strings.Contains(buf.String(), "force-push directly") {
		t.Errorf("expected the dry-run plan to describe the direct push, got:\n%s", buf.String())
	}

	after, err := exec.Command("git", "-C", wt, "ls-remote", "origin", "refs/heads/feat-x").Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("--dry-run must not push: origin/feat-x changed from %q to %q", before, after)
	}
}

// TestRunRebaseNoConflictOriginBehindPushRejectedSurfacesError confirms a
// rejected push (a pre-push hook here, standing in for any non-zero `git
// push` exit) must surface as a failing argus rebase run, not a silent
// no-op.
func TestRunRebaseNoConflictOriginBehindPushRejectedSurfacesError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := setupNoConflictOriginBehind(t)

	hooksDir, err := exec.Command("git", "-C", wt, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		t.Fatalf("rev-parse --git-path hooks: %v", err)
	}
	hookPath := filepath.Join(wt, strings.TrimSpace(string(hooksDir)), "pre-push")
	if werr := os.WriteFile(hookPath, []byte("#!/bin/sh\necho 'rejected by crap-gate' >&2\nexit 1\n"), 0o755); werr != nil {
		t.Fatalf("writing pre-push hook: %v", werr)
	}

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--base", "main"})
	err = cmd.Execute()
	if err == nil {
		t.Fatal("want a non-zero exit when the pre-push hook rejects the push, got nil")
	}
	if !strings.Contains(err.Error(), "rejected by crap-gate") {
		t.Errorf("error should surface the hook's own rejection message, got %v", err)
	}
}

// TestRebaseDryRunForcedShowsRepoRoot exercises the --force dry-run path (no
// conflict, but forced past it) that actually computes RepoRoot — the value
// WorktreeOpen needs as --cwd so herdr doesn't reject the request with
// "not_git_worktree" when the calling pane itself isn't repo-rooted. Dry-run
// resolves and prints it (read-only git plumbing, no side effect) so a
// broken worktree is caught here too, not just on the real dispatch.
// TestRebaseDryRunOmittedBaseUsesRepoConfig pins the omitted-base-falls-back-
// to-repo-config behavior end to end through the real CLI: with --base left
// unset, runRebase must resolve the repo's own .argus/config.yml base_branch
// instead of the flag's literal "main" default — here the repo's real
// default branch is "trunk", so a silent fallback to "main" would target the
// wrong ref entirely.
func TestRebaseDryRunOmittedBaseUsesRepoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	seed := t.TempDir()
	git(seed, "init", "-q")
	git(seed, "config", "user.email", "t@t")
	git(seed, "config", "user.name", "t")
	git(seed, "checkout", "-q", "-b", "trunk")
	git(seed, "commit", "-q", "--allow-empty", "-m", "base")
	git(seed, "remote", "add", "origin", remote)
	git(seed, "push", "-q", "-u", "origin", "trunk")

	wt := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, wt).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	git(wt, "config", "user.email", "t@t")
	git(wt, "config", "user.name", "t")
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/trunk")
	git(wt, "commit", "-q", "--allow-empty", "-m", "work")

	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{BaseBranch: "trunk"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", wt, "--dry-run"}) // no --base
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rebase dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "no conflict") {
		t.Errorf("expected a no-conflict message using the repo-config base:\n%s", buf.String())
	}
}

func TestRebaseDryRunForcedShowsRepoRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

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

	repo := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, repo).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	git(repo, "config", "user.email", "t@t")
	git(repo, "config", "user.name", "t")
	git(repo, "checkout", "-q", "-b", "feat-x", "origin/main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "work")

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", repo, "--base", "main", "--force", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rebase dry-run --force: %v", err)
	}
	if !strings.Contains(buf.String(), "repo:") {
		t.Errorf("expected the plan to include a repo: line, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), repo) {
		t.Errorf("expected the resolved repo root (%s) in the plan, got:\n%s", repo, buf.String())
	}
}

// TestBuildRebaseSpawnLineInjectsCredProxySentinel guards the small-consistency
// fix at cmd/rebase.go: this path used to pass workerEnv: nil unconditionally,
// so a rebase-dispatched worker never got the credproxy sentinel treatment
// spawn-mode workers get. With ANTHROPIC_API_KEY set and --no-cred-proxy
// unset, the spawn line must carry an injected ANTHROPIC_API_KEY=argus-
// sentinel-... assignment, not the real key and not nothing.
func TestBuildRebaseSpawnLineInjectsCredProxySentinel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-real-key-should-never-appear")
	logger := eventlog.New(nil, "rebase", "test-run", nil)

	spawnLine, cleanup, err := buildRebaseSpawnLine(context.Background(), logger, "/repo/wt", "feat-x", "claude", "", false, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("buildRebaseSpawnLine: %v", err)
	}
	if !strings.Contains(spawnLine, "ANTHROPIC_API_KEY='argus-sentinel-") {
		t.Errorf("spawn line should carry an injected credproxy sentinel, got %q", spawnLine)
	}
	if strings.Contains(spawnLine, "sk-ant-real-key-should-never-appear") {
		t.Errorf("spawn line must never carry the real API key: %q", spawnLine)
	}
}

// TestBuildRebaseSpawnLineNoCredProxyOptOut confirms --no-cred-proxy is honored
// even when an API key is present: no proxy starts, no env is injected.
func TestBuildRebaseSpawnLineNoCredProxyOptOut(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-real-key")
	logger := eventlog.New(nil, "rebase", "test-run", nil)

	spawnLine, cleanup, err := buildRebaseSpawnLine(context.Background(), logger, "/repo/wt", "feat-x", "claude", "", true, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("buildRebaseSpawnLine: %v", err)
	}
	if strings.Contains(spawnLine, "ANTHROPIC_API_KEY") {
		t.Errorf("--no-cred-proxy should inject no credential env at all, got %q", spawnLine)
	}
}

func TestRunRebaseEmptyWorktree(t *testing.T) {
	cmd := newRebaseCmd()
	err := runRebase(cmd, herdr.New(), &rebaseOpts{})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError for an empty worktree, got %v", err)
	}
}

// TestRunRebaseDryRunForcesPastNoConflict exercises the --force branch of the
// "!conflicts && !force" check (force=true keeps the early no-conflict return
// from firing) and then the --dry-run branch, without dispatching a worker.
func TestRunRebaseDryRunForcesPastNoConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initGitDir(t)
	// FetchBase needs an "origin" remote to fetch from; point it at the repo
	// itself so `git fetch origin main` succeeds without a real conflict.
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", dir).CombinedOutput(); err != nil {
		t.Fatalf("remote add: %v\n%s", err, out)
	}

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--worktree", dir, "--base", "feat-x", "--force", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rebase --force --dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "rebase plan (dry run)") {
		t.Errorf("expected a dry-run plan message:\n%s", buf.String())
	}
}

// TestDetectRebaseConflictFetchBaseError covers detectRebaseConflict's FetchBase
// error path: a repo with no "origin" remote can't fetch, so it fails before
// ConflictsWith ever runs.
func TestDetectRebaseConflictFetchBaseError(t *testing.T) {
	dir := initGitDir(t)
	if _, _, err := detectRebaseConflict(context.Background(), dir, "main"); err == nil {
		t.Fatal("want an error fetching from a repo with no origin remote")
	}
}

func TestDispatchRebaseWorkerWorktreeOpenError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("", errors.New("herdr: no such worktree"), nil)

	err := dispatchRebaseWorker(context.Background(), logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{worktree: t.TempDir(), base: "main"})
	if err == nil || !strings.Contains(err.Error(), "no such worktree") {
		t.Fatalf("want the WorktreeOpen error propagated, got %v", err)
	}
}

func TestDispatchRebaseWorkerEmptyPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("", nil, nil) // no pane_id in the reply

	err := dispatchRebaseWorker(context.Background(), logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{worktree: t.TempDir(), base: "main"})
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Fatalf("want a *ui.UserError when herdr opens no pane, got %v", err)
	}
}

func TestDispatchRebaseWorkerPaneRunError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, errors.New("herdr: pane gone"))

	err := dispatchRebaseWorker(context.Background(), logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{worktree: t.TempDir(), base: "main", launcher: "claude"})
	if err == nil || !strings.Contains(err.Error(), "pane gone") {
		t.Fatalf("want the PaneRun error propagated, got %v", err)
	}
}

// TestDispatchRebaseWorkerSuccessTerminal covers the happy path: herdr opens a
// pane and runs the worker, and a status.json the worker writes *after*
// dispatch is picked up once WaitForStatus polls again. A stale status.json
// left over from before dispatch is seeded too, to confirm it gets invalidated
// rather than short-circuiting the wait. worktree is a real
// git repo with its own origin (pointing at itself) already at local HEAD, so
// the post-status VerifyPushLanded check this test also exercises sees the
// push as having landed, the same as a worker whose force-push actually
// reached origin would.
func TestDispatchRebaseWorkerSuccessTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	initGitDirAt(t, worktree)
	stale := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now().Add(-time.Hour)}
	if err := protocol.Write(protocol.StatusPath(worktree), stale); err != nil {
		t.Fatalf("seeding stale status.json: %v", err)
	}

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)
	var buf bytes.Buffer

	// Simulate the dispatched worker writing its own fresh status shortly after
	// dispatch, the way a real worker pane eventually would.
	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now()}
		_ = protocol.Write(protocol.StatusPath(worktree), fresh)
	}()

	err := dispatchRebaseWorker(context.Background(), logger, client, &buf, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dispatchRebaseWorker: %v", err)
	}
	if !strings.Contains(buf.String(), "dispatched rebase worker") || !strings.Contains(buf.String(), "rebased and ready") {
		t.Errorf("expected dispatch + outcome messages:\n%s", buf.String())
	}
}

// setupRebaseUncommittedResolution builds a bare origin, publishes feat-x to
// it, then leaves an uncommitted file change in the worktree — the shape
// RebaseBrief now asks a worker to leave behind (conflict resolved, merge
// left staged but uncommitted) instead of committing and pushing itself.
func setupRebaseUncommittedResolution(t *testing.T) (worktree string) {
	t.Helper()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	remote := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	seed := t.TempDir()
	git(seed, "init", "-q")
	git(seed, "config", "user.email", "t@t")
	git(seed, "config", "user.name", "t")
	git(seed, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-q", "-m", "base")
	git(seed, "remote", "add", "origin", remote)
	git(seed, "push", "-q", "-u", "origin", "main")

	wt := t.TempDir()
	if out, err := exec.Command("git", "clone", "-q", remote, wt).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	git(wt, "config", "user.email", "t@t")
	git(wt, "config", "user.name", "t")
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/main")
	git(wt, "commit", "-q", "--allow-empty", "-m", "original work")
	git(wt, "push", "-q", "-u", "origin", "feat-x")

	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestDispatchRebaseWorkerCommitsResolutionAndPushes confirms the new
// contract end to end: a worker that reports awaiting_review having left its
// conflict resolution staged but uncommitted (per RebaseBrief) never runs
// git commit or git push itself — dispatchRebaseWorker commits the resolved
// diff using the worker's reported title and force-pushes it.
func TestDispatchRebaseWorkerCommitsResolutionAndPushes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := setupRebaseUncommittedResolution(t)

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)
	var buf bytes.Buffer

	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now(), Title: "fix: resolve conflict with origin/main"}
		_ = protocol.Write(protocol.StatusPath(worktree), fresh)
	}()

	err := dispatchRebaseWorker(context.Background(), logger, client, &buf, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dispatchRebaseWorker: %v", err)
	}
	if !strings.Contains(buf.String(), "rebased and ready") {
		t.Errorf("expected the success message, got:\n%s", buf.String())
	}

	logOut, lerr := exec.Command("git", "-C", worktree, "log", "-1", "--format=%s").Output()
	if lerr != nil {
		t.Fatalf("git log: %v", lerr)
	}
	if got := strings.TrimSpace(string(logOut)); got != "fix: resolve conflict with origin/main" {
		t.Errorf("commit message = %q, want the worker's reported title", got)
	}

	remoteHead, rerr := exec.Command("git", "-C", worktree, "ls-remote", "origin", "refs/heads/feat-x").Output()
	if rerr != nil {
		t.Fatalf("ls-remote: %v", rerr)
	}
	localHead, herr := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if herr != nil {
		t.Fatalf("rev-parse HEAD: %v", herr)
	}
	if !strings.HasPrefix(string(remoteHead), strings.TrimSpace(string(localHead))) {
		t.Errorf("origin/feat-x = %q, want it to equal the freshly committed local HEAD %q", remoteHead, localHead)
	}
}

// TestDispatchRebaseWorkerCommitSucceedsButPushRejectedFails confirms that
// once dispatchRebaseWorker has committed a worker's resolved diff, a
// rejected push (a pre-push hook here, standing in for any non-zero `git
// push` exit) fails the rebase run loudly instead of printing "rebased and
// ready" — the worker never pushes itself under the new contract, so this
// failure can only be argus's own to catch.
func TestDispatchRebaseWorkerCommitSucceedsButPushRejectedFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := setupRebaseUncommittedResolution(t)

	hooksDir, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		t.Fatalf("rev-parse --git-path hooks: %v", err)
	}
	hookPath := filepath.Join(worktree, strings.TrimSpace(string(hooksDir)), "pre-push")
	if werr := os.WriteFile(hookPath, []byte("#!/bin/sh\necho 'rejected by crap-gate' >&2\nexit 1\n"), 0o755); werr != nil {
		t.Fatalf("writing pre-push hook: %v", werr)
	}

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)
	var buf bytes.Buffer

	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now()}
		_ = protocol.Write(protocol.StatusPath(worktree), fresh)
	}()

	err = dispatchRebaseWorker(context.Background(), logger, client, &buf, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("want an error when the push is rejected, got nil")
	}
	if !strings.Contains(err.Error(), "rejected by crap-gate") {
		t.Errorf("error should surface the hook's own rejection message, got %v", err)
	}
	if strings.Contains(buf.String(), "rebased and ready") {
		t.Errorf("must not print the success message when the push was rejected:\n%s", buf.String())
	}

	logOut, lerr := exec.Command("git", "-C", worktree, "log", "-1", "--format=%s").Output()
	if lerr != nil {
		t.Fatalf("git log: %v", lerr)
	}
	if got := strings.TrimSpace(string(logOut)); !strings.HasPrefix(got, "rebase: resolve") {
		t.Errorf("commit message = %q, want the fallback rebase message (worker reported no title)", got)
	}
}

// setupRebaseZeroDivergence reproduces argus#348: a worktree whose branch was
// checked out straight off origin/main and never committed to (the worker
// only ever made uncommitted changes, per its brief — "do NOT git commit or
// push; argus ships"). origin/feat-x never existed, and after a rebase round
// HEAD is still exactly origin/main — zero commits of its own to publish.
func setupRebaseZeroDivergence(t *testing.T) (worktree string) {
	t.Helper()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

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
	git(wt, "checkout", "-q", "-b", "feat-x", "origin/main")
	return wt
}

// TestDispatchRebaseWorkerZeroDivergenceSucceedsWithNoOriginRef reproduces
// argus#348: a worker that resolves its rebase without ever having a commit
// of its own to force-push (HEAD lands exactly on origin/main) correctly
// performs no push at all — origin/feat-x staying absent is expected, not a
// failure, and dispatchRebaseWorker must report success rather than
// misreading the missing ref as a push that silently didn't land.
func TestDispatchRebaseWorkerZeroDivergenceSucceedsWithNoOriginRef(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := setupRebaseZeroDivergence(t)

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)
	var buf bytes.Buffer

	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now()}
		_ = protocol.Write(protocol.StatusPath(worktree), fresh)
	}()

	err := dispatchRebaseWorker(context.Background(), logger, client, &buf, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dispatchRebaseWorker should succeed when the branch has zero commits beyond base: %v", err)
	}
	if !strings.Contains(buf.String(), "rebased and ready") {
		t.Errorf("expected the success message, got:\n%s", buf.String())
	}
}

// TestDispatchRebaseWorkerIgnoresStaleStatus confirms a worktree carrying a
// leftover terminal status.json from before this rebase was dispatched (the
// normal supervise flow's own awaiting_review, unrelated to the rebase) is
// never reported as this dispatch's outcome. The fake herdr client never
// actually runs a worker, so no fresh status is written, and
// dispatchRebaseWorker must time out instead of reporting the stale phase as
// success. dispatchRebaseWorker re-creates status.json right after
// invalidating it (see TestDispatchRebaseWorkerPersistsBaseAfterInvalidate),
// so this only checks the stale phase doesn't survive, not that the file
// stays absent.
func TestDispatchRebaseWorkerIgnoresStaleStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	stale := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now().Add(-time.Hour)}
	if err := protocol.Write(protocol.StatusPath(worktree), stale); err != nil {
		t.Fatalf("seeding stale status.json: %v", err)
	}

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := dispatchRebaseWorker(ctx, logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "no status before the deadline") {
		t.Fatalf("want the no-status error for a stale pre-dispatch status.json, got %v", err)
	}
	status, lerr := protocol.Load(protocol.StatusPath(worktree))
	if lerr != nil {
		t.Fatalf("loading status.json after dispatch: %v", lerr)
	}
	if status.Phase == protocol.PhaseAwaitingReview {
		t.Errorf("expected the stale awaiting_review status to be invalidated before dispatch, got phase %v", status.Phase)
	}
}

// TestDispatchRebaseWorkerPersistsBaseAfterInvalidate confirms Base survives
// InvalidateStatus (see TestDispatchRebaseWorkerIgnoresStaleStatus above),
// which removes status.json entirely — a worker's own `argus worker report`
// never sets Base itself, only carries forward whatever value is already on
// disk, so a dropped Base stays dropped. No worker reports here (ctx times
// out first), so the only way status.json can carry Base afterward is
// dispatchRebaseWorker's own write.
func TestDispatchRebaseWorkerPersistsBaseAfterInvalidate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()

	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := dispatchRebaseWorker(ctx, logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "trunk", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "no status before the deadline") {
		t.Fatalf("want the no-status error, got %v", err)
	}
	status, lerr := protocol.Load(protocol.StatusPath(worktree))
	if lerr != nil {
		t.Fatalf("loading status.json after dispatch: %v", lerr)
	}
	if status.Base != "trunk" {
		t.Errorf("want Base %q preserved across InvalidateStatus, got %q", "trunk", status.Base)
	}
}

// TestDispatchRebaseWorkerNoStatus covers the !seen path: no status.json is ever
// written, so WaitForStatus returns once ctx is canceled.
func TestDispatchRebaseWorkerNoStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := fakeRebaseClient("w1:p1", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := dispatchRebaseWorker(ctx, logger, client, &bytes.Buffer{}, "/repo", "feat-x", &rebaseOpts{
		worktree: t.TempDir(), base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "no status before the deadline") {
		t.Fatalf("want the no-status error, got %v", err)
	}
}

// TestDispatchRebaseWorkerReusesLiveAgent is the regression test for argus issue
// #88: rebase targets a worktree an earlier task's worker already ran in, so its
// root pane very often still holds that worker's live, idle Claude Code session
// rather than a bare shell. When herdr's "agent get" reports one, dispatch must
// re-task it via AgentPrompt (the same as a human typing a new instruction into
// that session) instead of typing a `cd && claude ...` shell command line into the
// agent's own input box via PaneRun — which is what used to silently no-op,
// leaving the branch untouched with no status.json ever written. This fake's
// "agent get" reports a live agent, so a "pane run" call here would mean the fix
// regressed to the broken path.
func TestDispatchRebaseWorkerReusesLiveAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	worktree := t.TempDir()
	// A real repo whose origin (itself) already matches local HEAD, so the
	// post-status VerifyPushLanded check sees the push as landed. See
	// TestDispatchRebaseWorkerSuccessTerminal.
	initGitDirAt(t, worktree)
	logger := eventlog.New(nil, "rebase", "test-run", nil)

	var mu sync.Mutex
	var calls [][]string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), args...))
		mu.Unlock()
		switch {
		case args[0] == "worktree":
			return []byte(`{"result":{"root_pane":{"pane_id":"w1:p1"}}}`), nil
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"done"}}}`), nil
		case args[0] == "pane" && args[1] == "run":
			t.Errorf("dispatch used PaneRun (spawn) instead of AgentPrompt for a pane with a live agent: %v", args)
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})

	go func() {
		time.Sleep(30 * time.Millisecond)
		fresh := &protocol.Status{Phase: protocol.PhaseAwaitingReview, UpdatedAt: time.Now()}
		_ = protocol.Write(protocol.StatusPath(worktree), fresh)
	}()

	var buf bytes.Buffer
	err := dispatchRebaseWorker(context.Background(), logger, client, &buf, "/repo", "feat-x", &rebaseOpts{
		worktree: worktree, base: "main", launcher: "claude", interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("dispatchRebaseWorker: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawPrompt bool
	for _, c := range calls {
		if len(c) > 2 && c[0] == "agent" && c[1] == "prompt" && c[2] == "w1:p1" {
			sawPrompt = true
			if c[3] != supervisor.InitialPrompt {
				t.Errorf("agent prompt text = %q, want %q", c[3], supervisor.InitialPrompt)
			}
		}
	}
	if !sawPrompt {
		t.Errorf("want an `agent prompt` call re-tasking the live agent, calls: %v", calls)
	}
}

// TestDispatchIntoPaneAgentGetError confirms a genuine AgentGet failure (not
// herdr's "no live agent" outcome) aborts dispatch instead of falling through to
// spawning a second worker over an agent whose state herdr couldn't determine.
func TestDispatchIntoPaneAgentGetError(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "agent" && args[1] == "get" {
			return nil, errors.New("herdr: socket unavailable")
		}
		t.Fatalf("unexpected call: %v", args)
		return nil, nil
	})

	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", &dispatchTarget{worktree: t.TempDir(), launcher: "claude"})
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the AgentGet error propagated, got %v", err)
	}
}

// TestDispatchIntoPaneLiveAgentPromptNeverPickedUpReturnsError is a direct
// regression test: a live agent's AgentPrompt call used to return as soon as
// herdr accepted the text, with no confirmation the agent ever reacted — so
// a prompt silently dropped (idle/done agent, or a race with another
// concurrent AgentPrompt call) left the caller's subsequent status.json poll
// waiting forever, with no error surfaced
// anywhere. herdr's own `--wait --until working` now makes that failure
// mode observable: this fake models herdr reporting it never saw the
// working transition (the same outcome herdr's own "timeout"/
// "agent_prompt_stalled" error codes produce), and dispatchIntoPane must
// return promptly with an error naming the pane instead of reporting nil.
func TestDispatchIntoPaneLiveAgentPromptNeverPickedUpReturnsError(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"idle"}}}`), nil
		case args[0] == "agent" && args[1] == "prompt":
			return nil, errors.New(`herdr agent: exit status 1: {"error":{"code":"timeout","message":"no state change to working observed"}}`)
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	})

	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", &dispatchTarget{worktree: t.TempDir(), launcher: "claude"})
	if err == nil {
		t.Fatal("want an error when herdr never observes the live agent pick up the prompt, got nil")
	}
	if !strings.Contains(err.Error(), "w1:p1") {
		t.Errorf("error should name the pane, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should surface herdr's underlying failure, got %q", err.Error())
	}
}

// TestDispatchIntoPaneAgentPromptStalledFallsBackToPaneRun confirms herdr's
// "agent_prompt_stalled" code (distinct from the generic "timeout"
// TestDispatchIntoPaneLiveAgentPromptNeverPickedUpReturnsError covers) means
// the prompt landed on a pane whose agent had already returned to an idle
// prompt after finishing its prior turn — reachable, just not caught by
// AgentPrompt's own wait window — so dispatchIntoPane must recover via
// PaneRun plus an explicit PaneSendKeys "enter" instead of aborting.
func TestDispatchIntoPaneAgentPromptStalledFallsBackToPaneRun(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	var paneRunText string
	var sawEnterAfterPaneRun bool
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"done"}}}`), nil
		case args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled)
		case args[0] == "agent" && args[1] == "wait":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"working"}}}`), nil
		case args[0] == "pane" && args[1] == "run":
			paneRunText = args[3]
			return []byte(`{"result":{}}`), nil
		case args[0] == "pane" && args[1] == "send-keys":
			if paneRunText != "" && args[2] == "w1:p1" && len(args) > 3 && args[3] == "enter" {
				sawEnterAfterPaneRun = true
			}
			return []byte(`{"result":{}}`), nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	})

	if err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", &dispatchTarget{worktree: t.TempDir(), launcher: "claude"}); err != nil {
		t.Fatalf("dispatchIntoPane: want the pane-run fallback to succeed, got %v", err)
	}
	if paneRunText != supervisor.InitialPrompt {
		t.Errorf("pane run text = %q, want %q", paneRunText, supervisor.InitialPrompt)
	}
	if !sawEnterAfterPaneRun {
		t.Error("want a `pane send-keys w1:p1 enter` call submitting the pane-run text, saw none")
	}
}

// TestDispatchIntoPaneAgentPromptStalledFallbackAlsoFails confirms that when
// even the PaneRun fallback never gets the agent working, dispatchIntoPane
// still reports an error (naming both the original stall and the fallback
// failure) instead of returning nil and leaving the caller to wait forever.
func TestDispatchIntoPaneAgentPromptStalledFallbackAlsoFails(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"done"}}}`), nil
		case args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled)
		case args[0] == "agent" && args[1] == "wait":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrWaitTimeout)
		case args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		case args[0] == "pane" && args[1] == "send-keys":
			return []byte(`{"result":{}}`), nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	})

	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", &dispatchTarget{worktree: t.TempDir(), launcher: "claude"})
	if err == nil {
		t.Fatal("want an error when the pane-run fallback never gets the agent working, got nil")
	}
	if !strings.Contains(err.Error(), "w1:p1") {
		t.Errorf("error should name the pane, got %q", err.Error())
	}
}

// TestDispatchIntoPaneAgentPromptStalledSendKeysFails confirms a genuine
// PaneSendKeys error (not a fallback that merely never gets the agent
// working) is surfaced directly instead of proceeding to AgentWait as if the
// submit keystroke had gone through.
func TestDispatchIntoPaneAgentPromptStalledSendKeysFails(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"done"}}}`), nil
		case args[0] == "agent" && args[1] == "prompt":
			return nil, fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled)
		case args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		case args[0] == "pane" && args[1] == "send-keys":
			return nil, errors.New("herdr: socket unavailable")
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	})

	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", &dispatchTarget{worktree: t.TempDir(), launcher: "claude"})
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the PaneSendKeys error propagated, got %v", err)
	}
}

// TestDispatchIntoPaneLiveAgentPromptUsesLivenessTimeout confirms the
// live-agent-reuse branch's AgentPrompt call is bounded by the same
// livenessTimeout knob the spawn branch's waitForAgentLive poll uses (rather
// than herdr's own indefinite wait), and that a caller-supplied override
// reaches herdr's `--timeout` flag verbatim instead of always falling back
// to defaultLivenessTimeout.
func TestDispatchIntoPaneLiveAgentPromptUsesLivenessTimeout(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	var promptArgs []string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "agent" && args[1] == "get":
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"idle"}}}`), nil
		case args[0] == "agent" && args[1] == "prompt":
			promptArgs = append([]string(nil), args...)
			return []byte(`{"result":{}}`), nil
		default:
			t.Fatalf("unexpected call: %v", args)
			return nil, nil
		}
	})

	target := &dispatchTarget{worktree: t.TempDir(), launcher: "claude", livenessTimeout: 5 * time.Second}
	if err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", target); err != nil {
		t.Fatalf("dispatchIntoPane: %v", err)
	}

	want := []string{"agent", "prompt", "w1:p1", supervisor.InitialPrompt, "--wait", "--until", "working", "--timeout", "5000"}
	if strings.Join(promptArgs, " ") != strings.Join(want, " ") {
		t.Errorf("agent prompt args = %v, want %v", promptArgs, want)
	}
}

// capturingSpawnClient is fakeRebaseClient's spawn path (no live agent until a
// "pane run" call succeeds, so waitForAgentLive's first poll finds it live
// immediately) plus recording of the exact command line PaneRun was asked to
// type into the pane, so a test can assert on the resolved --worktree's
// absolute cd target.
func capturingSpawnClient(paneID string) (client herdr.Client, spawnLine func() string) {
	var mu sync.Mutex
	var spawned bool
	var line string
	client = herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "worktree":
			return fmt.Appendf(nil, `{"result":{"root_pane":{"pane_id":%q}}}`, paneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			live := spawned
			mu.Unlock()
			if !live {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"done"}}}`, paneID), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			mu.Lock()
			spawned = true
			if len(args) > 3 {
				line = args[3]
			}
			mu.Unlock()
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
	return client, func() string {
		mu.Lock()
		defer mu.Unlock()
		return line
	}
}

// TestRebaseSpawnLineUsesAbsoluteWorktree is a direct regression test:
// opts.worktree given relative to argus's own cwd (not the target pane's)
// must be resolved to absolute before it reaches the spawn
// line's `cd`, in every common relative form an operator or script might
// pass. A relative cd that a reused pane's own cwd happens to already satisfy
// silently no-ops the && chain, so the launcher never starts and dispatch
// hangs forever waiting on a status.json that will never come.
func TestRebaseSpawnLineUsesAbsoluteWorktree(t *testing.T) {
	cases := []struct {
		setup func(t *testing.T, base string) (repoDir, cwd, rel string)
		name  string
	}{
		{
			name: "nested (.claude/worktrees/x)",
			setup: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
				return filepath.Join(base, ".claude", "worktrees", "featx"), base, filepath.Join(".claude", "worktrees", "featx")
			},
		},
		{
			name: "dot-slash (./x)",
			setup: func(t *testing.T, base string) (string, string, string) {
				t.Helper()
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
			initGitDirAt(t, repoDir)
			t.Chdir(cwd)

			cmd := newRebaseCmd()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			// 500ms, not 200ms: runRebase's no-conflict path now also
			// round-trips ls-remote/rev-parse against origin before dispatch.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			cmd.SetContext(ctx)

			client, spawnLine := capturingSpawnClient("w1:p1")
			opts := &rebaseOpts{
				worktree: rel, base: "feat-x", force: true, launcher: "claude", noCredProxy: true,
				interval: 10 * time.Millisecond, livenessTimeout: 100 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
			}
			_ = runRebase(cmd, client, opts) // no status.json ever written; only the spawn line matters here

			if !filepath.IsAbs(opts.worktree) {
				t.Errorf("opts.worktree = %q after runRebase, want an absolute path", opts.worktree)
			}
			wantAbs, err := filepath.Abs(repoDir)
			if err != nil {
				t.Fatalf("filepath.Abs(%q): %v", repoDir, err)
			}
			wantCd := "cd '" + wantAbs + "' &&"
			if line := spawnLine(); !strings.Contains(line, wantCd) {
				t.Errorf("spawn line = %q, want it to contain %q", line, wantCd)
			}
		})
	}
}

// TestRebaseSpawnLineAbsoluteWorktreePassesThroughUnchanged confirms an
// already-absolute --worktree is left alone: filepath.Abs is idempotent on a
// clean absolute path, so runRebase's resolution should neither fail nor
// rewrite it to something else.
func TestRebaseSpawnLineAbsoluteWorktreePassesThroughUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := filepath.Join(t.TempDir(), "featx")
	initGitDirAt(t, repoDir)

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// 500ms, not 200ms: see TestRebaseSpawnLineUsesAbsoluteWorktree.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)

	client, spawnLine := capturingSpawnClient("w1:p1")
	opts := &rebaseOpts{
		worktree: repoDir, base: "feat-x", force: true, launcher: "claude", noCredProxy: true,
		interval: 10 * time.Millisecond, livenessTimeout: 100 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
	}
	_ = runRebase(cmd, client, opts)

	if opts.worktree != repoDir {
		t.Errorf("opts.worktree = %q, want the already-absolute %q unchanged", opts.worktree, repoDir)
	}
	wantCd := "cd '" + repoDir + "' &&"
	if line := spawnLine(); !strings.Contains(line, wantCd) {
		t.Errorf("spawn line = %q, want it to contain %q", line, wantCd)
	}
}

// TestRebaseSpawnLineWorktreeWithShellMetacharsSingleQuoted covers a relative
// --worktree whose final path segment contains spaces and shell
// metacharacters (a branch-derived directory name, e.g. from `feat
// $(whoami)`): the resolved absolute path must still land single-quoted in
// the spawn line, so the metacharacters are inert data to the pane's shell
// rather than something it evaluates.
func TestRebaseSpawnLineWorktreeWithShellMetacharsSingleQuoted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	const segment = "feat $(whoami)"
	repoDir := filepath.Join(base, segment)
	initGitDirAt(t, repoDir)
	t.Chdir(base)

	cmd := newRebaseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// 500ms, not 200ms: see TestRebaseSpawnLineUsesAbsoluteWorktree.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)

	client, spawnLine := capturingSpawnClient("w1:p1")
	opts := &rebaseOpts{
		worktree: "./" + segment, base: "feat-x", force: true, launcher: "claude", noCredProxy: true,
		interval: 10 * time.Millisecond, livenessTimeout: 100 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
	}
	_ = runRebase(cmd, client, opts)

	if !filepath.IsAbs(opts.worktree) {
		t.Errorf("opts.worktree = %q, want an absolute path", opts.worktree)
	}
	wantAbs, err := filepath.Abs(repoDir)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", repoDir, err)
	}
	wantCd := "cd '" + wantAbs + "' &&"
	line := spawnLine()
	if !strings.Contains(line, wantCd) {
		t.Errorf("spawn line = %q, want it to contain the single-quoted absolute path %q", line, wantCd)
	}
	if strings.Contains(line, "cd "+wantAbs+" &&") { // unquoted form would let the shell evaluate $(whoami)
		t.Errorf("spawn line = %q, worktree with shell metacharacters must be single-quoted", line)
	}
}

// TestDispatchIntoPaneSpawnNeverComesLive is the direct regression test for
// the spawn-side counterpart of TestRebaseSpawnLineUsesAbsoluteWorktree: a
// spawn-new-agent PaneRun that "succeeds" (herdr accepted the keystrokes) but
// whose `cd && <launcher>`
// chain silently failed inside the pane (e.g. because --worktree wasn't
// absolute and the pane was already rooted there) must not hang forever
// waiting on a status.json that will never be written. It must instead return
// a bounded, pane-naming error once the liveness poll's deadline passes.
func TestDispatchIntoPaneSpawnNeverComesLive(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})

	opts := &dispatchTarget{
		worktree: t.TempDir(), launcher: "claude", noCredProxy: true,
		livenessTimeout: 40 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
	}
	start := time.Now()
	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", opts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a bounded liveness error, got nil")
	}
	if !strings.Contains(err.Error(), "w1:p1") {
		t.Errorf("want the error to name the pane, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("want dispatchIntoPane to return once the %s liveness deadline passes, not hang; took %s", opts.livenessTimeout, elapsed)
	}
}

// TestDispatchIntoPaneSpawnLivenessRecovers confirms a spawned agent that
// takes a few polls to report live is not mistaken for a hang: dispatch must
// proceed normally (no error) once herdr eventually reports it live, well
// within the deadline.
func TestDispatchIntoPaneSpawnLivenessRecovers(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	var mu sync.Mutex
	var polls int
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n < 3 {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"live"}}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})

	opts := &dispatchTarget{
		worktree: t.TempDir(), launcher: "claude", noCredProxy: true,
		livenessTimeout: 500 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
	}
	if err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", opts); err != nil {
		t.Fatalf("dispatchIntoPane: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if polls < 3 {
		t.Errorf("want at least 3 agent-get polls before liveness was reported, got %d", polls)
	}
}

// TestDispatchIntoPaneSpawnLivenessAgentGetError confirms a genuine AgentGet
// failure during the post-spawn liveness poll (a transport/decode error, not
// herdr's expected "no live agent" outcome) surfaces immediately rather than
// being retried away until the deadline.
func TestDispatchIntoPaneSpawnLivenessAgentGetError(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	var mu sync.Mutex
	var polls int
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			mu.Lock()
			polls++
			n := polls
			mu.Unlock()
			if n == 1 {
				// The initial "does this pane already have a live agent?" check.
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return nil, errors.New("herdr: socket unavailable")
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})

	opts := &dispatchTarget{
		worktree: t.TempDir(), launcher: "claude", noCredProxy: true,
		livenessTimeout: 500 * time.Millisecond, livenessInterval: 5 * time.Millisecond,
	}
	start := time.Now()
	err := dispatchIntoPane(context.Background(), logger, client, "w1:p1", "feat-x", opts)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("want the AgentGet error surfaced, got %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("want the error surfaced immediately, not retried until the %s deadline; took %s", opts.livenessTimeout, elapsed)
	}
}

// TestDispatchIntoPaneSpawnLivenessContextCanceled confirms the liveness poll
// returns promptly via ctx.Err() when the parent context is canceled mid-poll,
// instead of running out its own timeout.
func TestDispatchIntoPaneSpawnLivenessContextCanceled(t *testing.T) {
	logger := eventlog.New(nil, "rebase", "test-run", nil)
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			return []byte(`{"result":{}}`), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	opts := &dispatchTarget{
		worktree: t.TempDir(), launcher: "claude", noCredProxy: true,
		livenessTimeout: 5 * time.Second, livenessInterval: 5 * time.Millisecond,
	}
	start := time.Now()
	err := dispatchIntoPane(ctx, logger, client, "w1:p1", "feat-x", opts)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("want prompt return on ctx cancellation, not waiting out the %s timeout; took %s", opts.livenessTimeout, elapsed)
	}
}
