package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeFakeBinary drops an executable shell script named name into dir,
// exiting with code, so tests can stand in for lefthook/pre-commit without
// depending on either being installed on the machine running `go test`.
func writeFakeBinary(t *testing.T, dir, name string, code int) {
	t.Helper()
	script := filepath.Join(dir, name)
	content := "#!/bin/sh\necho fake " + name + " ran\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
}

// minimalPATH restricts PATH to plain system directories for the duration of
// one test — none of which carry lefthook or pre-commit — so
// "configured but not installed" is reliably reproducible regardless of what
// happens to be on the machine actually running the test suite.
func minimalPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", "/usr/bin:/bin")
}

// fakeBinPATH prepends dir (expected to hold a writeFakeBinary script) ahead
// of the plain system directories, so exec.LookPath finds the fake before any
// real install of the same name elsewhere on the machine's PATH.
func fakeBinPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+":/usr/bin:/bin")
}

func TestEnforceHooksNoConfigIsNoop(t *testing.T) {
	minimalPATH(t)
	if err := EnforceHooks(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("EnforceHooks with no hook config: %v", err)
	}
}

func TestEnforceHooksConfiguredButBinaryMissingErrors(t *testing.T) {
	minimalPATH(t)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "lefthook.yml"), []byte("pre-commit:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnforceHooks(context.Background(), wt)
	if err == nil {
		t.Fatal("want error: lefthook.yml present but lefthook not on PATH")
	}
	if !strings.Contains(err.Error(), "lefthook") || !strings.Contains(err.Error(), "PATH") {
		t.Errorf("error should name the missing tool and PATH, got: %v", err)
	}
}

func TestEnforceHooksRunsLefthookAndFailsOnNonZeroExit(t *testing.T) {
	bin := t.TempDir()
	writeFakeBinary(t, bin, "lefthook", 1)
	fakeBinPATH(t, bin)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "lefthook.yml"), []byte("pre-commit:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnforceHooks(context.Background(), wt)
	if err == nil {
		t.Fatal("want error when the lefthook hook exits non-zero")
	}
	if !strings.Contains(err.Error(), "lefthook") {
		t.Errorf("error should name lefthook, got: %v", err)
	}
}

func TestEnforceHooksRunsLefthookSucceeds(t *testing.T) {
	bin := t.TempDir()
	writeFakeBinary(t, bin, "lefthook", 0)
	fakeBinPATH(t, bin)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "lefthook.yml"), []byte("pre-commit:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnforceHooks(context.Background(), wt); err != nil {
		t.Fatalf("EnforceHooks with a passing lefthook hook: %v", err)
	}
}

func TestEnforceHooksRunsPreCommitFramework(t *testing.T) {
	bin := t.TempDir()
	writeFakeBinary(t, bin, "pre-commit", 1)
	fakeBinPATH(t, bin)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".pre-commit-config.yaml"), []byte("repos: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnforceHooks(context.Background(), wt)
	if err == nil {
		t.Fatal("want error when the pre-commit hook exits non-zero")
	}
	if !strings.Contains(err.Error(), "pre-commit") {
		t.Errorf("error should name pre-commit, got: %v", err)
	}
}

func TestRunShipLintEmptyCommandIsNoop(t *testing.T) {
	if err := RunShipLint(context.Background(), t.TempDir(), ""); err != nil {
		t.Fatalf("RunShipLint with no command: %v", err)
	}
	if err := RunShipLint(context.Background(), t.TempDir(), "   "); err != nil {
		t.Fatalf("RunShipLint with a blank command: %v", err)
	}
}

func TestRunShipLintFailingCommandErrors(t *testing.T) {
	err := RunShipLint(context.Background(), t.TempDir(), "echo boom; exit 1")
	if err == nil {
		t.Fatal("want error when ship_lint exits non-zero")
	}
	if !strings.Contains(err.Error(), "ship_lint") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should name ship_lint and include its output, got: %v", err)
	}
}

func TestRunShipLintPassingCommandSucceeds(t *testing.T) {
	if err := RunShipLint(context.Background(), t.TempDir(), "true"); err != nil {
		t.Fatalf("RunShipLint with a passing command: %v", err)
	}
}
