package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestRunWorkerReportRejectsIllegalTransition(t *testing.T) {
	wt := t.TempDir()
	// No prior status.json: cur.Phase is "", so only planning is legal.
	err := runWorkerReport(wt, protocol.PhaseAwaitingReview, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error for an illegal first transition, got nil")
	}
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Errorf("error = %q, want it to mention the illegal transition", err.Error())
	}
	if _, loadErr := protocol.Load(protocol.StatusPath(wt)); loadErr == nil {
		t.Error("status.json should not have been written for a rejected transition")
	}
}

func TestRunWorkerReportRejectsDone(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhaseAwaitingReview}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseDone, &protocol.Status{}, fixedNow(time.Now())); err == nil {
		t.Fatal("want an error reporting done, got nil")
	}
}

func TestRunWorkerReportStampsArgusClockNotCallers(t *testing.T) {
	wt := t.TempDir()
	callerClaimed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	argusNow := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	body := protocol.Status{
		UpdatedAt: callerClaimed,
		Task:      "argus#92",
		Branch:    "fix-issue-92",
	}
	if err := runWorkerReport(wt, protocol.PhasePlanning, &body, fixedNow(argusNow)); err != nil {
		t.Fatalf("legal transition rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.UpdatedAt.Equal(argusNow) {
		t.Errorf("UpdatedAt = %v, want argus's clock %v (caller-claimed %v must be ignored)", got.UpdatedAt, argusNow, callerClaimed)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("Phase = %q, want %q", got.Phase, protocol.PhasePlanning)
	}
	if got.Task != "argus#92" || got.Branch != "fix-issue-92" {
		t.Errorf("rest of the body was not persisted: %+v", got)
	}
}

func TestRunWorkerReportRejectsWorkingWithoutPlanEvidence(t *testing.T) {
	wt := t.TempDir()
	// Planning report on file has no plan array — the real-world case of a
	// worker that reported the planning phase but never actually wrote a
	// todo list.
	seed := protocol.Status{Phase: protocol.PhasePlanning}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now()))
	if err == nil {
		t.Fatal("want an error moving planning -> working with no plan evidence, got nil")
	}
	if !strings.Contains(err.Error(), "no plan/todo evidence") {
		t.Errorf("error = %q, want it to mention missing plan evidence", err.Error())
	}
	got, loadErr := protocol.Load(protocol.StatusPath(wt))
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("status.json Phase = %q, want unchanged planning (rejected transition must not persist)", got.Phase)
	}
}

func TestRunWorkerReportAcceptsWorkingWithPlanEvidence(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Phase: protocol.PhasePlanning, Plan: []string{"read the brief", "write the fix"}}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}
	if err := runWorkerReport(wt, protocol.PhaseWorking, &protocol.Status{}, fixedNow(time.Now())); err != nil {
		t.Fatalf("legal transition with plan evidence rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseWorking {
		t.Errorf("Phase = %q, want working", got.Phase)
	}
}

func TestRunWorkerReportFullLegalSequence(t *testing.T) {
	wt := t.TempDir()
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	seq := []protocol.Phase{
		protocol.PhasePlanning,
		protocol.PhaseWorking,
		protocol.PhaseSelfTest,
		protocol.PhaseAwaitingReview,
	}
	for _, phase := range seq {
		now = now.Add(time.Minute)
		body := &protocol.Status{Task: "t", Branch: "b"}
		if phase == protocol.PhasePlanning {
			body.Plan = []string{"read the brief", "write the fix"}
		}
		if err := runWorkerReport(wt, phase, body, fixedNow(now)); err != nil {
			t.Fatalf("transition to %q rejected: %v", phase, err)
		}
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("final phase = %q, want awaiting_review", got.Phase)
	}
}

