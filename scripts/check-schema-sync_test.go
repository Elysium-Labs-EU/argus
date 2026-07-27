// check-schema-sync_test.go exercises the CI gate that fails the build if
// schemas/config.schema.json's property keys ever drift from the key set
// internal/repoconfig/yaml.go actually reads/writes for .argus/config.yml —
// the mistake that silently broke eos/themis's pubkey copies after a
// rotation touched only one of two hand-duplicated sources.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runSchemaSyncGate(t *testing.T, target string) (out string, err error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-schema-sync.sh")
	if err != nil {
		t.Fatalf("resolving check-schema-sync.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath, target)
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

const testYAMLGoSrc = `package repoconfig

func parseYAML(data string) (Config, error) {
	switch key {
	case "base_branch":
	case "worker_placement":
	}
	switch key {
	case "allow":
	}
	return cfg, nil
}
`

func writeSchemaSyncFixture(t *testing.T, dir, goSrc, schemaJSON string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "repoconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "repoconfig", "yaml.go"), []byte(goSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schemas", "config.schema.json"), []byte(schemaJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaSyncGatePassesWhenKeysMatch(t *testing.T) {
	dir := t.TempDir()
	writeSchemaSyncFixture(t, dir, testYAMLGoSrc, `{"properties": {"base_branch": {}, "worker_placement": {}, "allow": {}}}`)

	out, err := runSchemaSyncGate(t, dir)
	if err != nil {
		t.Fatalf("gate should pass when key sets match, got err: %v\n%s", err, out)
	}
}

func TestSchemaSyncGateFailsOnMissingSchemaKey(t *testing.T) {
	dir := t.TempDir()
	writeSchemaSyncFixture(t, dir, testYAMLGoSrc, `{"properties": {"base_branch": {}, "allow": {}}}`)

	out, err := runSchemaSyncGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when the schema is missing a key the Go loader recognizes, got output:\n%s", out)
	}
}

func TestSchemaSyncGateFailsOnExtraSchemaKey(t *testing.T) {
	dir := t.TempDir()
	writeSchemaSyncFixture(t, dir, testYAMLGoSrc, `{"properties": {"base_branch": {}, "worker_placement": {}, "allow": {}, "made_up_key": {}}}`)

	out, err := runSchemaSyncGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when the schema has a key the Go loader doesn't recognize, got output:\n%s", out)
	}
}

func TestSchemaSyncGatePassesOnRealRepo(t *testing.T) {
	out, err := runSchemaSyncGate(t, "..")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
