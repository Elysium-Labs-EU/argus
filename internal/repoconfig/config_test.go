package repoconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestPathDefault(t *testing.T) {
	t.Setenv(pathEnvVar, "")
	got := Path("/repo")
	want := filepath.Join("/repo", ".argus", "config.yml")
	if got != want {
		t.Errorf("Path(/repo) = %q, want %q", got, want)
	}
}

func TestPathEnvOverride(t *testing.T) {
	t.Setenv(pathEnvVar, "/tmp/somewhere/config.yml")
	got := Path("/repo")
	if got != "/tmp/somewhere/config.yml" {
		t.Errorf("Path(/repo) = %q, want env override", got)
	}
}

func TestLoadMissingFileIsZeroConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "nope", "config.yml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("Load(missing) = %+v, want zero Config", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{
		BaseBranch: "develop",
		Allow:      []string{"Bash(task *)", "Bash(pnpm *)"},
		BriefNote:  "Add a focused test and keep task frontend:ci green. Follow the repo AGENTS.md.",
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

func TestSaveLoadRoundTripReviewNote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{ReviewNote: "Flag any new dependency. Enforce the house error-wrapping style."}
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

func TestSaveLoadRoundTripGateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	maxDiffLines := 200
	want := Config{
		MaxDiffLines:       &maxDiffLines,
		ProofRequiredPaths: []string{"terraform", "deploy"},
		AlwaysReviewPaths:  []string{"auth", "billing"},
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

func TestSaveLoadRoundTripShipVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{ShipVerifyCommand: "make ci"}
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

func TestSaveLoadRoundTripGateVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{GateVerifyCommand: "make lint"}
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

func TestSaveLoadRoundTripTitlePrefixTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{TitlePrefixTemplate: "TICKET-{issue}: "}
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

func TestExperimentalWorkerSandboxDefaultsFalseAndEmpty(t *testing.T) {
	got, err := loadContent(t, "base_branch: \"develop\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ExperimentalWorkerSandbox {
		t.Error("ExperimentalWorkerSandbox default = true, want false when unset")
	}
	if got.SandboxAllowWrite != nil {
		t.Errorf("SandboxAllowWrite default = %v, want nil when unset", got.SandboxAllowWrite)
	}
}

func TestLoadExperimentalWorkerSandboxAndSandboxAllowWrite(t *testing.T) {
	got, err := loadContent(t, "experimental_worker_sandbox: true\nsandbox_allow_write:\n  - \"/home/me/go/pkg/mod\"\n  - \"/home/me/.cache/go-build\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.ExperimentalWorkerSandbox {
		t.Error("ExperimentalWorkerSandbox = false, want true")
	}
	want := []string{"/home/me/go/pkg/mod", "/home/me/.cache/go-build"}
	if !reflect.DeepEqual(got.SandboxAllowWrite, want) {
		t.Errorf("SandboxAllowWrite = %v, want %v", got.SandboxAllowWrite, want)
	}
}

func TestSaveLoadRoundTripExperimentalWorkerSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{ExperimentalWorkerSandbox: true, SandboxAllowWrite: []string{"/home/me/go/pkg/mod"}}
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

func TestSaveLoadRoundTripWorkspaceLabelTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{WorkspaceLabelTemplate: "{project}/{issue}-{summary}"}
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

func TestExperimentalWorkerSandboxInvalidBoolErrors(t *testing.T) {
	_, err := loadContent(t, "experimental_worker_sandbox: not-a-bool\n")
	if err == nil {
		t.Fatal("Load(experimental_worker_sandbox: not-a-bool): want error, got nil")
	}
	if !strings.Contains(err.Error(), "experimental_worker_sandbox") {
		t.Errorf("Load error = %q, want it to mention experimental_worker_sandbox", err.Error())
	}
}

func TestIssueCommentsDefaultsNil(t *testing.T) {
	got, err := loadContent(t, "base_branch: \"develop\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.IssueComments != nil {
		t.Errorf("IssueComments default = %v, want nil (unset) when the key is absent", got.IssueComments)
	}
}

func TestLoadIssueCommentsFalse(t *testing.T) {
	got, err := loadContent(t, "issue_comments: false\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.IssueComments == nil || *got.IssueComments != false {
		t.Errorf("IssueComments = %v, want a pointer to false (explicit opt-out, not unset)", got.IssueComments)
	}
}

// TestSaveLoadRoundTripIssueCommentsFalse pins the same "0 is legal, distinct
// from unset" tri-state MaxDiffLines/ReworkBudget already rely on: an
// explicit issue_comments: false must round-trip as a non-nil pointer to
// false, not collapse into the nil "unset" (default true) state.
func TestSaveLoadRoundTripIssueCommentsFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	off := false
	want := Config{IssueComments: &off}
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

func TestSaveLoadRoundTripIssueCommentsTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	on := true
	want := Config{IssueComments: &on}
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

func TestIssueCommentsInvalidBoolErrors(t *testing.T) {
	_, err := loadContent(t, "issue_comments: not-a-bool\n")
	if err == nil {
		t.Fatal("Load(issue_comments: not-a-bool): want error, got nil")
	}
	if !strings.Contains(err.Error(), "issue_comments") {
		t.Errorf("Load error = %q, want it to mention issue_comments", err.Error())
	}
}

func TestSaveLoadRoundTripOwnerStaleAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{OwnerStaleAfter: "1h"}
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

func TestSaveLoadRoundTripLauncher(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{Launcher: "codex --full-auto"}
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

func TestSaveLoadRoundTripWorktreeBootstrapCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{WorktreeBootstrapCommand: "cp ../.env .env"}
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

func TestSaveLoadRoundTripWorkerPlacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{WorkerPlacement: "tab"}
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

func TestSaveLoadRoundTripForge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{Forge: "gitlab"}
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

func TestSaveLoadRoundTripStatusPage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{StatusPage: "https://status.example.com"}
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

func TestSaveLoadRoundTripWorktreeDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{WorktreeDir: ".."}
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

func TestSaveLoadRoundTripReviewEffort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{ReviewEffort: "high"}
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

func TestSaveLoadRoundTripMaxDiffLinesZeroDisablesLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	zero := 0
	want := Config{MaxDiffLines: &zero}
	if err := Save(path, &want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.MaxDiffLines == nil || *got.MaxDiffLines != 0 {
		t.Errorf("MaxDiffLines = %v, want a pointer to 0 (explicit disable, not unset)", got.MaxDiffLines)
	}
}

func TestSaveLoadRoundTripReworkBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	budget := 6
	want := Config{ReworkBudget: &budget}
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

func TestSaveLoadRoundTripReworkBudgetZeroDisablesBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	zero := 0
	want := Config{ReworkBudget: &zero}
	if err := Save(path, &want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReworkBudget == nil || *got.ReworkBudget != 0 {
		t.Errorf("ReworkBudget = %v, want a pointer to 0 (explicit disable, not unset)", got.ReworkBudget)
	}
}

func TestSaveLoadEmptyConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := Save(path, &Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, Config{}) {
		t.Errorf("Load(empty) = %+v, want zero Config", got)
	}
}

func TestLoadBriefExampleFromIssue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `# .argus/config.yml
base_branch: "develop"
allow:
  - "Bash(task *)"
  - "Bash(pnpm *)"
brief_note: "Add a focused test and keep task frontend:ci green. Follow the repo AGENTS.md."
`
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		BaseBranch: "develop",
		Allow:      []string{"Bash(task *)", "Bash(pnpm *)"},
		BriefNote:  "Add a focused test and keep task frontend:ci green. Follow the repo AGENTS.md.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load(issue example) = %+v, want %+v", got, want)
	}
}

func TestLoadBareUnquotedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "base_branch: develop\nallow:\n  - Bash(git status*)\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{BaseBranch: "develop", Allow: []string{"Bash(git status*)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load(bare) = %+v, want %+v", got, want)
	}
}

func TestLoadStripsCommentAfterValueEndingInEscapedBackslash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	// The trailing \\ is one escaped backslash, not an escaped closing quote,
	// so the quote right after it still closes the string and the following
	// " # c" must be recognized as a comment, not kept as part of the value.
	content := `base_branch: "a\\" # c` + "\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{BaseBranch: `a\`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load(escaped backslash) = %+v, want %+v", got, want)
	}
}

// TestLoadUnknownKeyErrors pins the core fix: an unrecognized key at any
// level is now a hard error naming a line number, never silently skipped —
// a silent skip is exactly how a malformed double-nested brief_note could
// once sit in a config unnoticed.
func TestLoadUnknownKeyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "future_key: \"something argus doesn't know yet\"\nbase_branch: \"main\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(unknown key): want error, got nil")
	}
	if !strings.Contains(err.Error(), "line 1") || !strings.Contains(err.Error(), "future_key") {
		t.Errorf("Load error = %q, want it to name line 1 and future_key", err.Error())
	}
}

func TestLoadDeprecatedKeyAliasesStillParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "ship_lint: \"make ci\"\nverify_command: \"make lint\"\nworktree_setup_cmd: \"cp ../.env .env\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ShipVerifyCommand != "make ci" {
		t.Errorf("ShipVerifyCommand = %q, want %q (from deprecated ship_lint)", got.ShipVerifyCommand, "make ci")
	}
	if got.GateVerifyCommand != "make lint" {
		t.Errorf("GateVerifyCommand = %q, want %q (from deprecated verify_command)", got.GateVerifyCommand, "make lint")
	}
	if got.WorktreeBootstrapCommand != "cp ../.env .env" {
		t.Errorf("WorktreeBootstrapCommand = %q, want %q (from deprecated worktree_setup_cmd)", got.WorktreeBootstrapCommand, "cp ../.env .env")
	}
}

func TestLoadDeprecatedKeyUseRecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "ship_lint: \"make ci\"\nverify_command: \"make lint\"\nworktree_setup_cmd: \"cp ../.env .env\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []DeprecatedKeyUse{
		{Old: "ship_lint", New: "ship.verify_command"},
		{Old: "verify_command", New: "review.gate_verify_command"},
		{Old: "worktree_setup_cmd", New: "worktree_bootstrap_command"},
	}
	if !reflect.DeepEqual(got.Deprecated, want) {
		t.Errorf("Deprecated = %+v, want %+v", got.Deprecated, want)
	}
}

// TestLoadIntermediateWorktreeSetupCommandNameStillParses covers
// worktree_setup_command specifically: it was briefly the canonical name
// before worktree_bootstrap_command superseded it, so it's now a second
// deprecated alias (alongside the original worktree_setup_cmd) pointing at
// the same final field, not a dead end.
func TestLoadIntermediateWorktreeSetupCommandNameStillParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "worktree_setup_command: \"cp ../.env .env\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.WorktreeBootstrapCommand != "cp ../.env .env" {
		t.Errorf("WorktreeBootstrapCommand = %q, want %q (from deprecated worktree_setup_command)", got.WorktreeBootstrapCommand, "cp ../.env .env")
	}
	want := []DeprecatedKeyUse{{Old: "worktree_setup_command", New: "worktree_bootstrap_command"}}
	if !reflect.DeepEqual(got.Deprecated, want) {
		t.Errorf("Deprecated = %+v, want %+v", got.Deprecated, want)
	}
}

func TestLoadDeprecatedEmptyWhenOnlyNewNamesUsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "worktree_bootstrap_command: \"cp ../.env .env\"\n\nship:\n  verify_command: \"make ci\"\n\nreview:\n  gate_verify_command: \"make lint\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Deprecated) != 0 {
		t.Errorf("Deprecated = %+v, want empty when only new names/locations are used", got.Deprecated)
	}
	if got.ShipVerifyCommand != "make ci" || got.GateVerifyCommand != "make lint" || got.WorktreeBootstrapCommand != "cp ../.env .env" {
		t.Errorf("values = %+v, want the same values under the new nested shape", got)
	}
}

