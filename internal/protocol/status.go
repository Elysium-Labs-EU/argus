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
	"slices"
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
	// PhaseRebase is stamped by argus itself when it dispatches a worker to
	// resolve a post-merge conflict (see supervisor.RebasePhaseAllow) — never
	// reported by a worker's own `argus worker report`, the same way
	// PhaseDone never is. Before this existed, a rebase dispatch ran with no
	// phase at all (Phase("")), so its git fetch/merge grant had to be
	// injected blanket, reaching every phase instead of just this one.
	PhaseRebase Phase = "rebase"
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
	// CodeInsertions and CodeDeletions are the subset of Insertions/Deletions
	// that excludes files classified as test or documentation (see
	// isTestOrDocPath in internal/supervisor/measure.go). The review gate's
	// max_diff_lines ceiling compares against these, not the totals above, so
	// tests and an ADR a repo's own policy mandates for every change don't
	// count against a ceiling meant to bound reviewable code size.
	CodeInsertions int `json:"code_insertions"`
	CodeDeletions  int `json:"code_deletions"`
}

// TestRun records one test invocation and its outcome. Cmd must be the
// exact, fully self-contained, copy-paste-runnable command the worker ran —
// the gate replays it byte-for-byte to reproduce a claimed pass. Target is a
// descriptive label naming what Cmd exercised (a package, a VM, a service,
// a Dockerfile) and, only when it resolves to a real directory under the
// worktree, a hint for which directory to replay Cmd from (a monorepo with
// one Makefile/go.mod per module). Target is never appended to Cmd as an
// argument, whatever shape it takes — see replayCommands in
// internal/supervisor/testverify.go.
//
// ExpectedResult is optional and only meaningful when it equals ResultFail: it
// marks a run the worker deliberately broke to prove a check catches the
// break (e.g. proof-required paths ask for "break it, confirm the failure,
// revert"), so the gate can report it informationally instead of escalating
// on it like a real regression. Leaving it empty means "this result was
// meant to pass" — the existing, unmarked behavior.
type TestRun struct {
	Cmd            string `json:"cmd"`
	Target         string `json:"target"`
	Result         Result `json:"result"`
	ExpectedResult Result `json:"expected_result,omitempty"`
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

// Steer records one supervisor follow-up injected into a worker that is
// still in PhaseWorking or PhaseAwaitingReview — see `argus worker steer`.
// Unlike Answer, it resolves nothing and never touches Phase: it is a live
// chat message delivered into the worker's own turn, not a phase
// transition, so it sits outside legalTransitions entirely.
type Steer struct {
	DeliveredAt time.Time `json:"delivered_at"`
	Text        string    `json:"text"`
	// Delivered is false until the herdr pane delivery this entry recorded
	// actually succeeds. A worker whose agent is busy mid-turn returns "agent
	// wait timed out" from that delivery — the durable trace still keeps the
	// attempt, but MaxSteersPerWorking must not count it: a message that never
	// reached the worker did not consume any of its attention.
	Delivered bool `json:"delivered"`
}

// MaxSteersPerWorking caps how many *delivered* steer messages a worktree
// may receive across its whole lifetime before `argus worker steer` refuses
// further ones. Without a cap, steer would become an unbounded side-channel
// a supervisor could lean on instead of the phase-transition table itself —
// endlessly redirecting a worker mid-turn rather than ever letting it reach
// a terminal phase. Failed deliveries are recorded in Steers for the trace
// but excluded from this count, so a run of transient herdr/agent-busy
// failures can't lock a supervisor out of steering before a single message
// actually lands.
const MaxSteersPerWorking = 3

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
	// Steers is the durable trace of every `argus worker steer` message ever
	// delivered to this worktree. A worker's own report body never sets this
	// key — runWorkerReport carries the on-disk value forward the same way
	// it does Base, so a worker's next phase report can't silently erase a
	// trace the worker never wrote in the first place. MaxSteersPerWorking's
	// cap is therefore counted across the worktree's whole lifetime, not
	// reset per phase leg.
	Steers []Steer `json:"steers"`
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

// SelfReportEqual reports whether two statuses' worker-authored report content
// is identical: Title, RealWorldProof, DiffStat, FilesTouched, Plan, and
// Tests — every field an `argus worker report` body actually sets. It
// excludes UpdatedAt (changes on every write) and the fields argus itself
// owns or carries forward rather than the worker (Phase, BlockedReason,
// Question, Answer, Steers, Base, Task, Branch, PRURL), so a round whose only
// real fix is to this content — e.g. narrowing an over-claimed Tests entry —
// is still detected as having changed something, even when the source tree
// and HEAD are byte-for-byte identical to before.
func SelfReportEqual(a, b *Status) bool {
	return a.Title == b.Title &&
		a.RealWorldProof == b.RealWorldProof &&
		a.DiffStat == b.DiffStat &&
		slices.Equal(a.FilesTouched, b.FilesTouched) &&
		slices.Equal(a.Plan, b.Plan) &&
		slices.Equal(a.Tests, b.Tests)
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
