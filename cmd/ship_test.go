package cmd

import (
	"context"
	"strings"
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

func TestPrTitleForAndClosesLine(t *testing.T) {
	if got := prTitleFor("", 144, "fix-x"); got != "fix: fix-x (#144)" {
		t.Errorf("default+issue title: got %q", got)
	}
	if got := prTitleFor("feat: real", 0, "b"); got != "feat: real" {
		t.Errorf("explicit title: got %q", got)
	}
	if got := closesLine(144); !strings.Contains(got, "Closes #144") {
		t.Errorf("closesLine: got %q", got)
	}
	if got := closesLine(0); got != "" {
		t.Errorf("no issue should have no closes line: got %q", got)
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

func TestResolveRepoDetectsHostAndOverride(t *testing.T) {
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@github.com:acme/widget.git"})

	host, owner, name, err := resolveRepo(context.Background(), "", wt)
	if err != nil || host != "github.com" || owner != "acme" || name != "widget" {
		t.Errorf("detect: got host=%s %s/%s err=%v", host, owner, name, err)
	}
	// Override changes owner/name but the host still comes from the remote.
	host2, o2, n2, err := resolveRepo(context.Background(), "Other/Repo", wt)
	if err != nil || host2 != "github.com" || o2 != "Other" || n2 != "Repo" {
		t.Errorf("override: got host=%s %s/%s err=%v", host2, o2, n2, err)
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
