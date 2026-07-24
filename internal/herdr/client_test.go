package herdr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// paneListFixture is a trimmed but real `herdr pane list` reply (three panes
// lifted verbatim from a live session: an idle worker in a repo root, a worker
// inside a worktree, and a bare shell with no agent). It exercises the fields
// argus actually decodes.
const paneListFixture = `{"id":"cli:pane:list","result":{"panes":[
{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"62e8be33-2e70-4455-b7f4-371f3bc35eab"},"agent_status":"idle","cwd":"/Users/r/Coding/elysium-labs/eos","focused":false,"pane_id":"wT:p1","tab_id":"wT:t1","workspace_id":"wT"},
{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"5ff6840c-a641-4229-9892-a4f526b97dbc"},"agent_status":"done","cwd":"/Users/r/Coding/elysium-labs/eos/.claude/worktrees/test-coverage-review","focused":true,"pane_id":"wY:p1","tab_id":"wY:t1","workspace_id":"wY"},
{"agent_status":"unknown","cwd":"/Users/r/Coding/elysium-labs/themis","focused":false,"pane_id":"w18:pA","tab_id":"w18:t1","workspace_id":"w18"}
],"type":"pane_list"}}`

func fakeRunner(reply string, err error) Runner {
	return func(_ context.Context, _ ...string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(reply), nil
	}
}

func TestPaneListDecodesRealFixture(t *testing.T) {
	c := NewWithRunner(fakeRunner(paneListFixture, nil))
	panes, err := c.PaneList(context.Background())
	if err != nil {
		t.Fatalf("PaneList: %v", err)
	}
	if len(panes) != 3 {
		t.Fatalf("want 3 panes, got %d", len(panes))
	}

	first := panes[0]
	if first.PaneID != "wT:p1" {
		t.Errorf("PaneID: got %q want wT:p1", first.PaneID)
	}
	if first.Cwd != "/Users/r/Coding/elysium-labs/eos" {
		t.Errorf("Cwd: got %q", first.Cwd)
	}
	if first.Agent != "claude" || first.AgentStatus != "idle" {
		t.Errorf("agent fields: got %q/%q", first.Agent, first.AgentStatus)
	}
	if first.AgentSession.Value != "62e8be33-2e70-4455-b7f4-371f3bc35eab" {
		t.Errorf("session value: got %q", first.AgentSession.Value)
	}
	if first.Focused {
		t.Errorf("first pane should not be focused")
	}

	// The worktree pane is the one argus would watch for review.
	if panes[1].Cwd == panes[0].Cwd {
		t.Errorf("worktree pane should have a distinct cwd")
	}
	if !panes[1].Focused {
		t.Errorf("second pane should be focused")
	}

	// Bare shell: no agent, still a valid pane record.
	if panes[2].Agent != "" || panes[2].AgentStatus != "unknown" {
		t.Errorf("bare shell pane decoded wrong: %+v", panes[2])
	}
}

