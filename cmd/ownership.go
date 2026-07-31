package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// ownerFlagHelp, forceForeignOwnerFlagHelp, and ownerStaleAfterFlagHelp are
// shared verbatim by every mutating command that touches an existing
// worktree (rework, rebase, ship, worker answer — see internal/ownership),
// the same way credentialEnvFlagHelp keeps --credential-env's wording in
// sync across every command that registers it.
const (
	ownerFlagHelp             = "override this invocation's caller identity for the ownership-lease check (default: $ARGUS_OWNER_ID, then $HERDR_WORKSPACE_ID, then a generated id — the same resolution argus supervise itself uses at spawn time)"
	forceForeignOwnerFlagHelp = "act on this worktree even though its recorded owner lease (.claude/argus/owner.json) belongs to a different, still-active session"
	ownerStaleAfterFlagHelp   = "treat a worktree's owner lease as abandoned once its heartbeat is older than this; a mismatched caller then proceeds (with a logged notice) instead of being refused. Without this flag, this repo's .argus/config.yml owner_stale_after wins, then this default"
)

// ownerFlags bundles the three ownership-lease flags every mutating command
// (rework, rebase, ship, worker answer) registers — --owner,
// --force-foreign-owner, --owner-stale-after — mirroring gateFlags's own
// bundling of the review-gate flags rework and supervise share.
// ownerStaleAfterExplicit is cmd.Flags().Changed("owner-stale-after"), the
// same explicit/flagValue split gateFlags's own maxDiffLinesExplicit uses, so
// enforceOwnership knows whether ownerStaleAfter is a real operator override
// or just the flag's own zero-value default that a repo's config.yml is free
// to win over.
type ownerFlags struct {
	owner                   string
	ownerStaleAfter         time.Duration
	ownerStaleAfterExplicit bool
	forceForeignOwner       bool
}

// enforceOwnership resolves the caller's owner_id the same way every mutating
// command does — flag > $ARGUS_OWNER_ID > $HERDR_WORKSPACE_ID > a generated
// id, see ownership.ResolveOwnerID — and checks it against worktree's
// recorded lease (see ownership.Enforce). A mismatched, still-fresh lease
// refuses with a *ui.UserError naming the actual owner unless f.forceForeignOwner
// is set; a mismatched but stale lease, or no lease at all (a worktree
// predating this feature), both proceed — the former logging a notice to out
// so an abandoned lease still leaves a visible trace even though it doesn't
// block. Before any of that, f.ownerStaleAfter is resolved against this
// repo's own .argus/config.yml owner_stale_after key (see
// resolveOwnerStaleAfterForWorktree) — the one ownership-lease flag that is a
// repo-wide policy knob, not a per-invocation identity/override the way
// --owner/--force-foreign-owner are.
func enforceOwnership(ctx context.Context, out io.Writer, worktree string, f ownerFlags, now time.Time) error {
	staleAfter, err := resolveOwnerStaleAfterForWorktree(ctx, worktree, f.ownerStaleAfterExplicit, f.ownerStaleAfter)
	if err != nil {
		return err
	}
	ownerID := ownership.ResolveOwnerID(f.owner)
	notice, err := ownership.Enforce(worktree, ownerID, now, staleAfter, f.forceForeignOwner)
	if err != nil {
		return err
	}
	if notice != "" {
		_, _ = fmt.Fprintf(out, "%s %s\n", ui.LabelWarning.Render("!"), notice)
	}
	return nil
}

// resolveOwnerStaleAfterForWorktree loads worktree's repo .argus/config.yml
// (best-effort, mirroring cmd/ship.go's forgeConfigDefault/
// titlePrefixTemplateConfigDefault: a worktree outside any repo simply has no
// config to read, so it falls back to flagValue rather than erroring) and
// resolves owner_stale_after against it via resolveOwnerStaleAfter. Split out
// of enforceOwnership so every one of its four callers (rework, rebase, ship,
// worker answer) shares this one RepoRoot+Load derivation instead of each
// repeating it — a config file that does load but has a malformed
// owner_stale_after value is a real *ui.UserError, not silently ignored.
func resolveOwnerStaleAfterForWorktree(ctx context.Context, worktree string, explicit bool, flagValue time.Duration) (time.Duration, error) {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return flagValue, nil //nolint:nilerr // best-effort, mirroring cmd/ship.go's forgeConfigDefault: no repo root means no config to read, not a real failure
	}
	path := repoconfig.Path(repoRoot)
	rc, err := repoconfig.Load(path)
	if err != nil {
		return 0, &ui.UserError{Err: fmt.Errorf("loading %s: %w", path, err)}
	}
	return resolveOwnerStaleAfter(explicit, flagValue, &rc, path)
}
