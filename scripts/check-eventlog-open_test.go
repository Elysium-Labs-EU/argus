// check-eventlog-open_test.go exercises the CI gate that fails the build if any
// _test.go file calls eventlog.Open directly instead of eventlog.OpenForTest —
// the mistake that let a test leak real events into a developer's ~/.argus/runs.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runEventlogOpenGate(t *testing.T, target string) (out string, err error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-eventlog-open.sh")
	if err != nil {
		t.Fatalf("resolving check-eventlog-open.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath, target)
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

func TestEventlogOpenGatePassesWithoutDirectCalls(t *testing.T) {
	dir := t.TempDir()
	src := "package foo_test\nimport \"testing\"\nfunc TestFoo(t *testing.T) { _ = eventlog.OpenForTest }\n"
	if err := os.WriteFile(filepath.Join(dir, "foo_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runEventlogOpenGate(t, dir)
	if err != nil {
		t.Fatalf("gate should pass, got err: %v\n%s", err, out)
	}
}

func TestEventlogOpenGateFailsOnDirectCall(t *testing.T) {
	dir := t.TempDir()
	// Built by concatenation so this fixture string doesn't itself trip the gate
	// when it scans the real repo (this very file is a _test.go).
	badCall := "eventlog." + "Open(\"home\", \"cmd\", nil)"
	src := "package foo_test\nimport \"testing\"\nfunc TestFoo(t *testing.T) { " + badCall + " }\n"
	if err := os.WriteFile(filepath.Join(dir, "foo_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runEventlogOpenGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail on a direct eventlog.Open call, got output:\n%s", out)
	}
}

func TestEventlogOpenGatePassesOnRealRepo(t *testing.T) {
	out, err := runEventlogOpenGate(t, "..")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
