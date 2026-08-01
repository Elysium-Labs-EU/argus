package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

// fakeForge is a Forge stub for tests: it returns canned issues and records the
// PR it was asked to open.
type fakeForge struct {
	findPRErr    error
	prChecksErr  error
	issues       map[int]forge.Issue
	opened       *forge.PRRequest
	prChecksByPR map[int][][]forge.Check
	findPR       forge.PR
	prChecksCall int
	findPRFound  bool
}

func (f *fakeForge) Host() string { return "fake" }
func (f *fakeForge) FetchIssue(_ context.Context, _, _ string, n int) (forge.Issue, error) {
	return f.issues[n], nil
}
func (f *fakeForge) OpenPR(_ context.Context, req *forge.PRRequest) (forge.PR, error) {
	f.opened = req
	return forge.PR{Number: 99, HTMLURL: "https://fake/pull/99", State: "open"}, nil
}
func (f *fakeForge) FindPR(_ context.Context, _, _, _ string) (forge.PR, bool, error) {
	return f.findPR, f.findPRFound, f.findPRErr
}

// PRChecks returns the next queued batch of checks for number, letting a test
// simulate a poll loop observing checks go from in-flight to terminal across
// successive ticks. A call past the queued batches repeats the last one.
func (f *fakeForge) PRChecks(_ context.Context, _, _ string, number int) ([]forge.Check, error) {
	if f.prChecksErr != nil {
		return nil, f.prChecksErr
	}
	batches := f.prChecksByPR[number]
	if len(batches) == 0 {
		return nil, nil
	}
	i := f.prChecksCall
	if i >= len(batches) {
		i = len(batches) - 1
	}
	f.prChecksCall++
	return batches[i], nil
}

