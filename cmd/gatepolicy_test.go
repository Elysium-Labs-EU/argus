package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestResolveOwnerStaleAfterExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{OwnerStaleAfter: "1h"}
	got, err := resolveOwnerStaleAfter(true, 5*time.Minute, &rc, "/repo/.argus/config.yml")
	if err != nil {
		t.Fatalf("resolveOwnerStaleAfter: %v", err)
	}
	if got != 5*time.Minute {
		t.Errorf("resolveOwnerStaleAfter = %v, want the explicit flag value", got)
	}
}

func TestResolveOwnerStaleAfterPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{OwnerStaleAfter: "1h"}
	got, err := resolveOwnerStaleAfter(false, 30*time.Minute, &rc, "/repo/.argus/config.yml")
	if err != nil {
		t.Fatalf("resolveOwnerStaleAfter: %v", err)
	}
	if got != time.Hour {
		t.Errorf("resolveOwnerStaleAfter = %v, want the repo config value", got)
	}
}

func TestResolveOwnerStaleAfterFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got, err := resolveOwnerStaleAfter(false, 30*time.Minute, &repoconfig.Config{}, "/repo/.argus/config.yml")
	if err != nil {
		t.Fatalf("resolveOwnerStaleAfter: %v", err)
	}
	if got != 30*time.Minute {
		t.Errorf("resolveOwnerStaleAfter = %v, want the flag's own default", got)
	}
}

func TestResolveOwnerStaleAfterMalformedConfigValueErrors(t *testing.T) {
	rc := repoconfig.Config{OwnerStaleAfter: "not-a-duration"}
	_, err := resolveOwnerStaleAfter(false, 30*time.Minute, &rc, "/repo/.argus/config.yml")
	if err == nil {
		t.Fatal("want an error for a malformed owner_stale_after config value")
	}
}

func TestResolveOwnerStaleAfterExplicitFlagSkipsMalformedConfigValue(t *testing.T) {
	rc := repoconfig.Config{OwnerStaleAfter: "not-a-duration"}
	got, err := resolveOwnerStaleAfter(true, 5*time.Minute, &rc, "/repo/.argus/config.yml")
	if err != nil {
		t.Fatalf("an explicit flag should win before the malformed config value is ever parsed, got: %v", err)
	}
	if got != 5*time.Minute {
		t.Errorf("resolveOwnerStaleAfter = %v, want the explicit flag value", got)
	}
}

func TestResolveWorktreeDirExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{WorktreeDir: ".."}
	got := resolveWorktreeDir(true, "../worktrees", &rc)
	if got != "../worktrees" {
		t.Errorf("resolveWorktreeDir = %q, want the explicit flag value", got)
	}
}

func TestResolveWorktreeDirPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{WorktreeDir: ".."}
	got := resolveWorktreeDir(false, "", &rc)
	if got != ".." {
		t.Errorf("resolveWorktreeDir = %q, want the repo config value", got)
	}
}

func TestResolveWorktreeDirFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveWorktreeDir(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveWorktreeDir = %q, want empty (no worktree dir configured anywhere)", got)
	}
}

func TestWarnDeprecatedConfigKeysWritesOneLinePerKey(t *testing.T) {
	rc := &repoconfig.Config{Deprecated: []repoconfig.DeprecatedKeyUse{
		{Old: "ship_lint", New: "ship_verify_command"},
		{Old: "verify_command", New: "gate_verify_command"},
	}}
	var buf bytes.Buffer
	warnDeprecatedConfigKeys(&buf, rc)
	got := buf.String()
	for _, want := range []string{
		`warning: .argus/config.yml key "ship_lint" is deprecated, use "ship_verify_command" instead (both still work)`,
		`warning: .argus/config.yml key "verify_command" is deprecated, use "gate_verify_command" instead (both still work)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing line %q", got, want)
		}
	}
}

func TestWarnDeprecatedConfigKeysNoOutputWhenNoneDeprecated(t *testing.T) {
	var buf bytes.Buffer
	warnDeprecatedConfigKeys(&buf, &repoconfig.Config{})
	if buf.Len() != 0 {
		t.Errorf("output = %q, want empty when Deprecated is empty", buf.String())
	}
}
