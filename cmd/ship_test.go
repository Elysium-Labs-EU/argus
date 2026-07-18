package cmd

import (
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

func TestCheckApprovedRefusesWithoutVerdict(t *testing.T) {
	// No verdict.json at all: ship must refuse unless forced.
	if err := checkApproved(t.TempDir(), false); err == nil {
		t.Fatal("want error shipping a worktree argus never cleared")
	}
}

func TestCheckApprovedRefusesNotApproved(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: false, Source: "review", Summary: "missing UPDATE path"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(wt, false); err == nil {
		t.Fatal("want error shipping a change argus did not approve")
	}
}

func TestCheckApprovedAllowsApproved(t *testing.T) {
	wt := t.TempDir()
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	if err := checkApproved(wt, false); err != nil {
		t.Fatalf("approved change should ship: %v", err)
	}
}

func TestCheckApprovedForceBypassesEverything(t *testing.T) {
	// No verdict, but --force overrides.
	if err := checkApproved(t.TempDir(), true); err != nil {
		t.Fatalf("--force should bypass the verdict check: %v", err)
	}
}
