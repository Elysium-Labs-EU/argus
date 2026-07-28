// Package protocol defines the typed status contract a worker pane writes and
// argus reads. It replaces scraping terminal scrollback: each worker persists
// a single status.json under its worktree after every phase, and argus decodes
// that struct to learn worker state. The writer brief and the reader share this
// one file so the two halves can't drift.
package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Phase is the coarse lifecycle stage a worker reports. It is deliberately
// small: argus only needs to know when a worker wants review or is blocked.
type Phase string

const (
	PhasePlanning       Phase = "planning"
	PhaseWorking        Phase = "working"
	PhaseSelfTest       Phase = "self_test"
	PhaseAwaitingReview Phase = "awaiting_review"
	PhaseDone           Phase = "done"
	PhaseBlocked        Phase = "blocked"
)

// Result is the outcome of a single test a worker ran.
type Result string

const (
	ResultPass    Result = "pass"
	ResultFail    Result = "fail"
	ResultSkipped Result = "skipped"
)

// DiffStat summarizes the size of a worker's change, so argus can gate review
// intensity on it later without reading the diff itself.
type DiffStat struct {
	Files      int `json:"files"`
	Insertions int `json:"insertions"`
	Deletions  int `json:"deletions"`
}

// TestRun records one test invocation and its outcome. Cmd is the exact command
// the worker ran; Target names what it exercised (a package, a VM, a service).
type TestRun struct {
	Cmd    string `json:"cmd"`
	Target string `json:"target"`
	Result Result `json:"result"`
}

// Question is a worker's structured ask for a supervisor decision, reported
// alongside BlockedReason (which stays freeform prose for narrative context)
// so a supervisor's tooling has something machine-readable to display and
// answer against instead of only a string to read and free-type a reply to.
// Options, when non-empty, let `argus worker answer --option N` pick a choice
// by index instead of retyping it.
type Question struct {
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// Answer records a supervisor's resolution of a worker's Question. It is
// never set by a worker's own report — only `argus worker answer` writes
// it — so status.json keeps a durable trace of what was asked and how it
// was resolved, independent of the worker's own narration.
type Answer struct {
	AnsweredAt time.Time `json:"answered_at"`
	Text       string    `json:"text"`
	// Option is the 1-indexed choice into the Question's Options that produced
	// Text, or 0 when the supervisor answered with free-form text instead.
	Option int `json:"option,omitempty"`
}

// Status is the whole typed payload a worker writes to its status file. Fields
// are ordered for struct alignment (fieldalignment-enforced), not logical order.
type Status struct {
	UpdatedAt time.Time `json:"updated_at"`
	Task      string    `json:"task"`
	Branch    string    `json:"branch"`
	// Base is the branch this worker's worktree was created from — set by
	// supervise at worktree-creation time (see internal/repoconfig's
	// base_branch precedence), not by the worker itself, and carried forward
	// unchanged by every `argus worker report` (its JSON body never sets this
	// field). ship/rebase read it back instead of re-defaulting to the
	// literal "main" when --base is omitted.
	Base           string `json:"base"`
	Phase          Phase  `json:"phase"`
	RealWorldProof string `json:"real_world_proof"`
	PRURL          string `json:"pr_url"`
	BlockedReason  string `json:"blocked_reason"`
	// Title is the worker's own conventional-commit-style summary of what it
	// built (e.g. "feat: add retry backoff to forge client"), used as the PR/
	// commit title in place of a generic branch+issue default. Empty is legal —
	// ship then falls back to the fetched issue title. runWorkerReport carries
	// a prior non-empty Title forward when a later report's body doesn't set
	// one, so a rework round describing only its own narrower fix can't wipe
	// out the title an earlier report already established.
	Title        string   `json:"title"`
	FilesTouched []string `json:"files_touched"`
	// Question is the worker's structured ask, set alongside BlockedReason when
	// it wants a specific decision rather than a generic escalation. Answer is
	// argus's own record of how a supervisor resolved it — never set by a
	// worker's own report — carried forward by runWorkerReport the same way
	// Base is, so it survives the worker's next report instead of being wiped
	// by that report's own JSON body (which never sends it).
	Question *Question `json:"question,omitempty"`
	Answer   *Answer   `json:"answer,omitempty"`
	// Plan is the worker's todo list, reported during the planning phase (issue
	// #103). It is the typed evidence RequiresPlanEvidence checks before
	// letting a report move planning -> working: a prose "write a todo list
	// first" instruction in the brief went unenforced (a worker could and did
	// skip it, twice, in real sessions), so this field plus the transcript
	// cross-check in internal/supervisor/planevidence.go make it a checked
	// contract instead.
	Plan     []string  `json:"plan"`
	Tests    []TestRun `json:"tests"`
	DiffStat DiffStat  `json:"diff_stat"`
}

// IsTerminal reports whether a phase is one argus stops waiting on. A worker is
// terminal when it wants review (awaiting_review — argus's job then hands to the
// gate), is blocked (needs a decision), or is done (shipped, once PR automation
// exists). argus does not wait past awaiting_review, since shipping is a separate
// deliberate step, not something the worker advances to on its own.
func IsTerminal(p Phase) bool {
	return p == PhaseAwaitingReview || p == PhaseDone || p == PhaseBlocked
}

// StatusPath returns the status file location for a worker's worktree. Every
// worker writes exactly here; argus reads exactly here.
func StatusPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "status.json")
}

// BriefPath returns where argus writes a worker's task brief. argus hands the
// worker this file (rather than pasting a multi-line brief into its TUI, which a
// real agent would submit at the first newline) and the launch prompt tells it to
// read and follow the file.
func BriefPath(worktree string) string {
	return filepath.Join(worktree, ".claude", "argus", "brief.md")
}

// Load reads and decodes a worker's status file. A missing file returns
// os.ErrNotExist (wrapped), which argus treats as "worker hasn't reported yet"
// rather than an error.
func Load(path string) (Status, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is argus-derived from a worktree, not user input
	if err != nil {
		return Status{}, fmt.Errorf("reading status file: %w", err)
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("decoding status file: %w", err)
	}
	return s, nil
}

// Write encodes status to path atomically: it writes a temp file in the same
// directory, then renames it over the target. A concurrent reader therefore
// sees either the old complete file or the new complete file, never a partial
// write. The parent directory is created if missing. s is taken by pointer only
// to avoid copying a heavy struct; Write does not mutate it.
func Write(path string, s *Status) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating status dir: %w", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding status: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "status-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp status file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we fail before the rename; after a successful
	// rename the temp name no longer exists, so the remove is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp status file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp status file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming status file into place: %w", err)
	}
	return nil
}
