// check-golangci-pin_test.go exercises the CI gate that fails the build if
// golangci-lint's pinned version stops having exactly one source of truth
// across .golangci-lint-version, the Makefile, CI workflows, and lefthook.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGolangciPinGate(t *testing.T, repoDir string) (out string, err error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-golangci-pin.sh")
	if err != nil {
		t.Fatalf("resolving check-golangci-pin.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = repoDir
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

func writeGolangciPinFixture(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func cleanFixture() map[string]string {
	return map[string]string{
		".golangci-lint-version": "v2.12.2\n",
		"Makefile": "lint:\n" +
			"\tgo run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m\n" +
			"fix:\n" +
			"\tgo run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt\n",
		"lefthook.yml": "pre-commit:\n" +
			"  commands:\n" +
			"    lint:\n" +
			"      run: go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(cat .golangci-lint-version) run\n",
		".github/workflows/ci.yml": "jobs:\n  build:\n    steps:\n      - run: make ci\n",
	}
}

func TestGolangciPinGatePassesOnCleanFixture(t *testing.T) {
	dir := t.TempDir()
	writeGolangciPinFixture(t, dir, cleanFixture())

	out, err := runGolangciPinGate(t, dir)
	if err != nil {
		t.Fatalf("gate should pass on a clean fixture, got err: %v\n%s", err, out)
	}
}

func TestGolangciPinGateFailsOnMissingVersionFile(t *testing.T) {
	dir := t.TempDir()
	fixture := cleanFixture()
	delete(fixture, ".golangci-lint-version")
	writeGolangciPinFixture(t, dir, fixture)

	out, err := runGolangciPinGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when the version file is missing, got output:\n%s", out)
	}
}

func TestGolangciPinGateFailsOnMalformedVersion(t *testing.T) {
	dir := t.TempDir()
	fixture := cleanFixture()
	fixture[".golangci-lint-version"] = "v2.12\n"
	writeGolangciPinFixture(t, dir, fixture)

	out, err := runGolangciPinGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail on a non-exact version, got output:\n%s", out)
	}
}

func TestGolangciPinGateFailsOnHardcodedWorkflowVersion(t *testing.T) {
	dir := t.TempDir()
	fixture := cleanFixture()
	fixture[".github/workflows/ci.yml"] = "jobs:\n  lint:\n    steps:\n" +
		"      - uses: golangci/golangci-lint-action@v9\n" +
		"        with:\n          version: v2.11.4\n"
	writeGolangciPinFixture(t, dir, fixture)

	out, err := runGolangciPinGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when a workflow hardcodes a version, got output:\n%s", out)
	}
}

func TestGolangciPinGateFailsOnBareMakefileInvocation(t *testing.T) {
	dir := t.TempDir()
	fixture := cleanFixture()
	fixture["Makefile"] = "lint:\n\tgolangci-lint run --timeout=5m\n"
	writeGolangciPinFixture(t, dir, fixture)

	out, err := runGolangciPinGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when the Makefile calls golangci-lint from PATH, got output:\n%s", out)
	}
}

func TestGolangciPinGateFailsOnHardcodedLefthookVersion(t *testing.T) {
	dir := t.TempDir()
	fixture := cleanFixture()
	fixture["lefthook.yml"] = "pre-commit:\n  commands:\n    lint:\n" +
		"      run: go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.0 run\n"
	writeGolangciPinFixture(t, dir, fixture)

	out, err := runGolangciPinGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when lefthook.yml hardcodes a version, got output:\n%s", out)
	}
}

func TestGolangciPinGatePassesOnRealRepo(t *testing.T) {
	out, err := runGolangciPinGate(t, "..")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