func TestPaneListSurfacesErrorEnvelope(t *testing.T) {
	reply := `{"id":"cli:pane:list","error":{"message":"no such workspace"}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	_, err := c.PaneList(context.Background())
	if err == nil {
		t.Fatal("want error from error envelope, got nil")
	}
}

func TestPaneListPropagatesRunnerError(t *testing.T) {
	sentinel := errors.New("herdr not found")
	c := NewWithRunner(fakeRunner("", sentinel))
	_, err := c.PaneList(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped runner error, got %v", err)
	}
}

func TestWorktreeCreateReturnsRootPane(t *testing.T) {
	// Trimmed real `herdr worktree create --json` reply.
	reply := `{"id":"cli:worktree:create","result":{
"root_pane":{"pane_id":"w1M:p1","cwd":"/tmp/wt","workspace_id":"w1M"},
"worktree":{"branch":"argus-x","path":"/tmp/wt","is_linked_worktree":true}
}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	wt, err := c.WorktreeCreate(context.Background(), &WorktreeSpec{
		Cwd: "/repo", Branch: "argus-x", Base: "main", Path: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("WorktreeCreate: %v", err)
	}
	if wt.RootPaneID != "w1M:p1" {
		t.Errorf("root pane: got %q want w1M:p1", wt.RootPaneID)
	}
	if wt.Path != "/tmp/wt" {
		t.Errorf("path: got %q", wt.Path)
	}
}

func TestWorktreeCreatePassesWorkspaceWhenSet(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{"root_pane":{"pane_id":"w1:p1"},"worktree":{"path":"/tmp/wt"}}}`), nil
	})
	if _, err := c.WorktreeCreate(context.Background(), &WorktreeSpec{
		Cwd: "/repo", Branch: "argus-x", Base: "main", Path: "/tmp/wt", Workspace: "w1M",
	}); err != nil {
		t.Fatalf("WorktreeCreate: %v", err)
	}
	want := []string{"worktree", "create", "--cwd", "/repo", "--branch", "argus-x", "--base", "main", "--path", "/tmp/wt", "--no-focus", "--json", "--workspace", "w1M"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args: got %v want %v", gotArgs, want)
	}
}

func TestWorktreeCreateOmitsWorkspaceWhenUnset(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{"root_pane":{"pane_id":"w1:p1"},"worktree":{"path":"/tmp/wt"}}}`), nil
	})
	if _, err := c.WorktreeCreate(context.Background(), &WorktreeSpec{
		Cwd: "/repo", Branch: "argus-x", Base: "main", Path: "/tmp/wt",
	}); err != nil {
		t.Fatalf("WorktreeCreate: %v", err)
	}
	want := []string{"worktree", "create", "--cwd", "/repo", "--branch", "argus-x", "--base", "main", "--path", "/tmp/wt", "--no-focus", "--json"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args: got %v want %v", gotArgs, want)
	}
}

func TestAgentGetReportsLiveAgent(t *testing.T) {
	reply := `{"id":"cli:agent:get","result":{"agent":{"pane_id":"w1:p1","agent":"claude","agent_status":"done"}}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	pane, ok, err := c.AgentGet(context.Background(), "w1:p1")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true for a live agent")
	}
	if pane.PaneID != "w1:p1" || pane.AgentStatus != "done" {
		t.Errorf("unexpected pane: %+v", pane)
	}
}

func TestAgentGetReportsNoAgentWithoutError(t *testing.T) {
	c := NewWithRunner(func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("herdr agent get: %w", ErrAgentNotFound)
	})
	_, ok, err := c.AgentGet(context.Background(), "w1:p1")
	if err != nil {
		t.Fatalf("want no error for agent_not_found, got %v", err)
	}
	if ok {
		t.Error("want ok=false when herdr has no live agent for the target")
	}
}

func TestAgentGetPropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("herdr: socket unavailable")
	c := NewWithRunner(fakeRunner("", sentinel))
	_, ok, err := c.AgentGet(context.Background(), "w1:p1")
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the runner error propagated, got %v", err)
	}
	if ok {
		t.Error("want ok=false on error")
	}
}

func TestAgentPromptSendsTextToTarget(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{}}`), nil
	})
	if err := c.AgentPrompt(context.Background(), "w1:p1", "hello", 30*time.Second); err != nil {
		t.Fatalf("AgentPrompt: %v", err)
	}
	want := []string{"agent", "prompt", "w1:p1", "hello", "--wait", "--until", "working", "--timeout", "30000"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("AgentPrompt args = %v, want %v", gotArgs, want)
	}
}

func TestAgentPromptZeroTimeoutOmitsTimeoutFlag(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{}}`), nil
	})
	if err := c.AgentPrompt(context.Background(), "w1:p1", "hello", 0); err != nil {
		t.Fatalf("AgentPrompt: %v", err)
	}
	want := []string{"agent", "prompt", "w1:p1", "hello", "--wait", "--until", "working"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("AgentPrompt args = %v, want %v", gotArgs, want)
	}
}

func TestAgentPromptPropagatesTimeoutError(t *testing.T) {
	sentinel := errors.New(`herdr agent: exit status 1: {"error":{"code":"timeout","message":"no state change observed"}}`)
	c := NewWithRunner(fakeRunner("", sentinel))
	err := c.AgentPrompt(context.Background(), "w1:p1", "hello", 5*time.Second)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the runner's timeout error propagated, got %v", err)
	}
}

func TestPaneCloseSendsPaneCloseCommand(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{}}`), nil
	})
	if err := c.PaneClose(context.Background(), "w1:p1"); err != nil {
		t.Fatalf("PaneClose: %v", err)
	}
	want := []string{"pane", "close", "w1:p1"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("PaneClose args = %v, want %v", gotArgs, want)
	}
}

