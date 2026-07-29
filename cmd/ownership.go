package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
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
	ownerStaleAfterFlagHelp   = "treat a worktree's owner lease as abandoned once its heartbeat is older than this; a mismatched caller then proceeds (with a logged notice) instead of being refused"
)

// ownerFlags bundles the three ownership-lease flags every mutating command
// (rework, rebase, ship, worker answer) registers — --owner,
// --force-foreign-owner, --owner-stale-after — mirroring gateFlags's own
// bundling of the review-gate flags rework and supervise share.
type ownerFlags struct {
	owner             string
	ownerStaleAfter   time.Duration
	forceForeignOwner bool
}

// enforceOwnership resolves the caller's owner_id the same way every mutating
// command does — flag > $ARGUS_OWNER_ID > $HERDR_WORKSPACE_ID > a generated
// id, see ownership.ResolveOwnerID — and checks it against worktree's
// recorded lease (see ownership.Enforce). A mismatched, still-fresh lease
// refuses with a *ui.UserError naming the actual owner unless f.forceForeignOwner
// is set; a mismatched but stale lease, or no lease at all (a worktree
// predating this feature), both proceed — the former logging a notice to out
// so an abandoned lease still leaves a visible trace even though it doesn't
// block.
func enforceOwnership(out io.Writer, worktree string, f ownerFlags, now time.Time) error {
	ownerID := ownership.ResolveOwnerID(f.owner)
	notice, err := ownership.Enforce(worktree, ownerID, now, f.ownerStaleAfter, f.forceForeignOwner)
	if err != nil {
		return err
	}
	if notice != "" {
		_, _ = fmt.Fprintf(out, "%s %s\n", ui.LabelWarning.Render("!"), notice)
	}
	return nil
}
