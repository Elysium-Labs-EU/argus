// Package herdr is a thin, typed wrapper over the herdr CLI. argus talks to
// herdr only through its command line (herdr is a separate Rust process with
// its own release cadence), so every call here shells out to the herdr binary
// and decodes the JSON it prints. All process execution — the only I/O in this
// package — is funneled through a single injectable Runner so the decoding
// logic is testable without spawning herdr.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// Runner executes the herdr binary with the given args and returns its stdout.
// It is the one effectful seam in this package; tests substitute a fake.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// ErrAgentNotFound is what AgentGet's err wraps when herdr reports its
// "agent_not_found" code: the target pane has no live agent session herdr
// recognizes (a bare shell prompt), not a transport or decoding failure.
var ErrAgentNotFound = errors.New("herdr: agent not found")

// ErrWaitTimeout is what AgentWait's err wraps when herdr's own --timeout
// elapses before the pane reaches one of the requested states, rather than a
// transport or decoding failure.
var ErrWaitTimeout = errors.New("herdr: agent wait timed out")

// ErrAgentPromptStalled is what AgentPrompt's err wraps when herdr reports its
// "agent_prompt_stalled" code: the prompt was accepted but the pane's
// agent_status never observably changed within the wait window. AgentGet
// already confirmed a live agent occupies the pane, so this means the prompt
// landed on one already idle/done from a completed prior turn — not a dead
// pane (that surfaces as ErrAgentNotFound) — which callers can recover from
// with a plain pane submission instead.
var ErrAgentPromptStalled = errors.New("herdr: agent prompt stalled")

// ErrWorkspaceNotFound is what WorkspaceClose's err wraps when herdr reports
// its "workspace_not_found" code: the workspace is already gone by the time
// the call lands — herdr closes a workspace itself the moment its last pane
// closes, so an explicit close issued right after PaneClose on that same
// pane is racing a state herdr already reached, not a transport failure.
var ErrWorkspaceNotFound = errors.New("herdr: workspace not found")

// Client issues typed calls to herdr.
type Client struct {
	run Runner
}

// New returns a Client that shells out to the real herdr binary on PATH.
func New() Client {
	return Client{run: execRunner("herdr")}
}

// NewWithRunner returns a Client backed by a caller-supplied Runner, for tests.
func NewWithRunner(r Runner) Client {
	return Client{run: r}
}

func execRunner(bin string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		//nolint:gosec // bin is the fixed "herdr" binary and args are argus-composed herdr subcommands (pane/worktree ids from herdr itself), not user shell input
		out, err := exec.CommandContext(ctx, bin, args...).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				switch errorCode(ee.Stderr) {
				case "agent_not_found":
					return nil, fmt.Errorf("herdr %s: %w", args[0], ErrAgentNotFound)
				case "timeout":
					return nil, fmt.Errorf("herdr %s: %w", args[0], ErrWaitTimeout)
				case "agent_prompt_stalled":
					return nil, fmt.Errorf("herdr %s: %w", args[0], ErrAgentPromptStalled)
				case "workspace_not_found":
					return nil, fmt.Errorf("herdr %s: %w", args[0], ErrWorkspaceNotFound)
				}
				return nil, fmt.Errorf("herdr %s: %w: %s", args[0], err, ee.Stderr)
			}
			return nil, fmt.Errorf("herdr %s: %w", args[0], err)
		}
		return out, nil
	}
}

// errorCode extracts a herdr error envelope's machine-readable code from a
// failed command's stderr, or "" if stderr isn't a decodable error envelope.
func errorCode(stderr []byte) string {
	var env envelope
	if json.Unmarshal(stderr, &env) != nil || env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// AgentSession identifies the Claude Code session driving a pane. Its Value is
// the session UUID, which argus uses to locate that worker's session transcript
// for token accounting.
type AgentSession struct {
	Agent string `json:"agent"`
	Value string `json:"value"`
}

// Pane is the subset of a herdr pane record argus needs: enough to identify it,
// know where it's rooted, and see its detected agent state.
type Pane struct {
	PaneID       string       `json:"pane_id"`
	Cwd          string       `json:"cwd"`
	Agent        string       `json:"agent"`
	AgentStatus  string       `json:"agent_status"`
	WorkspaceID  string       `json:"workspace_id"`
	TabID        string       `json:"tab_id"`
	AgentSession AgentSession `json:"agent_session"`
	Focused      bool         `json:"focused"`
}

// envelope is herdr's common JSON reply wrapper. On success Result carries the
// command payload; on failure Error is populated.
type envelope struct {
	Error  *envelopeError  `json:"error"`
	Result json.RawMessage `json:"result"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeEnvelope(data []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decoding herdr reply: %w", err)
	}
	if env.Error != nil {
		return fmt.Errorf("herdr reported an error: %s", env.Error.Message)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decoding herdr result: %w", err)
	}
	return nil
}

// PaneList returns every pane herdr currently tracks.
func (c Client) PaneList(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "pane", "list")
	if err != nil {
		return nil, err
	}
	var result struct {
		Panes []Pane `json:"panes"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return nil, err
	}
	return result.Panes, nil
}

