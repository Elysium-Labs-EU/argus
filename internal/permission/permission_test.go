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
