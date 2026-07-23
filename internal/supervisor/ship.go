package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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
		return "", fmt.Errorf("git %s: %s", sub, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// controlPlanePaths are the argus-written files that must never land in a PR: the
// worker's brief/status/verdict and the generated permission file.
var controlPlanePaths = []string{".claude/argus", ".claude/settings.local.json"}

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
	s := strings.TrimSuffix(strings.TrimSpace(url), ".git")
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
	return parts[len(parts)-2], parts[len(parts)-1], nil
}