func TestIssuesToTasks(t *testing.T) {
	f := &fakeForge{issues: map[int]forge.Issue{
		142: {Number: 142, Title: "daemon down warning", Body: "warn when down"},
		145: {Number: 145, Title: "log backoff", Body: "back off on EACCES"},
	}}
	tasks, branches, err := issuesToTasks(context.Background(), io.Discard, f, "o", "r", t.TempDir(), []int{142, 145}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 2 || len(branches) != 2 {
		t.Fatalf("want 2 tasks/branches, got %d/%d", len(tasks), len(branches))
	}
	if !strings.Contains(tasks[0], "#142") || !strings.Contains(tasks[0], "daemon down warning") || !strings.Contains(tasks[0], "warn when down") {
		t.Errorf("task 0 missing issue content: %q", tasks[0])
	}
	if !strings.Contains(tasks[0], "Do NOT run git commit or git push yourself; argus ships.") {
		t.Errorf("task 0 missing the fixed ship-pipeline line: %q", tasks[0])
	}
	if branches[0] != "r-fix-issue-142" || branches[1] != "r-fix-issue-145" {
		t.Errorf("branches: %v", branches)
	}
}

// TestFixedBriefTailInstructsRunningRepoLint pins the fix for the gap where a
// worker's diff could earn a clean gate verdict and then fail at `argus
// ship`'s `git commit` when the repo's own pre-commit hook ran lint/build
// checks the gate never reproduced: every generated brief must tell the
// worker to run those checks itself before reporting a terminal phase.
func TestFixedBriefTailInstructsRunningRepoLint(t *testing.T) {
	got := fixedBriefTail("")
	if !strings.Contains(got, "lint/build/pre-commit") {
		t.Errorf("fixedBriefTail(\"\") = %q, want an instruction to run the repo's own lint/build/pre-commit checks", got)
	}
	if !strings.Contains(got, "awaiting_review") {
		t.Errorf("fixedBriefTail(\"\") = %q, want it to say when to run those checks (before a terminal phase)", got)
	}
}

// TestIssuesToTasksAppendsRepoBriefNote pins the fix that the old hardcoded
// "Add a focused test and keep make ci green. Follow the repo STYLE.md." is
// gone; a repo's own .argus/config.yml brief_note takes its place when
// present, and is appended before the fixed "don't commit" line.
func TestIssuesToTasksAppendsRepoBriefNote(t *testing.T) {
	repo := t.TempDir()
	if err := repoconfig.Save(repoconfig.Path(repo), &repoconfig.Config{
		BriefNote: "Keep task frontend:ci green.",
	}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	f := &fakeForge{issues: map[int]forge.Issue{142: {Number: 142, Title: "t", Body: "b"}}}
	tasks, _, err := issuesToTasks(context.Background(), io.Discard, f, "o", "r", repo, []int{142}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if !strings.Contains(tasks[0], "Keep task frontend:ci green. Do NOT run git commit or git push yourself; argus ships.") {
		t.Errorf("task missing brief_note ahead of the fixed line: %q", tasks[0])
	}
}

// TestIssuesToTasksNoRepoConfigOmitsToolchainText pins the other half of
// that fix: with no .argus/config.yml, argus asserts no toolchain opinion of
// its own — only the fixed line survives, not the old "make ci"/"STYLE.md"
// defaults.
func TestIssuesToTasksNoRepoConfigOmitsToolchainText(t *testing.T) {
	f := &fakeForge{issues: map[int]forge.Issue{142: {Number: 142, Title: "t", Body: "b"}}}
	tasks, _, err := issuesToTasks(context.Background(), io.Discard, f, "o", "r", t.TempDir(), []int{142}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if strings.Contains(tasks[0], "make ci") || strings.Contains(tasks[0], "STYLE.md") {
		t.Errorf("task should not assume a toolchain with no repo config: %q", tasks[0])
	}
	if !strings.Contains(tasks[0], "Do NOT run git commit or git push yourself; argus ships.") {
		t.Errorf("task missing the fixed ship-pipeline line: %q", tasks[0])
	}
}

// fakeJira is a jiraSpawnClient stub for tests: it returns canned issues keyed
// by Jira key instead of a numeric forge issue number, and records every
// Assign/Transition/Myself call so the pre-spawn hook is verifiable without a
// network.
type fakeJira struct {
	myselfErr     error
	assignErr     error
	transitionErr error
	issues        map[string]forge.Issue
	assigned      map[string]string
	transitioned  map[string]string
	myselfID      string
	myselfCalls   int
}

func (f *fakeJira) FetchIssue(_ context.Context, key string) (forge.Issue, error) {
	return f.issues[key], nil
}

func (f *fakeJira) Myself(context.Context) (string, error) {
	f.myselfCalls++
	if f.myselfErr != nil {
		return "", f.myselfErr
	}
	return f.myselfID, nil
}

func (f *fakeJira) Assign(_ context.Context, key, accountID string) error {
	if f.assignErr != nil {
		return f.assignErr
	}
	if f.assigned == nil {
		f.assigned = map[string]string{}
	}
	f.assigned[key] = accountID
	return nil
}

func (f *fakeJira) Transition(_ context.Context, key, idOrName string) error {
	if f.transitionErr != nil {
		return f.transitionErr
	}
	if f.transitioned == nil {
		f.transitioned = map[string]string{}
	}
	f.transitioned[key] = idOrName
	return nil
}

func TestJiraIssuesToTasks(t *testing.T) {
	f := &fakeJira{issues: map[string]forge.Issue{
		"PROJ-142": {Title: "daemon down warning", Body: "warn when down"},
		"PROJ-145": {Title: "log backoff", Body: "back off on EACCES"},
	}}
	tasks, branches, err := jiraIssuesToTasks(context.Background(), io.Discard, f, t.TempDir(), "myrepo", []string{"PROJ-142", "PROJ-145"}, jiraSpawnOpts{}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("jiraIssuesToTasks: %v", err)
	}
	if len(tasks) != 2 || len(branches) != 2 {
		t.Fatalf("want 2 tasks/branches, got %d/%d", len(tasks), len(branches))
	}
	if !strings.Contains(tasks[0], "PROJ-142") || !strings.Contains(tasks[0], "daemon down warning") || !strings.Contains(tasks[0], "warn when down") {
		t.Errorf("task 0 missing issue content: %q", tasks[0])
	}
	if branches[0] != "myrepo-fix-proj-142" || branches[1] != "myrepo-fix-proj-145" {
		t.Errorf("branches: %v", branches)
	}
	if f.myselfCalls != 0 || len(f.assigned) != 0 || len(f.transitioned) != 0 {
		t.Errorf("no jiraSpawnOpts set: want no assign/transition/myself calls, got myself=%d assigned=%v transitioned=%v", f.myselfCalls, f.assigned, f.transitioned)
	}
}

// TestJiraIssuesToTasksAssignsAndTransitionsOnSpawn covers the pre-spawn hook:
// with assignToCaller set, each issue is assigned to the accountID Myself
// resolves (fetched once, not once per issue); with transition set, each
// issue is also transitioned to that value.
func TestJiraIssuesToTasksAssignsAndTransitionsOnSpawn(t *testing.T) {
	f := &fakeJira{
		issues: map[string]forge.Issue{
			"PROJ-142": {Title: "daemon down warning", Body: "warn when down"},
			"PROJ-145": {Title: "log backoff", Body: "back off on EACCES"},
		},
		myselfID: "acc-caller",
	}
	_, _, err := jiraIssuesToTasks(context.Background(), io.Discard, f, t.TempDir(), "myrepo", []string{"PROJ-142", "PROJ-145"},
		jiraSpawnOpts{assignToCaller: true, transition: "In Progress"}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("jiraIssuesToTasks: %v", err)
	}
	if f.myselfCalls != 1 {
		t.Errorf("Myself calls = %d, want 1 (resolved once, not per issue)", f.myselfCalls)
	}
	want := map[string]string{"PROJ-142": "acc-caller", "PROJ-145": "acc-caller"}
	if !reflect.DeepEqual(f.assigned, want) {
		t.Errorf("assigned = %v, want %v", f.assigned, want)
	}
	wantTransitioned := map[string]string{"PROJ-142": "In Progress", "PROJ-145": "In Progress"}
	if !reflect.DeepEqual(f.transitioned, wantTransitioned) {
		t.Errorf("transitioned = %v, want %v", f.transitioned, wantTransitioned)
	}
}

// TestJiraIssuesToTasksAbortsOnAssignFailure covers the fail-fast contract:
// unlike ship's best-effort post-ship hook, a pre-spawn assign/transition
// failure must stop the spawn instead of degrading to a warning, since the
// PR/worker for that issue doesn't exist yet to protect.
func TestJiraIssuesToTasksAbortsOnAssignFailure(t *testing.T) {
	f := &fakeJira{
		issues:    map[string]forge.Issue{"PROJ-1": {Title: "t", Body: "b"}},
		myselfID:  "acc-caller",
		assignErr: errors.New("boom"),
	}
	_, _, err := jiraIssuesToTasks(context.Background(), io.Discard, f, t.TempDir(), "myrepo", []string{"PROJ-1"}, jiraSpawnOpts{assignToCaller: true}, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error when Assign fails")
	}
}

// TestJiraIssuesToTasksAbortsOnMyselfFailure covers resolving the caller's
// own accountID failing before any per-issue call is made.
func TestJiraIssuesToTasksAbortsOnMyselfFailure(t *testing.T) {
	f := &fakeJira{
		issues:    map[string]forge.Issue{"PROJ-1": {Title: "t", Body: "b"}},
		myselfErr: errors.New("not authenticated"),
	}
	_, _, err := jiraIssuesToTasks(context.Background(), io.Discard, f, t.TempDir(), "myrepo", []string{"PROJ-1"}, jiraSpawnOpts{assignToCaller: true}, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error when Myself fails")
	}
	if len(f.assigned) != 0 {
		t.Errorf("want no Assign call when Myself failed, got %v", f.assigned)
	}
}

// TestJiraIssuesToTasksAbortsOnTransitionFailure covers the transition-only
// path failing.
func TestJiraIssuesToTasksAbortsOnTransitionFailure(t *testing.T) {
	f := &fakeJira{
		issues:        map[string]forge.Issue{"PROJ-1": {Title: "t", Body: "b"}},
		transitionErr: errors.New("no such transition"),
	}
	_, _, err := jiraIssuesToTasks(context.Background(), io.Discard, f, t.TempDir(), "myrepo", []string{"PROJ-1"}, jiraSpawnOpts{transition: "Bogus"}, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error when Transition fails")
	}
}

// TestRepoBranchPrefixFallsBackToDirName pins the case repoBranchPrefix
// exists for: a local checkout with no origin remote (or one Jira can't be
// bothered to resolve a forge host for) still gets a stable, unique branch
// prefix instead of silently reverting to the collision-prone bare "fix-".
func TestRepoBranchPrefixFallsBackToDirName(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	got := repoBranchPrefix(context.Background(), repo)
	if want := filepath.Base(repo); got != want {
		t.Errorf("repoBranchPrefix() = %q, want %q", got, want)
	}
}

func TestBuildPRBodyReport(t *testing.T) {
	// A git worktree with a real diff plus argus's status and verdict files.
	wt := t.TempDir()
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", wt}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("branch", "-q", "-m", "main") // so origin/main-style base resolves via HEAD below
	if err := os.WriteFile(filepath.Join(wt, "f.go"), []byte("package x\n\nvar Added = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protocol.Write(protocol.StatusPath(wt), &protocol.Status{
		Phase: protocol.PhaseAwaitingReview,
		Tests: []protocol.TestRun{{Cmd: "make ci", Result: protocol.ResultPass}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteApproval(wt, &protocol.Approval{Approved: true, Source: "gate", Summary: "clean within policy"}); err != nil {
		t.Fatal(err)
	}

	f := &fakeForge{issues: map[int]forge.Issue{142: {Title: "daemon down warning"}}}
	// base "HEAD" makes MeasureDiff("origin/"+base) fail, so exercise with a base
	// that resolves: point at the initial commit via a local ref name.
	body := buildPRBody(context.Background(), f, wt, "HEAD", 142, "o", "r")

	for _, want := range []string{"## Target", "Closes #142", "daemon down warning", "## Verification", "1/1 passed", "## Verdict", "argus approved via gate"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
}
