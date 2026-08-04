package supervisor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// planEvidenceMarkers are the tool-use names that count as ground truth that a
// worker actually created a todo list, rather than merely claiming one in its
// planning report's Plan field. TodoWrite is Claude Code's built-in todo tool;
// TaskCreate is the typed-task variant some harness configurations use
// instead. Either appearing as a real tool call in the worker's own transcript
// is enough.
var planEvidenceMarkers = []string{"TodoWrite", "TaskCreate"}

// transcriptToolUseLine is the subset of a Claude Code session JSONL record
// needed to find real tool calls: an assistant message's content array can
// hold text and tool_use blocks side by side, and only a tool_use block's
// name is ground truth — the same name can otherwise appear anywhere in an
// assistant's prose or in an echoed tool_result without a call ever having
// happened.
type transcriptToolUseLine struct {
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// projectPathReplacer mirrors the directory-naming scheme Claude Code itself
// uses for a project's transcript directory under ~/.claude/projects: every
// "/" and "." in the project's absolute working-directory path becomes "-".
var projectPathReplacer = strings.NewReplacer("/", "-", ".", "-")

// HasPlanEvidence reports whether any session transcript for worktree's Claude
// Code project contains a real TodoWrite/TaskCreate tool call — the unfakeable
// backstop for the planning phase's self-reported Plan field,
// mirroring how MeasureDiff refuses to trust a worker's self-reported diff
// stat by measuring the real git diff instead. A worktree can span more than
// one worker session (e.g. an initial implementation run and a later
// review-feedback follow-up in the same worktree), so every transcript is
// checked rather than just the newest.
//
// false with a nil error means no matching transcript directory exists yet
// (the worker hasn't produced one, or home is misconfigured) — that is
// reported as "no evidence found," not a hard error, the same way a missing
// status file is treated as "hasn't reported yet" rather than an error.
//
// The second return value is how many transcript files were actually
// scanned. A caller logging a "no evidence" result alongside this count can
// tell a zero-transcript miss (wrong home, worker never started a Claude Code
// session) apart from a real grep miss against one or more transcripts that
// did exist — the two point at very different root causes, and the reason
// string alone conflates them.
func HasPlanEvidence(home, worktree string) (bool, int, error) {
	dir := filepath.Join(home, ".claude", "projects", projectPathReplacer.Replace(worktree))
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return false, 0, fmt.Errorf("globbing session transcripts for %s: %w", worktree, err)
	}
	for _, m := range matches {
		found, err := transcriptContainsAny(m, planEvidenceMarkers)
		if err != nil {
			return false, len(matches), err
		}
		if found {
			return true, len(matches), nil
		}
	}
	return false, len(matches), nil
}

// transcriptContainsAny reports whether any line of the transcript at path
// records a real tool_use block whose name is one of markers. Matching is
// scoped to parsed tool_use blocks, not a raw line substring, so a marker
// name mentioned in assistant prose or echoed back inside a tool_result
// (neither of which is a tool call) can't pass this check.
func transcriptContainsAny(path string, markers []string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // path came from a glob under home/.claude/projects, not user input
	if err != nil {
		return false, fmt.Errorf("opening transcript %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// Transcript lines can be large (tool results inlined); raise the cap to
	// match TokensForSession's scanner.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var line transcriptToolUseLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // skip malformed/non-message lines rather than fail the whole scan
		}
		for _, block := range line.Message.Content {
			if block.Type == "tool_use" && slices.Contains(markers, block.Name) {
				return true, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("scanning transcript %s: %w", path, err)
	}
	return false, nil
}
