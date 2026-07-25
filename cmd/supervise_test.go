package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

func TestSpawnWorkersIssuesOnlyPassesGuard(t *testing.T) {
	// --issues alone (no --tasks/--branches/--panes) used to trip the "no
	// workers given" guard, since that check ran before --issues was folded
	// into tasks/branches. repo is a non-git tempdir so it fails downstream
	// (resolving the forge) instead — proof the guard itself let it through.
	client := fakeClient()
	_, err := spawnWorkers(context.Background(), client, &workerInput{repo: t.TempDir()}, []int{1}, nil, nil)
	if err == nil {
		t.Fatal("want a downstream error resolving the forge for a non-git repo")
	}
	if strings.Contains(err.Error(), "no workers given") {
		t.Errorf("--issues alone should satisfy the worker-source guard, got: %v", err)
	}
}

func TestFoldIssueSourcesNoop(t *testing.T) {
	in := &workerInput{tasks: []string{"existing"}, branches: []string{"existing-branch"}}
	if err := foldIssueSources(context.Background(), in, nil, nil, nil); err != nil {
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
	if err := foldIssueSources(context.Background(), in, []int{1}, nil, nil); err == nil {
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
	if err := foldIssueSources(context.Background(), in, nil, []string{"PROJ-1"}, nil); err == nil {
		t.Fatal("want error building jira client without JIRA_* env vars or a config file")
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

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: t.TempDir()}}
	err := runSupervision(cmd, fakeClient(), workers, &superviseOpts{
		dryRun: true, base: "origin/main",
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

// TestParentWorkspaceDefaultPlacement pins down the original bugfix: with the
// "workspace" placement default (unchanged current behavior), an explicit
// --repo must still win outright over HERDR_WORKSPACE_ID auto-detection —
// nesting there is a fallback for when --repo was left to default, not
// something an explicit --repo layers on top of.
func TestParentWorkspaceDefaultPlacement(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "w1M")

	for _, placement := range []string{workerPlacementWorkspace, ""} {
		if got, err := parentWorkspace(placement, true); err != nil || got != "" {
			t.Errorf("parentWorkspace(%q, repoExplicit=true) = (%q, %v), want (\"\", nil)", placement, got, err)
		}
		if got, err := parentWorkspace(placement, false); err != nil || got != "w1M" {
			t.Errorf("parentWorkspace(%q, repoExplicit=false) = (%q, %v), want (\"w1M\", nil)", placement, got, err)
		}
	}
}

// TestParentWorkspaceTabPlacementForcesNesting proves --worker-placement tab
// overrides the --repo-implies-no-nesting rule the default placement keeps:
// this is the whole point of the flag existing (see parentWorkspace's docs).
func TestParentWorkspaceTabPlacementForcesNesting(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "w1M")

	got, err := parentWorkspace(workerPlacementTab, true)
	if err != nil {
		t.Fatalf("parentWorkspace(tab, repoExplicit=true): %v", err)
	}
	if got != "w1M" {
		t.Errorf("parentWorkspace(tab, repoExplicit=true) = %q, want w1M", got)
	}
}

// TestParentWorkspaceTabPlacementRequiresEnclosingWorkspace proves tab mode
// fails loudly rather than silently falling back to a new top-level
// workspace when there is nothing to nest into.
func TestParentWorkspaceTabPlacementRequiresEnclosingWorkspace(t *testing.T) {
	t.Setenv("HERDR_WORKSPACE_ID", "")

	_, err := parentWorkspace(workerPlacementTab, false)
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
	if _, err := parentWorkspace(workerPlacementPane, false); err == nil {
		t.Fatal("want an error for --worker-placement pane")
	}
}

// TestParentWorkspaceUnknownPlacementErrors proves an unrecognized
// --worker-placement value is rejected rather than silently falling back to
// some default.
func TestParentWorkspaceUnknownPlacementErrors(t *testing.T) {
	if _, err := parentWorkspace("bogus", false); err == nil {
		t.Fatal("want an error for an unknown --worker-placement value")
	}
}

// TestRunSupervisionSpawnRepoExplicitOmitsWorkspace exercises the actual
// spawn path end to end: with HERDR_WORKSPACE_ID set (as in every herdr pane)
// and repoExplicit true, the "worktree create" call argus sends to herdr must
// never carry --workspace alongside --cwd.
func TestRunSupervisionSpawnRepoExplicitOmitsWorkspace(t *testing.T) {
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

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: t.TempDir()}}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(context.Background())

	// The fake runner errors on every herdr call after "worktree create", so
	// this always returns an error; only the captured args matter here.
	_ = runSupervision(cmd, client, workers, &superviseOpts{base: "origin/main", repoExplicit: true})

	if len(worktreeCreateArgs) == 0 {
		t.Fatal("worktree create was never called")
	}
	for _, a := range worktreeCreateArgs {
		if a == "--workspace" {
			t.Fatalf("worktree create got --workspace alongside --cwd with --repo explicit: %v", worktreeCreateArgs)
		}
	}
}

