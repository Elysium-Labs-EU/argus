package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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

// TestStatsDebugReportsMalformedLine pins issue #391: with the global --debug
// flag set, a malformed run-log line is reported on stderr instead of
// disappearing silently, while the valid line on either side is still
// counted.
func TestStatsDebugReportsMalformedLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, strings.Join([]string{
		`{"run":"r1","action":"gate","target":"task-a","outcome":"auto-approve"}`,
		"not valid json",
		`{"run":"r1","action":"gate","target":"task-b","outcome":"escalate"}`,
		"",
	}, "\n"))

	debugLog = true
	t.Cleanup(func() { debugLog = false })

	cmd := newStatsCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(errOut.String(), "skipped 1 malformed line(s) in ") {
		t.Errorf("expected a skip report on stderr, got %q", errOut.String())
	}
	if !strings.Contains(out.String(), "1 auto-approve, 1 escalate") {
		t.Errorf("expected both valid lines counted, got %q", out.String())
	}
}

// tokensLine builds a "tokens" run-log line for a distinct task under run
// "r1", so tests can cheaply generate a per-task list longer than the
// default cap.
func tokensLine(task string, total int) string {
	return `{"run":"r1","action":"tokens","target":"` + task + `","fields":{"total":` + strconv.Itoa(total) + `}}`
}

// TestStatsDefaultCapsTaskListAt20 pins issue #390's fix: with no --limit/
// --all flag, a run history with more than 20 distinct tasks prints only the
// top 20 by spend plus an omitted-count footer, not an unbounded dump.
func TestStatsDefaultCapsTaskListAt20(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lines := make([]string, 0, 25)
	for i := range 25 {
		lines = append(lines, tokensLine("task-"+strconv.Itoa(i), 1000-i))
	}
	writeRunLog(t, home, strings.Join(lines, "\n")+"\n")

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if strings.Count(got, "· task-") != 20 {
		t.Errorf("expected exactly 20 task lines by default, got %d:\n%s", strings.Count(got, "· task-"), got)
	}
	if !strings.Contains(got, "5 more task(s) omitted (--limit 25 or --all to see them)") {
		t.Errorf("expected an omitted-count footer, got:\n%s", got)
	}
}

// TestStatsAllFlagShowsEveryTask pins that --all disables the cap entirely.
func TestStatsAllFlagShowsEveryTask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lines := make([]string, 0, 25)
	for i := range 25 {
		lines = append(lines, tokensLine("task-"+strconv.Itoa(i), 1000-i))
	}
	writeRunLog(t, home, strings.Join(lines, "\n")+"\n")

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if strings.Count(got, "· task-") != 25 {
		t.Errorf("expected all 25 task lines with --all, got %d:\n%s", strings.Count(got, "· task-"), got)
	}
	if strings.Contains(got, "omitted") {
		t.Errorf("no omitted footer expected with --all:\n%s", got)
	}
}

// TestStatsLimitFlagOverridesDefault pins that an explicit --limit N caps
// the list at N instead of the built-in default of 20.
func TestStatsLimitFlagOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, strings.Join([]string{
		tokensLine("task-a", 300),
		tokensLine("task-b", 200),
		tokensLine("task-c", 100),
		"",
	}, "\n"))

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--limit", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if strings.Count(got, "· task-") != 1 {
		t.Errorf("expected exactly 1 task line with --limit 1, got %d:\n%s", strings.Count(got, "· task-"), got)
	}
	if !strings.Contains(got, "task-a") {
		t.Errorf("expected the highest-spend task to survive the cap:\n%s", got)
	}
}

// TestStatsLimitZeroWithoutAllErrors pins that --limit 0 (or negative)
// without --all is rejected rather than silently printing nothing or falling
// back to the default — the same "reject, don't silently default" contract
// argus already applies to rework's --max-rounds.
func TestStatsLimitZeroWithoutAllErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRunLog(t, home, tokensLine("task-a", 100)+"\n")

	cmd := newStatsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--limit", "0"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected --limit 0 without --all to error, got output:\n%s", out.String())
	}
}