// SplitDirection is where a new pane opens relative to an existing one.
type SplitDirection string

const (
	SplitRight SplitDirection = "right"
	SplitDown  SplitDirection = "down"
)

// PaneSplit splits pane paneID in the given direction without moving focus, and
// returns the new pane's id.
func (c Client) PaneSplit(ctx context.Context, paneID string, dir SplitDirection) (string, error) {
	out, err := c.run(ctx, "pane", "split", paneID, "--direction", string(dir), "--no-focus")
	if err != nil {
		return "", err
	}
	var result struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return "", err
	}
	if result.Pane.PaneID == "" {
		return "", fmt.Errorf("herdr pane split returned no pane id")
	}
	return result.Pane.PaneID, nil
}

// PaneRun sends a command line plus Enter to a pane, the same as typing it at
// a bare interactive shell prompt. It is the wrong call for a pane that
// already has a live agent session running (see AgentGet, AgentPrompt): the
// agent's own input box would receive the literal shell text as a chat
// message instead of a command a shell executes.
func (c Client) PaneRun(ctx context.Context, paneID, command string) error {
	_, err := c.run(ctx, "pane", "run", paneID, command)
	return err
}

// PaneSendKeys sends literal key presses to a pane, the same as pressing them
// on the keyboard while it's focused — e.g. "enter" to submit text already
// typed but unsent in a live agent's own input box, since PaneRun's trailing
// newline landing as a genuine Enter keypress there is a matter of timing,
// not something PaneRun's own reply confirms.
func (c Client) PaneSendKeys(ctx context.Context, paneID string, keys ...string) error {
	args := append([]string{"pane", "send-keys", paneID}, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// PaneClose closes pane paneID.
func (c Client) PaneClose(ctx context.Context, paneID string) error {
	_, err := c.run(ctx, "pane", "close", paneID)
	return err
}

// WorkspaceClose closes workspace workspaceID.
func (c Client) WorkspaceClose(ctx context.Context, workspaceID string) error {
	_, err := c.run(ctx, "workspace", "close", workspaceID)
	return err
}

// AgentGet reports the agent herdr currently tracks for target (a pane id),
// and whether one exists at all. ok is false with a nil error when herdr's
// "agent_not_found" tells us the pane has no live agent — a bare shell
// prompt — which is an expected outcome for a caller deciding how to
// dispatch into it, not a failure.
func (c Client) AgentGet(ctx context.Context, target string) (Pane, bool, error) {
	out, err := c.run(ctx, "agent", "get", target)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			return Pane{}, false, nil
		}
		return Pane{}, false, err
	}
	var result struct {
		Agent Pane `json:"agent"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return Pane{}, false, err
	}
	return result.Agent, true, nil
}

// AgentPrompt submits text as a new prompt to the agent already running in
// target (a pane id), the same as typing it into that agent's own input box
// and pressing Enter. Use this — not PaneRun — when AgentGet reports a live
// agent already occupies the pane; it re-tasks that session instead of
// trying to launch a second one over it.
//
// A bare submission succeeds identically whether or not the agent ever
// reacts to it: herdr accepts the text and returns immediately, with no
// signal that it landed on a live turn rather than being silently dropped
// (argus issue #135 — an idle/done agent, or two AgentPrompt calls racing
// each other, produced no error and no observable effect, and the caller's
// subsequent status.json poll then waited forever). So this always passes
// herdr's own `--wait --until working` flags, which block in herdr until it
// observes the pane's agent_status actually become "working" — the only
// confirmation available that the prompt was picked up, not just accepted.
// timeout bounds that wait via `--timeout`; timeout <= 0 waits indefinitely.
func (c Client) AgentPrompt(ctx context.Context, target, text string, timeout time.Duration) error {
	args := []string{"agent", "prompt", target, text, "--wait", "--until", "working"}
	if timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	}
	_, err := c.run(ctx, args...)
	return err
}

// AgentWait blocks until pane target's agent reaches one of the states in
// until (herdr's own generic pane lifecycle vocabulary — "idle", "working",
// "blocked", "done", "unknown" — distinct from any caller-specific status),
// or timeout elapses (<=0 waits indefinitely), and returns the matched pane.
// This is a server-mediated blocking wait, not a client-side sleep-poll: the
// call only returns once herdr itself observes the pane reach one of until.
//
// herdr's wait is level-triggered — a pane already sitting in one of until
// when the call is made returns immediately — so a caller looping on this
// must not treat a fast return as confirmation that something just changed,
// only that it's worth checking again.
//
// err wraps ErrWaitTimeout when herdr's own --timeout elapsed with no match,
// which callers should treat as a cue to retry, not as a hard failure.
func (c Client) AgentWait(ctx context.Context, target string, until []string, timeout time.Duration) (Pane, error) {
	args := make([]string, 0, 3+2*len(until)+2)
	args = append(args, "agent", "wait", target)
	for _, u := range until {
		args = append(args, "--until", u)
	}
	if timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return Pane{}, err
	}
	var result struct {
		Agent Pane `json:"agent"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return Pane{}, err
	}
	return result.Agent, nil
}

