package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCovers(t *testing.T) {
	cases := map[string]bool{
		"Bash(argus *)":           true,
		"Bash(argus)":             true,
		"Bash(argus ship *)":      true,
		"Bash(argus supervise:*)": true,
		"Bash(argustest *)":       false,
		"Bash(git *)":             false,
		"Edit(argus/**)":          false,
	}
	for entry, want := range cases {
		if got := Covers(entry); got != want {
			t.Errorf("Covers(%q) = %v, want %v", entry, got, want)
		}
	}
}

func TestCoversShipForce(t *testing.T) {
	cases := map[string]bool{
		"Bash(argus *)":           true,
		"Bash(argus ship *)":      true,
		"Bash(argus ship:*)":      true,
		"Bash(argus)":             false,
		"Bash(argus ship)":        false,
		"Bash(argus supervise *)": false,
		"Bash(argus supervise:*)": false,
		"Bash(argus review *)":    false,
		"Bash(argustest *)":       false,
		"Bash(git *)":             false,
	}
	for entry, want := range cases {
		if got := CoversShipForce(entry); got != want {
			t.Errorf("CoversShipForce(%q) = %v, want %v", entry, got, want)
		}
	}
}

func TestCheckMissingFileIsNotCovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	covered, matches, err := Check(path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if covered {
		t.Errorf("expected uncovered for a missing file, got matches %v", matches)
	}
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckFindsExactEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"allow":["Bash(git status*)","Bash(argus *)"]}}`)
	covered, matches, err := Check(path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !covered {
		t.Fatal("expected covered")
	}
	if len(matches) != 1 || matches[0] != "Bash(argus *)" {
		t.Errorf("matches = %v, want [Bash(argus *)]", matches)
	}
}

func TestCheckFindsScopedEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"allow":["Bash(argus ship *)","Bash(argus supervise *)"]}}`)
	covered, matches, err := Check(path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !covered || len(matches) != 2 {
		t.Errorf("covered=%v matches=%v, want covered=true len=2", covered, matches)
	}
}

func TestCheckNoPermissionsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"model":"opus"}`)
	covered, _, err := Check(path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if covered {
		t.Error("expected uncovered when there's no permissions block at all")
	}
}

func TestCheckMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `not json`)
	if _, _, err := Check(path); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestEnsureCreatesFileAndDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	added, err := Ensure(path, DefaultAllowEntry)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !added {
		t.Fatal("expected added=true for a fresh file")
	}
	covered, matches, err := Check(path)
	if err != nil {
		t.Fatalf("Check after Ensure: %v", err)
	}
	if !covered || len(matches) != 1 || matches[0] != DefaultAllowEntry {
		t.Errorf("post-Ensure Check: covered=%v matches=%v", covered, matches)
	}
}

func TestEnsureIsIdempotentWhenAlreadyCovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"allow":["Bash(argus *)"]}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	added, err := Ensure(path, DefaultAllowEntry)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if added {
		t.Error("expected added=false when already covered")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("Ensure rewrote the file even though nothing needed to change")
	}
}

func TestEnsureDoesNotDuplicateWhenScopedEntryAlreadyCovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"allow":["Bash(argus ship *)"]}}`)
	added, err := Ensure(path, DefaultAllowEntry)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if added {
		t.Error("expected added=false: an existing scoped entry already covers argus")
	}
}

func TestEnsurePreservesUnrelatedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{
  "model": "opus",
  "hooks": {"Stop": [{"type": "command", "command": "task-contract-guard.sh"}]},
  "permissions": {
    "deny": ["Bash(rm -rf *)"],
    "allow": ["Bash(git status*)"]
  }
}`)
	added, err := Ensure(path, DefaultAllowEntry)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !added {
		t.Fatal("expected added=true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if raw["model"] != "opus" {
		t.Errorf("model was dropped or changed: %v", raw["model"])
	}
	if _, ok := raw["hooks"]; !ok {
		t.Error("hooks block was dropped")
	}
	perms, ok := raw["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions block missing or wrong shape: %v", raw["permissions"])
	}
	deny, ok := perms["deny"].([]any)
	if !ok || len(deny) != 1 || deny[0] != "Bash(rm -rf *)" {
		t.Errorf("deny list was dropped or changed: %v", perms["deny"])
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) != 2 {
		t.Fatalf("allow list = %v, want 2 entries", perms["allow"])
	}
	if allow[0] != "Bash(git status*)" || allow[1] != DefaultAllowEntry {
		t.Errorf("allow list = %v, want [Bash(git status*) %s]", allow, DefaultAllowEntry)
	}
}

func TestDenyEntryCovers(t *testing.T) {
	cases := []struct {
		entry, target string
		want          bool
	}{
		{"Bash(herdr pane send-text:*)", "pane send-text", true},
		{"Bash(herdr pane send-text *)", "pane send-text", true},
		{"Bash(herdr pane send-text)", "pane send-text", true},
		{"Bash(herdr pane *)", "pane send-text", true},
		{"Bash(herdr *)", "pane run", true},
		{"Bash(herdr)", "pane run", false},
		{"Bash(herdr pane run)", "pane run", true},
		{"Bash(herdr pane send-keys:*)", "pane send-text", false},
		{"Bash(herdr pane send:*)", "pane send-text", false},
		{"Bash(git *)", "pane run", false},
		{"Edit(herdr/**)", "pane run", false},
	}
	for _, c := range cases {
		if got := denyEntryCovers(c.entry, c.target); got != c.want {
			t.Errorf("denyEntryCovers(%q, %q) = %v, want %v", c.entry, c.target, got, c.want)
		}
	}
}

func TestDefaultDenyEntries(t *testing.T) {
	want := []string{
		"Bash(herdr pane send-text:*)",
		"Bash(herdr pane send-keys:*)",
		"Bash(herdr pane run:*)",
	}
	got := DefaultDenyEntries()
	if len(got) != len(want) {
		t.Fatalf("DefaultDenyEntries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DefaultDenyEntries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCheckDenyMissingFileReportsAllMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	missing, err := CheckDeny(path)
	if err != nil {
		t.Fatalf("CheckDeny: %v", err)
	}
	if len(missing) != len(DefaultDenyEntries()) {
		t.Errorf("missing = %v, want all %d default deny entries", missing, len(DefaultDenyEntries()))
	}
}

func TestCheckDenyAllPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"deny":["Bash(herdr pane send-text:*)","Bash(herdr pane send-keys:*)","Bash(herdr pane run:*)"]}}`)
	missing, err := CheckDeny(path)
	if err != nil {
		t.Fatalf("CheckDeny: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestCheckDenyBroaderEntryCoversAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"deny":["Bash(herdr pane *)"]}}`)
	missing, err := CheckDeny(path)
	if err != nil {
		t.Fatalf("CheckDeny: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none: a broader \"herdr pane *\" deny already covers every target", missing)
	}
}

func TestEnsureDenyCreatesFileAndDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	added, err := EnsureDeny(path)
	if err != nil {
		t.Fatalf("EnsureDeny: %v", err)
	}
	if len(added) != len(DefaultDenyEntries()) {
		t.Fatalf("added = %v, want all %d default deny entries", added, len(DefaultDenyEntries()))
	}
	missing, err := CheckDeny(path)
	if err != nil {
		t.Fatalf("CheckDeny after EnsureDeny: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing after EnsureDeny = %v, want none", missing)
	}
}

func TestEnsureDenyIsIdempotentWhenAllPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"deny":["Bash(herdr pane send-text:*)","Bash(herdr pane send-keys:*)","Bash(herdr pane run:*)"]}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	added, err := EnsureDeny(path)
	if err != nil {
		t.Fatalf("EnsureDeny: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("expected no entries added, got %v", added)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("EnsureDeny rewrote the file even though nothing needed to change")
	}
}

