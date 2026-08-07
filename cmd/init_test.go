package cmd

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/permission"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
)

// initPromptExemptFields lists Config fields runInit deliberately does not
// prompt for, with the reason it's fine to stay hand-edit-only (see
// schemas/config.schema.json). Phases is no longer exempt: promptPhaseAllow
// walks every protocol.ConfigurablePhases value's own allow list — only its
// Deny/Skip subkeys stay hand-edit-only.
var initPromptExemptFields = map[string]string{
	"ExperimentalWorkerSandbox": "experimental, opt-in via --experimental-sandbox or a hand-edited config key, not part of the interactive setup wizard",
	"SandboxAllowWrite":         "only meaningful once ExperimentalWorkerSandbox is on; hand-edit-only like the phases.*.deny/skip subkeys",
}

const wantConfigFieldCount = 25 // repoconfig.Config's current field count

func writeMarker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing marker %s: %v", name, err)
	}
}

// TestDetectRepoConfigMakefile pins the fix for a real double-write bug: a
// toolchain guess must land only in cfg.Phases (working/self_test), never in
// the phase-independent cfg.Allow — ResolvedAllowForPhase unions cfg.Allow
// into every phase, so setting both silently defeated the working/self_test
// -only restriction phasesForToolchainAllow exists to enforce.
func TestDetectRepoConfigMakefile(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cfg := detectRepoConfig(context.Background(), dir)
	if cfg.Allow != nil {
		t.Errorf("Allow = %v, want nil — the toolchain suggestion must only populate cfg.Phases", cfg.Allow)
	}
	if allow := cfg.Phases[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
		t.Errorf("phases.working.allow = %v, want [\"Bash(make *)\"]", allow)
	}
	if !strings.Contains(cfg.BriefNote, "make ci") {
		t.Errorf("BriefNote = %q, want it to mention make ci", cfg.BriefNote)
	}
}

func TestDetectRepoConfigGoMod(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod")
	cfg := detectRepoConfig(context.Background(), dir)
	if cfg.Allow != nil {
		t.Errorf("Allow = %v, want nil — the toolchain suggestion must only populate cfg.Phases", cfg.Allow)
	}
	if allow := cfg.Phases[protocol.PhaseWorking].Allow; len(allow) == 0 || allow[0] != "Bash(go build *)" {
		t.Errorf("phases.working.allow = %v, want it to start with go build", allow)
	}
}

func TestDetectRepoConfigTaskfilePrecedesMakefile(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Taskfile.yml")
	writeMarker(t, dir, "Makefile")
	cfg := detectRepoConfig(context.Background(), dir)
	if cfg.Allow != nil {
		t.Errorf("Allow = %v, want nil — the toolchain suggestion must only populate cfg.Phases", cfg.Allow)
	}
	if allow := cfg.Phases[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(task *)" {
		t.Errorf("phases.working.allow = %v, want Taskfile.yml to win over Makefile", allow)
	}
}

func TestDetectRepoConfigNoMarkersSuggestsNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := detectRepoConfig(context.Background(), dir)
	if cfg.Allow != nil || cfg.BriefNote != "" {
		t.Errorf("cfg = %+v, want an empty suggestion with no marker files", cfg)
	}
}

func TestRunInitYesWritesSuggestionWithoutPrompting(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("")) // --yes must never read from this

	if err := runInit(cmd, &initArgs{repo: dir, yes: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Allow != nil {
		t.Errorf("written Allow = %v, want nil — the toolchain suggestion only ever populates phases.working/self_test.allow", got.Allow)
	}
	if allow := got.Phases[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
		t.Errorf("written phases.working.allow = %v, want the detected Makefile suggestion", allow)
	}
}

func TestRunInitInteractivePromptsAcceptDefaults(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// Eight bare-Enter answers: base_branch, allow, the 5 phases.*.allow
	// prompts, and brief_note all keep the detected suggestion.
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", 8)))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Allow != nil {
		t.Errorf("written Allow = %v, want nil — the toolchain suggestion only ever populates phases.working/self_test.allow", got.Allow)
	}
	if allow := got.Phases[protocol.PhaseWorking].Allow; len(allow) == 0 || allow[0] != "Bash(go build *)" {
		t.Errorf("phases.working.allow = %v, want the detected go.mod suggestion preserved", allow)
	}
}

