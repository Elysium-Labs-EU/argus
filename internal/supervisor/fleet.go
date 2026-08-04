package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// FileStatus classifies how reading one worktree's control-plane file went.
// FileAbsent and FileUnreadable render differently on purpose: a worktree
// that hasn't reported yet (absent) looks nothing like one whose status.json
// exists but is corrupt (unreadable) — collapsing them would make corruption
// invisible, which is the fail-open behavior fleet exists to surface instead
// of inherit.
type FileStatus string

const (
	FileAbsent     FileStatus = "absent"
	FileOK         FileStatus = "ok"
	FileUnreadable FileStatus = "unreadable"
)

// FleetRow is one linked worktree's aggregated ground truth: the same
// records supervise/ship/rework already trust, joined for a read-only view.
// Each Load* field nests the loader's own return type verbatim rather than
// picking out individual fields, so `argus fleet --json` exposes exactly
// what supervise/ship/rework themselves see. Fields are ordered for struct
// alignment (fieldalignment-enforced, like protocol.Status), not logical
// order or json output order.
type FleetRow struct {
	Owner         ownership.Owner
	VerdictErr    string `json:",omitempty"`
	LifecycleFile FileStatus
	StatusErr     string `json:",omitempty"`
	Branch        string
	VerdictFile   FileStatus
	Path          string
	OwnerErr      string `json:",omitempty"`
	StatusFile    FileStatus
	LifecycleErr  string `json:",omitempty"`
	OwnerFile     FileStatus
	Lifecycle     protocol.Lifecycle
	Status        protocol.Status
	Verdict       protocol.Approval
	HeartbeatAge  time.Duration
}

// BuildFleet lists every worktree linked to repoRoot and joins each one's
// status.json/verdict.json/lifecycle.json/owner.json into a FleetRow. It is
// strictly read-only: unlike supervise's poll loop (which re-stamps
// heartbeat_at every tick), fleet only ever reads these files. now is
// injected rather than read via time.Now() here, so a test can supply a
// fixed clock for HeartbeatAge.
func BuildFleet(ctx context.Context, repoRoot string, now time.Time) ([]FleetRow, error) {
	entries, err := ListLinkedWorktrees(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	rows := make([]FleetRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, buildFleetRow(&e, now))
	}
	return rows, nil
}

func buildFleetRow(e *WorktreeEntry, now time.Time) FleetRow {
	row := FleetRow{Path: e.Path, Branch: e.Branch}

	s, err := protocol.Load(protocol.StatusPath(e.Path))
	row.StatusFile, row.StatusErr = classify(protocol.StatusPath(e.Path), true, err)
	row.Status = s

	a, foundV, errV := protocol.LoadApproval(e.Path)
	row.VerdictFile, row.VerdictErr = classify(protocol.VerdictPath(e.Path), foundV, errV)
	row.Verdict = a

	l, foundL, errL := protocol.LoadLifecycle(e.Path)
	row.LifecycleFile, row.LifecycleErr = classify(protocol.LifecyclePath(e.Path), foundL, errL)
	row.Lifecycle = l

	o, foundO, errO := ownership.Load(e.Path)
	row.OwnerFile, row.OwnerErr = classify(ownership.Path(e.Path), foundO, errO)
	row.Owner = o
	if row.OwnerFile == FileOK {
		row.HeartbeatAge = now.Sub(o.HeartbeatAt)
	}

	return row
}

// classify turns one loader's outcome into a FileStatus, folding in the
// null-JSON check every one of these loaders is missing: json.Unmarshal of a
// literal `null` into a struct pointer is a documented no-op that leaves the
// zero value with a nil error (protocol.LoadReworkState already guards this
// one case; status.go/lifecycle.go/verdict.go/ownership.go do not), so a
// clean decode still needs its own raw-bytes check before it can be trusted
// as FileOK. found/err follow the two shapes the loaders in this repo use:
// protocol.Load only returns err (a missing file wrapped as os.ErrNotExist,
// so callers pass found=true and let errors.Is decide); the other three
// return an explicit found bool alongside err.
func classify(path string, found bool, err error) (FileStatus, string) {
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileAbsent, ""
		}
		return FileUnreadable, err.Error()
	}
	if !found {
		return FileAbsent, ""
	}
	if isNullJSON(path) {
		return FileUnreadable, "file contains null, not a valid record"
	}
	return FileOK, ""
}

// isNullJSON reports whether path's content is the literal JSON value
// "null". It re-reads the file rather than reusing the loader's own decoded
// bytes — the loaders here don't expose them — but only on the already-cheap
// success path, and it never re-implements any of the loaders' own field
// decoding.
func isNullJSON(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path is argus-derived from a worktree, not user input
	if err != nil {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}
