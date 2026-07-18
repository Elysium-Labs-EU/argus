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
)

// Runner executes the herdr binary with the given args and returns its stdout.
// It is the one effectful seam in this package; tests substitute a fake.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

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
				return nil, fmt.Errorf("herdr %s: %w: %s", args[0], err, ee.Stderr)
			}
			return nil, fmt.Errorf("herdr %s: %w", args[0], err)
		}
		return out, nil
	}
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

// PaneRun sends a command line plus Enter to a pane, the same as typing it.
func (c Client) PaneRun(ctx context.Context, paneID, command string) error {
	_, err := c.run(ctx, "pane", "run", paneID, command)
	return err
}

// WorktreeSpec describes a worktree for herdr to create.
type WorktreeSpec struct {
	Cwd    string // repo the worktree derives from
	Branch string // new branch name
	Base   string // base ref, e.g. origin/main
	Path   string // where to place the worktree
	Label  string // herdr workspace label
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