func TestRunInitInteractivePromptsAcceptEdits(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// Override base_branch and allow, keep the 5 phases.*.allow prompts and
	// brief_note (bare Enter).
	cmd.SetIn(strings.NewReader("develop\nBash(task *), Bash(pnpm *)\n" + strings.Repeat("\n", 6)))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want the edited value %q", got.BaseBranch, "develop")
	}
	if len(got.Allow) != 2 || got.Allow[0] != "Bash(task *)" || got.Allow[1] != "Bash(pnpm *)" {
		t.Errorf("Allow = %v, want the edited list", got.Allow)
	}
	if !strings.Contains(got.BriefNote, "make ci") {
		t.Errorf("BriefNote = %q, want the detected suggestion preserved", got.BriefNote)
	}
}

// TestRunInitInteractivePromptsCoreFields exercises comma-list/int-parsing
// edge cases for a representative subset of runInit's prompts (base_branch,
// allow, brief_note, max_diff_lines, proof_required_paths,
// always_review_paths, worker_placement) — it does not assert completeness
// across every Config field; that guarantee is TestRunInitPromptsSetEveryConfigField's
// job.
func TestRunInitInteractivePromptsCoreFields(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// base_branch, allow, 5 phases.*.allow prompts (keep detected),
	// brief_note (keep detected), workspace_label_template (keep default),
	// max_diff_lines, rework.budget (keep default), rework.max_rounds (keep
	// default), proof_required_paths, always_review_paths, worker_placement —
	// a representative subset of init's prompts, each edited to a non-default
	// value.
	cmd.SetIn(strings.NewReader("main\nBash(make *)\n" + strings.Repeat("\n", 7) + "250\n\n\nterraform, deploy\nauth\ntab\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxDiffLines == nil || *got.MaxDiffLines != 250 {
		t.Errorf("MaxDiffLines = %v, want a pointer to 250", got.MaxDiffLines)
	}
	if len(got.ProofRequiredPaths) != 2 || got.ProofRequiredPaths[0] != "terraform" || got.ProofRequiredPaths[1] != "deploy" {
		t.Errorf("ProofRequiredPaths = %v, want [terraform deploy]", got.ProofRequiredPaths)
	}
	if len(got.AlwaysReviewPaths) != 1 || got.AlwaysReviewPaths[0] != "auth" {
		t.Errorf("AlwaysReviewPaths = %v, want [auth]", got.AlwaysReviewPaths)
	}
	if got.WorkerPlacement != "tab" {
		t.Errorf("WorkerPlacement = %q, want tab", got.WorkerPlacement)
	}
}

func TestRunInitInteractiveMaxDiffLinesNonNumericKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// A non-numeric max_diff_lines answer must not abort init; it keeps the
	// (unset) default and init still writes the rest. base_branch, allow, 5
	// phases.*.allow prompts, brief_note, workspace_label_template all bare
	// Enter, then max_diff_lines.
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", 9) + "notanumber\n\n\n\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxDiffLines != nil {
		t.Errorf("MaxDiffLines = %v, want nil (unparseable answer keeps the unset default)", got.MaxDiffLines)
	}
	if !strings.Contains(buf.String(), "not a number") {
		t.Errorf("output = %q, want a note that the answer was not a number", buf.String())
	}
}

// TestRunInitForgeFlagWritesConfigWithoutPrompting pins issue #256's --forge
// flag path: with --yes, an explicit --forge is written verbatim with no
// interactive prompt.
func TestRunInitForgeFlagWritesConfigWithoutPrompting(t *testing.T) {
	dir := t.TempDir()

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("")) // --yes must never read from this

	if err := runInit(cmd, &initArgs{repo: dir, yes: true, forgeKind: "gitea"}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Forge != "gitea" {
		t.Errorf("Forge = %q, want %q", got.Forge, "gitea")
	}
}

