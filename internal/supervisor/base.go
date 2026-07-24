package supervisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

// DetectDefaultBase reads a repo's origin/HEAD symbolic ref to find its
// default branch (e.g. "main", "develop") for a repo that never configured
// or was never passed one explicitly. It returns an error if origin/HEAD is
// unset — e.g. a clone that never ran `git remote set-head origin -a` — so
// callers fall back to their own next choice rather than trusting an empty
// branch name.
func DetectDefaultBase(ctx context.Context, repoRoot string) (string, error) {
	out, err := git(ctx, repoRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("detecting origin/HEAD: %w", err)
	}
	branch := strings.TrimPrefix(out, "origin/")
	if branch == "" {
		return "", fmt.Errorf("origin/HEAD resolved to an empty branch name")
	}
	return branch, nil
}

// ResolveBase determines the bare base branch name a worktree-scoped command
// (ship, rebase) should use, in order: an operator-supplied --base flag
// (flagExplicit), the base persisted by supervise in the worktree's own
// status.json (see protocol.Status.Base — set at worktree-creation time,
// closing issue #160), this repo's .argus/config.yml base_branch (see
// internal/repoconfig, for a worktree supervise never touched), the repo's
// detected origin/HEAD, and finally the literal "main". Every source but the
// flag is best-effort: a lookup failure just falls through to the next one.
func ResolveBase(ctx context.Context, worktree, flagValue string, flagExplicit bool) string {
	if flagExplicit {
		return flagValue
	}
	if st, err := protocol.Load(protocol.StatusPath(worktree)); err == nil && st.Base != "" {
		return st.Base
	}
	repoRoot, err := RepoRoot(ctx, worktree)
	if err != nil {
		return "main"
	}
	if rc, err := repoconfig.Load(repoconfig.Path(repoRoot)); err == nil && rc.BaseBranch != "" {
		return rc.BaseBranch
	}
	if detected, err := DetectDefaultBase(ctx, repoRoot); err == nil && detected != "" {
		return detected
	}
	return "main"
}
