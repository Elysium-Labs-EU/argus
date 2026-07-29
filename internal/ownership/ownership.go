// Package ownership implements a file-based lease that lets a spawned
// worktree/pane record which argus/herdr session spawned it, so a second,
// unrelated session cannot act on a worktree the first is still tracking.
// It mirrors internal/svcstatus and internal/repoconfig in being scoped to
// exactly this one concern rather than folded into internal/supervisor: the
// lease is read and written independently of any supervise/rework/rebase/ship
// business logic, which only ever calls the small surface here (Enforce at
// every mutating entry point, Spawn/Heartbeat from supervise's own
// worktree-creation and poll-loop code).
//
// This is purely an argus-side file convention — it makes no change to how
// herdr itself scopes sessions or panes, and it says nothing about pruning an
// abandoned worktree (a separate, not-yet-implemented feature); a stale
// lease here only ever changes whether a mismatched caller's command is
// allowed to proceed.
package ownership

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// Owner is a worktree's recorded lease: which session spawned it, a
// human-readable label for that session, and when. HeartbeatAt is advanced by
// the owning session's own supervise poll loop for as long as it keeps
// tracking the worktree (see Heartbeat) — a lease whose heartbeat has gone
// quiet for longer than a caller's --owner-stale-after is treated as
// abandoned (see Stale), the signal that lets a mismatched caller proceed
// instead of being refused forever by a session that has gone away.
type Owner struct {
	SpawnedAt   time.Time `json:"spawned_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	OwnerID     string    `json:"owner_id"`
	OwnerLabel  string    `json:"owner_label"`
}

// DefaultStaleAfter is the default --owner-stale-after threshold every
// enforcing command falls back to when the flag is left unset: long enough
// that a normal supervise poll interval (default 15s) never trips it, short
// enough that a session killed outright (crashed pane, closed terminal) stops
// blocking a second session within the same working session rather than
// requiring --force-foreign-owner indefinitely.
const DefaultStaleAfter = 30 * time.Minute

// Path is where a worktree's Owner lease lives, alongside its status.json,
// verdict.json, and lifecycle.json under the same argus control-plane
// directory (see protocol.StatusPath) — never staged into a PR.
func Path(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "owner.json")
}

// Write atomically persists o as worktree's owner.json. Unlike
// protocol.WriteLifecycle, it stamps neither timestamp itself — Spawn and
// Heartbeat each set exactly the field they own before calling this, since a
// heartbeat update must advance HeartbeatAt without disturbing the original
// SpawnedAt.
func Write(worktree string, o *Owner) error {
	path := Path(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating owner lease dir: %w", err)
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding owner lease: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing owner lease: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming owner lease into place: %w", err)
	}
	return nil
}

// Load reads a worktree's recorded lease. found is false (with no error) when
// no owner.json was ever written — either a worktree still mid-spawn, or one
// created before this feature shipped, both of which Enforce treats as
// unowned so an old worktree is never refused for a lease it never had a
// chance to record.
func Load(worktree string) (o Owner, found bool, err error) {
	data, err := os.ReadFile(Path(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return Owner{}, false, nil
	}
	if err != nil {
		return Owner{}, false, fmt.Errorf("reading owner lease: %w", err)
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return Owner{}, false, fmt.Errorf("decoding owner lease: %w", err)
	}
	return o, true, nil
}

// Spawn records a freshly created worktree's lease: ownerID/ownerLabel claim
// it for the spawning session, with both SpawnedAt and HeartbeatAt set to
// now. Called once, by supervise's own worktree-creation path, alongside its
// existing scoped-permission-file writes.
func Spawn(worktree, ownerID, ownerLabel string, now time.Time) error {
	return Write(worktree, &Owner{OwnerID: ownerID, OwnerLabel: ownerLabel, SpawnedAt: now, HeartbeatAt: now})
}

// Heartbeat advances a worktree's recorded lease HeartbeatAt to now, called
// once per tick by supervise's own poll loop for every worktree it is
// actively tracking (spawned or attached). A worktree with no recorded lease
// — attached from outside supervise's own spawn path, or created before this
// feature shipped — has nothing to advance; that is not an error, since
// Enforce already treats a missing lease as unowned regardless of whether a
// heartbeat was ever recorded for it.
func Heartbeat(worktree string, now time.Time) error {
	o, found, err := Load(worktree)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	o.HeartbeatAt = now
	return Write(worktree, &o)
}

// IsOwner reports whether callerID matches o's recorded owner_id. o is a
// pointer solely to avoid copying the struct at the call site; IsOwner does
// not mutate it.
func IsOwner(o *Owner, callerID string) bool {
	return o.OwnerID == callerID
}

// Stale reports whether o's lease has gone quiet for longer than after as of
// now — the signal that lets a mismatched caller proceed instead of being
// refused by a session that crashed or was closed without ever releasing the
// worktree (there being no interaction with a worktree-prune command to
// release it explicitly; see the package doc). o is a pointer solely to
// avoid copying the struct at the call site; Stale does not mutate it.
func Stale(o *Owner, now time.Time, after time.Duration) bool {
	return now.Sub(o.HeartbeatAt) > after
}

// ResolveOwnerID resolves the caller's stable identity for an ownership
// lease, in order: an explicit flag value (the --owner flag every enforcing
// command exposes; supervise itself has no such flag and always passes ""),
// then $ARGUS_OWNER_ID, then $HERDR_WORKSPACE_ID (the enclosing herdr
// workspace, when run from inside one), then a freshly generated id for a
// caller with none of the above. Every caller resolves this once per
// invocation — supervise once per run (shared across every worker it spawns
// in that run, not re-resolved per worktree), and each enforcing command once
// at its own start — so every command agrees on the same caller identity the
// same way.
func ResolveOwnerID(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("ARGUS_OWNER_ID"); v != "" {
		return v
	}
	if v := os.Getenv("HERDR_WORKSPACE_ID"); v != "" {
		return v
	}
	return newID()
}

// DefaultOwnerLabel builds the human-readable label Spawn records alongside a
// freshly generated (or env-resolved) owner_id, so a refusal message can name
// something more useful than an opaque id: the spawning host and process, as
// they were at spawn time. It is not re-derived later — a lease's OwnerLabel
// is fixed at Spawn and only ever displayed, never compared.
func DefaultOwnerLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s (pid %d)", host, os.Getpid())
}

// newID generates a random v4-shaped UUID as the owner_id of last resort, for
// a caller with no --owner, $ARGUS_OWNER_ID, or $HERDR_WORKSPACE_ID to
// identify it. crypto/rand.Read on the OS's CSPRNG does not fail or
// short-read in practice, so its error is deliberately ignored rather than
// threaded back through every ResolveOwnerID caller for a case that does not
// happen on any platform argus supports.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Enforce is the single check every mutating command (rework, rebase, ship,
// worker answer) runs before touching an existing worktree: it loads
// worktree's recorded lease and decides whether callerID may proceed.
//
// No lease at all (a worktree predating this feature, or one supervise never
// spawned) and a matching owner both proceed silently (notice == "",
// err == nil), as does force — the human-typed --force-foreign-owner an
// operator must explicitly pass to override a mismatch; it is never inferred.
// A mismatched lease whose heartbeat is older than staleAfter is treated as
// abandoned: it also proceeds, but returns a non-empty notice the caller
// should log, since an abandoned lease still deserves a visible trace even
// though it does not block. Only a mismatched, still-fresh lease refuses,
// returning a *ui.UserError naming the actual owner (OwnerLabel and OwnerID)
// so the operator knows who to ask before reaching for --force-foreign-owner.
func Enforce(worktree, callerID string, now time.Time, staleAfter time.Duration, force bool) (notice string, err error) {
	o, found, err := Load(worktree)
	if err != nil {
		return "", err
	}
	if !found || IsOwner(&o, callerID) || force {
		return "", nil
	}
	if Stale(&o, now, staleAfter) {
		return fmt.Sprintf("worktree %s's owner lease (%s, %s) has gone quiet since %s — proceeding despite the owner mismatch",
			worktree, o.OwnerLabel, o.OwnerID, o.HeartbeatAt.Format(time.RFC3339)), nil
	}
	return "", &ui.UserError{
		Err:  fmt.Errorf("worktree is owned by %s (%s), not you (%s)", o.OwnerLabel, o.OwnerID, callerID),
		Hint: "pass --force-foreign-owner to act on this worktree anyway",
	}
}
