// check-any-not-interface_test.go exercises the CI gate that fails the build
// if tracked Go source spells the empty interface the old way instead of
// any, so that a `grep`-for-any audit of the codebase stays exhaustive.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runAnyNotInterfaceGate(t *testing.T, target string) (out string, err error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-any-not-interface.sh")
	if err != nil {
		t.Fatalf("resolving check-any-not-interface.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath, target)
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

func TestAnyNotInterfaceGatePassesWithAny(t *testing.T) {
	dir := t.TempDir()
	src := "package foo\nfunc F(v any) any { return v }\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAnyNotInterfaceGate(t, dir)
	if err != nil {
		t.Fatalf("gate should pass, got err: %v\n%s", err, out)
	}
}

func TestAnyNotInterfaceGateFailsOnEmptyInterface(t *testing.T) {
	dir := t.TempDir()
	// Built by concatenation so this fixture string doesn't itself trip the
	// gate when it scans the real repo (this very file is tracked *.go).
	badType := "interface" + "{}"
	src := "package foo\nfunc F(v " + badType + ") " + badType + " { return v }\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAnyNotInterfaceGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail on an empty interface literal, got output:\n%s", out)
	}
}

func TestAnyNotInterfaceGatePassesOnRealRepo(t *testing.T) {
	out, err := runAnyNotInterfaceGate(t, "..")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
