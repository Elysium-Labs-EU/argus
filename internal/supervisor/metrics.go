package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// TokenUsage is the cumulative token spend of one worker's Claude Code session,
// summed across every assistant turn in its transcript. This is the honest
// black-box measurement (Adam Jacob's tactic 4): argus records what each worker
// actually cost so a later deterministic-review cut can be compared against it.
type TokenUsage struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheCreation int `json:"cache_creation"`
	CacheRead     int `json:"cache_read"`
}

// Total is the session's new-token spend: fresh input, output, and cache
// creation. It deliberately excludes CacheRead, which is reported per assistant
// turn and re-counts the same cached prefix on every turn, so summing it across a
// long session massively overcounts. CacheRead stays available as its own field
// for callers that want the cache-hit volume.
func (u TokenUsage) Total() int {
	return u.Input + u.Output + u.CacheCreation
}

// transcriptLine is the subset of a Claude Code session JSONL record argus reads:
// each assistant message carries a usage block.
type transcriptLine struct {
	Message struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// TokensForSession sums token usage for a Claude Code session by its UUID. It
// globs home/.claude/projects/*/<sessionID>.jsonl — the session id is unique, so
// this sidesteps guessing the project-dir path encoding. The bool is false when
// no transcript exists yet (worker hasn't produced one), which argus renders as
// "unknown" rather than zero.
func TokensForSession(home, sessionID string) (TokenUsage, bool, error) {
	if sessionID == "" {
		return TokenUsage{}, false, nil
	}
	pattern := filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return TokenUsage{}, false, fmt.Errorf("globbing session transcript: %w", err)
	}
	if len(matches) == 0 {
		return TokenUsage{}, false, nil
	}

	usage, err := sumTranscript(matches[0])
	if err != nil {
		return TokenUsage{}, false, err
	}
	return usage, true, nil
}

func sumTranscript(path string) (TokenUsage, error) {
	f, err := os.Open(path) //nolint:gosec // path came from a glob under home/.claude/projects, not user input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TokenUsage{}, nil
		}
		return TokenUsage{}, fmt.Errorf("opening session transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	var total TokenUsage
	sc := bufio.NewScanner(f)
	// Transcript lines can be large (tool results inlined); raise the cap.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var line transcriptLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // skip malformed/non-message lines rather than fail the whole sum
		}
		if line.Message.Usage == nil {
			continue
		}
		u := line.Message.Usage
		total.Input += u.InputTokens
		total.Output += u.OutputTokens
		total.CacheCreation += u.CacheCreationInputTokens
		total.CacheRead += u.CacheReadInputTokens
	}
	if err := sc.Err(); err != nil {
		return TokenUsage{}, fmt.Errorf("scanning session transcript: %w", err)
	}
	return total, nil
}

// elapsed rounds a duration to whole seconds for human-readable reporting.
func elapsed(start, end time.Time) time.Duration {
	return end.Sub(start).Round(time.Second)
}
