package supervisor

import (
	"context"
	"fmt"
	"os/exec"
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
