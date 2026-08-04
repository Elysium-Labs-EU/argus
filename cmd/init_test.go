package cmd

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/permission"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

// initPromptExemptFields lists Config fields runInit deliberately does not
// prompt for, with the reason it's fine to stay hand-edit-only (see
// schemas/config.schema.json).
var initPromptExemptFields = map[string]string{
	"Phases": "a per-phase policy map (allow/deny/skip per protocol.ConfigurablePhases entry), not a single scalar or flat list runInit's interactive prompts can represent — hand-edit .argus/config.yml's nested phases.<name>.<subkey> blocks directly, with schemas/config.schema.json's per-phase definitions giving editor validation/autocomplete.",
}

const wantConfigFieldCount = 21 // repoconfig.Config's current field count

func writeMarker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("writing marker %s: %v", name, err)
	}
}

func TestDetectRepoConfigMakefile(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cfg := detectRepoConfig(context.Background(), dir)
	if len(cfg.Allow) != 1 || cfg.Allow[0] != "Bash(make *)" {
		t.Errorf("Allow = %v, want [\"Bash(make *)\"]", cfg.Allow)
	}
	if !strings.Contains(cfg.BriefNote, "make ci") {
		t.Errorf("BriefNote = %q, want it to mention make ci", cfg.BriefNote)
	}
}

func TestDetectRepoConfigGoMod(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod")
	cfg := detectRepoConfig(context.Background(), dir)
	if len(cfg.Allow) == 0 || cfg.Allow[0] != "Bash(go build *)" {
		t.Errorf("Allow = %v, want it to start with go build", cfg.Allow)
	}
}

func TestDetectRepoConfigTaskfilePrecedesMakefile(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Taskfile.yml")
	writeMarker(t, dir, "Makefile")
	cfg := detectRepoConfig(context.Background(), dir)
	if len(cfg.Allow) != 1 || cfg.Allow[0] != "Bash(task *)" {
		t.Errorf("Allow = %v, want Taskfile.yml to win over Makefile", cfg.Allow)
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
	if len(got.Allow) != 1 || got.Allow[0] != "Bash(make *)" {
		t.Errorf("written Allow = %v, want the detected Makefile suggestion", got.Allow)
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
	// Three bare-Enter answers: base_branch, allow, brief_note all keep the
	// detected suggestion.
	cmd.SetIn(strings.NewReader("\n\n\n"))

	if err := runInit(cmd, &initArgs{repo: dir}); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	got, err := repoconfig.Load(repoconfig.Path(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Allow) == 0 || got.Allow[0] != "Bash(go build *)" {
		t.Errorf("written Allow = %v, want the detected go.mod suggestion preserved", got.Allow)
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
	// Override base_branch and allow, keep brief_note (bare Enter).
	cmd.SetIn(strings.NewReader("develop\nBash(task *), Bash(pnpm *)\n\n"))

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
	// base_branch, allow, brief_note (keep detected), max_diff_lines,
	// rework_budget (keep default), proof_required_paths,
	// always_review_paths, worker_placement — a representative subset of
	// init's prompts, each edited to a non-default value.
	cmd.SetIn(strings.NewReader("main\nBash(make *)\n\n250\n\nterraform, deploy\nauth\ntab\n"))

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
	// (unset) default and init still writes the rest.
	cmd.SetIn(strings.NewReader("\n\n\nnotanumber\n\n\n\n"))

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
	// base_branch, allow, brief_note, max_diff_lines, rework_budget,
	// proof_required_paths, always_review_paths, worker_placement,
	// ship_verify_command, gate_verify_command, worktree_bootstrap_command,
	// review_effort, launcher, worktree_dir, owner_stale_after,
	// title_prefix_template, review_note (all bare Enter, 17 prompts), then
	// forge.
	cmd.SetIn(strings.NewReader(strings.Repeat("\n", 17) + "gitlab\n"))

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
	if len(got.Allow) != 1 || got.Allow[0] != "Bash(make *)" {
		t.Errorf("Allow = %v, want the fresh Makefile suggestion", got.Allow)
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
	// allow, brief_note, max_diff_lines, rework_budget, proof_required_paths,
	// always_review_paths, worker_placement, ship_verify_command,
	// gate_verify_command, worktree_bootstrap_command, review_effort, launcher,
	// worktree_dir, owner_stale_after, title_prefix_template, review_note, forge,
	// status_page.
	answers := []string{
		"develop", "Bash(task *)", "custom brief", "250", "6", "terraform", "auth",
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
// emits the current nested ship:/phases: regions end to end — not just that
// repoconfig.Load can read the fields back (TestRunInitPromptsSetEveryConfigField
// already covers that), but that the raw file on disk uses the new shape
// (schemas/config.schema.json) rather than the deprecated flat keys runInit
// used to write before the region restructure.
func TestRunInitWritesNestedShipAndPhasesShape(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "Makefile")
	cmd := newInitCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	answers := []string{
		"develop", "Bash(task *)", "custom brief", "250", "6", "terraform", "auth",
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
	for _, want := range []string{"\nship:\n", "  verify_command: ", "  title_prefix_template: ", "\nphases:\n", "  awaiting_review:\n", "    gate_verify_command: ", "    max_diff_lines: ", "    review_note: ", "    review_effort: "} {
		if !strings.Contains(content, want) {
			t.Errorf("saved config = %q, want it to contain %q", content, want)
		}
	}
	for line := range strings.SplitSeq(content, "\n") {
		for _, old := range []string{"ship_verify_command:", "gate_verify_command:", "max_diff_lines:", "proof_required_paths:", "always_review_paths:", "review_note:", "review_effort:", "title_prefix_template:"} {
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
// default for all 19), so a test can append its own answer for the trailing
// config-check confirm.
const initPromptAnswers = 19

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
