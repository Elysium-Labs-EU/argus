package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// planEvidenceTestWorktree is the fixed worktree path every HasPlanEvidence
// test below encodes into a transcript directory name.
const planEvidenceTestWorktree = "/repo/.claude/worktrees/fix-issue-103"

// writePlanTranscript creates
// home/.claude/projects/<encoded planEvidenceTestWorktree>/<name>.jsonl with
// the given content, mirroring the directory layout Claude Code itself writes
// session transcripts under.
func writePlanTranscript(t *testing.T, home, name, content string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(planEvidenceTestWorktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func TestHasPlanEvidenceFindsTodoWriteToolCall(t *testing.T) {
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true for a transcript with a real TodoWrite tool call")
	}
	if transcripts != 1 {
		t.Errorf("transcripts checked = %d, want 1", transcripts)
	}
}

func TestHasPlanEvidenceFindsTaskCreateToolCall(t *testing.T) {
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TaskCreate","input":{}}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true for a transcript with a real TaskCreate tool call")
	}
	if transcripts != 1 {
		t.Errorf("transcripts checked = %d, want 1", transcripts)
	}
}

func TestHasPlanEvidenceFalseWhenMarkerOnlyInProse(t *testing.T) {
	// The exact fakeable case: the marker string appears in assistant text,
	// not inside a real tool_use block, so it must not count as evidence.
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I won't bother calling \"name\":\"TodoWrite\""}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false when the marker only appears in assistant prose, not a real tool_use block")
	}
	if transcripts != 1 {
		t.Errorf("transcripts checked = %d, want 1", transcripts)
	}
}

func TestHasPlanEvidenceFalseWhenNoMatchingToolCall(t *testing.T) {
	home := t.TempDir()
	// A transcript exists but never calls a todo/task tool — the exact issue
	// #103 case: a worker's brief said to write a todo list, and it didn't.
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false when no transcript line mentions TodoWrite/TaskCreate")
	}
	if transcripts != 1 {
		t.Errorf("transcripts checked = %d, want 1", transcripts)
	}
}

func TestHasPlanEvidenceFalseWhenNoTranscriptDirectory(t *testing.T) {
	home := t.TempDir()
	ok, transcripts, err := HasPlanEvidence(home, "/repo/.claude/worktrees/never-ran")
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false when the project's transcript directory doesn't exist")
	}
	if transcripts != 0 {
		t.Errorf("transcripts checked = %d, want 0 when the transcript directory doesn't exist", transcripts)
	}
}

func TestHasPlanEvidenceChecksEveryTranscriptForTheWorktree(t *testing.T) {
	// A worktree can span more than one worker session (e.g. an initial
	// implementation run and a later review-feedback follow-up) — the earlier
	// session carries the evidence, a later one doesn't, and either must count.
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}`+"\n")
	writePlanTranscript(t, home, "session-2",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true when any transcript for the worktree has the evidence")
	}
	if transcripts != 2 {
		t.Errorf("transcripts checked = %d, want 2", transcripts)
	}
}

func TestTranscriptContainsAnyReturnsErrorWhenTranscriptCannotBeOpened(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	_, err := transcriptContainsAny(missing, planEvidenceMarkers)
	if err == nil {
		t.Fatal("transcriptContainsAny err = nil, want error for a transcript path that doesn't exist")
	}
	if !strings.Contains(err.Error(), "opening transcript") {
		t.Errorf("err = %v, want it to mention opening the transcript", err)
	}
}

func TestTranscriptContainsAnyReturnsErrorWhenLineExceedsScannerBuffer(t *testing.T) {
	// bufio.Scanner caps a single token (one line, no embedded newline) at the
	// 8MB buffer transcriptContainsAny configures; a longer line must surface
	// as a scan error rather than being silently skipped like malformed JSON.
	path := filepath.Join(t.TempDir(), "huge.jsonl")
	huge := bytes.Repeat([]byte("a"), 9*1024*1024)
	if err := os.WriteFile(path, huge, 0o600); err != nil {
		t.Fatalf("write huge transcript: %v", err)
	}
	_, err := transcriptContainsAny(path, planEvidenceMarkers)
	if err == nil {
		t.Fatal("transcriptContainsAny err = nil, want error for a line exceeding the scanner buffer")
	}
	if !strings.Contains(err.Error(), "scanning transcript") {
		t.Errorf("err = %v, want it to mention scanning the transcript", err)
	}
}

func TestHasPlanEvidenceReturnsErrorWithTranscriptCountWhenATranscriptIsUnreadable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(planEvidenceTestWorktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	// A directory matching the *.jsonl glob opens fine but fails to scan as a
	// file, forcing HasPlanEvidence down its transcriptContainsAny-error path
	// instead of the open-error path exercised above.
	if err := os.Mkdir(filepath.Join(dir, "session-1.jsonl"), 0o755); err != nil {
		t.Fatalf("mkdir fake transcript: %v", err)
	}
	writePlanTranscript(t, home, "session-2",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err == nil {
		t.Fatal("HasPlanEvidence err = nil, want error when a matched transcript can't be read")
	}
	if ok {
		t.Error("HasPlanEvidence ok = true, want false alongside an error")
	}
	if transcripts != 2 {
		t.Errorf("transcripts checked = %d, want 2 (len(matches) reported even on error)", transcripts)
	}
}

func TestHasPlanEvidenceShortCircuitsOnFirstMatchWithoutScanningLaterTranscripts(t *testing.T) {
	home := t.TempDir()
	writePlanTranscript(t, home, "session-1",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"TodoWrite","input":{}}]}}`+"\n")
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(planEvidenceTestWorktree))
	// session-2 would surface a scan error if HasPlanEvidence ever reached it;
	// a clean true result here proves the loop returned right after session-1.
	if err := os.Mkdir(filepath.Join(dir, "session-2.jsonl"), 0o755); err != nil {
		t.Fatalf("mkdir fake transcript: %v", err)
	}

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if !ok {
		t.Error("HasPlanEvidence = false, want true for the first-checked transcript's real tool call")
	}
	if transcripts != 2 {
		t.Errorf("transcripts checked = %d, want 2 (both matches, even though only one was scanned)", transcripts)
	}
}

