package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/herdr"
)

const paneListReply = `{"result":{"panes":[
{"pane_id":"1-2","cwd":"/repo-a","agent":"claude","agent_status":"idle"},
{"pane_id":"1-3","cwd":"/repo-b","agent":"claude","agent_status":"idle"}
]}}`

func fakeClient() herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(paneListReply), nil
	})
}

func TestSpawnWorkersIssuesOnlyPassesGuard(t *testing.T) {
	// --issues alone (no --tasks/--branches/--panes) used to trip the "no
	// workers given" guard, since that check ran before --issues was folded
	// into tasks/branches. repo is a non-git tempdir so it fails downstream
	// (resolving the forge) instead — proof the guard itself let it through.
	client := fakeClient()
	_, err := spawnWorkers(context.Background(), client, &workerInput{repo: t.TempDir()}, []int{1}, nil)
	if err == nil {
		t.Fatal("want a downstream error resolving the forge for a non-git repo")
	}
	if strings.Contains(err.Error(), "no workers given") {
		t.Errorf("--issues alone should satisfy the worker-source guard, got: %v", err)
	}
}

func TestFoldIssueSourcesNoop(t *testing.T) {
	in := &workerInput{tasks: []string{"existing"}, branches: []string{"existing-branch"}}
	if err := foldIssueSources(context.Background(), in, nil, nil); err != nil {
		t.Fatalf("foldIssueSources: %v", err)
	}
	if len(in.tasks) != 1 || len(in.branches) != 1 {
		t.Errorf("no issue sources should leave tasks/branches untouched, got %v %v", in.tasks, in.branches)
	}
}

func TestFoldIssueSourcesIssuesError(t *testing.T) {
	// repo isn't a git checkout, so resolving the origin remote fails before any
	// network call — exercises the --issues error path without a real forge.
	in := &workerInput{repo: t.TempDir()}
	if err := foldIssueSources(context.Background(), in, []int{1}, nil); err == nil {
		t.Fatal("want error resolving forge for a non-git repo")
	}
}

func TestFoldIssueSourcesJiraError(t *testing.T) {
	for _, k := range []string{"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"} {
		t.Setenv(k, "")
	}
	in := &workerInput{repo: t.TempDir()}
	if err := foldIssueSources(context.Background(), in, nil, []string{"PROJ-1"}); err == nil {
		t.Fatal("want error building jira client without JIRA_* env vars")
	}
}

func TestBuildWorkersDerivesRepoAndDefaults(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		panes: []string{"1-2", "1-3"},
		// branches/tasks omitted → defaults
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers, got %d", len(workers))
	}
	if workers[0].RepoRoot != "/repo-a" {
		t.Errorf("repo root should come from pane cwd; got %q", workers[0].RepoRoot)
	}
	if workers[0].Branch != "argus-1-2" {
		t.Errorf("default branch: got %q want argus-1-2", workers[0].Branch)
	}
	if workers[0].Task != "1-2" {
		t.Errorf("default task should be the pane id; got %q", workers[0].Task)
	}
}

func TestBuildWorkersPairsBranchesAndTasks(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		panes:    []string{"1-2", "1-3"},
		branches: []string{"feat-a", "feat-b"},
		tasks:    []string{"eos#1", "eos#2"},
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if workers[1].Branch != "feat-b" || workers[1].Task != "eos#2" {
		t.Errorf("positional pairing wrong: %+v", workers[1])
	}
}

func TestBuildWorkersRepoOverride(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		panes: []string{"1-2"},
		repo:  "/pinned",
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if workers[0].RepoRoot != "/pinned" {
		t.Errorf("--repo should override pane cwd; got %q", workers[0].RepoRoot)
	}
}

func TestBuildWorkersNoPanesUsesRepoAndTaskSlug(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		repo:  "/pinned",
		tasks: []string{"eos#42 add env cmd", "themis#7"},
		// no panes, no branches → auto-pane mode, branches from task slugs
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers from 2 tasks, got %d", len(workers))
	}
	if workers[0].PaneID != "" {
		t.Errorf("auto-pane mode should leave PaneID empty, got %q", workers[0].PaneID)
	}
	if workers[0].RepoRoot != "/pinned" {
		t.Errorf("repo: got %q", workers[0].RepoRoot)
	}
	if workers[0].Branch != "argus-eos-42-add-env-cmd" {
		t.Errorf("branch slug: got %q", workers[0].Branch)
	}
}

func TestBuildWorkersNoPanesNoRepoErrors(t *testing.T) {
	client := fakeClient()
	_, err := buildWorkers(context.Background(), client, &workerInput{
		tasks: []string{"x"},
	})
	if err == nil {
		t.Fatal("want error when no --panes and no --repo, got nil")
	}
}

func TestBuildWorkersUnknownPane(t *testing.T) {
	client := fakeClient()
	_, err := buildWorkers(context.Background(), client, &workerInput{
		panes: []string{"9-9"},
	})
	if err == nil {
		t.Fatal("want error for a pane not in the list, got nil")
	}
}

func TestBuildWorkersPanesDefaultBranchAndTask(t *testing.T) {
	client := fakeClient()
	// A pane with no explicit task/branch: both default off the pane id.
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		panes: []string{"1-2"},
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want 1 worker, got %d", len(workers))
	}
	if workers[0].Task != "1-2" || workers[0].Branch != "argus-1-2" {
		t.Errorf("pane defaults: got task=%q branch=%q", workers[0].Task, workers[0].Branch)
	}
}

func TestBuildWorkersRejectsUnsafeBranch(t *testing.T) {
	client := fakeClient()
	_, err := buildWorkers(context.Background(), client, &workerInput{
		repo:     "/repo",
		tasks:    []string{"x"},
		branches: []string{"feat$(whoami)"},
	})
	if err == nil {
		t.Fatal("want error for a branch with shell metacharacters, got nil")
	}
}

func TestValidBranch(t *testing.T) {
	ok := []string{"feat-x", "fix/started-at-144", "release_1.2.3", "a/b/c"}
	bad := []string{"feat x", "feat$(cmd)", "a;b", "-leading", "/abs", "back`tick", ""}
	for _, b := range ok {
		if !validBranch(b) {
			t.Errorf("validBranch(%q) = false, want true", b)
		}
	}
	for _, b := range bad {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true, want false", b)
		}
	}
}