// TestTasksFlagRejectsFreeTextBrief pins down the reported bug (#40): --tasks is
// parsed as CSV by pflag, so a free-text brief containing commas and quotes fails
// at flag-parse time with "bare \" in non-quoted-field" rather than being taken
// literally. --tasks-file (tested below) is the escape hatch for that content.
func TestTasksFlagRejectsFreeTextBrief(t *testing.T) {
	cmd := newSuperviseCmd()
	brief := `Fix the parser: it treats "quoted" text, and commas, as CSV.`
	err := cmd.Flags().Parse([]string{"--tasks", brief})
	if err == nil {
		t.Fatal("want a CSV parse error for a comma-and-quote brief passed to --tasks, got nil")
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

	tasks, err := loadTasksFile(path)
	if err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	if len(tasks) != 1 || tasks[0] != brief {
		t.Errorf("want the brief back untouched, got %#v", tasks)
	}
}

func TestLoadTasksFileMultipleLinesSkipsBlank(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	content := "first task, with a comma\n\nsecond task \"quoted\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tasks, err := loadTasksFile(path)
	if err != nil {
		t.Fatalf("loadTasksFile: %v", err)
	}
	want := []string{"first task, with a comma", `second task "quoted"`}
	if len(tasks) != len(want) || tasks[0] != want[0] || tasks[1] != want[1] {
		t.Errorf("got %#v, want %#v", tasks, want)
	}
}

func TestLoadTasksFileEmptyErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte("\n\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := loadTasksFile(path); err == nil {
		t.Fatal("want error for a tasks file with no non-empty lines, got nil")
	}
}

func TestLoadTasksFileMissingErrors(t *testing.T) {
	if _, err := loadTasksFile(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
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

	workers, err := spawnWorkers(context.Background(), client, &workerInput{
		repo: "/pinned", tasksFile: path,
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("spawnWorkers: %v", err)
	}
	if len(workers) != 1 || workers[0].Task != brief {
		t.Errorf("want one worker with the brief as its task, got %+v", workers)
	}
}

// TestSpawnWorkersRelativeRepoResolvesAbsolute guards against a bug (argus
// issue #68) where an explicit `--repo .` (or any relative --repo) flowed
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
	workers, err := spawnWorkers(context.Background(), client, &workerInput{
		repo: ".", tasks: []string{"eos#1"},
	}, nil, nil, nil)
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

// runGitForSuperviseTest mirrors internal/supervisor's test-only git setup
// helper — used here to build a real repo with a detectable origin/HEAD.
func runGitForSuperviseTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// repoWithOriginHEAD builds a real git repo whose refs/remotes/origin/HEAD
// resolves to defaultBranch, by cloning a bare "origin" seeded with one
// commit there.
func repoWithOriginHEAD(t *testing.T, defaultBranch string) string {
	t.Helper()
	origin := t.TempDir()
	runGitForSuperviseTest(t, origin, "init", "-q", "--initial-branch="+defaultBranch)
	runGitForSuperviseTest(t, origin, "config", "user.email", "t@t")
	runGitForSuperviseTest(t, origin, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitForSuperviseTest(t, origin, "add", "README.md")
	runGitForSuperviseTest(t, origin, "commit", "-q", "-m", "init")

	repo := t.TempDir()
	runGitForSuperviseTest(t, filepath.Dir(repo), "clone", "-q", origin, repo)
	return repo
}

func TestResolveSuperviseBaseExplicitFlagWinsOutright(t *testing.T) {
	repo := repoWithOriginHEAD(t, "trunk")
	rc := repoconfig.Config{BaseBranch: "develop"}
	got := resolveSuperviseBase(context.Background(), true, "origin/explicit", repo, &rc)
	if got != "origin/explicit" {
		t.Errorf("resolveSuperviseBase = %q, want the explicit flag value", got)
	}
}

func TestResolveSuperviseBasePrefersRepoConfig(t *testing.T) {
	repo := repoWithOriginHEAD(t, "trunk")
	rc := repoconfig.Config{BaseBranch: "develop"}
	got := resolveSuperviseBase(context.Background(), false, "origin/main", repo, &rc)
	if got != "origin/develop" {
		t.Errorf("resolveSuperviseBase = %q, want origin/%s from repo config", got, "develop")
	}
}

func TestResolveSuperviseBaseFallsBackToDetectedOriginHEAD(t *testing.T) {
	repo := repoWithOriginHEAD(t, "trunk")
	got := resolveSuperviseBase(context.Background(), false, "origin/main", repo, &repoconfig.Config{})
	if got != "origin/trunk" {
		t.Errorf("resolveSuperviseBase = %q, want origin/%s detected from origin/HEAD", got, "trunk")
	}
}

func TestResolveSuperviseBaseFallsBackToFlagDefault(t *testing.T) {
	got := resolveSuperviseBase(context.Background(), false, "origin/main", "", &repoconfig.Config{})
	if got != "origin/main" {
		t.Errorf("resolveSuperviseBase = %q, want the flag's own default when nothing else resolves", got)
	}
}

func TestResolveWorkerPlacementExplicitFlagWinsOutright(t *testing.T) {
	rc := repoconfig.Config{WorkerPlacement: "tab"}
	got := resolveWorkerPlacement(true, workerPlacementWorkspace, &rc)
	if got != workerPlacementWorkspace {
		t.Errorf("resolveWorkerPlacement = %q, want the explicit flag value even with a repo config default", got)
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

	workers := []supervisor.Worker{{Task: "t", Branch: "b", RepoRoot: t.TempDir()}}
	err := runSupervision(cmd, fakeClient(), workers, &superviseOpts{
		dryRun: true, base: "origin/main", repoAllow: []string{"Bash(pnpm *)"},
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
