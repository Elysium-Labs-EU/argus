package protocol

import (
	"os"
	"testing"
	"time"
)

func TestReworkStateRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &ReworkState{
		RoundsAttempted: 2,
		UpdatedAt:       time.Now().Truncate(time.Second),
	}
	if err := WriteReworkState(wt, in); err != nil {
		t.Fatalf("WriteReworkState: %v", err)
	}
	got, err := LoadReworkState(wt)
	if err != nil {
		t.Fatalf("LoadReworkState: %v", err)
	}
	if got.RoundsAttempted != 2 {
		t.Errorf("RoundsAttempted = %d, want 2", got.RoundsAttempted)
	}
}

func TestLoadReworkStateMissingIsZeroValue(t *testing.T) {
	got, err := LoadReworkState(t.TempDir())
	if err != nil {
		t.Fatalf("missing rework state should not error: %v", err)
	}
	if got.RoundsAttempted != 0 {
		t.Errorf("RoundsAttempted = %d, want 0 for a worktree that never reworked", got.RoundsAttempted)
	}
}

func TestReworkStateSurvivesStatusAndVerdictRemoval(t *testing.T) {
	// supervisor.InvalidateStatus removes status.json and verdict.json before
	// every rework round; a persisted budget count that lived in either file
	// would reset to zero every round, defeating the whole point of a
	// cross-invocation budget. This test replicates that removal directly
	// (protocol cannot import supervisor) to pin that ReworkState lives
	// elsewhere and survives it.
	wt := t.TempDir()
	if err := WriteReworkState(wt, &ReworkState{RoundsAttempted: 3}); err != nil {
		t.Fatalf("WriteReworkState: %v", err)
	}
	if err := Write(StatusPath(wt), &Status{}); err != nil {
		t.Fatalf("Write status: %v", err)
	}
	if err := WriteApproval(wt, &Approval{}); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	if err := os.Remove(StatusPath(wt)); err != nil {
		t.Fatalf("removing status.json: %v", err)
	}
	if err := os.Remove(VerdictPath(wt)); err != nil {
		t.Fatalf("removing verdict.json: %v", err)
	}
	got, err := LoadReworkState(wt)
	if err != nil {
		t.Fatalf("LoadReworkState: %v", err)
	}
	if got.RoundsAttempted != 3 {
		t.Errorf("RoundsAttempted = %d after status/verdict removal, want 3 (rework state must survive it)", got.RoundsAttempted)
	}
}
