package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
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

// gitInitDir makes t.TempDir() a real (if empty) git repo, so it passes
// Preflight's --repo validation the same way a real checkout would.
func gitInitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
	return dir
}

func TestSpawnWorkersIssuesOnlyPassesGuard(t *testing.T) {
	// --issues alone (no --tasks/--branches/--panes) used to trip the "no
	// workers given" guard, since that check ran before --issues was folded
	// into tasks/branches. repo is a non-git tempdir so it fails downstream
	// (resolving the forge) instead — proof the guard itself let it through.
	client := fakeClient()
	_, err := spawnWorkers(context.Background(), io.Discard, client, &workerInput{repo: t.TempDir()}, []int{1}, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err == nil {
		t.Fatal("want a downstream error resolving the forge for a non-git repo")
	}
	if strings.Contains(err.Error(), "no workers given") {
		t.Errorf("--issues alone should satisfy the worker-source guard, got: %v", err)
	}
}

func TestFoldIssueSourcesNoop(t *testing.T) {
	in := &workerInput{tasks: []string{"existing"}, branches: []string{"existing-branch"}}
	if err := foldIssueSources(context.Background(), io.Discard, in, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{}); err != nil {
		t.Fatalf("foldIssueSources: %v", err)
	}
	if len(in.tasks) != 1 || len(in.branches) != 1 {
		t.Errorf("no issue sources should leave tasks/branches untouched, got %v %v", in.tasks, in.branches)
	}
}

// TestMergeFetchedFieldPartialExplicitBranches pins issue #293: an explicit
// --branches shorter than the total worker count (covering only earlier
// manual --tasks workers) used to make foldIssueSources skip merging in the
// fetched --issues/--jira-issues default branches entirely, because it only
// merged when in.branches started out completely empty. The issue worker's
// branch slot must still get its fetched default, not stay missing.
func TestMergeFetchedFieldPartialExplicitBranches(t *testing.T) {
	branches := []string{"manual-branch"} // covers only the first (manual) worker
	preCount := 1                         // one manual task already in in.tasks
	got := mergeFetchedField(branches, preCount, []string{"widget-fix-issue-7"})
	want := []string{"manual-branch", "widget-fix-issue-7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeFetchedField(%v, %d, ...) = %v, want %v", branches, preCount, got, want)
	}
}

// TestMergeFetchedFieldExplicitSlotWins covers the other half: an explicit
// value already occupying one of the fetched slots (e.g. --branches given
// for every worker up front, issue workers included) must win over the
// fetched default at that same position. Applies identically to --labels
// (see foldIssueSources), since mergeFetchedField's merge logic is shared.
func TestMergeFetchedFieldExplicitSlotWins(t *testing.T) {
	branches := []string{"manual-branch", "explicit-issue-branch"}
	got := mergeFetchedField(branches, 1, []string{"widget-fix-issue-7"})
	want := []string{"manual-branch", "explicit-issue-branch"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeFetchedField(%v, 1, ...) = %v, want %v", branches, got, want)
	}
}

// TestBuildWorkersIssueBranchSurvivesPartialBranches is the end-to-end
// regression for issue #293: it drives the real issuesToTasks fetch (via
// fakeForge, no network) and the fixed merge helper the same way
// foldIssueSources calls them, then feeds the result into buildWorkers —
// proving the issue worker lands on its normal <repo>-fix-issue-N branch
// instead of buildWorkers falling through to defaultBranch and slugging the
// entire fetched issue body (title+body+tail) into an unusable branch name.
func TestBuildWorkersIssueBranchSurvivesPartialBranches(t *testing.T) {
	in := &workerInput{
		repo:     "/pinned",
		tasks:    []string{"manual task for an earlier --tasks worker"},
		branches: []string{"manual-branch"},
	}
	f := &fakeForge{issues: map[int]forge.Issue{
		7: {Number: 7, Title: "t", Body: strings.Repeat("very long issue body ", 200)},
	}}
	fetchedTasks, fetchedBranches, _, err := issuesToTasks(context.Background(), io.Discard, f, "o", "widget", in.repo, []int{7}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	preCount := len(in.tasks)
	in.tasks = append(in.tasks, fetchedTasks...)
	in.branches = mergeFetchedField(in.branches, preCount, fetchedBranches)

	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, in)
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers, got %d", len(workers))
	}
	if workers[0].Branch != "manual-branch" {
		t.Errorf("manual worker branch: got %q, want manual-branch", workers[0].Branch)
	}
	if want := "widget-fix-issue-7"; workers[1].Branch != want {
		t.Errorf("issue worker branch = %q, want %q (fetched default, not a slug of the issue body)", workers[1].Branch, want)
	}
}

// TestBuildWorkersIssueLabelSurvivesPartialLabels mirrors
// TestBuildWorkersIssueBranchSurvivesPartialBranches for the label-by-key
// fix: an explicit --labels covering only an earlier manual --tasks worker
// must still leave the issue worker's label as its fetched bare ticket key
// ("#7"), not empty — a caller scanning `herdr workspace list` needs the key
// to find this worker, and an empty Label would fall through to BuildPlan's
// task-derived default instead (the whole class of bug this fix closes).
func TestBuildWorkersIssueLabelSurvivesPartialLabels(t *testing.T) {
	in := &workerInput{
		repo:     "/pinned",
		tasks:    []string{"manual task for an earlier --tasks worker"},
		branches: []string{"manual-branch"},
		labels:   []string{"manual-label"},
	}
	f := &fakeForge{issues: map[int]forge.Issue{
		7: {Number: 7, Title: "t", Body: "b"},
	}}
	fetchedTasks, fetchedBranches, fetchedLabels, err := issuesToTasks(context.Background(), io.Discard, f, "o", "widget", in.repo, []int{7}, briefNoteOverride{})
	if err != nil {
		t.Fatalf("issuesToTasks: %v", err)
	}
	preCount := len(in.tasks)
	in.tasks = append(in.tasks, fetchedTasks...)
	in.branches = mergeFetchedField(in.branches, preCount, fetchedBranches)
	in.labels = mergeFetchedField(in.labels, preCount, fetchedLabels)

	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, in)
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers, got %d", len(workers))
	}
	if workers[0].Label != "manual-label" {
		t.Errorf("manual worker label: got %q, want manual-label", workers[0].Label)
	}
	if want := "#7"; workers[1].Label != want {
		t.Errorf("issue worker label = %q, want %q (bare fetched ticket key, not empty)", workers[1].Label, want)
	}
}

