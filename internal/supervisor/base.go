package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
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
// status.json (see protocol.Status.Base — set at worktree-creation time),
// this repo's .argus/config.yml base_branch (see
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

// BaseSource names where a resolved gate/review base ref came from, so a
// fail-fast "base ref does not exist" error (see VerifyBaseRef) can point at
// the actual thing to fix instead of a bare "no such ref".
type BaseSource string

const (
	BaseSourceFlag     BaseSource = "flag"
	BaseSourceConfig   BaseSource = "base_branch config"
	BaseSourceDetected BaseSource = "detected origin/HEAD"
	BaseSourceDefault  BaseSource = "flag default"
)

// ResolvedBase is a gate/review base ref together with where it came from.
type ResolvedBase struct {
	Ref    string
	Source BaseSource
}

// ResolveGateBase applies the one precedence supervise and rework both need
// for their gate/review diff ref: explicit --base flag > this repo's
// .argus/config.yml base_branch (origin-prefixed) > detected origin/HEAD >
// the flag's own default (normally "origin/main"). Centralizing this here —
// rather than each command re-deriving it — is the fix for two
// independently-correct-looking call sites drifting apart (style.md's "one
// derivation site per ambient fact"): before this, rework set its
// Config.Base straight from the flag and never consulted BaseBranch at all,
// so it silently diffed against origin/main on a repo whose trunk was
// anything else.
//
// This is deliberately not the same convention as ResolveBase (ship/rebase's
// own --base): that resolves a bare branch name, with its own separate
// fallback chain including a worktree's persisted status.json Base.
// Unifying the two conventions is a separate, larger change, left out of
// scope here. explicit is the caller's cmd.Flags().Changed("base"). rc is a
// pointer solely to avoid copying the struct at the call site.
func ResolveGateBase(ctx context.Context, explicit bool, flagValue, repoRoot string, rc *repoconfig.Config) ResolvedBase {
	if explicit {
		return ResolvedBase{Ref: flagValue, Source: BaseSourceFlag}
	}
	if rc.BaseBranch != "" {
		return ResolvedBase{Ref: "origin/" + rc.BaseBranch, Source: BaseSourceConfig}
	}
	if repoRoot != "" {
		if detected, err := DetectDefaultBase(ctx, repoRoot); err == nil && detected != "" {
			return ResolvedBase{Ref: "origin/" + detected, Source: BaseSourceDetected}
		}
	}
	return ResolvedBase{Ref: flagValue, Source: BaseSourceDefault}
}

// VerifyBaseRef fails fast when rb.Ref does not actually resolve inside
// gitDir's git history. A base ref that doesn't exist is a
// configuration/infra problem — a typo'd --base, a base_branch naming a
// branch this remote doesn't have, an origin/HEAD that was never set — never
// a review outcome, so it must be caught here, before any worker is spawned
// or judged, instead of surfacing deep inside a per-worker measure_diff
// failure that reads as an ordinary review escalation (see
// measureReconcileDiffs, which today just logs MeasureDiff's error and moves
// on). An empty rb.Ref is a no-op: a Config with no Base configured at all
// has nothing for this check to validate. gitDir accepts anything git -C
// does — a repo root or one of its linked worktrees both work, since a
// linked worktree shares its parent's refs.
func VerifyBaseRef(ctx context.Context, gitDir string, rb ResolvedBase) error {
	if rb.Ref == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-parse", "--verify", "--quiet", rb.Ref+"^{commit}") //nolint:gosec // fixed git binary; gitDir/rb.Ref are argus-derived
	if err := cmd.Run(); err != nil {
		return &ui.UserError{
			Err:  fmt.Errorf("base ref %q does not exist in this repo (resolved from: %s)", rb.Ref, rb.Source),
			Hint: "check base_branch in .argus/config.yml, or pass --base origin/<branch>",
		}
	}
	return nil
}

// ResolveEffectiveDiffBase resolves the ref MeasureDiff and DiffFor actually
// diff a worktree's HEAD against: merge-base(base, HEAD) — the three-dot
// equivalent a merge would actually apply — rather than base's own moving
// tip. A worker never commits, and never rebases onto argus's own base while
// it works, so once base advances past the worktree's spawn point (another
// PR merges to origin/main while this worker is still running), a plain
// two-dot `git diff base` includes a revert of every intervening merge: it
// inflates the measured size and invents deletions the branch never made,
// which used to fabricate an unwaivable "under-reported diff" hard reason
// and show the reviewer phantom reverts of already-merged work.
//
// This is the one place that derivation happens — both MeasureDiff and
// DiffFor call it, so the size the gate checks and the diff the reviewer
// reads can never diverge. It does not care whether base itself carries an
// "origin/" prefix or not (callers pass either shape today): `git
// merge-base` resolves whatever ref string it's given the same way `git
// diff`/`git rev-parse` already do.
func ResolveEffectiveDiffBase(ctx context.Context, worktree, base string) (string, error) {
	ref, err := git(ctx, worktree, "merge-base", base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving merge-base of %s and HEAD: %w", base, err)
	}
	return ref, nil
}

// CommitsBehindBase reports how many commits base has moved ahead of the
// worktree's HEAD since they diverged — a distinct, informational signal
// from what MeasureDiff/DiffFor measure. Without it, a base that advanced
// while a worker was running had no way to surface as "this checkout is
// stale" instead of masquerading as an inflated or under-reported diff (see
// ResolveEffectiveDiffBase). Best-effort: callers that only want this for
// display should treat an error as "unknown" rather than fail on it.
func CommitsBehindBase(ctx context.Context, worktree, base string) (int, error) {
	out, err := git(ctx, worktree, "rev-list", "--count", "HEAD.."+base)
	if err != nil {
		return 0, fmt.Errorf("counting commits base %s has moved ahead of HEAD: %w", base, err)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parsing commits-behind count %q: %w", out, err)
	}
	return n, nil
}
