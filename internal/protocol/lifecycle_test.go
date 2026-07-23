package protocol

import "testing"

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