// TestRunInitForgeFlagRejectsUnknownKind pins the other half: a typo in
// --forge fails fast instead of silently writing a bogus config value.
func TestRunInitForgeFlagRejectsUnknownKind(t *testing.T) {
	dir := t.TempDir()

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	if err := runInit(cmd, &initArgs{repo: dir, yes: true, forgeKind: "bogus"}); err == nil {
		t.Fatal("want an error for an unrecognized --forge value")
	}
	if _, err := os.Stat(repoconfig.Path(dir)); !os.IsNotExist(err) {
		t.Errorf("a rejected --forge value should write nothing, stat err: %v", err)
	}
}

// TestRunInitInteractivePromptWritesForge exercises the forge prompt itself
// (no --forge flag), confirming an interactive answer reaches the written
// config.
func TestRunInitInteractivePromptWritesForge(t *testing.T) {
	dir := t.TempDir()

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// base_branch, allow, the 5 phases.*.allow prompts, brief_note,
	// workspace_label_template, max_diff_lines, rework.budget,
	// rework.max_rounds, proof_required_paths, always_review_paths,
	// worker_placement, ship_verify_command, gate_verify_command,
	// worktree_bootstrap_command, review_effort, launcher, worktree_dir,
	// owner_stale_after, title_prefix_template, review_note (all bare Enter,
	// 24 prompts), then forge.
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", 24) + "gitlab\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Forge != "gitlab" {
		t.Errorf("Forge = %q, want %q", got.Forge, "gitlab")
	}
}