func TestEncodeYAMLNeverEmitsOldKeyNamesAfterLoadingOldNamedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	if err := writeFile(path, "ship_lint: \"make ci\"\nverify_command: \"make lint\"\nworktree_setup_cmd: \"cp ../.env .env\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if serr := Save(path, &loaded); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		for _, old := range []string{"ship_lint:", "verify_command:", "worktree_setup_cmd:", "worktree_setup_command:"} {
			if strings.HasPrefix(line, old) {
				t.Errorf("saved config line %q starts with deprecated key %q, want only new names", line, old)
			}
		}
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (reload): %v", err)
	}
	if reloaded.ShipVerifyCommand != "make ci" || reloaded.GateVerifyCommand != "make lint" || reloaded.WorktreeBootstrapCommand != "cp ../.env .env" {
		t.Errorf("reloaded values = %+v, want the same values under the new names", reloaded)
	}
	if len(reloaded.Deprecated) != 0 {
		t.Errorf("Deprecated = %+v, want empty on the second load (file now uses new names only)", reloaded.Deprecated)
	}
}

func TestSaveLoadRoundTripPhases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{
		Phases: protocol.PhaseConfig{
			protocol.PhasePlanning: {Deny: []string{"npm publish"}},
			protocol.PhaseWorking:  {Skip: true, Deny: []string{"docker push"}},
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

func TestLoadPhaseKeyUnrecognizedPhaseErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.plannning.skip: true\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(unrecognized phase name): want error, got nil")
	}
}

func TestLoadPhaseKeyUnrecognizedSubkeyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.planning.frobnicate: true\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(unrecognized phase subkey): want error, got nil")
	}
}

func TestLoadMalformedLineErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "this is not key value\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(malformed): want error, got nil")
	}
}

// TestLoadStrayListItemAtTopLevelErrors covers parseYAML's own
// list-item-outside-a-recognized-key check: a top-level "- value" line with
// no preceding list key (allow:, proof_required_paths:, ...) to belong to.
func TestLoadStrayListItemAtTopLevelErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "- stray\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(stray top-level list item): want error, got nil")
	}
	if !strings.Contains(err.Error(), "outside of a recognized key") {
		t.Errorf("Load error = %q, want it to mention outside of a recognized key", err.Error())
	}
}

// TestLoadUnreadablePathNonNotExistError covers Load's other os.ReadFile
// branch: a path whose parent component exists but isn't a directory fails
// with ENOTDIR, not ENOENT, so os.IsNotExist(err) is false and Load must
// propagate the error instead of treating it as "missing file".
func TestLoadUnreadablePathNonNotExistError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(filepath.Join(blocker, "config.yml"))
	if err == nil {
		t.Fatal("Load(path through a file): want error, got nil")
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("Load(path through a file) cfg = %+v, want zero Config", cfg)
	}
}

// TestSaveMkdirAllErrorWrapped covers Save's MkdirAll error path: the parent
// directory can't be created because a path component is already a regular
// file, not a directory.
func TestSaveMkdirAllErrorWrapped(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := Save(filepath.Join(blocker, "sub", "config.yml"), &Config{})
	if err == nil {
		t.Fatal("Save(parent path blocked by a file): want error, got nil")
	}
	if !strings.Contains(err.Error(), "creating config directory") {
		t.Errorf("Save error = %q, want it to mention %q", err.Error(), "creating config directory")
	}
}

// TestSaveWriteFileErrorWrapped covers Save's WriteFile error path: MkdirAll
// succeeds (the parent already exists) but the target path is itself a
// directory, so WriteFile fails with EISDIR.
func TestSaveWriteFileErrorWrapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	err := Save(path, &Config{})
	if err == nil {
		t.Fatal("Save(target path is a directory): want error, got nil")
	}
	if !strings.Contains(err.Error(), "writing config file") {
		t.Errorf("Save error = %q, want it to mention %q", err.Error(), "writing config file")
	}
}

