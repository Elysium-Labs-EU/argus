package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Approval is argus's recorded disposition of a worker's change, written by
// supervise and read by ship. It is the typed handoff that makes a verdict
// enforceable rather than advisory: ship refuses to open a PR for a worktree
// whose Approval is missing or not Approved (absent --force). Source is "gate"
// when the deterministic gate cleared it, or "review" when the LLM reviewer did.
type Approval struct {
	UpdatedAt time.Time `json:"updated_at"`
	Source    string    `json:"source"`
	Summary   string    `json:"summary"`
	// ContentHash binds this verdict to the exact worktree content that was
	// measured at approval time — a hash over every touched file's bytes,
	// not just their line counts. ship recomputes it just before committing
	// and refuses to ship if the worktree no longer matches, so an edit that
	// lands after approval can't ride an old verdict that never saw it.
	ContentHash string   `json:"content_hash"`
	Reasons     []string `json:"reasons,omitempty"`
	// MeasuredDiff is argus's own ground-truth diff size at the moment this
	// verdict was recorded. A later round subtracts it from a fresh
	// measurement so the under-report check judges only what changed since
	// this verdict, not a change size that already cleared review once.
	MeasuredDiff DiffStat `json:"measured_diff"`
	Approved     bool     `json:"approved"`
}

// Provenance names who cleared a worker's change and — the operational payload —
// whether the operator still needs to hand-read its diff before ship. It exists
// to retire the redundant third pass: a supervise run already verifies each diff
// up to twice (the deterministic gate, then an LLM review only for what the gate
// escalated), yet an operator who can't tell an auto-approved worker from one
// merely surfaced for a decision re-reads every diff by hand. Making the source
// explicit lets the safe habit shrink to "hand-read only what asks for a human."
type Provenance string

const (
	// ProvenanceGateApproved: the deterministic gate cleared it on plain facts,
	// zero LLM cost — already verified, no human read needed.
	ProvenanceGateApproved Provenance = "gate-auto-approved"
	// ProvenanceReviewerApproved: the gate escalated and the LLM reviewer
	// approved — already verified twice, no human read needed.
	ProvenanceReviewerApproved Provenance = "reviewer-approved"
	// ProvenanceAwaitingHuman: no approving verdict — the gate surfaced this for
	// a human decision (escalated with no reviewer, a request-changes, an
	// unwaivable hard reason, or a blocked worker). This is the only kind an
	// operator must hand-read.
	ProvenanceAwaitingHuman Provenance = "surfaced-awaiting-human"
)

// Provenance derives who cleared this verdict from Source+Approved rather than
// storing a separate field, so a verdict.json written before this existed still
// classifies correctly. A rejected verdict is surfaced-awaiting-human regardless
// of source: reviewOne records reviewer request-changes and unwaivable hard-reason
// overrides both with Approved=false, and either way a human must look.
func (a *Approval) Provenance() Provenance {
	if !a.Approved {
		return ProvenanceAwaitingHuman
	}
	if a.Source == "review" {
		return ProvenanceReviewerApproved
	}
	return ProvenanceGateApproved
}

// NeedsHumanRead reports whether the operator should hand-read this worker's diff
// before ship. Only a surfaced-awaiting-human worker does; a gate- or
// reviewer-approved one has already been verified and re-reading it is the
// avoidable spend this signal exists to cut.
func (p Provenance) NeedsHumanRead() bool {
	return p == ProvenanceAwaitingHuman
}

// VerdictPath is where a worker's Approval lives inside its worktree. It sits
// under .claude/argus so ship (and CommitAll's excludes) can find it, and so it
// never lands in the PR.
func VerdictPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "verdict.json")
}

// WriteApproval atomically records a's disposition for a worktree.
func WriteApproval(worktree string, a *Approval) error {
	path := VerdictPath(worktree)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating verdict dir: %w", err)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding verdict: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing verdict: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming verdict into place: %w", err)
	}
	return nil
}

// LoadApproval reads a worktree's recorded verdict. found is false (with no
// error) when no verdict was written — the "supervise never cleared this" case
// ship must treat as "not approved".
func LoadApproval(worktree string) (approval Approval, found bool, err error) {
	data, err := os.ReadFile(VerdictPath(worktree))
	if errors.Is(err, fs.ErrNotExist) {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, fmt.Errorf("reading verdict: %w", err)
	}
	if err := json.Unmarshal(data, &approval); err != nil {
		return Approval{}, false, fmt.Errorf("decoding verdict: %w", err)
	}
	return approval, true, nil
}
