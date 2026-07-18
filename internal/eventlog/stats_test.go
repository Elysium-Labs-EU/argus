package eventlog

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleEvents() []Event {
	return []Event{
		{Run: "r1", Action: "gate", Target: "a", Outcome: "auto-approve"},
		{Run: "r1", Action: "gate", Target: "b", Outcome: "escalate"},
		{Run: "r1", Action: "review", Target: "b", Outcome: "approve"},
		{Run: "r1", Action: "review_reask", Target: "b", Outcome: "parse-retry"},
		{Run: "r1", Action: "verdict", Target: "a", Outcome: "approved"},
		{Run: "r1", Action: "verdict", Target: "b", Outcome: "approved"},
		{Run: "r1", Action: "phase", Target: "a", Outcome: "awaiting_review"},
		{Run: "r1", Action: "tokens", Target: "a", Fields: map[string]any{"total": float64(1000)}},
		{Run: "r1", Action: "tokens", Target: "b", Fields: map[string]any{"total": float64(2000)}},
		{Run: "r1", Action: "run_summary", Fields: map[string]any{"workers": float64(2)}},
		{Run: "r2", Action: "gate", Target: "c", Outcome: "escalate"},
		{Run: "r2", Action: "review", Target: "c", Outcome: "request-changes"},
		{Run: "r2", Action: "verdict", Target: "c", Outcome: "not-approved"},
	}
}

func TestSummarizeCountsAndRates(t *testing.T) {
	s := Summarize(sampleEvents())

	if s.Runs != 2 {
		t.Errorf("runs: got %d want 2", s.Runs)
	}
	if s.Workers != 2 {
		t.Errorf("workers: got %d want 2", s.Workers)
	}
	if s.GateAutoApprove != 1 || s.GateEscalate != 2 {
		t.Errorf("gate: auto=%d esc=%d want 1/2", s.GateAutoApprove, s.GateEscalate)
	}
	// 2 escalate of 3 gate decisions.
	if got := s.EscalationRate(); got < 0.66 || got > 0.67 {
		t.Errorf("escalation rate: got %.3f want ~0.667", got)
	}
	// 1 re-ask of 2 reviews.
	if got := s.ReAskRate(); got != 0.5 {
		t.Errorf("re-ask rate: got %.3f want 0.5", got)
	}
	if s.Approved != 2 || s.NotApproved != 1 {
		t.Errorf("verdicts: approved=%d not=%d want 2/1", s.Approved, s.NotApproved)
	}
	if s.TokensByTask["a"] != 1000 || s.TokensByTask["b"] != 2000 {
		t.Errorf("tokens: %+v", s.TokensByTask)
	}
	if s.ReviewDecisions["approve"] != 1 || s.ReviewDecisions["request-changes"] != 1 {
		t.Errorf("review decisions: %+v", s.ReviewDecisions)
	}
	if s.Phases["awaiting_review"] != 1 {
		t.Errorf("phases: %+v", s.Phases)
	}
}

func TestReadDirReadsJSONL(t *testing.T) {
	dir := t.TempDir()
	l := New(mustCreate(t, filepath.Join(dir, "run.jsonl")), "supervise", "r1", nil)
	l.Action("gate", "a", "escalate", "")
	l.Action("review", "a", "approve", "")

	events, err := ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	s := Summarize(events)
	if s.GateEscalate != 1 || s.Reviews != 1 {
		t.Errorf("unexpected summary: %+v", s)
	}
}

func TestRatesHandleZeroDenominators(t *testing.T) {
	var empty Stats
	if empty.EscalationRate() != 0 {
		t.Error("escalation rate with no gate decisions should be 0")
	}
	if empty.ReAskRate() != 0 {
		t.Error("re-ask rate with no reviews should be 0")
	}
}

func TestInt64FieldMissingAndNonNumeric(t *testing.T) {
	if int64Field(nil, "x") != 0 {
		t.Error("missing key should yield 0")
	}
	if int64Field(map[string]any{"x": "not a number"}, "x") != 0 {
		t.Error("non-numeric value should yield 0")
	}
	if int64Field(map[string]any{"x": float64(42)}, "x") != 42 {
		t.Error("float64 value should decode to its int64")
	}
}

func TestReadDirMissingIsEmpty(t *testing.T) {
	events, err := ReadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want no events, got %d", len(events))
	}
}

func mustCreate(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