func TestFoldIssueSourcesIssuesError(t *testing.T) {
	// repo isn't a git checkout, so resolving the origin remote fails before any
	// network call — exercises the --issues error path without a real forge.
	in := &workerInput{repo: t.TempDir()}
	if err := foldIssueSources(context.Background(), io.Discard, in, []int{1}, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{}); err == nil {
		t.Fatal("want error resolving forge for a non-git repo")
	}
}

func TestFoldIssueSourcesJiraError(t *testing.T) {
	for _, k := range []string{"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"} {
		t.Setenv(k, "")
	}
	// Point the config-file fallback at a path that doesn't exist, so this
	// doesn't accidentally pass on a machine with a real ~/.argus/jira.json.
	t.Setenv("JIRA_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))
	in := &workerInput{repo: t.TempDir()}
	if err := foldIssueSources(context.Background(), io.Discard, in, nil, []string{"PROJ-1"}, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{}); err == nil {
		t.Fatal("want error building jira client without JIRA_* env vars or a config file")
	}
}

// TestFoldIssueSourcesSelfHostedRequiresForge pins issue #256's supervise half:
// --issues against a self-hosted host argus can't shape-detect refuses without
// --forge (or a repo config forge key), same as ship already does.
func TestFoldIssueSourcesSelfHostedRequiresForge(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	in := &workerInput{repo: wt}
	if err := foldIssueSources(context.Background(), io.Discard, in, []int{1}, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{}); err == nil {
		t.Fatal("want error: a self-hosted host with no --forge/config default should refuse")
	}
}

// TestFoldIssueSourcesSelfHostedForgeFlagUnblocks is the other half: an
// explicit --forge lets --issues fetch from a self-hosted host.
func TestFoldIssueSourcesSelfHostedForgeFlagUnblocks(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	in := &workerInput{repo: wt}
	err := foldIssueSources(context.Background(), io.Discard, in, []int{1}, nil, nil, jiraSpawnOpts{}, "gitea", true, briefNoteOverride{})
	// The fetch itself still fails (no real forge to talk to), but it must fail
	// past forge construction, not on the ambiguous-host refusal.
	if err == nil {
		t.Fatal("want a downstream fetch error (no real forge to talk to)")
	}
	if strings.Contains(err.Error(), "not one of the auto-detected forges") {
		t.Errorf("explicit --forge gitea should bypass the ambiguous-host refusal, got: %v", err)
	}
}

// TestFoldIssueSourcesSelfHostedForgeConfigUnblocks mirrors the flag case but
// via this repo's .argus/config.yml forge key instead of --forge.
func TestFoldIssueSourcesSelfHostedForgeConfigUnblocks(t *testing.T) {
	t.Setenv("FORGE_TOKEN", "tok")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	if err := repoconfig.Save(repoconfig.Path(wt), &repoconfig.Config{Forge: "gitea"}); err != nil {
		t.Fatalf("seeding repo config: %v", err)
	}
	in := &workerInput{repo: wt}
	err := foldIssueSources(context.Background(), io.Discard, in, []int{1}, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err == nil {
		t.Fatal("want a downstream fetch error (no real forge to talk to)")
	}
	if strings.Contains(err.Error(), "not one of the auto-detected forges") {
		t.Errorf("repo config forge:gitea should bypass the ambiguous-host refusal, got: %v", err)
	}
}

// TestFoldIssueSourcesSelfHostedNoTokenShowsForgeAmbiguityFirst pins the fix
// for the error-ordering bug: an unrecognized self-hosted host with no token
// configured must surface the ambiguous-host refusal (the actually-blocking
// problem, fixable only with --forge), not the missing-token error — the
// latter's implied remediation (set a token) would not have fixed anything.
func TestFoldIssueSourcesSelfHostedNoTokenShowsForgeAmbiguityFirst(t *testing.T) {
	t.Setenv("GIT_EXAMPLE_COM_TOKEN", "")
	t.Setenv("FORGE_TOKEN", "")
	wt := gitRepo(t, []string{"remote", "add", "origin", "git@git.example.com:acme/widget.git"})
	in := &workerInput{repo: wt}
	err := foldIssueSources(context.Background(), io.Discard, in, []int{1}, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error: a self-hosted host with no --forge/config default should refuse")
	}
	if strings.Contains(err.Error(), "no API token") {
		t.Errorf("missing-token error should not mask the forge-ambiguity error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not one of the auto-detected forges") {
		t.Errorf("want forge-ambiguity error, got: %v", err)
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

func TestBuildWorkersPairsLabels(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		panes:    []string{"1-2", "1-3"},
		branches: []string{"feat-a", "feat-b"},
		labels:   []string{"worker-a"},
		// second worker omits --labels → Worker.Label stays "" here; the
		// task-derived default is BuildPlan's job, not buildWorkers's.
	})
	if err != nil {
		t.Fatalf("buildWorkers: %v", err)
	}
	if workers[0].Label != "worker-a" {
		t.Errorf("--labels should set Worker.Label positionally; got %q", workers[0].Label)
	}
	if workers[1].Label != "" {
		t.Errorf("a worker with no --labels entry should get an empty Label, not a default; got %q", workers[1].Label)
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

// TestBuildWorkersTaskBranchCountMismatchErrors pins issue #327: a --tasks-file
// multi-line brief (or a comma-split --tasks entry) can silently parse into
// more tasks than --branches has entries for. That used to leave every extra
// task falling through to defaultBranch's slug of the raw task text instead
// of erroring, which for a whole brief can exceed git's ref length limit and
// abort worktree creation. buildWorkers must now refuse the mismatch outright.
func TestBuildWorkersTaskBranchCountMismatchErrors(t *testing.T) {
	client := fakeClient()
	_, err := buildWorkers(context.Background(), client, &workerInput{
		repo:     "/repo",
		tasks:    []string{"intro paragraph", "step 1", "step 2"},
		branches: []string{"only-one-branch"},
	})
	if err == nil {
		t.Fatal("want error for 3 tasks paired with 1 branch, got nil")
	}
	if !strings.Contains(err.Error(), "3 tasks but 1 branches") {
		t.Errorf("error should name the mismatched counts, got: %v", err)
	}
}

// TestBuildWorkersTaskBranchCountMatchesNoError is the control for the above:
// an equal count is still accepted (this is exactly the shape --issues/--jira-issues
// leave buildWorkers with after mergeFetchedBranches pads to the full task count).
func TestBuildWorkersTaskBranchCountMatchesNoError(t *testing.T) {
	client := fakeClient()
	_, err := buildWorkers(context.Background(), client, &workerInput{
		repo:     "/repo",
		tasks:    []string{"task a", "task b"},
		branches: []string{"branch-a", "branch-b"},
	})
	if err != nil {
		t.Fatalf("buildWorkers with matching counts should succeed, got: %v", err)
	}
}

// TestBuildWorkersNoBranchesStillDefaultsWithMismatchedTaskCount proves the
// mismatch check only fires when --branches is actually given: omitting
// --branches entirely still auto-names every worker off its task, unchanged.
func TestBuildWorkersNoBranchesStillDefaultsWithMismatchedTaskCount(t *testing.T) {
	client := fakeClient()
	workers, err := buildWorkers(context.Background(), client, &workerInput{
		repo:  "/repo",
		tasks: []string{"task a", "task b", "task c"},
	})
	if err != nil {
		t.Fatalf("buildWorkers with no --branches should default, got: %v", err)
	}
	if len(workers) != 3 {
		t.Fatalf("want 3 workers, got %d", len(workers))
	}
}

// TestSpawnWorkersTasksFileMultiLineBriefWithSingleBranchErrors is the
// end-to-end regression for issue #327: passing a multi-line brief via
// --tasks-file together with a single --branches entry (the natural way to
// invoke supervise with one worker and a multi-paragraph brief) must produce
// a clear count-mismatch error, not silently spawn extra workers with
// unsafe auto-generated branch names.
func TestSpawnWorkersTasksFileMultiLineBriefWithSingleBranchErrors(t *testing.T) {
	client := fakeClient()
	brief := "Fix the parser.\n1. Do the first thing.\n2. Do the second thing.\n"
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte(brief), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := spawnWorkers(context.Background(), io.Discard, client, &workerInput{
		repo: "/pinned", tasksFile: path, branches: []string{"single-branch"},
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error: 3 tasks-file lines paired with 1 --branches entry")
	}
	if !strings.Contains(err.Error(), "3 tasks but 1 branches") {
		t.Errorf("error should name the mismatched counts, got: %v", err)
	}
}

// TestSpawnWorkersTasksFileParagraphNoBranchesErrors is the end-to-end
// regression for issue #334: the count-mismatch check above (issue #327)
// only fires once --branches is given at all, so a --tasks-file paragraph
// brief passed with no --branches — the actual live-session repro, where a
// multi-paragraph brief with blank lines between paragraphs was intended as
// a single worker — sailed straight through buildWorkers' auto-default path
// (TestBuildWorkersNoBranchesStillDefaultsWithMismatchedTaskCount) and
// silently produced one worker per line. The blank-line check inside
// loadTasksFile must catch it before that default path is ever reached, with
// no --branches involved at all.
func TestSpawnWorkersTasksFileParagraphNoBranchesErrors(t *testing.T) {
	client := fakeClient()
	brief := "Fix the parser so it handles nested quotes.\n\n" +
		"It should also handle escaped delimiters and trailing commas,\n" +
		"and emit a clear error message when the input is malformed.\n"
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte(brief), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := spawnWorkers(context.Background(), io.Discard, client, &workerInput{
		repo: "/pinned", tasksFile: path,
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err == nil {
		t.Fatal("want error: a paragraph-shaped --tasks-file brief must be refused even with no --branches given")
	}
	if !strings.Contains(err.Error(), "blank line") {
		t.Errorf("error should name the blank line, got: %v", err)
	}
}

// TestDefaultBranchTruncatesLongTask proves a very long task string (the
// whole-brief-as-one-line shape --tasks-file lines and --tasks entries can
// both produce) no longer slugs into a branch name long enough to blow past
// filesystem/git ref length limits — the root cause of issue #327's "cannot
// lock ref ... File name too long" worktree-creation failure.
func TestDefaultBranchTruncatesLongTask(t *testing.T) {
	longTask := strings.Repeat("a very long multi sentence brief describing the work in detail ", 20)
	branch := defaultBranch("", longTask, 0)
	if len(branch) > maxAutoBranchLen {
		t.Errorf("branch name too long: %d bytes, want <= %d", len(branch), maxAutoBranchLen)
	}
	if !validBranch(branch) {
		t.Errorf("truncated branch name %q should still be a valid branch name", branch)
	}
}

// TestDefaultBranchTruncationStaysDistinct proves two different long task
// strings that happen to share the same truncated prefix don't collide onto
// the same auto-generated branch name.
func TestDefaultBranchTruncationStaysDistinct(t *testing.T) {
	prefix := strings.Repeat("shared prefix text that is long enough to force truncation ", 5)
	a := defaultBranch("", prefix+"first ending", 0)
	b := defaultBranch("", prefix+"second ending", 0)
	if a == b {
		t.Errorf("two distinct long tasks with a shared prefix should not collide onto the same branch name, got %q for both", a)
	}
}

func TestRunSupervisionAttachWarnsAndSkipsCredProxy(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // openRunLog writes under ~/.argus
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-be-used")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	err := runSupervision(cmd, fakeClient(), nil, &superviseOpts{
		attach: true, base: "origin/main", review: true,
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
	if !strings.Contains(buf.String(), "--attach does not manage isolation") {
		t.Errorf("want the attach-isolation warning in output, got %q", buf.String())
	}
}

func TestRunSupervisionSpawnDryRunSkipsCredProxy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-be-used")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: gitInitDir(t)}}
	// base left empty: this test is about cred-proxy skipping under --dry-run,
	// not base-ref resolution, and gitInitDir's bare repo has no commit for a
	// real ref to resolve against.
	err := runSupervision(cmd, fakeClient(), workers, &superviseOpts{
		dryRun: true,
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
	if strings.Contains(buf.String(), "--attach") {
		t.Errorf("dry-run spawn should not print the attach warning, got %q", buf.String())
	}
}

// TestRunSupervisionWorkerRuntimeNoKeyFailsFast pins down the fix for issue
// #57: --worker-runtime docker/podman isolates the worker from the host's
// ~/.claude, so with no ANTHROPIC_API_KEY the worker previously got silently
// spawned with zero credentials and failed deep inside the container. It
// should now fail fast, before ever spawning anything.
func TestRunSupervisionWorkerRuntimeNoKeyFailsFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	err := runSupervision(cmd, fakeClient(), nil, &superviseOpts{
		base: "origin/main", workerRuntime: "docker",
	})
	var userErr *ui.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("want *ui.UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Error(), "--worker-runtime docker has no credential path") {
		t.Errorf("unexpected message: %q", userErr.Error())
	}
	if userErr.Hint == "" {
		t.Error("want an actionable hint, got none")
	}
}

// TestRunSupervisionWorkerRuntimeNoneSkipsFailFast confirms the unwrapped path
// (no runtime adapter) is unaffected: it still reaches host ~/.claude
// directly, exactly as before this fix.
func TestRunSupervisionWorkerRuntimeNoneSkipsFailFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	err := runSupervision(cmd, fakeClient(), nil, &superviseOpts{
		base: "origin/main", workerRuntime: "none",
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
}

// TestRunSupervisionWorkerRuntimeDryRunSkipsFailFast confirms --dry-run still
// previews the plan even with no ANTHROPIC_API_KEY: nothing is actually
// spawned, so there is nothing to fail fast about yet.
func TestRunSupervisionWorkerRuntimeDryRunSkipsFailFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	err := runSupervision(cmd, fakeClient(), nil, &superviseOpts{
		dryRun: true, base: "origin/main", workerRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
}

// TestRunSupervisionWorkerRuntimeWithKeySucceeds confirms a real
// ANTHROPIC_API_KEY (credproxy on) still works with a runtime adapter
// configured — the fail-fast check must not fire when a credential path
// actually exists.
func TestRunSupervisionWorkerRuntimeWithKeySucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	err := runSupervision(cmd, fakeClient(), nil, &superviseOpts{
		base: "origin/main", workerRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
}

// TestParentWorkspaceDefaultPlacementAlwaysTopLevel pins the fix: the
// "workspace" placement default now always opens a fresh top-level
// workspace, regardless of HERDR_WORKSPACE_ID. An earlier revision nested
// here whenever --repo was left to default — a standing surprise that
// contradicted the flag's own documented default (see parentWorkspace's
// docs) — so this proves HERDR_WORKSPACE_ID being set no longer changes the
// outcome at all for this placement.
func TestParentWorkspaceDefaultPlacementAlwaysTopLevel(t *testing.T) {
	for _, ws := range []string{"", "w1M"} {
		t.Setenv("HERDR_WORKSPACE_ID", ws)
		for _, placement := range []string{workerPlacementWorkspace, ""} {
			if got, err := parentWorkspace(placement); err != nil || got != "" {
				t.Errorf("parentWorkspace(%q) with HERDR_WORKSPACE_ID=%q = (%q, %v), want (\"\", nil)", placement, ws, got, err)
			}
		}
	}
}

// TestParentWorkspaceTabPlacementForcesNesting proves --worker-placement tab
// is the only way to nest, unconditionally: this is the whole point of the
// flag existing (see parentWorkspace's docs).
func TestParentWorkspaceTabPlacementForcesNesting(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "w1M")

	got, err := parentWorkspace(workerPlacementTab)
	if err != nil {
		t.Fatalf("parentWorkspace(tab): %v", err)
	}
	if got != "w1M" {
		t.Errorf("parentWorkspace(tab) = %q, want w1M", got)
	}
}

// TestParentWorkspaceTabPlacementRequiresEnclosingWorkspace proves tab mode
// fails loudly rather than silently falling back to a new top-level
// workspace when there is nothing to nest into.
func TestParentWorkspaceTabPlacementRequiresEnclosingWorkspace(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "")

	_, err := parentWorkspace(workerPlacementTab)
	if err == nil {
		t.Fatal("want an error when --worker-placement tab has no HERDR_WORKSPACE_ID to nest into")
	}
	if _, ok := errors.AsType[*ui.UserError](err); !ok {
		t.Errorf("want a *ui.UserError, got %T: %v", err, err)
	}
}

// TestParentWorkspacePanePlacementNotImplemented proves --worker-placement
// pane fails with a clear message instead of silently behaving like some
// other mode: pane-per-worker needs herdr-side support that doesn't exist
// yet (see the flag's help text).
func TestParentWorkspacePanePlacementNotImplemented(t *testing.T) {
	if _, err := parentWorkspace(workerPlacementPane); err == nil {
		t.Fatal("want an error for --worker-placement pane")
	}
}

// TestParentWorkspaceUnknownPlacementErrors proves an unrecognized
// --worker-placement value is rejected rather than silently falling back to
// some default.
func TestParentWorkspaceUnknownPlacementErrors(t *testing.T) {
	if _, err := parentWorkspace("bogus"); err == nil {
		t.Fatal("want an error for an unknown --worker-placement value")
	}
}

// TestRunSupervisionSpawnDefaultPlacementOmitsWorkspaceFlag exercises the
// actual spawn path end to end: with the default "workspace" placement and
// HERDR_WORKSPACE_ID set (as in every herdr pane), the "worktree create" call
// argus sends to herdr must never carry --workspace alongside --cwd —
// WorktreeCreate has no such flag at all (see its doc comment), and with the
// default placement no longer nesting (see parentWorkspace), no PaneMove
// call should be attempted here either.
func TestRunSupervisionSpawnDefaultPlacementOmitsWorkspaceFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_WORKSPACE_ID", "w1M")

	var worktreeCreateArgs []string
	client := herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "create" {
			worktreeCreateArgs = append([]string{}, args...)
			path := ""
			for i, a := range args {
				if a == "--path" && i+1 < len(args) {
					path = args[i+1]
				}
			}
			return []byte(`{"result":{"root_pane":{"pane_id":"w1:p1"},"worktree":{"path":"` + path + `"}}}`), nil
		}
		return nil, errors.New("stop after worktree create")
	})

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: gitInitDir(t)}}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	// The fake runner errors on every herdr call after "worktree create" (the
	// next real call is ensureFreshPane's AgentGet), so this always returns an
	// error; only the captured worktree-create args matter here. base is left
	// empty: gitInitDir's bare repo has no commit for a real ref to resolve
	// against, and this test isn't about base-ref resolution.
	_ = runSupervision(cmd, client, workers, &superviseOpts{})

	if len(worktreeCreateArgs) == 0 {
		t.Fatal("worktree create was never called")
	}
	for _, a := range worktreeCreateArgs {
		if a == "--workspace" {
			t.Fatalf("worktree create got --workspace alongside --cwd: %v", worktreeCreateArgs)
		}
	}
}

// TestTasksFlagRejectsFreeTextBrief pins down the reported bug (#40): --tasks is
// parsed as CSV by pflag, so a free-text brief containing a bare, unmatched `"`
// fails at flag-parse time with "bare \" in non-quoted-field" rather than being
// taken literally. --tasks-file (tested below) is the escape hatch for that
// content. The comma in this brief is incidental to the failure — see
// TestTasksFlagSplitsOnUnquotedCommaWithoutFailing for what a comma alone does.
func TestTasksFlagRejectsFreeTextBrief(t *testing.T) {
	cmd := newSuperviseCmd()
	brief := `Fix the parser: it treats "quoted" text, and commas, as CSV.`
	err := cmd.Flags().Parse([]string{"--tasks", brief})
	if err == nil {
		t.Fatal("want a CSV parse error for a bare-quote brief passed to --tasks, got nil")
	}
}

// TestTasksFlagSplitsOnUnquotedCommaWithoutFailing pins down the actual bug
// reported for a comma-only brief (no quotes): unlike a bare quote, an unquoted
// comma is not a parse error — pflag's CSV reader treats it as an ordinary field
// separator, so one intended brief silently becomes two, the second with its
// leading space intact. This is exactly the shape warnAmbiguousTaskSplit exists
// to flag (see TestSpawnWorkersWarnsOnUnquotedCommaSplit): the flag layer alone
// cannot fail this without also breaking a deliberate "task one,task two" list.
func TestTasksFlagSplitsOnUnquotedCommaWithoutFailing(t *testing.T) {
	cmd := newSuperviseCmd()
	if err := cmd.Flags().Parse([]string{"--tasks", "risky change, with a comma no quotes"}); err != nil {
		t.Fatalf("want no CSV parse error for an unquoted comma, got %v", err)
	}
	got, err := cmd.Flags().GetStringSlice("tasks")
	if err != nil {
		t.Fatalf("GetStringSlice: %v", err)
	}
	want := []string{"risky change", " with a comma no quotes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want the brief split in two with the leading space preserved %q, got %q", want, got)
	}
}

// TestLoadTasksFileSurvivesCommasAndQuotes proves --tasks-file sidesteps the CSV
// parsing entirely: a line containing commas and unescaped quotes comes back
// byte-for-byte instead of erroring.
func TestLoadTasksFileSurvivesCommasAndQuotes(t *testing.T) {
	brief := `Fix the parser: it treats "quoted" text, and commas, as CSV.`
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte(brief+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tasks, err := loadTasksFile(io.Discard, path)
	if err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	if len(tasks) != 1 || tasks[0] != brief {
		t.Errorf("want the brief back untouched, got %#v", tasks)
	}
}

// TestLoadTasksFileInteriorBlankLineErrors pins issue #334: a blank line
// between two non-blank lines is the paragraph-separator shape a
// multi-paragraph brief pasted into --tasks-file by mistake actually
// produces. Before this, loadTasksFile silently dropped the blank line and
// treated the two paragraphs' worth of sentences as separate one-line tasks
// — with no --branches count-mismatch to catch it when --branches was never
// given at all (see TestSpawnWorkersTasksFileParagraphNoBranchesErrors for
// the end-to-end regression). It must now refuse outright instead.
func TestLoadTasksFileInteriorBlankLineErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	content := "first task, with a comma\n\nsecond task \"quoted\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := loadTasksFile(io.Discard, path); err == nil {
		t.Fatal("want error for a blank line between non-blank lines, got nil")
	} else if !strings.Contains(err.Error(), "blank line") {
		t.Errorf("error should name the blank line, got: %v", err)
	}
}

// TestLoadTasksFileTrailingNewlinesOK is the control for the above: a
// trailing blank line (or several) at end of file — the normal shape of any
// file written with a final newline, or a few extra out of habit — is not a
// paragraph separator and must not error.
func TestLoadTasksFileTrailingNewlinesOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	content := "first task\nsecond task\n\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tasks, err := loadTasksFile(io.Discard, path)
	if err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	want := []string{"first task", "second task"}
	if len(tasks) != len(want) || tasks[0] != want[0] || tasks[1] != want[1] {
		t.Errorf("got %#v, want %#v", tasks, want)
	}
}

// TestLoadTasksFileWarnsAnomalousLineLengths covers the second-order signal:
// a paragraph broken one sentence per line, with no blank lines at all, so
// the hard blank-line check above never fires. Wildly varying line lengths
// (several short fragments next to one long line) is a softer, fuzzier
// signal, so it only warns — it must not block a run that might genuinely
// be a mixed-granularity task list.
func TestLoadTasksFileWarnsAnomalousLineLengths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	content := "Fix the parser.\n1. Do the first thing.\n2. Do the second thing in more detail than the others, spelling out every step along the way.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	tasks, err := loadTasksFile(&buf, path)
	if err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(tasks))
	}
	if !strings.Contains(buf.String(), "--tasks-file") {
		t.Errorf("want a warning about wildly varying line lengths, got %q", buf.String())
	}
}

