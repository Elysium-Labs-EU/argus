package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// WorktreeEntry is one linked worktree as reported by `git worktree list
// --porcelain`. Prunable mirrors git's own detection: the working directory
// has already gone missing (e.g. someone ran `trash <path>` directly instead
// of the matching git command, exactly the rough edge issue #101 describes),
// leaving a stale registration behind.
type WorktreeEntry struct {
	Path     string
	Branch   string
	Prunable bool
}

// ListLinkedWorktrees lists every worktree linked to repoRoot's repository
// except the main one repoRoot itself names — `git worktree list` always
// reports the main worktree first, and it is never a prune candidate.
func ListLinkedWorktrees(ctx context.Context, repoRoot string) ([]WorktreeEntry, error) {
	out, err := git(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	entries := parseWorktreePorcelain(out)
	if len(entries) > 0 {
		entries = entries[1:]
	}
	return entries, nil
}

// parseWorktreePorcelain decodes `git worktree list --porcelain`'s
// blank-line-separated record format into WorktreeEntry values. A bare or
// detached-HEAD worktree carries no "branch" line; callers treat an empty
// Branch as unmanageable (it cannot be matched to a forge PR) rather than
// erroring the whole listing over it.
func parseWorktreePorcelain(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var cur *WorktreeEntry
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			if cur != nil {
				entries = append(entries, *cur)
			}
			cur = &WorktreeEntry{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch "):
			if cur != nil {
				cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			}
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	return entries
}

// PruneCandidate is one worktree's prune evaluation: the deterministic,
// testable verdict `argus worktree prune` renders per worktree, same shape as
// review's approve/request-changes/needs-human — safe_to_clean plus the
// specific reasons, so a manager relays a real decision instead of
// re-deriving it from raw git/PR-state calls by hand.
type PruneCandidate struct {
	Path   string
	Branch string
	PRURL  string
	// PaneID is the herdr pane recorded for this worktree in the repo's pane
	// registry, if any (see protocol.PaneRegistry, written by prepareWorktree)
	// — CleanWorktree closes it, and its workspace if left empty, once the
	// candidate is confirmed safe to clean. Resolved from the registry, not
	// the worktree's own lifecycle.json, so it is still known even when the
	// worktree directory (and everything inside it) is already gone.
	PaneID      string
	Reasons     []string
	Merged      bool
	DirGone     bool
	SafeToClean bool
}

// EvaluateCandidate is the deterministic (no LLM) safety check behind prune:
// is the branch's PR merged, and — when the working directory still exists —
// is it free of uncommitted changes, unpushed commits, and stash entries. See
// resolveMergeState for how the merge check is sourced. repoRoot is the main
// repository (not worktree, which may itself be a linked worktree already
// deleted) — it is where the pane registry lives, so PaneID resolves
// correctly even when the worktree directory is gone. dryRun must be true for
// a --dry-run invocation: it still reads lifecycle.json to decide
// safe-to-clean, but never writes a state transition to disk — "confirm
// first, no changes" means no changes, not even to argus's own bookkeeping.
func EvaluateCandidate(ctx context.Context, f forge.Forge, owner, repo, repoRoot, worktree, branch string, dirGone, dryRun bool) (*PruneCandidate, error) {
	c := &PruneCandidate{Path: worktree, Branch: branch, DirGone: dirGone}

	reg, regErr := protocol.LoadPaneRegistry(repoRoot)
	if regErr != nil {
		return nil, regErr
	}
	c.PaneID = reg.Panes[worktree]

	merged, prFound, prURL, prState, err := resolveMergeState(ctx, f, owner, repo, worktree, branch, dirGone, dryRun)
	if err != nil {
		return nil, err
	}
	c.PRURL = prURL
	c.Merged = merged
	switch {
	case !prFound:
		c.Reasons = append(c.Reasons, "no PR found for this branch")
	case !merged:
		c.Reasons = append(c.Reasons, fmt.Sprintf("PR %s not merged (state=%s)", prURL, prState))
	}

	if !dirGone {
		dirty, derr := hasUncommittedChanges(ctx, worktree)
		if derr != nil {
			return nil, derr
		}
		if dirty {
			c.Reasons = append(c.Reasons, "uncommitted changes")
		}
		stashed, serr := hasStash(ctx, worktree)
		if serr != nil {
			return nil, serr
		}
		if stashed {
			c.Reasons = append(c.Reasons, "stash entries present")
		}
		if hasUnpushedCommits(ctx, worktree) {
			c.Reasons = append(c.Reasons, "unpushed commits")
		}
	}

	c.SafeToClean = len(c.Reasons) == 0
	return c, nil
}

// resolveMergeState is prune's Lifecycle-first merge check. A worktree's own
// protocol.Lifecycle record (written by ship, advanced here) is the primary
// source: a cached LifecycleMerged state is trusted outright — merges don't
// unmerge, so re-asking the forge would be pure waste — and any other
// existing record is re-checked against the forge using the lifecycle's own
// owner/repo/branch, advancing it to LifecycleMerged the instant a merge is
// confirmed so neither this branch nor CleanWorktree's later prune-marking
// ever has to ask again. Only a worktree with no lifecycle file at all — one
// ship opened before this record existed, or with a binary that predates it —
// falls back to forge.Forge.FindPR by branch (see ship.go's WriteLifecycle
// comment for the same fallback framing). dryRun suppresses the
// shipped->merged write: --dry-run may still read and evaluate the lifecycle
// state to decide safe-to-clean, but must never persist a transition.
func resolveMergeState(ctx context.Context, f forge.Forge, owner, repo, worktree, branch string, dirGone, dryRun bool) (merged, prFound bool, prURL, prState string, err error) {
	lc, lcFound, lerr := protocol.LoadLifecycle(worktree)
	if lerr != nil {
		return false, false, "", "", lerr
	}
	if lcFound && lc.State == protocol.LifecycleMerged {
		return true, true, lc.PRURL, string(protocol.LifecycleMerged), nil
	}

	lookupOwner, lookupRepo, lookupBranch := owner, repo, branch
	if lcFound && lc.Owner != "" && lc.Repo != "" && lc.Branch != "" {
		lookupOwner, lookupRepo, lookupBranch = lc.Owner, lc.Repo, lc.Branch
	}

	pr, found, ferr := f.FindPR(ctx, lookupOwner, lookupRepo, lookupBranch)
	if ferr != nil {
		return false, false, "", "", fmt.Errorf("checking PR state for branch %s: %w", branch, ferr)
	}
	if !found {
		return false, false, "", "", nil
	}
	merged = pr.Merged()
	if merged && lcFound && !dirGone && !dryRun {
		next := lc
		next.State = protocol.LifecycleMerged
		next.PRURL = pr.HTMLURL
		if pr.Number != 0 {
			next.PRNumber = pr.Number
		}
		if werr := protocol.WriteLifecycle(worktree, &next); werr != nil {
			return false, false, "", "", werr
		}
	}
	return merged, true, pr.HTMLURL, pr.State, nil
}

// hasUncommittedChanges is the Go equivalent of the bash hook's dirty-tree
// check: any staged or unstaged change, or an untracked file — except argus's
// own control-plane files (status.json, lifecycle.json, the generated
// settings; see controlPlanePaths), which live untracked inside every worktree
// a worker ever ran in and would otherwise make this report every shipped
// worktree dirty forever, regardless of whether the actual change is
// committed (ship's CommitAll excludes the same paths from a real commit for
// the same reason). --untracked-files=all is required for that exclusion to
// work at all: git's default collapses a directory with nothing tracked in it
// to one "?? .claude/" line, which isControlPlanePath can't tell apart from a
// non-argus untracked directory living alongside it; the "=all" form lists
// every individual file instead, so the exclusion always sees real paths.
func hasUncommittedChanges(ctx context.Context, worktree string) (bool, error) {
	out, err := git(ctx, worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if len(line) <= 3 {
			continue
		}
		if isControlPlanePath(strings.TrimSpace(line[3:])) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// isControlPlanePath reports whether path (as reported by `git status
// --porcelain`) is one of argus's own control-plane files or falls under one
// of its directories.
func isControlPlanePath(path string) bool {
	for _, p := range controlPlanePaths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// hasStash reports whether worktree carries any stash entries.
func hasStash(ctx context.Context, worktree string) (bool, error) {
	out, err := git(ctx, worktree, "stash", "list")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// hasUnpushedCommits reports whether HEAD is ahead of its upstream. A branch
// with no upstream tracking ref at all is treated as unpushed (unsafe) rather
// than erroring — ship always sets one via `git push -u` (see Push), so a
// missing upstream on an otherwise-merged branch means something bypassed
// that path and deserves a human look, not a silent clean.
func hasUnpushedCommits(ctx context.Context, worktree string) bool {
	out, err := git(ctx, worktree, "rev-list", "--count", "@{u}..HEAD")
	if err != nil {
		return true // no upstream tracking ref is itself the "unsafe" signal
	}
	return strings.TrimSpace(out) != "0"
}

// CleanWorktree performs prune's actual cleanup for a candidate already
// confirmed SafeToClean: a recoverable relocation of the working directory
// (never a raw rm) when it still exists, then a targeted
// `git worktree remove --force` for just this one path — the per-worktree
// counterpart to `git worktree prune`, which issue #101 notes cannot be
// scoped to a single entry at all. It returns the path content was moved to
// (empty when the directory was already gone), so a caller can tell an
// operator where to look to undo it.
//
// When c.PaneID is set, it also closes that herdr pane — and the pane's
// workspace too, if it was left as the only one there — mirroring how
// prepareWorktree spawns a worktree and its pane together. This step is
// best-effort: the worktree itself is already fully cleaned by the time it
// runs, so a herdr-side failure (already closed by hand, herdr not reachable)
// is reported back as paneWarning rather than turned into err.
func CleanWorktree(ctx context.Context, repoRoot string, client herdr.Client, c *PruneCandidate) (trashPath, paneWarning string, err error) {
	if !c.DirGone {
		markLifecyclePruned(c.Path)
		trashPath, err = recoverableRemove(ctx, repoRoot, c.Path)
		if err != nil {
			return "", "", err
		}
	}
	if _, err := git(ctx, repoRoot, "worktree", "remove", "--force", c.Path); err != nil {
		return trashPath, "", fmt.Errorf("cleaning worktree registration for %s: %w", c.Path, err)
	}
	if c.PaneID != "" {
		if cerr := ClosePaneAndEmptyWorkspace(ctx, client, c.PaneID); cerr != nil {
			paneWarning = fmt.Sprintf("worktree cleaned, but closing herdr pane %s failed: %v", c.PaneID, cerr)
		}
		forgetPaneRecord(repoRoot, c.Path)
	}
	return trashPath, paneWarning, nil
}

// forgetPaneRecord removes worktree's entry from repoRoot's pane registry
// once CleanWorktree is done with it. Best-effort and silent, like
// markLifecyclePruned: a stale registry entry pointing at a worktree that
// git no longer even lists is dead weight, but a bookkeeping write must never
// surface as a prune failure once the worktree itself is already gone —
// regardless of whether the herdr close above succeeded, there is no future
// prune run left that could retry it (the worktree registration is gone from
// git too by this point).
func forgetPaneRecord(repoRoot, worktree string) {
	reg, err := protocol.LoadPaneRegistry(repoRoot)
	if err != nil {
		return
	}
	if _, ok := reg.Panes[worktree]; !ok {
		return
	}
	delete(reg.Panes, worktree)
	_ = protocol.WritePaneRegistry(repoRoot, reg)
}

// markLifecyclePruned advances a worktree's existing lifecycle record to its
// terminal LifecyclePruned state before recoverableRemove relocates the
// directory, so the relocated copy — the one place left to look once the
// worktree is gone — carries an accurate final record. It is best-effort and
// silent: a worktree ship never tracked has no lifecycle.json to advance, and
// either way a bookkeeping write must never block the clean itself.
func markLifecyclePruned(worktree string) {
	lc, found, err := protocol.LoadLifecycle(worktree)
	if err != nil || !found {
		return
	}
	lc.State = protocol.LifecyclePruned
	_ = protocol.WriteLifecycle(worktree, &lc)
}

// recoverableRemove relocates path into a holding directory under the
// repository's common git dir rather than deleting it, so a clean that turns
// out to be wrong is a `mv` away from undone, not gone. It lives inside the
// git common dir (not e.g. the OS trash) so it survives independent of any
// platform-specific trash tool being installed — CI and Linux runners have
// none — and so it is trivially found relative to the repo it came from.
func recoverableRemove(ctx context.Context, repoRoot, path string) (string, error) {
	commonDir, err := git(ctx, repoRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolving git common dir: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoRoot, commonDir)
	}
	holding := filepath.Join(commonDir, "argus-trash")
	if err := os.MkdirAll(holding, 0o755); err != nil { //nolint:gosec // repo-internal holding dir, standard perms
		return "", fmt.Errorf("creating trash holding dir: %w", err)
	}
	dest := filepath.Join(holding, fmt.Sprintf("%s-%d", filepath.Base(path), time.Now().UnixNano()))
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("moving %s to recoverable trash: %w", path, err)
	}
	return dest, nil
}
