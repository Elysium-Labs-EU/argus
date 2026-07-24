package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
)

// fakeForge is a Forge stub for tests: it returns canned issues and records the
// PR it was asked to open.
type fakeForge struct {
	findPRErr   error
	issues      map[int]forge.Issue
	opened      *forge.PRRequest
	findPR      forge.PR
	findPRFound bool
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

func TestIssuesToTasks(t *testing.T) {
	f := &fakeForge{issues: map[int]forge.Issue{
		142: {Number: 142, Title: "daemon down warning", Body: "warn when down"},
		145: {Number: 145, Title: "log backoff", Body: "back off on EACCES"},
	}}
	tasks, branches, err := issuesToTasks(context.Background(), f, "o", "r", t.TempDir(), []int{142, 145})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 2 || len(branches) != 2 {
		t.Fatalf("want 2 tasks/branches, got %d/%d", len(tasks), len(branches))
	}
	if !strings.Contains(tasks[0], "#142") || !strings.Contains(tasks[0], "daemon down warning") || !strings.Contains(tasks[0], "warn when down") {
		t.Errorf("task 0 missing issue content: %q", tasks[0])
	}
	if !strings.Contains(tasks[0], "Do NOT git commit or push; argus ships.") {
		t.Errorf("task 0 missing the fixed ship-pipeline line: %q", tasks[0])
	}
	if branches[0] != "fix-issue-142" || branches[1] != "fix-issue-145" {
		t.Errorf("branches: %v", branches)
	}
}

// TestIssuesToTasksAppendsRepoBriefNote pins issue #161: the old hardcoded
// "Add a focused test and keep make ci green. Follow the repo STYLE.md." is
// gone; a repo's own .argus/config.yml brief_note takes its place when
// present, and is appended before the fixed "don't commit" line.
func TestIssuesToTasksAppendsRepoBriefNote(t *testing.T) {
	repo := t.TempDir()
	if err := repoconfig.Save(repoconfig.Path(repo), repoconfig.Config{
		BriefNote: "Keep task frontend:ci green.",
	}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	f := &fakeForge{issues: map[int]forge.Issue{142: {Number: 142, Title: "t", Body: "b"}}}
	tasks, _, err := issuesToTasks(context.Background(), f, "o", "r", repo, []int{142})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if !strings.Contains(tasks[0], "Keep task frontend:ci green. Do NOT git commit or push; argus ships.") {
		t.Errorf("task missing brief_note ahead of the fixed line: %q", tasks[0])
	}
}

// TestIssuesToTasksNoRepoConfigOmitsToolchainText pins the other half of
// issue #161: with no .argus/config.yml, argus asserts no toolchain opinion
// of its own — only the fixed line survives, not the old "make ci"/"STYLE.md"
// defaults.
func TestIssuesToTasksNoRepoConfigOmitsToolchainText(t *testing.T) {
	f := &fakeForge{issues: map[int]forge.Issue{142: {Number: 142, Title: "t", Body: "b"}}}
	tasks, _, err := issuesToTasks(context.Background(), f, "o", "r", t.TempDir(), []int{142})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	if strings.Contains(tasks[0], "make ci") || strings.Contains(tasks[0], "STYLE.md") {
		t.Errorf("task should not assume a toolchain with no repo config: %q", tasks[0])
	}
	if !strings.Contains(tasks[0], "Do NOT git commit or push; argus ships.") {
		t.Errorf("task missing the fixed ship-pipeline line: %q", tasks[0])
	}
}

// fakeJira is a jiraIssueFetcher stub for tests: it returns canned issues keyed
// by Jira key instead of a numeric forge issue number.
type fakeJira struct {
	issues map[string]forge.Issue
}

func (f *fakeJira) FetchIssue(_ context.Context, key string) (forge.Issue, error) {
	return f.issues[key], nil
}

func TestJiraIssuesToTasks(t *testing.T) {
	f := &fakeJira{issues: map[string]forge.Issue{
		"PROJ-142": {Title: "daemon down warning", Body: "warn when down"},
		"PROJ-145": {Title: "log backoff", Body: "back off on EACCES"},
	}}
	tasks, branches, err := jiraIssuesToTasks(context.Background(), f, t.TempDir(), []string{"PROJ-142", "PROJ-145"})
	if err != nil {
		t.Fatalf("jiraIssuesToTasks: %v", err)
	}
	if len(tasks) != 2 || len(branches) != 2 {
		t.Fatalf("want 2 tasks/branches, got %d/%d", len(tasks), len(branches))
	}
	if !strings.Contains(tasks[0], "PROJ-142") || !strings.Contains(tasks[0], "daemon down warning") || !strings.Contains(tasks[0], "warn when down") {
		t.Errorf("task 0 missing issue content: %q", tasks[0])
	}
	if branches[0] != "fix-proj-142" || branches[1] != "fix-proj-145" {
		t.Errorf("branches: %v", branches)
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