// WorktreeSpec describes a worktree for herdr to create.
type WorktreeSpec struct {
	Cwd       string // repo the worktree derives from
	Branch    string // new branch name
	Base      string // base ref, e.g. origin/main
	Path      string // where to place the worktree
	Label     string // herdr workspace label
	Workspace string // parent workspace id to nest the new pane under as a tab; "" opens a new top-level workspace
}

// Worktree is what herdr created: the checkout path (which argus supplied) and
// the id of the root pane herdr opened inside it. argus runs the worker in that
// pane, so it needs no separately-provided pane.
type Worktree struct {
	Path       string
	RootPaneID string
}

// WorktreeCreate asks herdr to create a git worktree and open it, returning the
// worktree's root pane so the caller can spawn a worker there directly. spec is
// taken by pointer only to avoid copying; it is not mutated.
func (c Client) WorktreeCreate(ctx context.Context, spec *WorktreeSpec) (Worktree, error) {
	args := []string{
		"worktree", "create",
		"--cwd", spec.Cwd,
		"--branch", spec.Branch,
		"--base", spec.Base,
		"--path", spec.Path,
		"--no-focus", "--json",
	}
	if spec.Label != "" {
		args = append(args, "--label", spec.Label)
	}
	if spec.Workspace != "" {
		args = append(args, "--workspace", spec.Workspace)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return Worktree{}, err
	}
	var result struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
		Worktree struct {
			Path string `json:"path"`
		} `json:"worktree"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return Worktree{}, err
	}
	path := result.Worktree.Path
	if path == "" {
		path = spec.Path
	}
	return Worktree{Path: path, RootPaneID: result.RootPane.PaneID}, nil
}

// WorktreeOpen opens an already-existing worktree in herdr and returns its root
// pane, so a command like rebase can dispatch a worker into a worktree argus
// created earlier (whose herdr workspace may since have been closed).
//
// cwd must be the linked worktree's parent repo (see supervisor.RepoRoot),
// not path itself: herdr's `worktree open` determines whether the calling
// context is "inside a git work tree" from cwd, not from path or from the
// invoking process's own working directory. Without it, a caller whose own
// pane isn't itself rooted in a git repo (or that omits cwd altogether) gets
// "not_git_worktree" even though path is a perfectly valid worktree — this
// bit argus itself once, dispatching `argus rebase` from a pane rooted
// outside any repo. WorktreeCreate sidesteps the same check by always
// passing --cwd for the repo being worked on; this mirrors that.
func (c Client) WorktreeOpen(ctx context.Context, cwd, path string) (Worktree, error) {
	out, err := c.run(ctx, "worktree", "open", "--cwd", cwd, "--path", path, "--no-focus", "--json")
	if err != nil {
		return Worktree{}, err
	}
	var result struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
		Worktree struct {
			Path string `json:"path"`
		} `json:"worktree"`
	}
	if err := decodeEnvelope(out, &result); err != nil {
		return Worktree{}, err
	}
	p := result.Worktree.Path
	if p == "" {
		p = path
	}
	return Worktree{Path: p, RootPaneID: result.RootPane.PaneID}, nil
}
