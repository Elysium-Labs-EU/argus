package repoconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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

func TestSaveLoadRoundTripShipLint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{ShipLint: "make ci"}
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

func TestSaveLoadRoundTripVerifyCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{VerifyCommand: "make lint"}
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

func TestSaveLoadRoundTripWorktreeSetupCmd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".argus", "config.yml")
	want := Config{WorktreeSetupCmd: "cp ../.env .env"}
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

func TestLoadUnknownKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := "future_key: \"something argus doesn't know yet\"\nbase_branch: \"main\"\n"
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", got.BaseBranch)
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

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
