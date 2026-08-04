// nested_test.go exercises the ship:/phases: nested grammar and per-phase
// schema enforcement added to internal/repoconfig — parseYAML's new
// grouping of .argus/config.yml keys by when they actually run
// (top-level/ship:/phases:) instead of a flat key list, and the
// backward-compatible deprecated aliases that still parse the old shapes.
package repoconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func loadContent(t *testing.T, content string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	return Load(path)
}

func TestLoadShipBlockParsesVerifyCommandAndTitlePrefixTemplate(t *testing.T) {
	got, err := loadContent(t, "ship:\n  verify_command: \"make ci\"\n  title_prefix_template: \"TICKET-{issue}: \"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ShipVerifyCommand != "make ci" {
		t.Errorf("ShipVerifyCommand = %q, want %q", got.ShipVerifyCommand, "make ci")
	}
	if got.TitlePrefixTemplate != "TICKET-{issue}: " {
		t.Errorf("TitlePrefixTemplate = %q, want %q", got.TitlePrefixTemplate, "TICKET-{issue}: ")
	}
	if len(got.Deprecated) != 0 {
		t.Errorf("Deprecated = %+v, want empty for the current nested shape", got.Deprecated)
	}
}

func TestLoadShipBlockInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "ship: not-a-block\n")
	if err == nil {
		t.Fatal("Load(ship: inline value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("Load error = %q, want it to mention a nested block", err.Error())
	}
}

func TestLoadShipBlockUnrecognizedKeyErrors(t *testing.T) {
	_, err := loadContent(t, "ship:\n  frobnicate: true\n")
	if err == nil {
		t.Fatal("Load(ship.frobnicate): want error, got nil")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("Load error = %q, want it to mention frobnicate", err.Error())
	}
}

func TestLoadShipBlockMalformedLineErrors(t *testing.T) {
	_, err := loadContent(t, "ship:\n  this is not key value\n")
	if err == nil {
		t.Fatal("Load(malformed ship line): want error, got nil")
	}
}

func TestLoadShipBlockBadQuotedValueErrors(t *testing.T) {
	_, err := loadContent(t, "ship:\n  verify_command: \"\\x\"\n")
	if err == nil {
		t.Fatal("Load(bad quoted ship value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad value") {
		t.Errorf("Load error = %q, want it to mention bad value", err.Error())
	}
}

func TestLoadPhasesBlockInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases: not-a-block\n")
	if err == nil {
		t.Fatal("Load(phases: inline value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("Load error = %q, want it to mention a nested block", err.Error())
	}
}

func TestLoadPhasesBlockUnrecognizedPhaseErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  plannning:\n    deny:\n      - foo\n")
	if err == nil {
		t.Fatal("Load(phases.plannning): want error, got nil")
	}
	if !strings.Contains(err.Error(), "plannning") {
		t.Errorf("Load error = %q, want it to mention plannning", err.Error())
	}
}

func TestLoadPhasesBlockMalformedLineErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  this is not key value\n")
	if err == nil {
		t.Fatal("Load(malformed phases line): want error, got nil")
	}
}

func TestLoadPhasesBlockPhaseInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning: true\n")
	if err == nil {
		t.Fatal("Load(phases.planning: true): want error, got nil")
	}
	if !strings.Contains(err.Error(), "nested block") {
		t.Errorf("Load error = %q, want it to mention a nested block", err.Error())
	}
}

func TestLoadPhaseSubBlockAllowAndDenyParse(t *testing.T) {
	got, err := loadContent(t, "phases:\n  working:\n    allow:\n      - \"Bash(make *)\"\n    deny:\n      - \"docker push\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := protocol.PhaseConfig{
		protocol.PhaseWorking: {Allow: []string{"Bash(make *)"}, Deny: []string{"docker push"}},
	}
	if !reflect.DeepEqual(got.Phases, want) {
		t.Errorf("Phases = %+v, want %+v", got.Phases, want)
	}
}

func TestLoadPhaseSubBlockSkipParses(t *testing.T) {
	got, err := loadContent(t, "phases:\n  planning:\n    skip: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := protocol.PhaseConfig{protocol.PhasePlanning: {Skip: true}}
	if !reflect.DeepEqual(got.Phases, want) {
		t.Errorf("Phases = %+v, want %+v", got.Phases, want)
	}
}

func TestLoadPhaseSubBlockSkipBadBoolErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning:\n    skip: notabool\n")
	if err == nil {
		t.Fatal("Load(phases.planning.skip: notabool): want error, got nil")
	}
	if !strings.Contains(err.Error(), "phases.planning.skip") {
		t.Errorf("Load error = %q, want it to mention phases.planning.skip", err.Error())
	}
}

func TestLoadPhaseSubBlockDenyInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning:\n    deny: inlineval\n")
	if err == nil {
		t.Fatal("Load(phases.planning.deny: inlineval): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention expects a list", err.Error())
	}
}

func TestLoadPhaseSubBlockAllowInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning:\n    allow: inlineval\n")
	if err == nil {
		t.Fatal("Load(phases.planning.allow: inlineval): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention expects a list", err.Error())
	}
}

func TestLoadPhaseSubBlockUnrecognizedKeyErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning:\n    frobnicate: true\n")
	if err == nil {
		t.Fatal("Load(phases.planning.frobnicate): want error, got nil")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("Load error = %q, want it to mention frobnicate", err.Error())
	}
}

func TestLoadPhaseSubBlockMalformedLineErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  planning:\n    this is not key value\n")
	if err == nil {
		t.Fatal("Load(malformed phase sub-block line): want error, got nil")
	}
}

// TestLoadGateClusterUnderWrongPhaseErrorsNamingPhase pins this issue's core
// enforcement requirement: a gate/review key placed under any phase other
// than awaiting_review is a hard error naming the phase it belongs to,
// instead of silently taking effect somewhere it can never fire from.
func TestLoadGateClusterUnderWrongPhaseErrorsNamingPhase(t *testing.T) {
	_, err := loadContent(t, "phases:\n  working:\n    gate_verify_command: \"make ci\"\n")
	if err == nil {
		t.Fatal("Load(phases.working.gate_verify_command): want error, got nil")
	}
	if !strings.Contains(err.Error(), "phases.awaiting_review") || !strings.Contains(err.Error(), "phases.working") {
		t.Errorf("Load error = %q, want it to name both phases.awaiting_review and phases.working", err.Error())
	}
}

func TestLoadGateClusterUnderAwaitingReviewParsesAllSix(t *testing.T) {
	content := "phases:\n" +
		"  awaiting_review:\n" +
		"    gate_verify_command: \"make ci\"\n" +
		"    max_diff_lines: 800\n" +
		"    proof_required_paths:\n" +
		"      - terraform\n" +
		"    always_review_paths:\n" +
		"      - auth\n" +
		"    review_note: \"pay attention\"\n" +
		"    review_effort: high\n"
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
		t.Errorf("Phases = %+v, want empty — the gate cluster is plain Config fields, not phase policy", got.Phases)
	}
}

func TestLoadGateClusterListInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  awaiting_review:\n    proof_required_paths: inlineval\n")
	if err == nil {
		t.Fatal("Load(proof_required_paths inline value under awaiting_review): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention expects a list", err.Error())
	}
}

func TestLoadGateClusterBadQuotedValueErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  awaiting_review:\n    review_note: \"\\x\"\n")
	if err == nil {
		t.Fatal("Load(bad quoted review_note under awaiting_review): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad value") {
		t.Errorf("Load error = %q, want it to mention bad value", err.Error())
	}
}

func TestLoadGateClusterBadIntErrors(t *testing.T) {
	_, err := loadContent(t, "phases:\n  awaiting_review:\n    max_diff_lines: notanint\n")
	if err == nil {
		t.Fatal("Load(max_diff_lines: notanint under awaiting_review): want error, got nil")
	}
	if !strings.Contains(err.Error(), "max_diff_lines") {
		t.Errorf("Load error = %q, want it to mention max_diff_lines", err.Error())
	}
}

