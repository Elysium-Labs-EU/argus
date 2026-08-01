package eventlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

func TestSummarizeAggregatesBlockedOnQuestion(t *testing.T) {
	events := []Event{
		{Run: "r1", Action: "run_summary", Fields: map[string]any{"workers": float64(2), "blocked": float64(1), "blocked_on_question": float64(1)}},
		{Run: "r2", Action: "run_summary", Fields: map[string]any{"workers": float64(1), "blocked": float64(1), "blocked_on_question": float64(0)}},
	}
	s := Summarize(events)
	if s.BlockedOnQuestion != 1 {
		t.Errorf("BlockedOnQuestion: got %d want 1", s.BlockedOnQuestion)
	}
}

func TestReadDirReadsJSONL(t *testing.T) {
	dir := t.TempDir()
	l := New(mustCreate(t, filepath.Join(dir, "run.jsonl")), "supervise", "r1", nil)
	l.Action("gate", "a", "escalate", "")
	l.Action("review", "a", "approve", "")

	events, err := ReadDir(dir, nil)
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

// TestReadDirReportsMalformedLinesUnderDebug pins the issue #391 fix: a
// malformed line is skipped (not a hard failure), but with a non-nil debug
// writer the skip is surfaced instead of silently disappearing.
func TestReadDirReportsMalformedLinesUnderDebug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.jsonl")
	content := `{"run":"r1","action":"gate","target":"a","outcome":"escalate"}
not valid json
{"run":"r1","action":"review","target":"a","outcome":"approve"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var debug bytes.Buffer
	events, err := ReadDir(dir, &debug)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events (bad line skipped, good ones kept), got %d", len(events))
	}
	if got := debug.String(); !strings.Contains(got, "skipped 1 malformed line(s) in "+path) {
		t.Errorf("debug output missing skip report: %q", got)
	}
}

// TestReadDirSilentWithoutDebug pins that a nil debug writer produces no
// report — the pre-fix default behavior for a normal (non-debug) stats run.
func TestReadDirSilentWithoutDebug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.jsonl")
	content := "not valid json\n{\"run\":\"r1\",\"action\":\"gate\",\"target\":\"a\",\"outcome\":\"escalate\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	events, err := ReadDir(dir, nil)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
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
	events, err := ReadDir(filepath.Join(t.TempDir(), "nope"), nil)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("want no events, got %d", len(events))
	}
}

// TestJoinTasksJoinsByRunAndTarget pins the join contract: one row per
// run+target, with model/effort carried from the tokens event and outcome
// fields from the gate/review/verdict/phase events for the same task.
func TestJoinTasksJoinsByRunAndTarget(t *testing.T) {
	events := []Event{
		{Run: "r1", Action: "tokens", Target: "a", Fields: map[string]any{
			"total": float64(1000), "input": float64(100), "output": float64(50),
			"cache_creation": float64(0), "cache_read": float64(900),
			"model": "opus", "effort": "high",
		}},
		{Run: "r1", Action: "gate", Target: "a", Outcome: "escalate"},
		{Run: "r1", Action: "review", Target: "a", Outcome: "approve"},
		{Run: "r1", Action: "verdict", Target: "a", Outcome: "approved"},
		{Run: "r1", Action: "phase", Target: "a", Outcome: "awaiting_review"},
		// Same task label, different run: must not merge into r1's row.
		{Run: "r2", Action: "tokens", Target: "a", Fields: map[string]any{"total": float64(10), "input": float64(10)}},
	}
	rows := JoinTasks(events)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (one per run), got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Run != "r1" || r.Task != "a" {
		t.Fatalf("unexpected first row: %+v", r)
	}
	if r.Model != "opus" || r.Effort != "high" {
		t.Errorf("model/effort not joined: %+v", r)
	}
	if r.TokensTotal != 1000 || r.TokensInput != 100 || r.CacheRead != 900 {
		t.Errorf("token fields not joined: %+v", r)
	}
	if r.GateOutcome != "escalate" || r.ReviewOutcome != "approve" || r.Verdict != "approved" || r.Phase != "awaiting_review" {
		t.Errorf("outcome fields not joined: %+v", r)
	}
	if got, want := r.CacheReadRatio, 9.0; got != want {
		t.Errorf("cache_read/input ratio = %v, want %v", got, want)
	}
}

// TestJoinTasksZeroInputRatioIsZero pins that a task with cache_read but no
// input tokens gets a 0 ratio instead of dividing by zero into Inf/NaN.
func TestJoinTasksZeroInputRatioIsZero(t *testing.T) {
	rows := JoinTasks([]Event{
		{Run: "r1", Action: "tokens", Target: "a", Fields: map[string]any{"cache_read": float64(500)}},
	})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].CacheReadRatio != 0 {
		t.Errorf("CacheReadRatio = %v, want 0", rows[0].CacheReadRatio)
	}
}

// TestJoinTasksIgnoresEventsWithoutTarget pins that run-level events like
// run_summary (no Target) don't produce a spurious empty-task row.
func TestJoinTasksIgnoresEventsWithoutTarget(t *testing.T) {
	rows := JoinTasks([]Event{
		{Run: "r1", Action: "run_summary", Fields: map[string]any{"workers": float64(2)}},
	})
	if len(rows) != 0 {
		t.Errorf("want 0 rows, got %d: %+v", len(rows), rows)
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
