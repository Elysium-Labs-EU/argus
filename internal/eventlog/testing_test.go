package eventlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenForTestClosesLogOnCleanup proves the t.Cleanup registered inside
// OpenForTest actually invokes closeFn, not merely that it runs without
// panicking. Reading the run log back after the subtest ends can't tell us
// this directly — t.TempDir()'s own cleanup removes the directory in the
// same LIFO teardown, so the file is gone either way. Calling closer a
// second time does: closing an already-closed *os.File is the one operation
// that only errors if the automatic cleanup got there first.
func TestOpenForTestClosesLogOnCleanup(t *testing.T) {
	var closer func() error
	t.Run("inner", func(t *testing.T) {
		_, _, closer = OpenForTest(t)
	})

	if err := closer(); err == nil {
		t.Fatal("want error re-closing after t.Cleanup, got nil — cleanup did not close the file")
	}
}

func TestOpenForTestCallsProduceIndependentLogs(t *testing.T) {
	tests := []struct {
		name   string
		action string
	}{
		{name: "first call", action: "spawn"},
		{name: "second call", action: "gate"},
		{name: "third call", action: "ship"},
	}

	seen := map[string]bool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, path, closer := OpenForTest(t)
			if l == nil {
				t.Fatal("want non-nil logger")
			}
			if !strings.Contains(path, filepath.Join(".argus", "runs")) {
				t.Errorf("run log not under .argus/runs: %s", path)
			}
			if seen[path] {
				t.Errorf("path reused across calls: %s", path)
			}
			seen[path] = true

			l.Action(tt.action, "t", "ok", "")
			if err := closer(); err != nil {
				t.Fatalf("closer: %v", err)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading run log: %v", err)
			}
			if !strings.Contains(string(data), tt.action) {
				t.Errorf("run log missing action %q: %q", tt.action, data)
			}
		})
	}
}