func TestRunInitRefusesOverwriteWithoutConfirmation(t *testing.T) {
	dir := t.TempDir()
	if err := repoconfig.Save(repoconfig.Path(dir), &repoconfig.Config{BaseBranch: "existing"}); err != nil {
		t.Fatalf("seeding existing config: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("n\n")) // decline the overwrite prompt

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if !strings.Contains(buf.String(), "Canceled") {
		t.Errorf("output = %q, want a cancellation message", buf.String())
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "existing" {
		t.Errorf("BaseBranch = %q, want the pre-existing config left untouched", got.BaseBranch)
	}
}

func TestRunInitYesOverwritesExistingConfigWithoutAsking(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	if err := repoconfig.Save(repoconfig.Path(dir), &repoconfig.Config{BaseBranch: "stale"}); err != nil {
		t.Fatalf("seeding existing config: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))

	if err := runInit(cmd, &initArgs{repo: dir, yes: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch == "stale" {
		t.Error("--yes should overwrite an existing config without asking")
	}
	if got.Allow != nil {
		t.Errorf("Allow = %v, want nil — the toolchain suggestion only ever populates phases.working/self_test.allow", got.Allow)
	}
	if allow := got.Phases[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
		t.Errorf("phases.working.allow = %v, want the fresh Makefile suggestion", allow)
	}
}

func TestRunInitPromptsSetEveryConfigField(t *testing.T) {
	typ := reflect.TypeFor[repoconfig.Config]()
	if typ.NumField() != wantConfigFieldCount {
		t.Fatalf("repoconfig.Config has %d fields, want %d — a field was added or removed: update wantConfigFieldCount and either add a runInit prompt for it or add it to initPromptExemptFields with a documented reason", typ.NumField(), wantConfigFieldCount)
	}

	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	// One recognizable answer per prompt, in runInit's own order: base_branch,
	// allow, the 5 phases.*.allow prompts (planning/working/self_test/
	// awaiting_review/blocked), brief_note, workspace_label_template,
	// max_diff_lines, rework.budget, rework.max_rounds, proof_required_paths,
	// always_review_paths, worker_placement, ship_verify_command,
	// gate_verify_command, worktree_bootstrap_command, review_effort,
	// launcher, worktree_dir, owner_stale_after, title_prefix_template,
	// review_note, forge, status_page.
	answers := []string{
		"develop", "Bash(task *)",
		"Bash(planning-tool *)", "Bash(working-tool *)", "Bash(self-test-tool *)", "Bash(review-tool *)", "Bash(blocked-tool *)",
		"custom brief", "{project}/{issue}", "250", "6", "4", "terraform", "auth",
		"tab", "make lint", "make ci", "cp ../.env .env", "high",
		"codex --full-auto", "..", "45m", "TICKET-{issue}: ", "pay attention", "gitlab",
		"https://status.example.com",
	}
	cmd.SetIn(strings.NewReader(strings.Join(answers, "\n") + "\n"))
	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := reflect.ValueOf(got)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if name == "Deprecated" {
			continue // populated only when an old-named key is read back, not by writing a fresh config
		}
		if reason, ok := initPromptExemptFields[name]; ok {
			t.Logf("field %q exempt from prompting: %s", name, reason)
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("Config field %q is still zero after every prompt was answered — runInit has no prompt for it", name)
		}
	}
}

// TestRunInitWritesNestedShipAndPhasesShape confirms runInit's Save call
// emits the current nested ship:/rework:/review:/phases: regions end to end —
// not just that repoconfig.Load can read the fields back
// (TestRunInitPromptsSetEveryConfigField already covers that), but that the
// raw file on disk uses the new shape (schemas/config.schema.json) rather
// than the deprecated flat keys, or the deprecated phases.awaiting_review
// gate/review cluster location, runInit used to write before the region
// restructure.
func TestRunInitWritesNestedShipAndPhasesShape(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	answers := []string{
		"develop", "Bash(task *)",
		"", "", "", "", "",
		"custom brief", "{project}/{issue}", "250", "6", "4", "terraform", "auth",
		"tab", "make lint", "make ci", "cp ../.env .env", "high",
		"codex --full-auto", "..", "45m", "TICKET-{issue}: ", "pay attention", "gitlab",
		"https://status.example.com",
	}
	cmd.SetIn(strings.NewReader(strings.Join(answers, "\n") + "\n"))
	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	raw, err := os.ReadFile(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"workspace_label_template: ",
		"\nship:\n", "  verify_command: ", "  title_prefix_template: ",
		"\nrework:\n", "  budget: 6\n", "  max_rounds: 4\n",
		"\nreview:\n", "  gate_verify_command: ", "  max_diff_lines: ", "  review_note: ", "  review_effort: ",
		"\nphases:\n", "  working:\n", "    allow:\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("saved config = %q, want it to contain %q", content, want)
		}
	}
	if strings.Contains(content, "awaiting_review:") {
		t.Errorf("saved config = %q, want no phases.awaiting_review block — the gate/review cluster now lives under review:", content)
	}
	for line := range strings.SplitSeq(content, "\n") {
		for _, old := range []string{"ship_verify_command:", "gate_verify_command:", "max_diff_lines:", "proof_required_paths:", "always_review_paths:", "review_note:", "review_effort:", "title_prefix_template:", "rework_budget:"} {
			if strings.HasPrefix(line, old) {
				t.Errorf("saved config line %q starts with deprecated flat key %q, want only the nested shape", line, old)
			}
		}
	}
}

func TestRunInitPrintsNextSteps(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))

	if err := runInit(cmd, &initArgs{repo: dir, yes: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"Next steps", "config check --write", "supervise <task> --dry-run"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to mention %q", out, want)
		}
	}
}

// TestRunInitYesSkipsConfigCheckOffer pins that the mutating inline offer never
// fires on the non-interactive --yes path: settings.json stays untouched.
func TestRunInitYesSkipsConfigCheckOffer(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))

	if err := runInit(cmd, &initArgs{repo: dir, yes: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(permission.SettingsPath(dir)); !os.IsNotExist(err) {
		t.Errorf("--yes should not touch .claude/settings.json, stat err: %v", err)
	}
}

// initPromptAnswers is one bare-Enter answer per interactive prompt (accept the
// default for all 26 — including the 5 phases.*.allow prompts), so a test
// can append its own answer for the trailing config-check confirm.
const initPromptAnswers = 26

func TestRunInitInlineConfigCheckAcceptedWritesSettings(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", initPromptAnswers) + "y\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	covered, _, err := permission.Check(permission.SettingsPath(dir))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !covered {
		t.Error("accepting the inline offer should allowlist argus in .claude/settings.json")
	}
}

func TestRunInitInlineConfigCheckDeclinedLeavesSettings(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", initPromptAnswers) + "n\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(permission.SettingsPath(dir)); !os.IsNotExist(err) {
		t.Errorf("declining the inline offer should not touch .claude/settings.json, stat err: %v", err)
	}
}

// TestOfferConfigCheckYesSuccess drives offerConfigCheck's confirm-yes path
// through a successful runConfigCheck: with no pre-existing settings.json the
// write succeeds and argus ends up allowlisted.
func TestOfferConfigCheckYesSuccess(t *testing.T) {
	dir := t.TempDir()

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	offerConfigCheck(cmd, bufio.NewReader(strings.NewReader("y\n")), &buf, dir)

	covered, _, err := permission.Check(permission.SettingsPath(dir))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !covered {
		t.Error("a yes confirmation with a writable repo should allowlist argus")
	}
}

// TestOfferConfigCheckYesFailureWarns drives the confirm-yes error branch: a
// malformed settings.json makes runConfigCheck fail, and offerConfigCheck must
// warn rather than let init exit non-zero over the optional follow-up.
func TestOfferConfigCheckYesFailureWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	// Invalid JSON: permission.Check fails to parse it, so runConfigCheck
	// returns an error instead of writing.
	if err := os.WriteFile(permission.SettingsPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seeding malformed settings.json: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	offerConfigCheck(cmd, bufio.NewReader(strings.NewReader("y\n")), &buf, dir)

	if !strings.Contains(buf.String(), "config check:") {
		t.Errorf("output = %q, want a 'config check:' warning when runConfigCheck fails", buf.String())
	}
}

func TestRunInitRelativeRepoResolvesAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	t.Chdir(dir)

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))

	if err := runInit(cmd, &initArgs{repo: ".", yes: true}); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".argus", "config.yml")); err != nil {
		t.Errorf(".argus/config.yml not written under the resolved repo root: %v", err)
	}
}