func TestLoadUnexpectedIndentationErrors(t *testing.T) {
	_, err := loadContent(t, "  base_branch: \"main\"\n")
	if err == nil {
		t.Fatal("Load(unexpectedly indented top-level line): want error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected indentation") {
		t.Errorf("Load error = %q, want it to mention unexpected indentation", err.Error())
	}
}

func TestLoadTopLevelListInlineValueErrors(t *testing.T) {
	_, err := loadContent(t, "allow: inlineval\n")
	if err == nil {
		t.Fatal("Load(allow: inlineval): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention expects a list", err.Error())
	}
}

// TestLoadDeprecatedDottedPhaseKeysStillParseAndRecordNewLocation covers the
// deprecated dotted phase.<name>.<subkey> form end to end: it must still
// assign the same field the nested phases.<name>.<subkey> form would, and
// record a Deprecated entry pointing at that nested location.
func TestLoadDeprecatedDottedPhaseKeysStillParseAndRecordNewLocation(t *testing.T) {
	content := "phase.planning.skip: true\nphase.working.deny:\n  - \"docker push\"\n"
	got, err := loadContent(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := protocol.PhaseConfig{
		protocol.PhasePlanning: {Skip: true},
		protocol.PhaseWorking:  {Deny: []string{"docker push"}},
	}
	if !reflect.DeepEqual(got.Phases, want) {
		t.Errorf("Phases = %+v, want %+v", got.Phases, want)
	}
	wantDeprecated := []DeprecatedKeyUse{
		{Old: "phase.planning.skip", New: "phases.planning.skip"},
		{Old: "phase.working.deny", New: "phases.working.deny"},
	}
	if !reflect.DeepEqual(got.Deprecated, wantDeprecated) {
		t.Errorf("Deprecated = %+v, want %+v", got.Deprecated, wantDeprecated)
	}
}

// TestLoadDeprecatedFlatReviewPolicyKeysStillParse covers every flat
// top-level review-policy key that moved under phases.awaiting_review: each
// must still assign its Config field and record a Deprecated entry pointing
// at its new nested location.
func TestLoadDeprecatedFlatReviewPolicyKeysStillParse(t *testing.T) {
	content := "max_diff_lines: 400\n" +
		"proof_required_paths:\n  - terraform\n" +
		"always_review_paths:\n  - auth\n" +
		"review_note: \"pay attention\"\n" +
		"review_effort: high\n" +
		"gate_verify_command: \"make ci\"\n" +
		"ship_verify_command: \"make lint\"\n" +
		"title_prefix_template: \"TICKET-{issue}: \"\n"
	got, err := loadContent(t, content)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxDiffLines == nil || *got.MaxDiffLines != 400 {
		t.Errorf("MaxDiffLines = %v, want 400", got.MaxDiffLines)
	}
	if !reflect.DeepEqual(got.ProofRequiredPaths, []string{"terraform"}) {
		t.Errorf("ProofRequiredPaths = %v, want [terraform]", got.ProofRequiredPaths)
	}
	if !reflect.DeepEqual(got.AlwaysReviewPaths, []string{"auth"}) {
		t.Errorf("AlwaysReviewPaths = %v, want [auth]", got.AlwaysReviewPaths)
	}
	if got.ReviewNote != "pay attention" || got.ReviewEffort != "high" {
		t.Errorf("ReviewNote/ReviewEffort = %q/%q, want %q/%q", got.ReviewNote, got.ReviewEffort, "pay attention", "high")
	}
	if got.GateVerifyCommand != "make ci" || got.ShipVerifyCommand != "make lint" {
		t.Errorf("GateVerifyCommand/ShipVerifyCommand = %q/%q, want %q/%q", got.GateVerifyCommand, got.ShipVerifyCommand, "make ci", "make lint")
	}
	if got.TitlePrefixTemplate != "TICKET-{issue}: " {
		t.Errorf("TitlePrefixTemplate = %q, want %q", got.TitlePrefixTemplate, "TICKET-{issue}: ")
	}
	want := []DeprecatedKeyUse{
		{Old: "max_diff_lines", New: "phases.awaiting_review.max_diff_lines"},
		{Old: "proof_required_paths", New: "phases.awaiting_review.proof_required_paths"},
		{Old: "always_review_paths", New: "phases.awaiting_review.always_review_paths"},
		{Old: "review_note", New: "phases.awaiting_review.review_note"},
		{Old: "review_effort", New: "phases.awaiting_review.review_effort"},
		{Old: "gate_verify_command", New: "phases.awaiting_review.gate_verify_command"},
		{Old: "ship_verify_command", New: "ship.verify_command"},
		{Old: "title_prefix_template", New: "ship.title_prefix_template"},
	}
	if !reflect.DeepEqual(got.Deprecated, want) {
		t.Errorf("Deprecated = %+v, want %+v", got.Deprecated, want)
	}
}

func TestSaveEmitsNestedShipAndPhasesBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	maxDiffLines := 400
	cfg := Config{
		ShipVerifyCommand: "make ci",
		GateVerifyCommand: "make lint",
		MaxDiffLines:      &maxDiffLines,
		Phases: protocol.PhaseConfig{
			protocol.PhaseWorking: {Allow: []string{"Bash(make *)"}},
		},
	}
	if err := Save(path, &cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	raw := string(rawBytes)
	for _, want := range []string{"\nship:\n", "  verify_command: \"make ci\"\n", "\nphases:\n", "  working:\n", "    allow:\n", "  awaiting_review:\n", "    gate_verify_command: \"make lint\"\n", "    max_diff_lines: 400\n"} {
		if !strings.Contains(raw, want) {
			t.Errorf("saved config = %q, want it to contain %q", raw, want)
		}
	}
}

func TestSaveLoadRoundTripPhaseAllow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{
		Phases: protocol.PhaseConfig{
			protocol.PhaseWorking: {Allow: []string{"Bash(make *)"}},
		},
	}
	if err := Save(path, &want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