func TestPaneClosePropagatesRunnerError(t *testing.T) {
	sentinel := errors.New("herdr: no such pane")
	c := NewWithRunner(fakeRunner("", sentinel))
	if err := c.PaneClose(context.Background(), "w1:p1"); !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped runner error, got %v", err)
	}
}

func TestWorkspaceCloseSendsWorkspaceCloseCommand(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{}}`), nil
	})
	if err := c.WorkspaceClose(context.Background(), "w1"); err != nil {
		t.Fatalf("WorkspaceClose: %v", err)
	}
	want := []string{"workspace", "close", "w1"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("WorkspaceClose args = %v, want %v", gotArgs, want)
	}
}

func TestWorkspaceClosePropagatesRunnerError(t *testing.T) {
	sentinel := errors.New("herdr: no such workspace")
	c := NewWithRunner(fakeRunner("", sentinel))
	if err := c.WorkspaceClose(context.Background(), "w1"); !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped runner error, got %v", err)
	}
}

func TestAgentWaitBuildsUntilFlagsAndTimeout(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{"agent":{"pane_id":"w1:p1","agent_status":"idle"}}}`), nil
	})
	pane, err := c.AgentWait(context.Background(), "w1:p1", []string{"idle", "blocked", "done"}, 15*time.Second)
	if err != nil {
		t.Fatalf("AgentWait: %v", err)
	}
	want := []string{"agent", "wait", "w1:p1", "--until", "idle", "--until", "blocked", "--until", "done", "--timeout", "15000"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("AgentWait args = %v, want %v", gotArgs, want)
	}
	if pane.PaneID != "w1:p1" || pane.AgentStatus != "idle" {
		t.Errorf("unexpected pane: %+v", pane)
	}
}

func TestAgentWaitZeroTimeoutOmitsTimeoutFlag(t *testing.T) {
	var gotArgs []string
	c := NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"result":{"agent":{"pane_id":"w1:p1"}}}`), nil
	})
	if _, err := c.AgentWait(context.Background(), "w1:p1", []string{"idle"}, 0); err != nil {
		t.Fatalf("AgentWait: %v", err)
	}
	want := []string{"agent", "wait", "w1:p1", "--until", "idle"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("AgentWait args = %v, want %v", gotArgs, want)
	}
}

func TestAgentWaitWrapsTimeoutCode(t *testing.T) {
	sentinel := fmt.Errorf("herdr agent: %w", ErrWaitTimeout)
	c := NewWithRunner(fakeRunner("", sentinel))
	_, err := c.AgentWait(context.Background(), "w1:p1", []string{"idle"}, time.Second)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("want ErrWaitTimeout, got %v", err)
	}
}

func TestAgentWaitPropagatesOtherErrors(t *testing.T) {
	sentinel := errors.New("herdr: socket unavailable")
	c := NewWithRunner(fakeRunner("", sentinel))
	_, err := c.AgentWait(context.Background(), "w1:p1", []string{"idle"}, time.Second)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the runner error propagated, got %v", err)
	}
}

func TestPaneSplitReturnsNewID(t *testing.T) {
	reply := `{"id":"cli:pane:split","result":{"pane":{"pane_id":"wZ:p2"}}}`
	c := NewWithRunner(fakeRunner(reply, nil))
	id, err := c.PaneSplit(context.Background(), "wZ:p1", SplitRight)
	if err != nil {
		t.Fatalf("PaneSplit: %v", err)
	}
	if id != "wZ:p2" {
		t.Errorf("got %q want wZ:p2", id)
	}
}
