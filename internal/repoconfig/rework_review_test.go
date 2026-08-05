// rework_review_test.go covers the region restructure that gives argus's
// rework and review operations their own config regions, sibling to ship:,
// instead of smearing rework_budget onto the top level and the gate/review
// cluster onto the phases.awaiting_review worker-permission phase. The core
// guarantee under test: an old-shape document and its new-shape equivalent
// must resolve to identical effective Config (Deprecated aside), so migrating
// a repo's config.yml is never a behavior change.
package repoconfig

import (
	"reflect"
	"strings"
	"testing"
)

// TestLoadReworkBlockParsesBudgetAndMaxRounds is the rework: happy path,
// mirroring TestLoadShipBlockParsesVerifyCommandAndTitlePrefixTemplate.
func TestLoadReworkBlockParsesBudgetAndMaxRounds(t *testing.T) {
	got, err := loadContent(t, "rework:\n  budget: 6\n  max_rounds: 4\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReworkBudget == nil || *got.ReworkBudget != 6 {
		t.Errorf("ReworkBudget = %v, want a pointer to 6", got.ReworkBudget)
	}
	if got.MaxReworkRounds == nil || *got.MaxReworkRounds != 4 {
		t.Errorf("MaxReworkRounds = %v, want a pointer to 4", got.MaxReworkRounds)
	}
	if len(got.Deprecated) != 0 {
		t.Errorf("Deprecated = %+v, want empty for the current nested shape", got.Deprecated)
	}
}

func TestLoadReworkBlockInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "rework: not-a-block\n")
	if err == nil {
		t.Fatal("Load(rework: inline value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("Load error = %q, want it to mention a nested block", err.Error())
	}
}

func TestLoadReworkBlockUnrecognizedKeyErrors(t *testing.T) {
	_, err := loadContent(t, "rework:\n  frobnicate: true\n")
	if err == nil {
		t.Fatal("Load(rework.frobnicate): want error, got nil")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("Load error = %q, want it to mention frobnicate", err.Error())
	}
}

func TestLoadReworkBlockBadIntErrors(t *testing.T) {
	_, err := loadContent(t, "rework:\n  budget: notanint\n")
	if err == nil {
		t.Fatal("Load(rework.budget: notanint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "rework.budget") {
		t.Errorf("Load error = %q, want it to mention rework.budget", err.Error())
	}
}

func TestLoadReworkBlockMaxRoundsBadIntErrors(t *testing.T) {
	_, err := loadContent(t, "rework:\n  max_rounds: notanint\n")
	if err == nil {
		t.Fatal("Load(rework.max_rounds: notanint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "rework.max_rounds") {
		t.Errorf("Load error = %q, want it to mention rework.max_rounds", err.Error())
	}
}

func TestLoadReworkBlockMalformedLineErrors(t *testing.T) {
	_, err := loadContent(t, "rework:\n  this is not key value\n")
	if err == nil {
		t.Fatal("Load(malformed rework line): want error, got nil")
	}
}

// TestLoadReviewBlockParsesAllSix is review:'s happy path, mirroring
// TestLoadGateClusterUnderAwaitingReviewParsesAllSix but for the canonical
// location.
func TestLoadReviewBlockParsesAllSix(t *testing.T) {
	content := "review:\n" +
		"  gate_verify_command: \"make ci\"\n" +
		"  max_diff_lines: 800\n" +
		"  proof_required_paths:\n" +
		"    - terraform\n" +
		"  always_review_paths:\n" +
		"    - auth\n" +
		"  review_note: \"pay attention\"\n" +
		"  review_effort: high\n"
	got, err := loadContent(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.GateVerifyCommand != "make ci" {
		t.Errorf("GateVerifyCommand = %q, want make ci", got.GateVerifyCommand)
	}
	if got.MaxDiffLines == nil || *got.MaxDiffLines != 800 {
		t.Errorf("MaxDiffLines = %v, want 800", got.MaxDiffLines)
	}
	if !reflect.DeepEqual(got.ProofRequiredPaths, []string{"terraform"}) {
		t.Errorf("ProofRequiredPaths = %v, want [terraform]", got.ProofRequiredPaths)
	}
	if !reflect.DeepEqual(got.AlwaysReviewPaths, []string{"auth"}) {
		t.Errorf("AlwaysReviewPaths = %v, want [auth]", got.AlwaysReviewPaths)
	}
	if got.ReviewNote != "pay attention" {
		t.Errorf("ReviewNote = %q, want %q", got.ReviewNote, "pay attention")
	}
	if got.ReviewEffort != "high" {
		t.Errorf("ReviewEffort = %q, want high", got.ReviewEffort)
	}
	if len(got.Phases) != 0 {
		t.Errorf("Phases = %+v, want empty — review: is not a worker permission phase", got.Phases)
	}
	if len(got.Deprecated) != 0 {
		t.Errorf("Deprecated = %+v, want empty for the current nested shape", got.Deprecated)
	}
}

func TestLoadReviewBlockInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "review: not-a-block\n")
	if err == nil {
		t.Fatal("Load(review: inline value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("Load error = %q, want it to mention a nested block", err.Error())
	}
}

func TestLoadReviewBlockUnrecognizedKeyErrors(t *testing.T) {
	_, err := loadContent(t, "review:\n  frobnicate: true\n")
	if err == nil {
		t.Fatal("Load(review.frobnicate): want error, got nil")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("Load error = %q, want it to mention frobnicate", err.Error())
	}
}

// TestLoadReviewBlockAllowKeyIsUnrecognized proves review: does not silently
// accept "allow" the way listFieldFor's generic top-level dispatch would —
// review: is not a worker permission phase, so "allow" here is a typo/mistake,
// not a valid key, and must error rather than silently writing cfg.Allow.
func TestLoadReviewBlockAllowKeyIsUnrecognized(t *testing.T) {
	_, err := loadContent(t, "review:\n  allow:\n    - \"Bash(make *)\"\n")
	if err == nil {
		t.Fatal("Load(review.allow): want error, got nil")
	}
	if !strings.Contains(err.Error(), "allow") {
		t.Errorf("Load error = %q, want it to mention allow", err.Error())
	}
}

func TestLoadReviewBlockListInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "review:\n  proof_required_paths: inlineval\n")
	if err == nil {
		t.Fatal("Load(review.proof_required_paths inline value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention expects a list", err.Error())
	}
}

