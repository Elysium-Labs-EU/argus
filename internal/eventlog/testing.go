package eventlog

import "testing"

// OpenForTest opens a run log under t.TempDir() instead of a caller-supplied home,
// so a test cannot accidentally resolve the developer's real ~/.argus/runs (the
// mistake this guards against: a test-only home string that silently defaults to
// the real one). The CI gate in scripts/check-eventlog-open.sh fails the build on
// any _test.go file that calls Open directly, so this is the only path tests have
// to a Logger.
func OpenForTest(t *testing.T) (logger *Logger, path string, closer func() error) {
	t.Helper()
	l, p, closeFn, err := Open(t.TempDir(), "test", nil)
	if err != nil {
		// t.TempDir() always returns a fresh, writable directory, so Open's
		// MkdirAll/OpenFile calls cannot fail here — this branch is a permanent
		// coverage gap, not a missing test.
		t.Fatalf("eventlog.OpenForTest: %v", err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return l, p, closeFn
}
