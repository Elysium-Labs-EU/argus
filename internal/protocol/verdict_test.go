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
