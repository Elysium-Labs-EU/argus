package cmd

import (
	"reflect"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

func TestResolveGatePolicyExplicitFlagWinsOutright(t *testing.T) {
	rcMax := 999
	rc := repoconfig.Config{
		MaxDiffLines:       &rcMax,
		ProofRequiredPaths: []string{"from-config"},
		AlwaysReviewPaths:  []string{"from-config"},
	}
	f := gateFlags{
		maxDiffLines:          100,
		proofRequiredPaths:    []string{"from-flag"},
		alwaysReviewPaths:     []string{"from-flag"},
		maxDiffLinesExplicit:  true,
		proofRequiredExplicit: true,
		alwaysReviewExplicit:  true,
	}
	got := resolveGatePolicy(f, &rc)
	if got.MaxDiffLines != 100 {
		t.Errorf("MaxDiffLines = %d, want the explicit flag value", got.MaxDiffLines)
	}
	if !reflect.DeepEqual(got.ProofRequiredPaths, []string{"from-flag"}) {
		t.Errorf("ProofRequiredPaths = %v, want the explicit flag value", got.ProofRequiredPaths)
	}
	if !reflect.DeepEqual(got.AlwaysReviewPaths, []string{"from-flag"}) {
		t.Errorf("AlwaysReviewPaths = %v, want the explicit flag value", got.AlwaysReviewPaths)
	}
}

func TestResolveGatePolicyPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rcMax := 250
	rc := repoconfig.Config{
		MaxDiffLines:       &rcMax,
		ProofRequiredPaths: []string{"terraform", "deploy"},
		AlwaysReviewPaths:  []string{"auth", "billing"},
	}
	f := gateFlags{maxDiffLines: 400, proofRequiredPaths: []string{"default"}, alwaysReviewPaths: []string{"default"}}
	got := resolveGatePolicy(f, &rc)
	if got.MaxDiffLines != 250 {
		t.Errorf("MaxDiffLines = %d, want 250 from repo config", got.MaxDiffLines)
	}
	if !reflect.DeepEqual(got.ProofRequiredPaths, []string{"terraform", "deploy"}) {
		t.Errorf("ProofRequiredPaths = %v, want the repo config value", got.ProofRequiredPaths)
	}
	if !reflect.DeepEqual(got.AlwaysReviewPaths, []string{"auth", "billing"}) {
		t.Errorf("AlwaysReviewPaths = %v, want the repo config value", got.AlwaysReviewPaths)
	}
}

func TestResolveGatePolicyFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	f := gateFlags{maxDiffLines: 400, proofRequiredPaths: []string{"systemd"}, alwaysReviewPaths: []string{"monitor"}}
	got := resolveGatePolicy(f, &repoconfig.Config{})
	if got.MaxDiffLines != 400 {
		t.Errorf("MaxDiffLines = %d, want the flag's own default", got.MaxDiffLines)
	}
	if !reflect.DeepEqual(got.ProofRequiredPaths, []string{"systemd"}) {
		t.Errorf("ProofRequiredPaths = %v, want the flag's own default", got.ProofRequiredPaths)
	}
	if !reflect.DeepEqual(got.AlwaysReviewPaths, []string{"monitor"}) {
		t.Errorf("AlwaysReviewPaths = %v, want the flag's own default", got.AlwaysReviewPaths)
	}
}

// TestResolveGatePolicyRepoConfigMaxDiffLinesZeroIsMeaningful checks that a
// repo config explicitly disabling the diff ceiling (max_diff_lines: 0) is
// honored rather than treated as "not set" — the whole reason
// repoconfig.Config.MaxDiffLines is a pointer.
func TestResolveGatePolicyRepoConfigMaxDiffLinesZeroIsMeaningful(t *testing.T) {
	zero := 0
	rc := repoconfig.Config{MaxDiffLines: &zero}
	got := resolveGatePolicy(gateFlags{maxDiffLines: 400}, &rc)
	if got.MaxDiffLines != 0 {
		t.Errorf("MaxDiffLines = %d, want 0 (explicit disable from repo config)", got.MaxDiffLines)
	}
}

func TestResolveVerifyCommandExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{VerifyCommand: "make ci"}
	got := resolveVerifyCommand(true, "make lint", &rc)
	if got != "make lint" {
		t.Errorf("resolveVerifyCommand = %q, want the explicit flag value", got)
	}
}

func TestResolveVerifyCommandPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{VerifyCommand: "make ci"}
	got := resolveVerifyCommand(false, "", &rc)
	if got != "make ci" {
		t.Errorf("resolveVerifyCommand = %q, want the repo config value", got)
	}
}

func TestResolveVerifyCommandFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveVerifyCommand(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveVerifyCommand = %q, want empty (no verify command configured anywhere)", got)
	}
}

func TestResolveWorktreeSetupCmdExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{WorktreeSetupCmd: "cp ../.env .env"}
	got := resolveWorktreeSetupCmd(true, "cp ../.env.local .env.local", &rc)
	if got != "cp ../.env.local .env.local" {
		t.Errorf("resolveWorktreeSetupCmd = %q, want the explicit flag value", got)
	}
}

func TestResolveWorktreeSetupCmdPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{WorktreeSetupCmd: "cp ../.env .env"}
	got := resolveWorktreeSetupCmd(false, "", &rc)
	if got != "cp ../.env .env" {
		t.Errorf("resolveWorktreeSetupCmd = %q, want the repo config value", got)
	}
}

func TestResolveWorktreeSetupCmdFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveWorktreeSetupCmd(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveWorktreeSetupCmd = %q, want empty (no worktree setup command configured anywhere)", got)
	}
}