// TestRunWorkerReportPreservesBaseAcrossReports pins the carry-forward fix in
// runWorkerReport: Base is set once by supervise (never by the worker), and a
// worker's own report body has no "base" key at all, so every subsequent
// report must not clobber it back to empty.
func TestRunWorkerReportPreservesBaseAcrossReports(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Base: "develop"}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b", Plan: []string{"do the thing"}}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Base != "develop" {
		t.Errorf("Base = %q after planning report, want it preserved as %q", got.Base, "develop")
	}

	body2 := &protocol.Status{Task: "t", Branch: "b"}
	if err = runWorkerReport(wt, protocol.PhaseWorking, body2, fixedNow(time.Now())); err != nil {
		t.Fatalf("working report rejected: %v", err)
	}
	got2, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got2.Base != "develop" {
		t.Errorf("Base = %q after working report, want it still preserved as %q", got2.Base, "develop")
	}
}

// TestRunWorkerReportPreservesTitleWhenReportOmitsIt pins the fix for a
// rework round's report (which reasonably describes only that round's fix)
// silently clobbering an earlier, more accurate title: a report with no
// title of its own must not wipe out one already on file.
func TestRunWorkerReportPreservesTitleWhenReportOmitsIt(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Title: "feat: interactive shell-completion installer for argus completion"}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != seed.Title {
		t.Errorf("Title = %q after title-less report, want it preserved as %q", got.Title, seed.Title)
	}
}

