package cmd

import (
	"context"
	"testing"

	"codeberg.org/Elysium_Labs/argus/internal/herdr"
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
