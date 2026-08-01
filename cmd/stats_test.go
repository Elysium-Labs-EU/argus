package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRunLog writes a minimal jsonl run log under home/.argus/runs so
// newStatsCmd's --export path has something to join.
func writeRunLog(t *testing.T, home, lines string) {
	t.Helper()
	dir := filepath.Join(home, ".argus", "runs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStatsExportJoinsTokensAndOutcome pins the --export contract: one CSV
// row per task, joining token spend, model/effort, and gate/review/verdict/
// phase outcome by run+target — the row a manual dispatch-tuning pass reads
// instead of hand-correlating the separate event types in the jsonl.
func TestStatsExportJoinsTokensAndOutcome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, strings.Join([]string{
		`{"run":"r1","action":"tokens","target":"task-a","fields":{"total":1000,"input":100,"output":50,"cache_creation":0,"cache_read":900,"model":"opus","effort":"high"}}`,
		`{"run":"r1","action":"gate","target":"task-a","outcome":"escalate"}`,
		`{"run":"r1","action":"review","target":"task-a","outcome":"approve"}`,
		`{"run":"r1","action":"verdict","target":"task-a","outcome":"approved"}`,
		`{"run":"r1","action":"phase","target":"task-a","outcome":"awaiting_review"}`,
		"",
	}, "\n"))

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--export"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := out.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + 1 row, got %d lines:\n%s", len(lines), got)
	}
	if lines[0] != "run,task,model,effort,tokens_total,tokens_input,tokens_output,cache_creation,cache_read,cache_read_ratio,gate,review,verdict,phase" {
		t.Errorf("unexpected header: %q", lines[0])
	}
	want := "r1,task-a,opus,high,1000,100,50,0,900,9.0000,escalate,approve,approved,awaiting_review"
	if lines[1] != want {
		t.Errorf("row = %q, want %q", lines[1], want)
	}
}

// TestStatsExportZeroInputRatioDoesNotPanic pins that a task with cache_read
// but no input tokens gets a 0 ratio rather than dividing by zero into Inf/NaN.
func TestStatsExportZeroInputRatioDoesNotPanic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, `{"run":"r1","action":"tokens","target":"task-a","fields":{"total":500,"input":0,"cache_read":500}}`+"\n")

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--export"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + 1 row, got %d lines:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], ",0.0000,") {
		t.Errorf("expected a 0 cache_read_ratio for zero input tokens, got %q", lines[1])
	}
}

// TestStatsWithoutExportStillPrintsSummary pins that the default (no
// --export) path is unchanged: the aggregate summary, not CSV.
func TestStatsWithoutExportStillPrintsSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, `{"run":"r1","action":"gate","target":"task-a","outcome":"auto-approve"}`+"\n")

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "argus stats") {
		t.Errorf("expected the summary render, got %q", out.String())
	}
}