func TestDetectRepoConfigSuggestsPerPhaseAllow(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cfg := detectRepoConfig(context.Background(), dir)

	for _, p := range []protocol.Phase{protocol.PhaseWorking, protocol.PhaseSelfTest} {
		allow := cfg.Phases[p].Allow
		if len(allow) != 1 || allow[0] != "Bash(make *)" {
			t.Errorf("phases.%s.allow = %v, want [\"Bash(make *)\"]", p, allow)
		}
	}
	for _, p := range []protocol.Phase{protocol.PhasePlanning, protocol.PhaseAwaitingReview, protocol.PhaseBlocked} {
		if allow := cfg.Phases[p].Allow; len(allow) != 0 {
			t.Errorf("phases.%s.allow = %v, want none beyond the structural floor", p, allow)
		}
	}
}

// TestDetectRepoConfigResolvedAllowIsPhaseScoped is the regression test for
// the double-write bug a review round caught: TestDetectRepoConfigSuggestsPerPhaseAllow
// above only inspects cfg.Phases in isolation, which is exactly why it
// missed that cfg.Allow (the phase-independent top-level list, unioned into
// EVERY phase by supervisor.ResolvedAllowForPhase) was *also* being set to
// the same toolchain command — silently granting it in planning/
// awaiting_review/blocked too, and making phasesForToolchainAllow's own
// working/self_test-only restriction a complete no-op. This test asserts
// against the actually-resolved set instead, for both toolchain guesses
// runInit's default (non-interactive, --yes) path would produce.
func TestDetectRepoConfigResolvedAllowIsPhaseScoped(t *testing.T) {
	cases := []struct {
		marker string
		want   string
	}{
		{"Makefile", "Bash(make *)"},
		{"go.mod", "Bash(go build *)"},
	}
	for _, c := range cases {
		t.Run(c.marker, func(t *testing.T) {
			dir := t.TempDir()
			writeMarker(t, dir, c.marker)
			cfg := detectRepoConfig(context.Background(), dir)

			for _, p := range []protocol.Phase{protocol.PhaseWorking, protocol.PhaseSelfTest} {
				resolved := supervisor.ResolvedAllowForPhase(p, "/tmp/wt", cfg.Phases, cfg.Allow, nil)
				if !slices.Contains(resolved, c.want) {
					t.Errorf("resolved allow for phase %q = %v, want it to contain %q", p, resolved, c.want)
				}
			}
			for _, p := range []protocol.Phase{protocol.PhasePlanning, protocol.PhaseAwaitingReview, protocol.PhaseBlocked} {
				resolved := supervisor.ResolvedAllowForPhase(p, "/tmp/wt", cfg.Phases, cfg.Allow, nil)
				if slices.Contains(resolved, c.want) {
					t.Errorf("resolved allow for phase %q = %v, want it to NOT contain %q (structural-floor-only)", p, resolved, c.want)
				}
			}
		})
	}
}

