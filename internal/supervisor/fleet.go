package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
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

// Foreign reports whether row's recorded owner.json names a controller other
// than controllerID. A worktree with no owner.json (FileAbsent — predates the
// lease feature, or was never spawned by supervise) or one whose owner.json
// failed to decode (FileUnreadable — a corrupt lease is a real anomaly worth
// surfacing, not something that can be silently attributed away) is never
// foreign: both are "can't tell whose this is," which FilterFleet's default
// scope treats as belonging to the caller rather than hiding.
func (r *FleetRow) Foreign(controllerID string) bool {
	return r.OwnerFile == FileOK && r.Owner.OwnerID != controllerID
}

// Idle reports whether row has no status.json at all — a worktree that
// exists (`git worktree list` sees it) but that no worker has ever reported
// into. Distinct from an unreadable status.json, which is a real anomaly and
// must stay visible regardless of this check.
func (r *FleetRow) Idle() bool {
	return r.StatusFile == FileAbsent
}

// FleetFilterResult is FilterFleet's output: the rows that survived scoping,
// plus how many were dropped by each filter — so a caller (table or --json)
// can distinguish "nothing here" from "filtered out" instead of silently
// shrinking the list.
type FleetFilterResult struct {
	Rows                 []FleetRow
	ExcludedForeignCount int
	ExcludedIdleCount    int
}

// FilterFleet applies fleet's two default-on filters — owner scope and idle
// hiding — to rows, in that order: a row excluded as foreign is never also
// counted as idle, so the two counts never double-count the same row. all
// disables the owner-scope filter (ExcludedForeignCount stays 0);
// includeIdle disables the idle filter (ExcludedIdleCount stays 0). rows is
// taken by value (a slice header, not a copy of its backing array) since
// FilterFleet only ever reads it.
func FilterFleet(rows []FleetRow, controllerID string, all, includeIdle bool) FleetFilterResult {
	result := FleetFilterResult{Rows: make([]FleetRow, 0, len(rows))}
	for i := range rows {
		r := &rows[i]
		if !all && r.Foreign(controllerID) {
			result.ExcludedForeignCount++
			continue
		}
		if !includeIdle && r.Idle() {
			result.ExcludedIdleCount++
			continue
		}
		result.Rows = append(result.Rows, *r)
	}
	return result
}

// ticketPattern matches a leading ticket key at the start of a branch name —
// one or more letters, a hyphen, one or more digits (e.g. "AP-1169",
// "ap-1166") — the shape a Jira/Linear/GitHub-issue-style branch prefix
// takes regardless of which casing its project key was typed in.
var ticketPattern = regexp.MustCompile(`(?i)^([a-z]+)-(\d+)`)

// normalizeTicket extracts and uppercases a leading ticket key from a branch
// name (e.g. "ap-1166-fix-thing" -> "AP-1166"), so a fleet row maps to its
// tracker key without eyeballing casing that varies branch to branch. Empty
// when branch carries no such prefix — a plain descriptive branch, or one
// argus itself auto-generated from a task slug.
func normalizeTicket(branch string) string {
	m := ticketPattern.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + "-" + m[2]
}

// nullableTime renders as JSON null for a zero time.Time instead of Go's
// 0001-01-01T00:00:00Z sentinel — encoding/json's omitempty never treats a
// zero time.Time as empty, so a control-plane record's own never-written
// timestamp would otherwise be indistinguishable from a real one that
// happens to fall on it.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// nullableStatus, nullableApproval, nullableLifecycle, and nullableOwner each
// embed their plain record and shadow its own timestamp field with an
// omitempty *time.Time (see nullableTime) — the shallower, explicitly named
// field wins over the deeper promoted one of the same JSON name, so encoding
// only ever emits the corrected value. Used by FleetRow.MarshalJSON.
type (
	nullableStatus struct {
		UpdatedAt *time.Time `json:"updated_at,omitempty"`
		protocol.Status
	}
	nullableApproval struct {
		UpdatedAt *time.Time `json:"updated_at,omitempty"`
		protocol.Approval
	}
	nullableLifecycle struct {
		UpdatedAt *time.Time `json:"updated_at,omitempty"`
		protocol.Lifecycle
	}
	nullableOwner struct {
		SpawnedAt   *time.Time `json:"spawned_at,omitempty"`
		HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
		ownership.Owner
	}
)

// MarshalJSON renders FleetRow for `argus fleet --json`: every Go field
// unchanged, except each embedded record's own zero-valued timestamp is
// nulled (see nullableTime) and two fields with no Go-struct home of their
// own are added — Ticket (see normalizeTicket) and PhaseUpdatedAt, fleet's
// own name for Status.UpdatedAt ("last report time," not status.json's own
// vocabulary — see BuildFleet's doc comment on Phase vs. lifecycle State).
func (r *FleetRow) MarshalJSON() ([]byte, error) {
	type alias FleetRow // same fields, no MarshalJSON — breaks recursion
	type jsonRow struct {
		Status         nullableStatus    `json:"Status"`
		Verdict        nullableApproval  `json:"Verdict"`
		Lifecycle      nullableLifecycle `json:"Lifecycle"`
		Owner          nullableOwner     `json:"Owner"`
		PhaseUpdatedAt *time.Time        `json:"phase_updated_at,omitempty"`
		Ticket         string            `json:"ticket"`
		alias
	}
	return json.Marshal(jsonRow{
		alias:          alias(*r),
		Status:         nullableStatus{Status: r.Status, UpdatedAt: nullableTime(r.Status.UpdatedAt)},
		Verdict:        nullableApproval{Approval: r.Verdict, UpdatedAt: nullableTime(r.Verdict.UpdatedAt)},
		Lifecycle:      nullableLifecycle{Lifecycle: r.Lifecycle, UpdatedAt: nullableTime(r.Lifecycle.UpdatedAt)},
		Owner:          nullableOwner{Owner: r.Owner, SpawnedAt: nullableTime(r.Owner.SpawnedAt), HeartbeatAt: nullableTime(r.Owner.HeartbeatAt)},
		Ticket:         normalizeTicket(r.Branch),
		PhaseUpdatedAt: nullableTime(r.Status.UpdatedAt),
	})
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
