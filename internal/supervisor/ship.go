package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// ErrNothingToCommit means the worktree had no changes to ship.
var ErrNothingToCommit = errors.New("nothing to commit")

func git(ctx context.Context, worktree string, args ...string) (string, error) {
	full := append([]string{"-C", worktree}, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // fixed git binary; worktree/args are argus-derived
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		sub := "command"
		if len(args) > 0 {
			sub = args[0]
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		if uerr := translateGitFailure(worktree, msg); uerr != nil {
			return "", uerr
		}
		return "", fmt.Errorf("git %s: %s", sub, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// translateGitFailure recognizes the two git-subprocess failures every
// --worktree/--base-shaped argus command can hit — git can't cd into the
// worktree path, or git can't resolve a ref (almost always a bad --base) —
// and turns them into a message naming the actual bad input instead of
// git's raw "fatal: ..." text, which otherwise reaches the terminal
// verbatim (main.go renders a *ui.UserError specially; anything else just
// prints raw). Every argus git call goes through git(), so translating once
// here reaches ship, rebase, tend, and worktree prune alike; DiffFor
// (reviewer.go) calls git() too, for the same reason. Returns nil for
// anything unrecognized, so the caller falls back to its original, less
// friendly but still accurate "git <sub>: <msg>" wrap.
func translateGitFailure(worktree, msg string) error {
	if i := strings.Index(msg, "cannot change to"); i >= 0 {
		reason := msg[i:]
		if j := strings.LastIndex(reason, ": "); j >= 0 {
			reason = reason[j+2:]
		}
		return &ui.UserError{
			Err:  fmt.Errorf("cannot use worktree %s: %s", worktree, reason),
			Hint: "check --worktree (or --repo for `argus worktree prune`)",
		}
	}
	if _, ref, ok := strings.Cut(msg, "couldn't find remote ref "); ok {
		return &ui.UserError{Err: fmt.Errorf("no such ref on origin: %s", strings.TrimSpace(ref)), Hint: "check --base"}
	}
	if ref, ok := quotedAfter(msg, "ambiguous argument '"); ok {
		return &ui.UserError{Err: fmt.Errorf("no such ref: %s", ref), Hint: "check --base"}
	}
	if ref, ok := quotedAfter(msg, "bad revision '"); ok {
		return &ui.UserError{Err: fmt.Errorf("no such ref: %s", ref), Hint: "check --base"}
	}
	return nil
}

// quotedAfter extracts the text between prefix and the single quote that
// follows it, e.g. quotedAfter("fatal: ambiguous argument 'foo': unknown
// revision...", "ambiguous argument '") == ("foo", true).
func quotedAfter(msg, prefix string) (string, bool) {
	_, rest, ok := strings.Cut(msg, prefix)
	if !ok {
		return "", false
	}
	ref, _, ok := strings.Cut(rest, "'")
	return ref, ok
}

// controlPlanePaths are the argus-written files that must never land in a PR: the
// worker's brief/status/verdict and the generated permission file. MeasureDiff
// excludes the same paths (via isControlPlanePath, prune.go) so the gate never
// escalates on lines that ship was always going to drop.
//
// .argus-report-body.json is the one entry here argus itself never writes —
// it's the worktree-root scratch file a worker materializes when it passes
// `argus worker report --file` a body it wrote out first instead of piping it
// over stdin. It still needs the same treatment as the rest of this list:
// left alone, it inflates the measured diff enough to trip the unwaivable
// under-report check, and an unfiltered `git add -A` would sweep it into the
// PR.
var controlPlanePaths = []string{".claude/argus", ".claude/settings.local.json", ".argus-report-body.json"}

// CommitAll stages every change in the worktree and commits it. It returns
// ErrNothingToCommit when nothing worth shipping remains, so shipping a worktree
// the worker already committed (or didn't touch) fails loudly rather than opening
// an empty PR. argus's own control-plane files are unstaged before the commit so
// they never reach the PR, even in a repo that doesn't gitignore .claude. (We
// stage-then-unstage rather than exclude via pathspec because naming an ignored
// path in a git pathspec is itself a fatal error.)
func CommitAll(ctx context.Context, worktree, message string) error {
	if _, err := git(ctx, worktree, "add", "-A"); err != nil {
		return err
	}
	// Unstage the control plane. reset of a path that isn't staged is a harmless
	// no-op, so this is safe whether or not the repo ignores .claude.
	resetArgs := append([]string{"reset", "-q", "--"}, controlPlanePaths...)
	if _, err := git(ctx, worktree, resetArgs...); err != nil {
		return err
	}
	staged, err := git(ctx, worktree, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if staged == "" {
		return ErrNothingToCommit
	}
	if _, err := git(ctx, worktree, "commit", "-m", message); err != nil {
		return err
	}
	return nil
}

// Push publishes the worktree's branch to origin, setting upstream.
func Push(ctx context.Context, worktree, branch string) error {
	_, err := git(ctx, worktree, "push", "-u", "origin", branch)
	return err
}

// CurrentBranch returns the worktree's checked-out branch.
func CurrentBranch(ctx context.Context, worktree string) (string, error) {
	return git(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
}

// RepoRoot returns the main repository's working directory for worktree —
// worktree may itself be a linked worktree (e.g. .claude/worktrees/<branch>),
// in which case this is its parent repo, not worktree itself. herdr's
// `worktree open` (used to reopen a pane for an already-created worktree,
// e.g. by `argus rebase`) requires --cwd to name the repo the linked
// worktree belongs to, not the linked worktree's own path — passing the
// worktree path itself is rejected (herdr error "linked_worktree_source").
//
// `git rev-parse --git-common-dir` returns an absolute path for a linked
// worktree, but a bare ".git" (relative to worktree, not to argus's own cwd)
// for a plain, non-linked repo — every caller before `argus worktree prune`
// only ever passed an already-linked worker worktree, so that relative case
// went unexercised until prune's --repo pointed straight at a main repo and
// filepath.Dir(".git") silently resolved to argus's own process cwd instead.
func RepoRoot(ctx context.Context, worktree string) (string, error) {
	commonDir, err := git(ctx, worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktree, commonDir)
	}
	return filepath.Dir(commonDir), nil
}

// ErrNotLinkedWorktree means the given path is not a linked git worktree —
// either not a git repository at all, or the main repository checkout itself
// rather than one `git worktree add` created.
var ErrNotLinkedWorktree = errors.New("not inside a linked git worktree")

// VerifyLinkedWorktree confirms path is a real, linked git worktree rather
// than an arbitrary directory or the main repository checkout. `argus worker
// report` (see cmd/worker_report.go) calls this before writing anything: a
// worker whose pane cd'd to the wrong place — a mistaken cd, a stale pane —
// would otherwise happily create status.json wherever it landed instead of
// failing with a clear error.
//
// A linked worktree's --git-dir (e.g. <repo>/.git/worktrees/<name>) differs
// from its --git-common-dir (<repo>/.git); the main checkout's are the same
// path. --is-inside-work-tree is checked first so a plain non-git directory
// fails on that git error rather than an ambiguous path comparison.
func VerifyLinkedWorktree(ctx context.Context, path string) error {
	inside, err := git(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return fmt.Errorf("%s: %w", path, ErrNotLinkedWorktree)
	}
	gitDir, err := git(ctx, path, "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("resolving git dir for %s: %w", path, err)
	}
	commonDir, err := git(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolving git common dir for %s: %w", path, err)
	}
	if resolveGitPath(path, gitDir) == resolveGitPath(path, commonDir) {
		return fmt.Errorf("%s: %w", path, ErrNotLinkedWorktree)
	}
	return nil
}

// resolveGitPath makes a possibly-relative `git rev-parse` output absolute
// against worktree, the same relative-to-worktree convention RepoRoot relies
// on for --git-common-dir.
func resolveGitPath(worktree, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(worktree, p))
}

// RemoteURL returns the raw origin remote URL of a worktree, for callers that
// need the host (not just owner/repo) such as forge detection.
func RemoteURL(ctx context.Context, worktree string) (string, error) {
	return git(ctx, worktree, "remote", "get-url", "origin")
}

// RemoteOwnerRepo parses the origin remote of a worktree into owner and repo,
// handling both SSH (git@host:Owner/Repo.git) and HTTPS (https://host/Owner/Repo)
// forms.
func RemoteOwnerRepo(ctx context.Context, worktree string) (owner, repo string, err error) {
	url, err := git(ctx, worktree, "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	return parseOwnerRepo(url)
}

func parseOwnerRepo(url string) (owner, repo string, err error) {
	// Trim a trailing slash before ".git" so "Repo.git/" strips to "Repo",
	// not "Repo.git".
	s := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(url), "/"), ".git")
	// Normalize SSH scp-form (git@host:Owner/Repo) and URL forms to a tail path.
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		// no slash after the colon means this colon isn't a scheme separator
		s = s[i+1:]
	} else if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '/'); j >= 0 {
			s = s[j+1:]
		}
	} else if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote %q", url)
	}
	// GitLab/Gitea nest repos under arbitrarily deep subgroups; only the final
	// segment is ever the repo, so everything before it is the owner.
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
}
