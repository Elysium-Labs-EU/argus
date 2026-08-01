package protocol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLifecycleRoundTrips(t *testing.T) {
	wt := t.TempDir()
	in := &Lifecycle{
		State: LifecycleShipped, Host: "codeberg.org", Owner: "o", Repo: "r",
		Branch: "feat-x", PRURL: "https://codeberg.org/o/r/pulls/7", PRNumber: 7,
	}
	if err := WriteLifecycle(wt, in); err != nil {
		t.Fatalf("WriteLifecycle: %v", err)
	}
	got, found, err := LoadLifecycle(wt)
	if err != nil || !found {
		t.Fatalf("LoadLifecycle: found=%v err=%v", found, err)
	}
	if got.State != LifecycleShipped || got.PRNumber != 7 || got.PRURL != in.PRURL {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("WriteLifecycle should stamp UpdatedAt")
	}
}

func TestLoadLifecycleMissingIsNotFound(t *testing.T) {
	_, found, err := LoadLifecycle(t.TempDir())
	if err != nil {
		t.Fatalf("missing lifecycle should not error: %v", err)
	}
	if found {
		t.Error("found should be false when no lifecycle was written")
	}
}

func TestWriteLifecycleUnwritableDir(t *testing.T) {
	wt := t.TempDir()
	if err := os.Chmod(wt, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt, 0o700) }) // let t.TempDir's own cleanup remove it

	if err := WriteLifecycle(wt, &Lifecycle{State: LifecycleShipped}); err == nil {
		t.Fatal("want error writing lifecycle under a read-only worktree, got nil")
	}
}

func TestLoadLifecycleMalformedJSON(t *testing.T) {
	wt := t.TempDir()
	path := LifecyclePath(wt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadLifecycle(wt); err == nil {
		t.Fatal("want error decoding malformed lifecycle, got nil")
	}
}