// TestRunWorkerReportOverwritesTitleWhenReportSetsIt pins the other half: a
// report that does name a new title is a deliberate retitle and must win.
func TestRunWorkerReportOverwritesTitleWhenReportSetsIt(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{Title: "feat: old title"}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b", Title: "feat: new title"}
	if err := runWorkerReport(wt, protocol.PhasePlanning, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("planning report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != "feat: new title" {
		t.Errorf("Title = %q, want the report's new title %q", got.Title, "feat: new title")
	}
}

// TestRunWorkerReportPreservesAnswerAcrossReports pins the carry-forward
// behavior for the answer trace: a supervisor's `argus worker answer` writes
// Question/Answer directly to status.json, outside runWorkerReport, so the
// worker's own next report body (which never sends either field) must not
// silently clobber that record back to nil.
func TestRunWorkerReportPreservesAnswerAcrossReports(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:         protocol.PhaseBlocked,
		BlockedReason: "which base?",
		Question: &protocol.Question{
			Text:    "wait and rebase, or cherry-pick now?",
			Options: []string{"wait and rebase", "cherry-pick now"},
		},
		Answer: &protocol.Answer{Text: "cherry-pick now", Option: 2},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhaseWorking, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("working report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Question == nil || got.Question.Text != seed.Question.Text {
		t.Errorf("Question = %+v, want it preserved as %+v", got.Question, seed.Question)
	}
	if got.Answer == nil || got.Answer.Text != "cherry-pick now" {
		t.Errorf("Answer = %+v, want it preserved as %+v", got.Answer, seed.Answer)
	}
}

// TestRunWorkerReportFreshQuestionResetsAnswer pins the other half: a worker
// that reports blocked again with a brand-new Question means a new blocked
// cycle started, so any earlier Answer no longer applies to it and must not
// leak forward as if it resolved the new question too.
func TestRunWorkerReportFreshQuestionResetsAnswer(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:  protocol.PhaseWorking,
		Answer: &protocol.Answer{Text: "cherry-pick now", Option: 2},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{
		Task:          "t",
		Branch:        "b",
		BlockedReason: "now which forge?",
		Question:      &protocol.Question{Text: "github or gitlab?"},
	}
	if err := runWorkerReport(wt, protocol.PhaseBlocked, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("blocked report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Question == nil || got.Question.Text != "github or gitlab?" {
		t.Errorf("Question = %+v, want the fresh one", got.Question)
	}
	if got.Answer != nil {
		t.Errorf("Answer = %+v, want it reset to nil for the new question", got.Answer)
	}
}

// TestRunWorkerReportResetsSteersAcrossReports pins the reset half of the
// steer budget: a worker's own report body never carries a "steers" key, and
// runWorkerReport does not carry the prior value forward the way it does
// Base/Title/Question/Answer — so any worker-initiated phase transition
// clears the working leg's steer count, giving the next working leg a fresh
// MaxSteersPerWorking budget instead of accumulating across the worktree's
// whole lifetime.
func TestRunWorkerReportResetsSteersAcrossReports(t *testing.T) {
	wt := t.TempDir()
	seed := protocol.Status{
		Phase:  protocol.PhaseWorking,
		Steers: []protocol.Steer{{Text: "note 1"}, {Text: "note 2"}},
	}
	if err := protocol.Write(protocol.StatusPath(wt), &seed); err != nil {
		t.Fatalf("seeding status: %v", err)
	}

	body := &protocol.Status{Task: "t", Branch: "b"}
	if err := runWorkerReport(wt, protocol.PhaseSelfTest, body, fixedNow(time.Now())); err != nil {
		t.Fatalf("self_test report rejected: %v", err)
	}
	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steers) != 0 {
		t.Errorf("Steers = %+v after the worker's own report, want it reset to empty", got.Steers)
	}
}

func TestParseReportablePhaseRejectsUnknown(t *testing.T) {
	for _, s := range []string{"done", "bogus", ""} {
		if _, err := parseReportablePhase(s); err == nil {
			t.Errorf("parseReportablePhase(%q) = nil error, want rejection", s)
		}
	}
	for _, s := range []string{"planning", "working", "self_test", "awaiting_review", "blocked"} {
		if p, err := parseReportablePhase(s); err != nil || string(p) != s {
			t.Errorf("parseReportablePhase(%q) = %q, %v, want %q, nil", s, p, err, s)
		}
	}
}

// TestWorkerReportCmdStdinBriefCompliance is the brief-compliance smoke test:
// it drives the actual cobra command (as a worker following the brief text in
// protocol.WriterBrief would) with a JSON body piped on stdin, and checks the
// worker's own claimed updated_at never makes it into the persisted file.
func TestWorkerReportCmdStdinBriefCompliance(t *testing.T) {
	wt := t.TempDir()

	body, err := json.Marshal(protocol.Status{
		UpdatedAt:      time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), // a lying worker clock
		Task:           "argus#92",
		Branch:         "fix-issue-92",
		RealWorldProof: "go build ./... && go test ./...",
		FilesTouched:   []string{"cmd/worker_report.go"},
		Tests: []protocol.TestRun{
			{Cmd: "make test", Target: "./...", Result: protocol.ResultPass},
		},
		DiffStat: protocol.DiffStat{Files: 1, Insertions: 10, Deletions: 0},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"planning", "--worktree", wt})
	cmd.SetIn(bytes.NewReader(body))
	var out bytes.Buffer
	cmd.SetOut(&out)

	if execErr := cmd.Execute(); execErr != nil {
		t.Fatalf("cmd.Execute: %v", execErr)
	}

	got, err := protocol.Load(protocol.StatusPath(wt))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Phase != protocol.PhasePlanning {
		t.Errorf("Phase = %q, want planning", got.Phase)
	}
	if got.UpdatedAt.Year() == 1999 {
		t.Errorf("UpdatedAt = %v, worker-claimed timestamp leaked through", got.UpdatedAt)
	}
	if got.Task != "argus#92" {
		t.Errorf("Task = %q, want argus#92", got.Task)
	}
}

func TestWorkerReportCmdRejectsUnknownPhase(t *testing.T) {
	wt := t.TempDir()
	cmd := newWorkerReportCmd()
	cmd.SetArgs([]string{"done", "--worktree", wt})
	cmd.SetIn(bytes.NewReader([]byte(`{}`)))
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error reporting done via the CLI, got nil")
	}
}

func TestWorkerReportCmdWorktreeFlagDefaultsEmpty(t *testing.T) {
	// An empty default means RunE falls back to os.Getwd() (see
	// newWorkerReportCmd) — a worker running `argus worker report <phase>`
	// from inside its own worktree (SpawnCommand cd's it there) needs no
	// --worktree flag at all. Exercising the actual getwd fallback would
	// require a chdir, which races other parallel tests, so this only pins
	// the flag wiring.
	cmd := newWorkerReportCmd()
	f := cmd.Flags().Lookup("worktree")
	if f == nil || f.DefValue != "" {
		t.Fatalf("worktree flag = %+v, want a registered flag defaulting to empty", f)
	}
}
