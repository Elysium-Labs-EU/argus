package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// resetDebugLog saves/restores the package-level debugLog flag that --debug
// sets, since tests run in the same process and must not leak state between
// cases.
func resetDebugLog(t *testing.T, v bool) {
	t.Helper()
	prev := debugLog
	debugLog = v
	t.Cleanup(func() { debugLog = prev })
}

func TestOpenRunLog_HomeDirError(t *testing.T) {
	resetDebugLog(t, false)
	t.Setenv("HOME", "")

	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetErr(buf)

	logger, closer := openRunLog(cmd, "test")
	if logger != nil {
		t.Errorf("want nil logger when os.UserHomeDir fails, got %v", logger)
	}
	closer() // must be a safe no-op, not nil
	if buf.Len() != 0 {
		t.Errorf("want nothing written to stderr, got %q", buf.String())
	}
}

func TestOpenRunLog_EventlogOpenError(t *testing.T) {
	resetDebugLog(t, false)
	blocker := filepath.Join(t.TempDir(), "home-is-a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}
	t.Setenv("HOME", blocker)

	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetErr(buf)

	logger, closer := openRunLog(cmd, "test")
	if logger != nil {
		t.Errorf("want nil logger when eventlog.Open fails, got %v", logger)
	}
	closer() // must be a safe no-op, not nil
	if buf.Len() != 0 {
		t.Errorf("want nothing written to stderr on eventlog.Open failure, got %q", buf.String())
	}
}

func TestOpenRunLog_DebugPrintsPathAndTeesEvents(t *testing.T) {
	resetDebugLog(t, true)
	t.Setenv("HOME", t.TempDir())

	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetErr(buf)

	logger, closer := openRunLog(cmd, "test")
	if logger == nil {
		t.Fatal("want non-nil logger on success")
	}
	defer closer()

	printed := buf.String()
	if !strings.HasPrefix(printed, "run log: ") {
		t.Fatalf("want stderr to start with %q, got %q", "run log: ", printed)
	}
	path := strings.TrimSpace(strings.TrimPrefix(printed, "run log: "))
	if !strings.Contains(path, filepath.Join(".argus", "runs")) {
		t.Errorf("printed path not under .argus/runs: %q", path)
	}

	buf.Reset()
	logger.Action("test-action", "target", "outcome", "detail")
	if !strings.Contains(buf.String(), "test-action") {
		t.Errorf("debug writer not wired to the logger: stderr = %q", buf.String())
	}
}

func TestOpenRunLog_NoDebugStaysSilent(t *testing.T) {
	resetDebugLog(t, false)
	t.Setenv("HOME", t.TempDir())

	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetErr(buf)

	logger, closer := openRunLog(cmd, "test")
	if logger == nil {
		t.Fatal("want non-nil logger on success")
	}
	defer closer()

	if buf.Len() != 0 {
		t.Errorf("want no stderr output without --debug, got %q", buf.String())
	}

	logger.Action("test-action", "target", "outcome", "detail")
	if buf.Len() != 0 {
		t.Errorf("want events not teed to stderr without --debug, got %q", buf.String())
	}
}
