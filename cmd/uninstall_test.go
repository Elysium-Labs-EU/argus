package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "argus")
	if err := os.WriteFile(path, []byte("fake"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestRunUninstallDeclinedLeavesEverything(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader("n\n"), buf, exePath, dataDir, false, false); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}

	if _, err := os.Stat(exePath); err != nil {
		t.Errorf("expected binary to survive a declined confirmation, stat err: %v", err)
	}
}

func TestRunUninstallYesRemovesBinaryLeavesData(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, dataDir, true, false); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}

	if _, err := os.Stat(exePath); !os.IsNotExist(err) {
		t.Errorf("expected binary to be removed, stat err: %v", err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Errorf("expected data dir to survive without --purge, stat err: %v", err)
	}
	if !strings.Contains(buf.String(), "rm -rf") {
		t.Errorf("output = %q, want a manual-cleanup hint", buf.String())
	}
}

// TestRunUninstallBothPromptsAnsweredYes exercises both Confirm prompts
// (binary removal, then data removal) answered together in one input stream
// — the exact path that regresses if Confirm ever goes back to wrapping a
// fresh bufio.Reader per call instead of sharing one across a runUninstall
// invocation: the second "y" would get stranded in the first call's read-ahead
// buffer and silently default to false.
func TestRunUninstallBothPromptsAnsweredYes(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader("y\ny\n"), buf, exePath, dataDir, false, false); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}

	if _, err := os.Stat(exePath); !os.IsNotExist(err) {
		t.Errorf("expected binary to be removed, stat err: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("expected data dir to be removed when the second prompt is answered yes, stat err: %v", err)
	}
}

func TestRunUninstallPurgeRemovesData(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, dataDir, true, true); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}

	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("expected data dir to be removed with --purge, stat err: %v", err)
	}
}

func TestRunUninstallMissingBinaryIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "already-gone")
	dataDir := filepath.Join(dir, "data")

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, dataDir, true, false); err != nil {
		t.Fatalf("runUninstall on missing binary: %v", err)
	}
}

func TestRunUninstallDataDirAbsentPrintsNoHint(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "does-not-exist")

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, dataDir, true, false); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}
	if strings.Contains(buf.String(), "rm -rf") {
		t.Errorf("output = %q, want no manual-cleanup hint when data dir was never there", buf.String())
	}
}

func TestRunUninstallRemoveExeFailsNotNotExist(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory makes os.Remove fail with ENOTEMPTY, not
	// ErrNotExist — the branch runUninstall must still surface as an error.
	exePath := filepath.Join(dir, "exe-dir")
	if err := os.MkdirAll(exePath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(exePath, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	buf := &bytes.Buffer{}
	err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, filepath.Join(dir, "data"), true, false)
	if err == nil {
		t.Fatal("expected an error removing a non-empty exePath")
	}
	if !strings.Contains(err.Error(), "removing "+exePath) {
		t.Errorf("error = %q, want it to mention removing %s", err, exePath)
	}
}

func TestRunUninstallRemoveDataDirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the permission check this test relies on")
	}
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	protected := filepath.Join(dataDir, "protected")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(protected, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(protected, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// Registered after t.TempDir()'s own cleanup, so it runs first (LIFO)
	// and restores permissions before TempDir tries to remove the tree.
	t.Cleanup(func() { _ = os.Chmod(protected, 0o755) })

	buf := &bytes.Buffer{}
	err := runUninstall(context.Background(), strings.NewReader(""), buf, exePath, dataDir, true, true)
	if err == nil {
		t.Fatal("expected an error removing a dataDir with an unreadable subdirectory")
	}
	if !strings.Contains(err.Error(), "removing "+dataDir) {
		t.Errorf("error = %q, want it to mention removing %s", err, dataDir)
	}
}

func TestRunUninstallPurgeDoesNotOverrideDeclinedBinaryConfirm(t *testing.T) {
	dir := t.TempDir()
	exePath := writeFakeBinary(t, dir)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := runUninstall(context.Background(), strings.NewReader("n\n"), buf, exePath, dataDir, false, true); err != nil {
		t.Fatalf("runUninstall: %v", err)
	}
	if !strings.Contains(buf.String(), "Canceled.") {
		t.Errorf("output = %q, want Canceled.", buf.String())
	}
	if _, err := os.Stat(exePath); err != nil {
		t.Errorf("expected binary to survive a declined confirmation, stat err: %v", err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Errorf("--purge must not remove data when the binary-removal prompt is declined, stat err: %v", err)
	}
}

func TestArgusDataDirSuccess(t *testing.T) {
	got, err := argusDataDir()
	if err != nil {
		t.Fatalf("argusDataDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if want := filepath.Join(home, ".argus"); got != want {
		t.Errorf("argusDataDir() = %q, want %q", got, want)
	}
}

func TestArgusDataDirHomeUnset(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := argusDataDir(); err == nil {
		t.Fatal("expected argusDataDir to fail when $HOME is unset")
	}
}

func TestNewUninstallCmdRunEDeclinedNeverTouchesRealBinary(t *testing.T) {
	cmd := newUninstallCmd()
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(buf.String(), "Canceled.") {
		t.Errorf("output = %q, want Canceled. (empty stdin must decline, not touch the running test binary)", buf.String())
	}
}

func TestNewUninstallCmdRunEArgusDataDirError(t *testing.T) {
	t.Setenv("HOME", "")
	cmd := newUninstallCmd()
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error when $HOME is unset")
	}
	if !strings.Contains(err.Error(), "resolving argus data dir") {
		t.Errorf("error = %q, want it to mention resolving argus data dir", err)
	}
}
