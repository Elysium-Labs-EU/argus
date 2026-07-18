package cmd

import (
	"context"
	"strings"
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

func TestShipText(t *testing.T) {
	commit, prTitle, prBody := shipText("", 144, "fix-x")
	if !strings.Contains(commit, "Closes #144") || !strings.Contains(prBody, "Closes #144") {
		t.Errorf("issue not referenced: commit=%q body=%q", commit, prBody)
	}
	if prTitle != "fix: fix-x" {
		t.Errorf("default title: got %q", prTitle)
	}
	c2, t2, _ := shipText("feat: real title", 0, "b")
	if t2 != "feat: real title" || strings.Contains(c2, "Closes") {
		t.Errorf("explicit title / no issue: title=%q commit=%q", t2, c2)
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	owner, name, ok := splitOwnerRepo("Elysium_Labs/argus")
	if !ok || owner != "Elysium_Labs" || name != "argus" {
		t.Errorf("got %s/%s ok=%v", owner, name, ok)
	}
	for _, bad := range []string{"noslash", "/leading", "trailing/"} {
		if _, _, ok := splitOwnerRepo(bad); ok {
			t.Errorf("splitOwnerRepo(%q) should fail", bad)
		}
	}
}

func TestResolveRepoOverride(t *testing.T) {
	owner, name, err := resolveRepo(context.Background(), "Owner/Repo", "/ignored")
	if err != nil || owner != "Owner" || name != "Repo" {
		t.Errorf("override: got %s/%s err %v", owner, name, err)
	}
	if _, _, err := resolveRepo(context.Background(), "garbage", "/ignored"); err == nil {
		t.Error("a non owner/name override should error")
	}
}

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