// TestLoadTasksFileNoWarningForSimilarLengths is the control for the above:
// same-length-ish one-line tasks, the normal shape of an intentional
// multi-worker --tasks-file, produce no warning at all.
func TestLoadTasksFileNoWarningForSimilarLengths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	content := "fix the login bug\nadd retry to the uploader\nupdate the readme\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if _, err := loadTasksFile(&buf, path); err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("want no warning for similarly-sized lines, got %q", buf.String())
	}
}

func TestLoadTasksFileEmptyErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte("\n\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadTasksFile(io.Discard, path); err == nil {
		t.Fatal("want error for a tasks file with no non-empty lines, got nil")
	}
}

func TestLoadTasksFileMissingErrors(t *testing.T) {
	if _, err := loadTasksFile(io.Discard, filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("want error for a missing --tasks-file, got nil")
	}
}

// TestSpawnWorkersTasksFileAppendsToTasks exercises --tasks-file through
// spawnWorkers end to end: the file's lines land in in.tasks (appended after any
// --tasks) and flow into buildWorkers as ordinary task strings, commas and quotes
// intact.
func TestSpawnWorkersTasksFileAppendsToTasks(t *testing.T) {
	client := fakeClient()
	brief := `Full brief, with commas and "quotes" — not a CSV field.`
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte(brief+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	workers, err := spawnWorkers(context.Background(), io.Discard, client, &workerInput{
		repo: "/pinned", tasksFile: path,
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err != nil {
		t.Fatalf("spawnWorkers: %v", err)
	}
	if len(workers) != 1 || workers[0].Task != brief {
		t.Errorf("want one worker with the brief as its task, got %+v", workers)
	}
}

// TestSpawnWorkersWarnsOnUnquotedCommaSplit reproduces the exact footgun
// reported for --tasks: an unquoted comma inside one intended brief is not a
// CSV parse error, it's read as a field separator, silently turning one
// worker into two (leading space on the second field preserved). argus can't
// fail the run outright — a genuine multi-task list has the identical shape
// — but it must at least warn, since the plan otherwise looks completely
// sane despite being semantically wrong.
func TestSpawnWorkersWarnsOnUnquotedCommaSplit(t *testing.T) {
	client := fakeClient()
	var buf bytes.Buffer
	workers, err := spawnWorkers(context.Background(), &buf, client, &workerInput{
		repo: "/pinned", tasks: []string{"risky change", " with a comma no quotes"}, branches: []string{"x", "y"},
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err != nil {
		t.Fatalf("spawnWorkers: %v", err)
	}
	if len(workers) != 2 {
		t.Fatalf("want 2 workers (the run still proceeds), got %d", len(workers))
	}
	if !strings.Contains(buf.String(), "--tasks item 2") || !strings.Contains(buf.String(), "leading/trailing whitespace") {
		t.Errorf("want a warning about the ambiguous split, got %q", buf.String())
	}
}

// TestSpawnWorkersNoWarningForCleanTasks is the control: an intentional,
// evenly-written multi-task list produces no warning at all, since that's
// the ordinary --tasks usage and must not become noisy.
func TestSpawnWorkersNoWarningForCleanTasks(t *testing.T) {
	client := fakeClient()
	var buf bytes.Buffer
	_, err := spawnWorkers(context.Background(), &buf, client, &workerInput{
		repo: "/pinned", tasks: []string{"fix the login bug", "add retry to the uploader"}, branches: []string{"x", "y"},
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err != nil {
		t.Fatalf("spawnWorkers: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("want no warning for a clean task list, got %q", buf.String())
	}
}

// TestWarnMissingLabelsInDryRunWarnsForUnlabeledWorker covers the --tasks
// case the fix targets: no --labels entry is a reasonable default (BuildPlan
// falls back to the task/branch text), but --dry-run should say so up front
// instead of the operator only discovering it in herdr's own workspace list
// after the real run.
func TestWarnMissingLabelsInDryRunWarnsForUnlabeledWorker(t *testing.T) {
	var buf bytes.Buffer
	workers := []supervisor.Worker{{Task: "t", Branch: "feat-x"}}
	warnMissingLabelsInDryRun(&buf, true, false, workers)
	if !strings.Contains(buf.String(), "feat-x") || !strings.Contains(buf.String(), "no --labels entry") {
		t.Errorf("want a warning naming the branch, got %q", buf.String())
	}
}

// TestWarnMissingLabelsInDryRunSilentWhenLabeled is the control: a worker
// that already has a Label (explicit --labels, or the bare ticket key an
// --issues/--jira-issues fetch sets — see foldIssueSources) produces no
// warning at all.
func TestWarnMissingLabelsInDryRunSilentWhenLabeled(t *testing.T) {
	var buf bytes.Buffer
	workers := []supervisor.Worker{{Task: "t", Branch: "feat-x", Label: "AP-1207"}}
	warnMissingLabelsInDryRun(&buf, true, false, workers)
	if buf.Len() != 0 {
		t.Errorf("want no warning for a labeled worker, got %q", buf.String())
	}
}

// TestWarnMissingLabelsInDryRunSkipsWhenNotDryRun proves the warning is
// dry-run-only noise, not printed on a real spawn.
func TestWarnMissingLabelsInDryRunSkipsWhenNotDryRun(t *testing.T) {
	var buf bytes.Buffer
	workers := []supervisor.Worker{{Task: "t", Branch: "feat-x"}}
	warnMissingLabelsInDryRun(&buf, false, false, workers)
	if buf.Len() != 0 {
		t.Errorf("want no warning outside --dry-run, got %q", buf.String())
	}
}

// TestWarnMissingLabelsInDryRunSkipsAttach proves --attach workers (never
// spawned, so no herdr label is ever derived for them) are skipped outright.
func TestWarnMissingLabelsInDryRunSkipsAttach(t *testing.T) {
	var buf bytes.Buffer
	workers := []supervisor.Worker{{Task: "t", Branch: "feat-x"}}
	warnMissingLabelsInDryRun(&buf, true, true, workers)
	if buf.Len() != 0 {
		t.Errorf("want no warning for --attach workers, got %q", buf.String())
	}
}

// TestSpawnWorkersRelativeRepoResolvesAbsolute guards against a bug where an
// explicit `--repo .` (or any relative --repo) flowed
// through to the worker's RepoRoot unresolved. internal/supervisor/loop.go
// then joins RepoRoot with ".claude/worktrees/<branch>" to build the
// worktree path — filepath.Join collapses a leading "." so the resulting
// worktree path was itself relative, and the "cd <path> && claude ..." line
// argus types into the worker's pane silently failed whenever the pane's
// shell wasn't sitting at exactly the directory --repo was relative to.
func TestSpawnWorkersRelativeRepoResolvesAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	client := fakeClient()
	workers, err := spawnWorkers(context.Background(), io.Discard, client, &workerInput{
		repo: ".", tasks: []string{"eos#1"},
	}, nil, nil, nil, jiraSpawnOpts{}, "", false, briefNoteOverride{})
	if err != nil {
		t.Fatalf("spawnWorkers: %v", err)
	}
	if len(workers) != 1 {
		t.Fatalf("want 1 worker, got %d", len(workers))
	}
	if !filepath.IsAbs(workers[0].RepoRoot) {
		t.Errorf("RepoRoot from relative --repo should be resolved absolute, got %q", workers[0].RepoRoot)
	}
	wantAbs, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	if workers[0].RepoRoot != wantAbs {
		t.Errorf("RepoRoot: got %q want %q", workers[0].RepoRoot, wantAbs)
	}
}

// supervisor.ResolveGateBase's precedence — what this file's own
// resolveSuperviseBase tests used to pin directly — now lives in
// internal/supervisor's own TestResolveGateBase* tests (base_test.go),
// alongside ResolveBase and DetectDefaultBase. See in particular
// TestResolveGateBaseAgreesForSupervisePathAndReworkPath there, and
// TestReworkAndSuperviseAgreeOnGateBase in cmd/rework_test.go, which pin
// that supervise and rework resolve an identical ref for the same
// repo/config through this one shared helper.

func TestResolveWorkerPlacementExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{WorkerPlacement: "tab"}
	got := resolveWorkerPlacement(true, workerPlacementWorkspace, &rc)
	if got != workerPlacementWorkspace {
		t.Errorf("resolveWorkerPlacement = %q, want the explicit flag value even with a repo config default", got)
	}
}

func TestApplyRepoWorktreeDirSetsEmptyWorktreeWorkers(t *testing.T) {
	workers := []supervisor.Worker{{Branch: "b", RepoRoot: "/repo-a"}}
	applyRepoWorktreeDir(workers, "..")
	if got := workers[0].WorktreeDir; got != ".." {
		t.Errorf("WorktreeDir = %q, want %q", got, "..")
	}
}

func TestApplyRepoWorktreeDirLeavesExplicitWorktreeAlone(t *testing.T) {
	// --attach's workers already carry an explicit Worktree (the existing
	// directory being observed); a repo's configured worktree_dir must not
	// touch them.
	workers := []supervisor.Worker{{Branch: "b", Worktree: "/pinned/path"}}
	applyRepoWorktreeDir(workers, "..")
	if got := workers[0].WorktreeDir; got != "" {
		t.Errorf("WorktreeDir = %q, want empty for a worker with an explicit Worktree", got)
	}
}

func TestResolveWorkerPlacementPrefersRepoConfig(t *testing.T) {
	rc := repoconfig.Config{WorkerPlacement: "tab"}
	got := resolveWorkerPlacement(false, workerPlacementWorkspace, &rc)
	if got != "tab" {
		t.Errorf("resolveWorkerPlacement = %q, want the repo config value when the flag was not passed", got)
	}
}

func TestResolveWorkerPlacementFallsBackToFlagDefault(t *testing.T) {
	got := resolveWorkerPlacement(false, workerPlacementWorkspace, &repoconfig.Config{})
	if got != workerPlacementWorkspace {
		t.Errorf("resolveWorkerPlacement = %q, want the flag's own default when config sets nothing", got)
	}
}

func TestResolveReviewEffortExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{ReviewEffort: "max"}
	got := resolveReviewEffort(true, "low", &rc)
	if got != "low" {
		t.Errorf("resolveReviewEffort = %q, want the explicit flag value even with a repo config default", got)
	}
}

func TestResolveReviewEffortPrefersRepoConfig(t *testing.T) {
	rc := repoconfig.Config{ReviewEffort: "max"}
	got := resolveReviewEffort(false, "", &rc)
	if got != "max" {
		t.Errorf("resolveReviewEffort = %q, want the repo config value when the flag was not passed", got)
	}
}

func TestResolveReviewEffortFallsBackToFlagDefault(t *testing.T) {
	got := resolveReviewEffort(false, "", &repoconfig.Config{})
	if got != "" {
		t.Errorf("resolveReviewEffort = %q, want the flag's own default (empty, claude's default) when config sets nothing", got)
	}
}

func TestResolveLauncherExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{Launcher: "codex --full-auto"}
	got := resolveLauncher(true, "aider --yes", &rc)
	if got != "aider --yes" {
		t.Errorf("resolveLauncher = %q, want the explicit flag value even with a repo config default", got)
	}
}

func TestResolveLauncherPrefersRepoConfig(t *testing.T) {
	rc := repoconfig.Config{Launcher: "codex --full-auto"}
	got := resolveLauncher(false, supervisor.DefaultLauncher, &rc)
	if got != "codex --full-auto" {
		t.Errorf("resolveLauncher = %q, want the repo config value when the flag was not passed", got)
	}
}

func TestResolveLauncherFallsBackToFlagDefault(t *testing.T) {
	got := resolveLauncher(false, supervisor.DefaultLauncher, &repoconfig.Config{})
	if got != supervisor.DefaultLauncher {
		t.Errorf("resolveLauncher = %q, want the flag's own default when config sets nothing", got)
	}
}

// TestRunSupervisionSpawnDryRunPlumbsRepoAllow checks that superviseOpts.repoAllow
// (loaded from .argus/config.yml) reaches every worker's generated
// settings.local.json, the same way --allow always has — the dry-run plan
// prints each worker's rendered settings verbatim.
func TestRunSupervisionSpawnDryRunPlumbsRepoAllow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-be-used")

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetContext(context.Background())

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: gitInitDir(t)}}
	// base left empty: this test is about repoAllow reaching the rendered
	// plan, not base-ref resolution, and gitInitDir's bare repo has no commit
	// for a real ref to resolve against.
	err := runSupervision(cmd, fakeClient(), workers, &superviseOpts{
		dryRun: true, repoAllow: []string{"Bash(pnpm *)"},
	})
	if err != nil {
		t.Fatalf("runSupervision: %v", err)
	}
	if !strings.Contains(buf.String(), "Bash(pnpm *)") {
		t.Errorf("dry-run plan should reflect the repo config allow list; got:\n%s", buf.String())
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

// TestSuperviseVerifyCmdFlagDeprecatedAliasStillWorks and
// TestSuperviseWorktreeSetupCmdFlagDeprecatedAliasStillWorks pin the flag
// rename's backward-compat contract for supervise's two renamed flags:
// --verify-cmd -> --gate-verify-command and --worktree-setup-cmd ->
// --worktree-bootstrap-command. Each old name must still parse (bound to
// the same variable as its replacement) and print a deprecation warning.
func TestSuperviseVerifyCmdFlagDeprecatedAliasStillWorks(t *testing.T) {
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.ParseFlags([]string{"--verify-cmd", "make lint"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cmd.Flags().Changed("verify-cmd") {
		t.Error("Changed(\"verify-cmd\") = false, want true")
	}
	f := cmd.Flags().Lookup("gate-verify-command")
	if f == nil {
		t.Fatal("expected --gate-verify-command flag to be registered")
	}
	if got := f.Value.String(); got != "make lint" {
		t.Errorf("--gate-verify-command's bound value = %q, want %q (shared with --verify-cmd)", got, "make lint")
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "gate-verify-command") {
		t.Errorf("output = %q, want a deprecation warning pointing at --gate-verify-command", buf.String())
	}
}

func TestSuperviseWorktreeSetupCmdFlagDeprecatedAliasStillWorks(t *testing.T) {
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.ParseFlags([]string{"--worktree-setup-cmd", "cp ../.env .env"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cmd.Flags().Changed("worktree-setup-cmd") {
		t.Error("Changed(\"worktree-setup-cmd\") = false, want true")
	}
	f := cmd.Flags().Lookup("worktree-bootstrap-command")
	if f == nil {
		t.Fatal("expected --worktree-bootstrap-command flag to be registered")
	}
	if got := f.Value.String(); got != "cp ../.env .env" {
		t.Errorf("--worktree-bootstrap-command's bound value = %q, want %q (shared with --worktree-setup-cmd)", got, "cp ../.env .env")
	}
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "worktree-bootstrap-command") {
		t.Errorf("output = %q, want a deprecation warning pointing at --worktree-bootstrap-command", buf.String())
	}
}

// TestSuperviseNewFlagNamesNoDeprecationWarning is the other half: the new
// flag names print no warning and need no old-name involvement.
func TestSuperviseNewFlagNamesNoDeprecationWarning(t *testing.T) {
	cmd := newSuperviseCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.ParseFlags([]string{"--gate-verify-command", "make lint", "--worktree-bootstrap-command", "cp ../.env .env"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cmd.Flags().Changed("verify-cmd") || cmd.Flags().Changed("worktree-setup-cmd") {
		t.Error("old flag names should not report Changed when only the new names were passed")
	}
	if buf.Len() != 0 {
		t.Errorf("output = %q, want no deprecation warning for the new flag names", buf.String())
	}
}