func TestDetectRepoConfigNoMarkersSuggestsNoPhaseAllow(t *testing.T) {
	dir := t.TempDir()
	cfg := detectRepoConfig(context.Background(), dir)
	if cfg.Phases != nil {
		t.Errorf("Phases = %+v, want nil with no marker files", cfg.Phases)
	}
}

// TestRunInitRefreshRematerializesAllowPreservingOtherKeys is the --refresh
// acceptance test: a repo whose config.yml predates an improved toolchain
// default picks up the new phases.*.allow suggestion, while every other
// hand-set key — base_branch, review_note, a hand-authored deny on a phase
// the toolchain guess never touches, and even a hand-authored top-level
// allow (never toolchain-derived, so never re-materialized) — survives
// untouched.
func TestRunInitRefreshRematerializesAllowPreservingOtherKeys(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")

	existing := &repoconfig.Config{
		BaseBranch: "develop",
		Allow:      []string{"Bash(old-tool *)"},
		ReviewNote: "hand-authored, must survive refresh",
		Phases: protocol.PhaseConfig{
			protocol.PhaseWorking:  {Allow: []string{"Bash(old-tool *)"}},
			protocol.PhasePlanning: {Deny: []string{"npm publish"}},
		},
	}
	if err := repoconfig.Save(repoconfig.Path(dir), existing); err != nil {
		t.Fatalf("seeding existing config: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("")) // --yes must never read from this

	if err := runInit(cmd, &initArgs{repo: dir, yes: true, refresh: true}); err != nil {
		t.Fatalf("runInit --refresh: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want the preserved existing value %q", got.BaseBranch, "develop")
	}
	if got.ReviewNote != "hand-authored, must survive refresh" {
		t.Errorf("ReviewNote = %q, want it preserved untouched", got.ReviewNote)
	}
	// The toolchain suggestion only ever re-materializes phases.*.allow — a
	// hand-authored top-level allow is not toolchain-derived, so --refresh
	// must leave it exactly as loaded, not overwrite or clear it.
	if len(got.Allow) != 1 || got.Allow[0] != "Bash(old-tool *)" {
		t.Errorf("Allow = %v, want the hand-authored top-level entry preserved untouched", got.Allow)
	}
	if allow := got.Phases[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
		t.Errorf("phases.working.allow = %v, want it re-materialized to the current Makefile suggestion", allow)
	}
	if deny := got.Phases[protocol.PhasePlanning].Deny; len(deny) != 1 || deny[0] != "npm publish" {
		t.Errorf("phases.planning.deny = %v, want the hand-authored entry preserved untouched", deny)
	}
}

// TestRunInitRefreshPropagatesLoadError confirms --refresh surfaces a
// malformed existing config.yml as a real error instead of silently
// discarding it — refresh only ever re-materializes the allow suggestion, it
// must never paper over an unrelated parse failure in the rest of the file.
func TestRunInitRefreshPropagatesLoadError(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	if err := os.MkdirAll(filepath.Dir(repoconfig.Path(dir)), 0o755); err != nil {
		t.Fatalf("mkdir .argus: %v", err)
	}
	if err := os.WriteFile(repoconfig.Path(dir), []byte("not: [valid\nallow"), 0o600); err != nil {
		t.Fatalf("seeding malformed config: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))

	if err := runInit(cmd, &initArgs{repo: dir, yes: true, refresh: true}); err == nil {
		t.Fatal("want an error when --refresh's underlying config.yml fails to parse")
	}
}

// TestRunInitRefreshSkipsOverwriteConfirmation confirms --refresh never asks
// "already exists — overwrite?" — loading and updating in place is the whole
// point of the flag, not a destructive surprise gated behind a prompt.
func TestRunInitRefreshSkipsOverwriteConfirmation(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	if err := repoconfig.Save(repoconfig.Path(dir), &repoconfig.Config{BaseBranch: "existing"}); err != nil {
		t.Fatalf("seeding existing config: %v", err)
	}

	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("")) // no answer available for a confirm prompt

	if err := runInit(cmd, &initArgs{repo: dir, yes: true, refresh: true}); err != nil {
		t.Fatalf("runInit --refresh: %v", err)
	}
	if strings.Contains(buf.String(), "overwrite?") {
		t.Errorf("output = %q, --refresh should never prompt to overwrite", buf.String())
	}
	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "existing" {
		t.Errorf("BaseBranch = %q, want the preserved existing value", got.BaseBranch)
	}
}

func TestPhasesForToolchainAllow(t *testing.T) {
	if got := phasesForToolchainAllow(nil); got != nil {
		t.Errorf("phasesForToolchainAllow(nil) = %+v, want nil", got)
	}
	got := phasesForToolchainAllow([]string{"Bash(make *)"})
	for _, p := range []protocol.Phase{protocol.PhaseWorking, protocol.PhaseSelfTest} {
		if allow := got[p].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
			t.Errorf("phases.%s.allow = %v, want [\"Bash(make *)\"]", p, allow)
		}
	}
}

func TestMergeAllowIntoPhases(t *testing.T) {
	existing := protocol.PhaseConfig{
		protocol.PhaseWorking:  {Allow: []string{"Bash(old *)"}, Deny: []string{"npm publish"}},
		protocol.PhasePlanning: {Skip: true},
	}
	suggested := protocol.PhaseConfig{
		protocol.PhaseWorking:  {Allow: []string{"Bash(new *)"}},
		protocol.PhaseSelfTest: {Allow: []string{"Bash(new *)"}},
	}
	got := mergeAllowIntoPhases(existing, suggested)

	if allow := got[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(new *)" {
		t.Errorf("phases.working.allow = %v, want it replaced by the suggestion", allow)
	}
	if deny := got[protocol.PhaseWorking].Deny; len(deny) != 1 || deny[0] != "npm publish" {
		t.Errorf("phases.working.deny = %v, want it preserved untouched", deny)
	}
	if !got[protocol.PhasePlanning].Skip {
		t.Error("phases.planning.skip should be preserved untouched (not part of the suggestion)")
	}
	if allow := got[protocol.PhaseSelfTest].Allow; len(allow) != 1 || allow[0] != "Bash(new *)" {
		t.Errorf("phases.self_test.allow = %v, want the new suggestion (phase absent from existing)", allow)
	}
}

func TestMergeAllowIntoPhasesBothEmptyReturnsNil(t *testing.T) {
	if got := mergeAllowIntoPhases(nil, nil); got != nil {
		t.Errorf("mergeAllowIntoPhases(nil, nil) = %+v, want nil", got)
	}
}

func TestPromptPhaseAllow(t *testing.T) {
	var buf bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("Bash(edited *)\n\n\n\n\n"))
	suggested := protocol.PhaseConfig{
		protocol.PhaseWorking:  {Allow: []string{"Bash(make *)"}, Deny: []string{"npm publish"}},
		protocol.PhaseSelfTest: {Allow: []string{"Bash(make *)"}},
	}
	// planning gets an edited answer; every other phase accepts its default.
	got := promptPhaseAllow(reader, &buf, suggested)

	if allow := got[protocol.PhasePlanning].Allow; len(allow) != 1 || allow[0] != "Bash(edited *)" {
		t.Errorf("phases.planning.allow = %v, want the edited entry", allow)
	}
	if allow := got[protocol.PhaseWorking].Allow; len(allow) != 1 || allow[0] != "Bash(make *)" {
		t.Errorf("phases.working.allow = %v, want the suggested default preserved", allow)
	}
	if deny := got[protocol.PhaseWorking].Deny; len(deny) != 1 || deny[0] != "npm publish" {
		t.Errorf("phases.working.deny = %v, want it preserved untouched by an allow-only prompt", deny)
	}
	if _, ok := got[protocol.PhaseAwaitingReview]; ok {
		t.Errorf("phases.awaiting_review should be dropped entirely (no allow/deny/skip), got %+v", got[protocol.PhaseAwaitingReview])
	}
}
