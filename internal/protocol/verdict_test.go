package protocol

import (
	"testing"
	"time"
)

func TestApprovalRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &Approval{
		Approved:  true,
		Source:    "review",
		Summary:   "looks correct",
		Reasons:   []string{"diff verified"},
		UpdatedAt: time.Now().Truncate(time.Second),
	}
	if err := WriteApproval(wt, in); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	got, found, err := LoadApproval(wt)
	if err != nil || !found {
		t.Fatalf("LoadApproval: found=%v err=%v", found, err)
	}
	if !got.Approved || got.Source != "review" || got.Summary != "looks correct" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadApprovalMissingIsNotFound(t *testing.T) {
	_, found, err := LoadApproval(t.TempDir())
	if err != nil {
		t.Fatalf("missing verdict should not error: %v", err)
	}
	if found {
		t.Error("found should be false when no verdict was written")
	}
}

// TestProvenanceClassifiesApproval pins the derivation both the supervise report
// and run_summary rely on: a rejected verdict always needs a human regardless of
// which source rejected it, a reviewer approve and a gate approve are told apart
// by Source, and only the awaiting-human case reports NeedsHumanRead. This is the
// signal that lets an operator skip re-reading an already-cleared diff.
func TestProvenanceClassifiesApproval(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		want      Provenance
		approved  bool
		wantHuman bool
	}{
		{"rejected by reviewer", "review", ProvenanceAwaitingHuman, false, true},
		{"rejected/escalated by gate", "gate", ProvenanceAwaitingHuman, false, true},
		{"rejected with no source", "", ProvenanceAwaitingHuman, false, true},
		{"reviewer approved", "review", ProvenanceReviewerApproved, true, false},
		{"gate auto-approved", "gate", ProvenanceGateApproved, true, false},
		{"approved by any non-review source", "other", ProvenanceGateApproved, true, false},
		{"rework budget exceeded", SourceReworkBudget, ProvenanceReworkBudgetExceeded, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Approval{Approved: tc.approved, Source: tc.source}
			got := a.Provenance()
			if got != tc.want {
				t.Errorf("Provenance() = %q, want %q", got, tc.want)
			}
			if got.NeedsHumanRead() != tc.wantHuman {
				t.Errorf("NeedsHumanRead() = %v, want %v", got.NeedsHumanRead(), tc.wantHuman)
			}
		})
	}
}
