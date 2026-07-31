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

func TestResolveMaxReworkBudgetExplicitFlagWinsOutright(t *testing.T) {
	rcBudget := 10
	rc := repoconfig.Config{ReworkBudget: &rcBudget}
	got := resolveMaxReworkBudget(true, 3, &rc)
	if got != 3 {
		t.Errorf("resolveMaxReworkBudget = %d, want the explicit flag value 3", got)
	}
}

func TestResolveMaxReworkBudgetPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rcBudget := 10
	rc := repoconfig.Config{ReworkBudget: &rcBudget}
	got := resolveMaxReworkBudget(false, 3, &rc)
	if got != 10 {
		t.Errorf("resolveMaxReworkBudget = %d, want 10 from repo config", got)
	}
}

func TestResolveMaxReworkBudgetFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveMaxReworkBudget(false, 3, &repoconfig.Config{})
	if got != 3 {
		t.Errorf("resolveMaxReworkBudget = %d, want the flag's own default 3", got)
	}
}

// TestResolveMaxReworkBudgetRepoConfigZeroIsMeaningful checks that a repo
// config explicitly disabling the budget (rework_budget: 0) is honored
// rather than treated as "not set" — the whole reason
// repoconfig.Config.ReworkBudget is a pointer.
func TestResolveMaxReworkBudgetRepoConfigZeroIsMeaningful(t *testing.T) {
	zero := 0
	rc := repoconfig.Config{ReworkBudget: &zero}
	got := resolveMaxReworkBudget(false, 3, &rc)
	if got != 0 {
		t.Errorf("resolveMaxReworkBudget = %d, want 0 (explicit disable from repo config)", got)
	}
}

func TestResolveGateVerifyCommandExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{GateVerifyCommand: "make ci"}
	got := resolveGateVerifyCommand(true, "make lint", &rc)
	if got != "make lint" {
		t.Errorf("resolveGateVerifyCommand = %q, want the explicit flag value", got)
	}
}

func TestResolveGateVerifyCommandPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{GateVerifyCommand: "make ci"}
	got := resolveGateVerifyCommand(false, "", &rc)
	if got != "make ci" {
		t.Errorf("resolveGateVerifyCommand = %q, want the repo config value", got)
	}
}

func TestResolveGateVerifyCommandFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveGateVerifyCommand(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveGateVerifyCommand = %q, want empty (no verify command configured anywhere)", got)
	}
}

func TestResolveWorktreeBootstrapCommandExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{WorktreeBootstrapCommand: "cp ../.env .env"}
	got := resolveWorktreeBootstrapCommand(true, "cp ../.env.local .env.local", &rc)
	if got != "cp ../.env.local .env.local" {
		t.Errorf("resolveWorktreeBootstrapCommand = %q, want the explicit flag value", got)
	}
}

func TestResolveWorktreeBootstrapCommandPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{WorktreeBootstrapCommand: "cp ../.env .env"}
	got := resolveWorktreeBootstrapCommand(false, "", &rc)
	if got != "cp ../.env .env" {
		t.Errorf("resolveWorktreeBootstrapCommand = %q, want the repo config value", got)
	}
}

func TestResolveWorktreeBootstrapCommandFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveWorktreeBootstrapCommand(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveWorktreeBootstrapCommand = %q, want empty (no worktree setup command configured anywhere)", got)
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

func TestResolveShipVerifyCommandExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{ShipVerifyCommand: "make ci"}
	got := resolveShipVerifyCommand(true, "make lint", &rc)
	if got != "make lint" {
		t.Errorf("resolveShipVerifyCommand = %q, want the explicit flag value", got)
	}
}

func TestResolveShipVerifyCommandPrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{ShipVerifyCommand: "make ci"}
	got := resolveShipVerifyCommand(false, "", &rc)
	if got != "make ci" {
		t.Errorf("resolveShipVerifyCommand = %q, want the repo config value", got)
	}
}

func TestResolveShipVerifyCommandFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveShipVerifyCommand(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveShipVerifyCommand = %q, want empty (no ship verify command configured anywhere)", got)
	}
}

func TestResolveReviewNoteExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{ReviewNote: "from config"}
	got := resolveReviewNote(true, "from flag", &rc)
	if got != "from flag" {
		t.Errorf("resolveReviewNote = %q, want the explicit flag value", got)
	}
}

func TestResolveReviewNotePrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{ReviewNote: "from config"}
	got := resolveReviewNote(false, "", &rc)
	if got != "from config" {
		t.Errorf("resolveReviewNote = %q, want the repo config value", got)
	}
}

func TestResolveReviewNoteFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveReviewNote(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveReviewNote = %q, want empty (no review note configured anywhere)", got)
	}
}

func TestResolveBriefNoteExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{BriefNote: "from config"}
	got := resolveBriefNote(true, "from flag", &rc)
	if got != "from flag" {
		t.Errorf("resolveBriefNote = %q, want the explicit flag value", got)
	}
}

func TestResolveBriefNotePrefersRepoConfigWhenFlagNotPassed(t *testing.T) {
	rc := repoconfig.Config{BriefNote: "from config"}
	got := resolveBriefNote(false, "", &rc)
	if got != "from config" {
		t.Errorf("resolveBriefNote = %q, want the repo config value", got)
	}
}

func TestResolveBriefNoteFallsBackToFlagDefaultWhenNeitherSet(t *testing.T) {
	got := resolveBriefNote(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveBriefNote = %q, want empty (no brief note configured anywhere)", got)
	}
}