// TestLoadUnknownKeyWithListBlockErrors covers the same unknown-top-level-key
// hard error as TestLoadUnknownKeyErrors, but for a key shaped like a list
// block (its own indented "- " lines) rather than a scalar — the error must
// fire at the unknown key itself, before any of its own content is read.
func TestLoadUnknownKeyWithListBlockErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "future_list_key:\n  - one\n  - two\nbase_branch: \"main\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(unknown list-shaped key): want error, got nil")
	}
	if !strings.Contains(err.Error(), "future_list_key") {
		t.Errorf("Load error = %q, want it to mention future_list_key", err.Error())
	}
}

// TestLoadBadQuotedScalarValueErrors covers unquoteYAML's error path as
// surfaced through a scalar field: an invalid Go quote escape is a hard
// parse error, not a value that's silently left bare.
func TestLoadBadQuotedScalarValueErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "base_branch: \"\\x\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(bad quoted scalar): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad value") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "bad value")
	}
}

// TestLoadBadQuotedListItemErrors covers parseYAMLList's own unquoteYAML
// error path, distinct from the scalar-value one above.
func TestLoadBadQuotedListItemErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "allow:\n  - \"\\x\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(bad quoted list item): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad list item") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "bad list item")
	}
}

// TestLoadPhaseSkipBadBoolErrors covers assignPhaseKey's ParseBool error
// path for the skip subkey.
func TestLoadPhaseSkipBadBoolErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.planning.skip: notabool\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(phase.planning.skip: notabool): want error, got nil")
	}
	if !strings.Contains(err.Error(), "phase.planning.skip") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "phase.planning.skip")
	}
}

// TestLoadDottedPhaseKeyBadQuotedValueErrors covers parseDottedPhaseKey's own
// unquoteYAML error path — distinct from assignPhaseKey's ParseBool error
// above, since the bad quoting is caught before the value ever reaches
// assignPhaseKey.
func TestLoadDottedPhaseKeyBadQuotedValueErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.planning.skip: \"\\x\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(bad quoted dotted phase key value): want error, got nil")
	}
	if !strings.Contains(err.Error(), "bad value") {
		t.Errorf("Load error = %q, want it to mention bad value", err.Error())
	}
}

// TestLoadPhaseDenyInlineValueErrors covers assignPhaseKey's deny-must-be-a-
// list-not-an-inline-value error path.
func TestLoadPhaseDenyInlineValueErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.planning.deny: inlineval\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(phase.planning.deny: inlineval): want error, got nil")
	}
	if !strings.Contains(err.Error(), "expects a list") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "expects a list")
	}
}

// TestLoadMaxDiffLinesBadIntErrors covers assignScalarField's Atoi error
// path for max_diff_lines.
func TestLoadMaxDiffLinesBadIntErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "max_diff_lines: notanint\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(max_diff_lines: notanint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "max_diff_lines") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "max_diff_lines")
	}
}

// TestLoadReworkBudgetBadIntErrors covers assignScalarField's Atoi error
// path for rework_budget.
func TestLoadReworkBudgetBadIntErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "rework_budget: notanint\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(rework_budget: notanint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "rework_budget") {
		t.Errorf("Load error = %q, want it to mention %q", err.Error(), "rework_budget")
	}
}

// TestLoadBarePhaseKeyErrors pins parsePhaseKey's ok=false case: a
// phase.<name> key with no ".<subkey>" is not shaped like the schema
// expects, so it falls through as an ordinary unrecognized top-level key —
// a hard error, same as any other unknown key — rather than the
// "unrecognized phase policy key" error a phase.<name>.<badsubkey> key gets.
func TestLoadBarePhaseKeyErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := writeFile(path, "phase.planning: true\nbase_branch: \"main\"\n"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(bare phase.planning key): want error, got nil")
	}
	if !strings.Contains(err.Error(), "phase.planning") {
		t.Errorf("Load error = %q, want it to mention phase.planning", err.Error())
	}
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
