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
)

// fakeForge is a Forge stub for tests: it returns canned issues and records the
// PR it was asked to open.
type fakeForge struct {
	issues map[int]forge.Issue
	opened *forge.PRRequest
}

func (f *fakeForge) Host() string { return "fake" }
func (f *fakeForge) FetchIssue(_ context.Context, _, _ string, n int) (forge.Issue, error) {
	return f.issues[n], nil
}
func (f *fakeForge) OpenPR(_ context.Context, req *forge.PRRequest) (forge.PR, error) {
	f.opened = req
	return forge.PR{Number: 99, HTMLURL: "https://fake/pull/99", State: "open"}, nil
}

func TestIssuesToTasks(t *testing.T) {
	f := &fakeForge{issues: map[int]forge.Issue{
		142: {Number: 142, Title: "daemon down warning", Body: "warn when down"},
		145: {Number: 145, Title: "log backoff", Body: "back off on EACCES"},
	}}
	tasks, branches, err := issuesToTasks(context.Background(), f, "o", "r", []int{142, 145})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	if len(tasks) != 2 || len(branches) != 2 {
		t.Fatalf("want 2 tasks/branches, got %d/%d", len(tasks), len(branches))
	}
	if !strings.Contains(tasks[0], "#142") || !strings.Contains(tasks[0], "daemon down warning") || !strings.Contains(tasks[0], "warn when down") {
		t.Errorf("task 0 missing issue content: %q", tasks[0])
	}
	if branches[0] != "fix-issue-142" || branches[1] != "fix-issue-145" {
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