func TestLoadReviewBlockBadIntErrors(t *testing.T) {
	_, err := loadContent(t, "review:\n  max_diff_lines: notanint\n")
	if err == nil {
		t.Fatal("Load(review.max_diff_lines: notanint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "max_diff_lines") {
		t.Errorf("Load error = %q, want it to mention max_diff_lines", err.Error())
	}
}

func TestLoadReviewBlockMalformedLineErrors(t *testing.T) {
	_, err := loadContent(t, "review:\n  this is not key value\n")
	if err == nil {
		t.Fatal("Load(malformed review line): want error, got nil")
	}
}

func TestLoadReviewBlockBadQuotedValueErrors(t *testing.T) {
	_, err := loadContent(t, "review:\n  review_note: \"\\x\"\n")
	if err == nil {
		t.Fatal("Load(bad quoted review.review_note): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad value") {
		t.Errorf("Load error = %q, want it to mention bad value", err.Error())
	}
}

// oldShapeReworkReviewConfig is a full old-shape document: rework_budget
// floating at the top level among static repo facts, and the gate/review
// cluster nested under phases.awaiting_review — an operation's own config
// smeared onto the top level and a worker permission phase, instead of
// sitting in its own rework:/review: region.
const oldShapeReworkReviewConfig = `base_branch: "main"
rework_budget: 6

phases:
  working:
    allow:
      - "Bash(make *)"
  awaiting_review:
    gate_verify_command: "make ci"
    max_diff_lines: 800
    proof_required_paths:
      - terraform
    always_review_paths:
      - auth
    review_note: "pay attention"
    review_effort: high
`

// newShapeReworkReviewConfig is the same effective settings expressed in the
// current shape: rework: and review: as their own regions, sibling to
// phases:, phases: holding only allow/deny/skip.
const newShapeReworkReviewConfig = `base_branch: "main"

rework:
  budget: 6

review:
  gate_verify_command: "make ci"
  max_diff_lines: 800
  proof_required_paths:
    - terraform
  always_review_paths:
    - auth
  review_note: "pay attention"
  review_effort: high

phases:
  working:
    allow:
      - "Bash(make *)"
`

// TestOldShapeAndNewShapeReworkReviewConfigsResolveIdentically is this
// issue's core acceptance test: loading an old-shape document (rework_budget
// at top level, the gate/review cluster under phases.awaiting_review) and its
// new-shape equivalent (rework:/review: regions) must produce the identical
// effective Config, Deprecated aside — migrating a repo's config.yml to the
// new region shape is never a behavior change, only a naming/location one.
func TestOldShapeAndNewShapeReworkReviewConfigsResolveIdentically(t *testing.T) {
	oldCfg, err := loadContent(t, oldShapeReworkReviewConfig)
	if err != nil {
		t.Fatalf("Load(old shape): %v", err)
	}
	newCfg, err := loadContent(t, newShapeReworkReviewConfig)
	if err != nil {
		t.Fatalf("Load(new shape): %v", err)
	}

	if len(oldCfg.Deprecated) == 0 {
		t.Error("old-shape config: Deprecated is empty, want an entry recorded for rework_budget and each phases.awaiting_review.* key")
	}
	if len(newCfg.Deprecated) != 0 {
		t.Errorf("new-shape config: Deprecated = %+v, want empty — every key used its current canonical region", newCfg.Deprecated)
	}

	// Deprecated itself is expected to differ (only the old shape records
	// anything); every other field must be byte-for-byte identical, since
	// that's the actual runtime behavior every consumer (resolveMaxReworkBudget,
	// resolveMaxReworkRounds, resolveGatePolicy, resolveGateVerifyCommand,
	// resolveReviewNote, resolveReviewEffort) reads.
	oldCfg.Deprecated = nil
	newCfg.Deprecated = nil
	if !reflect.DeepEqual(oldCfg, newCfg) {
		t.Errorf("old-shape config = %+v\nnew-shape config = %+v\nwant identical effective Config (Deprecated aside)", oldCfg, newCfg)
	}
}

// TestOldShapeReworkReviewConfigRecordsExpectedDeprecations pins the exact
// Deprecated entries the old-shape document above should produce, so a
// future change to legacyFlatKeys/assignAwaitingReviewKey that silently drops
// one of these mappings is caught here, not just by the identical-resolution
// check above (which would only notice a value regression, not a missing
// warning).
func TestOldShapeReworkReviewConfigRecordsExpectedDeprecations(t *testing.T) {
	got, err := loadContent(t, oldShapeReworkReviewConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []DeprecatedKeyUse{
		{Old: "rework_budget", New: "rework.budget"},
		{Old: "phases.awaiting_review.gate_verify_command", New: "review.gate_verify_command"},
		{Old: "phases.awaiting_review.max_diff_lines", New: "review.max_diff_lines"},
		{Old: "phases.awaiting_review.proof_required_paths", New: "review.proof_required_paths"},
		{Old: "phases.awaiting_review.always_review_paths", New: "review.always_review_paths"},
		{Old: "phases.awaiting_review.review_note", New: "review.review_note"},
		{Old: "phases.awaiting_review.review_effort", New: "review.review_effort"},
	}
	if !reflect.DeepEqual(got.Deprecated, want) {
		t.Errorf("Deprecated = %+v, want %+v", got.Deprecated, want)
	}
}

// TestSaveEmitsReworkAndReviewBlocks proves Save (the encoder side) writes
// the current rework:/review: region shape, never the deprecated top-level
// rework_budget or phases.awaiting_review gate/review cluster shape,
// regardless of which shape the in-memory Config was originally loaded from.
func TestSaveEmitsReworkAndReviewBlocks(t *testing.T) {
	oldCfg, err := loadContent(t, oldShapeReworkReviewConfig)
	if err != nil {
		t.Fatalf("Load(old shape): %v", err)
	}
	raw := encodeYAML(&oldCfg)
	for _, want := range []string{
		"\nrework:\n", "  budget: 6\n",
		"\nreview:\n", "  gate_verify_command: \"make ci\"\n", "  max_diff_lines: 800\n",
		"  proof_required_paths:\n", "    - \"terraform\"\n",
		"  always_review_paths:\n", "    - \"auth\"\n",
		"  review_note: \"pay attention\"\n", "  review_effort: \"high\"\n",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("encoded config = %q, want it to contain %q", raw, want)
		}
	}
	if strings.Contains(raw, "rework_budget:") || strings.Contains(raw, "awaiting_review:") {
		t.Errorf("encoded config = %q, want no deprecated top-level rework_budget or phases.awaiting_review block", raw)
	}
}

// TestSaveEmitsReworkBlockMaxRoundsOnly covers writeReworkBlock's other
// single-field branch (MaxReworkRounds set, ReworkBudget nil) — the round
// trip above only ever exercises budget-set/max_rounds-nil.
func TestSaveEmitsReworkBlockMaxRoundsOnly(t *testing.T) {
	maxRounds := 4
	cfg := Config{MaxReworkRounds: &maxRounds}
	raw := encodeYAML(&cfg)
	if !strings.Contains(raw, "\nrework:\n  max_rounds: 4\n") {
		t.Errorf("encoded config = %q, want a rework: block with only max_rounds set", raw)
	}
	if strings.Contains(raw, "budget:") {
		t.Errorf("encoded config = %q, want no budget line when ReworkBudget is nil", raw)
	}
}
