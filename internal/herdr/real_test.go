package herdr

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealWorktreeCreateAndPaneMoveNest exercises WorktreeCreate and PaneMove
// against a live herdr binary instead of a mocked Runner. It exists because a
// mocked-Runner test can only ever assert on the args argus sends — it can't
// catch a wrong belief about what herdr actually does with them. That's
// exactly what shipped in issue #216's first pass: WorktreeCreate's own
// --workspace param was believed to nest a pane into an existing workspace
// (mocked tests happily encoded that belief), but a real herdr never does
// that — `worktree create --workspace <id>` always opens its own new
// top-level workspace regardless, confirmed only by running the real binary.
// PaneMove (pane move --new-tab) is the actual nesting primitive; this test
// pins that down against a live server so this class of bug can't pass on
// mocks alone again.
//
// Skipped unless herdr is on PATH and a live session is reachable
// (HERDR_SOCKET_PATH set) — this never runs in ordinary CI, only on a
// developer machine already inside a herdr session.
func TestRealWorktreeCreateAndPaneMoveNest(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not on PATH")
	}
	if os.Getenv("HERDR_SOCKET_PATH") == "" {
		t.Skip("no live herdr session (HERDR_SOCKET_PATH unset)")
	}

	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")

	// A brand-new top-level workspace gives PaneMove a genuine, isolated
	// "repo parent" to nest into — reusing an existing real pane's workspace
	// risks disturbing someone else's live session, and a linked worktree's
	// own workspace doesn't qualify anyway (herdr's "linked_worktree_source"
	// check refuses to nest into one, confirmed directly).
	parentPaneID, parentWorkspace := realHerdrWorkspaceCreate(t, repo)
	t.Cleanup(func() { _ = realHerdrCLI(t, "pane", "close", parentPaneID) })

	client := New()
	ctx := context.Background()
	wt, err := client.WorktreeCreate(ctx, &WorktreeSpec{
		Cwd: repo, Branch: "argus-real-nesting-test", Base: "main",
		Path: filepath.Join(t.TempDir(), "wt"), Label: "argus-real-nesting-test",
	})
	if err != nil {
		t.Fatalf("WorktreeCreate against live herdr: %v", err)
	}
	if wt.RootPaneID == "" {
		t.Fatal("WorktreeCreate returned no root pane")
	}

	pane, err := client.PaneMove(ctx, wt.RootPaneID, parentWorkspace)
	if err != nil {
		t.Fatalf("PaneMove against live herdr: %v", err)
	}
	t.Cleanup(func() { _ = realHerdrCLI(t, "pane", "close", pane.PaneID) })

	if !strings.HasPrefix(pane.PaneID, parentWorkspace+":") {
		t.Errorf("moved pane %q is not under parent workspace %q — nesting did not actually happen", pane.PaneID, parentWorkspace)
	}
}

// realHerdrWorkspaceCreate opens a fresh, isolated top-level herdr workspace
// rooted at cwd and returns its root pane and workspace id. It shells out to
// the herdr binary directly rather than through Client, since creating a
// throwaway parent workspace is test scaffolding argus itself never needs to
// do in production.
func realHerdrWorkspaceCreate(t *testing.T, cwd string) (paneID, workspaceID string) {
	t.Helper()
	out := realHerdrCLI(t, "workspace", "create", "--cwd", cwd, "--no-focus")
	var reply struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
			Workspace struct {
				WorkspaceID string `json:"workspace_id"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		t.Fatalf("decoding workspace create reply: %v\n%s", err, out)
	}
	if reply.Result.RootPane.PaneID == "" || reply.Result.Workspace.WorkspaceID == "" {
		t.Fatalf("workspace create reply missing ids: %s", out)
	}
	return reply.Result.RootPane.PaneID, reply.Result.Workspace.WorkspaceID
}

// realHerdrCLI runs the real herdr binary and returns its stdout, failing the
// test on a non-zero exit. It deliberately does not use t.Context(): that
// context is already canceled by the time a t.Cleanup callback runs, and
// every use here (including cleanup) needs the process to actually complete.
func realHerdrCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "herdr", args...).Output()
	if err != nil {
		t.Fatalf("herdr %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// runGit runs git in dir, failing the test on a non-zero exit. The scratch
// repo it operates on lives entirely under t.TempDir(), so Go's own test
// cleanup removes it — no git-worktree-remove/rm -rf dance needed here.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