func TestEnsureDenyAppendsToExistingDifferentDenyBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"deny":["Bash(rm -rf *)"]}}`)
	added, err := EnsureDeny(path)
	if err != nil {
		t.Fatalf("EnsureDeny: %v", err)
	}
	if len(added) != len(DefaultDenyEntries()) {
		t.Fatalf("added = %v, want all %d default deny entries", added, len(DefaultDenyEntries()))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	deny, ok := raw["permissions"].(map[string]any)["deny"].([]any)
	if !ok || len(deny) != 4 {
		t.Fatalf("deny = %v, want the pre-existing entry plus 3 new ones", raw["permissions"])
	}
	if deny[0] != "Bash(rm -rf *)" {
		t.Errorf("pre-existing deny entry disturbed: %v", deny)
	}
}

func TestEnsureDenyPreservesUnrelatedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(argus *)"]
  }
}`)
	added, err := EnsureDeny(path)
	if err != nil {
		t.Fatalf("EnsureDeny: %v", err)
	}
	if len(added) != len(DefaultDenyEntries()) {
		t.Fatalf("added = %v, want all %d default deny entries", added, len(DefaultDenyEntries()))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if raw["model"] != "opus" {
		t.Errorf("model was dropped or changed: %v", raw["model"])
	}
	perms, ok := raw["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions block missing or wrong shape: %v", raw["permissions"])
	}
	allow, ok := perms["allow"].([]any)
	if !ok || len(allow) != 1 || allow[0] != "Bash(argus *)" {
		t.Errorf("allow list was dropped or changed: %v", perms["allow"])
	}
}

func TestSettingsPath(t *testing.T) {
	cases := []struct{ repo, want string }{
		{"/home/user/repo", "/home/user/repo/.claude/settings.json"},
		{"relative/repo", "relative/repo/.claude/settings.json"},
		{".", ".claude/settings.json"},
		{"", ".claude/settings.json"},
	}
	for _, c := range cases {
		if got := SettingsPath(c.repo); got != c.want {
			t.Errorf("SettingsPath(%q) = %q, want %q", c.repo, got, c.want)
		}
	}
}

// skipIfRoot skips permission-based negative tests when running as root, since
// root bypasses the filesystem permission bits the test relies on to force a
// write failure.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denial tests can't force a failure")
	}
}

func TestWriteSettingsFileMkdirFailsWhenParentIsAFile(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notADir, ".claude", "settings.json")
	if err := writeSettingsFile(path, rawSettings{}); err == nil {
		t.Fatal("expected an error when the settings dir's parent is a regular file")
	}
}

func TestWriteSettingsFileFailsInReadOnlyDir(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	path := filepath.Join(dir, "settings.json")
	if err := writeSettingsFile(path, rawSettings{}); err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
}

func TestWriteSettingsFileRenameFailsOntoExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSettingsFile(path, rawSettings{}); err == nil {
		t.Fatal("expected an error renaming a file into place over an existing directory")
	}
}

func TestEnsureLoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `not json`)
	added, err := Ensure(path, DefaultAllowEntry)
	if err == nil {
		t.Fatal("expected an error loading malformed settings.json")
	}
	if added {
		t.Error("expected added=false on error")
	}
}

func TestEnsureWriteError(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	added, err := Ensure(path, DefaultAllowEntry)
	if err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
	if added {
		t.Error("expected added=false on error")
	}
}

func TestCheckMalformedPermissionsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":"not an object"}`)
	if _, _, err := Check(path); err == nil {
		t.Fatal("expected an error when permissions is not an object")
	}
}

func TestCheckMalformedAllowList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeSettings(t, path, `{"permissions":{"allow":[1,2,3]}}`)
	if _, _, err := Check(path); err == nil {
		t.Fatal("expected an error when permissions.allow isn't a string list")
	}
}

func TestEnsureCustomEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	added, err := Ensure(path, "Bash(argus ship *)")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !added {
		t.Fatal("expected added=true")
	}
	covered, matches, err := Check(path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !covered || len(matches) != 1 || matches[0] != "Bash(argus ship *)" {
		t.Errorf("covered=%v matches=%v", covered, matches)
	}
}