func TestHasPlanEvidenceFalseForPrettyPrintedToolUseBlock(t *testing.T) {
	// transcriptContainsAny parses one JSON object per line; a pretty-printed
	// (multi-line) tool_use block never round-trips as a single line of valid
	// JSON, so this pins the compact-JSON assumption rather than treating it
	// as an accidental gap.
	home := t.TempDir()
	pretty := `{
  "type": "assistant",
  "message": {
    "content": [
      {
        "type": "tool_use",
        "name": "TodoWrite"
      }
    ]
  }
}
`
	writePlanTranscript(t, home, "session-1", pretty)

	ok, transcripts, err := HasPlanEvidence(home, planEvidenceTestWorktree)
	if err != nil {
		t.Fatalf("HasPlanEvidence: %v", err)
	}
	if ok {
		t.Error("HasPlanEvidence = true, want false for a pretty-printed tool_use block split across lines")
	}
	if transcripts != 1 {
		t.Errorf("transcripts checked = %d, want 1", transcripts)
	}
}

func TestGateEscalatesWhenPlanEvidenceMissing(t *testing.T) {
	st := &workerState{
		hasFile:         true,
		measuredOK:      true,
		measured:        protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:   []string{"cmd/root.go"},
		planEvidenceOK:  true,
		hasPlanEvidence: false,
		plan:            &WorkerPlan{Worker: Worker{Task: "no-todo-list"}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Plan:  []string{"claims a plan"}, // self-reported, but no matching tool call
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve a worker with no plan/todo evidence in its transcript")
	}
	if !hasReasonContaining(v.Reasons, "no TodoWrite/TaskCreate tool call") {
		t.Errorf("expected a missing-plan-evidence reason, got %v", v.Reasons)
	}
}

func TestGateEscalatesWhenPlanEvidenceUnverifiable(t *testing.T) {
	st := &workerState{
		hasFile:         true,
		measuredOK:      true,
		measured:        protocol.DiffStat{Files: 1, Insertions: 3},
		measuredFiles:   []string{"cmd/root.go"},
		planEvidenceErr: os.ErrPermission,
		plan:            &WorkerPlan{Worker: Worker{Task: "unreadable-transcript"}},
		status: protocol.Status{
			Phase: protocol.PhaseAwaitingReview,
			Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}},
		},
	}
	v := gateVerdict(st, nil)
	if v.AutoApprove {
		t.Fatal("gate must not auto-approve when it could not verify plan evidence")
	}
	if !hasReasonContaining(v.Reasons, "could not verify plan evidence") {
		t.Errorf("expected an unverifiable reason, got %v", v.Reasons)
	}
}

func TestUsesDefaultLauncher(t *testing.T) {
	cases := []struct {
		launcher string
		want     bool
	}{
		{"", true},
		{DefaultLauncher, true},
		{"codex --full-auto", false},
		{"claude", false}, // a real but non-default launcher string still counts as an override
	}
	for _, c := range cases {
		if got := usesDefaultLauncher(c.launcher); got != c.want {
			t.Errorf("usesDefaultLauncher(%q) = %v, want %v", c.launcher, got, c.want)
		}
	}
}

// TestReconcileSkipsPlanEvidenceForNonDefaultLauncher is a regression test: a
// worker spawned with a non-Claude-Code --launcher can
// never produce a ~/.claude/projects transcript, so reconcile must not judge
// it against one — even when a transcript happens to exist (e.g. left over
// from an unrelated Claude Code session in the same worktree) and it carries
// no plan evidence, which is exactly the state that used to force a
// permanent escalation.
func TestReconcileSkipsPlanEvidenceForNonDefaultLauncher(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	noEvidence := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(noEvidence), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	states := []*workerState{{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "t", Worktree: wt}}}}
	cfg := &Config{Base: "HEAD", Home: home, Launcher: "codex --full-auto"}
	reconcile(context.Background(), cfg, states)

	if states[0].planEvidenceOK {
		t.Error("planEvidenceOK should stay false for a non-default launcher — plan evidence is not applicable, not checked")
	}
	if states[0].hasPlanEvidence {
		t.Error("hasPlanEvidence should stay false — HasPlanEvidence must not have run")
	}

	states[0].status = protocol.Status{Phase: protocol.PhaseAwaitingReview, Tests: []protocol.TestRun{{Cmd: "go test", Result: protocol.ResultPass}}}
	v := gateVerdict(states[0], nil)
	if hasReasonContaining(v.Reasons, "plan evidence") || hasReasonContaining(v.Reasons, "TodoWrite") {
		t.Errorf("gate must not escalate a non-default-launcher worker for missing plan evidence, got %v", v.Reasons)
	}
}

// TestReconcileChecksPlanEvidenceForDefaultLauncher is the control for
// TestReconcileSkipsPlanEvidenceForNonDefaultLauncher: the default (or
// unset) launcher is a real Claude Code session, so reconcile must still run
// HasPlanEvidence and let the gate escalate on a genuine missing-evidence
// transcript.
func TestReconcileChecksPlanEvidenceForDefaultLauncher(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	noEvidence := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(noEvidence), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	states := []*workerState{{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "t", Worktree: wt}}}}
	cfg := &Config{Base: "HEAD", Home: home} // Launcher unset -> default
	reconcile(context.Background(), cfg, states)

	if !states[0].planEvidenceOK {
		t.Fatal("planEvidenceOK should be true — HasPlanEvidence must run for the default launcher")
	}
	if states[0].hasPlanEvidence {
		t.Error("hasPlanEvidence should be false — the transcript has no TodoWrite/TaskCreate call")
	}
}

// TestReconcileLogsTranscriptCountOnMissingPlanEvidence guards the diagnostic
// this issue exists for: a "no evidence" verdict alone can't tell a
// zero-transcript miss (wrong home, worker never started a session) apart
// from a real grep miss against a transcript that did exist. The run log
// must carry the transcript count so a recurrence of the soft gate flag can
// be pattern-matched from the log alone.
func TestReconcileLogsTranscriptCountOnMissingPlanEvidence(t *testing.T) {
	wt := gitWorktreeWithDiff(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(wt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	noEvidence := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "session-1.jsonl"), []byte(noEvidence), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var buf bytes.Buffer
	states := []*workerState{{hasFile: true, plan: &WorkerPlan{Worker: Worker{Task: "t", Worktree: wt}}}}
	cfg := &Config{Base: "HEAD", Home: home, Log: eventlog.New(&buf, "supervise", "run1", nil)}
	reconcile(context.Background(), cfg, states)

	var evt struct {
		Fields  map[string]any `json:"fields"`
		Action  string         `json:"action"`
		Outcome string         `json:"outcome"`
	}
	found := false
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal event line %q: %v", line, err)
		}
		if evt.Action == "plan_evidence" && evt.Outcome == "not-found" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a plan_evidence/not-found event, got log:\n%s", buf.String())
	}
	if got := evt.Fields["transcripts_checked"]; got != float64(1) {
		t.Errorf("transcripts_checked = %v, want 1", got)
	}
}

func TestGateDoesNotRequirePlanEvidenceCheckForNonTerminalPhase(t *testing.T) {
	// planEvidenceOK/hasPlanEvidence are both zero-valued (unchecked) here —
	// mirroring how the diff checks stay silent until measuredOK is set, the
	// plan-evidence check must not fire for a worker still mid-flight.
	st := &workerState{
		hasFile: true,
		plan:    &WorkerPlan{Worker: Worker{Task: "still-working"}},
		status:  protocol.Status{Phase: protocol.PhaseWorking},
	}
	v := gateVerdict(st, nil)
	if hasReasonContaining(v.Reasons, "plan evidence") || hasReasonContaining(v.Reasons, "TodoWrite") {
		t.Errorf("plan-evidence check must not run for a non-terminal phase, got %v", v.Reasons)
	}
}
