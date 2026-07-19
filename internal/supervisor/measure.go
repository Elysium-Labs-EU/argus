package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

// MeasureDiff computes the ground-truth diff of a worktree against base, read-only.
// It exists because argus must not trust a worker's self-reported DiffStat/
// FilesTouched when gating: the worker could be buggy or omit files. The measured
// numbers are what the gate and report use; status.json is only a hint.
//
// It combines two sources because `git diff` alone would miss the common case of a
// worker ADDING new files (untracked files are invisible to git diff, which once
// let a 257-line new test file gate through as "2 lines"): the tracked diff via
// `git diff --numstat <base>`, plus every untracked, non-ignored file counted as
// added lines.
func MeasureDiff(ctx context.Context, worktree, base string) (protocol.DiffStat, []string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--numstat", base) //nolint:gosec // fixed git binary; worktree/base are argus-derived
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return protocol.DiffStat{}, nil, fmt.Errorf("measuring diff against %s: %w", base, err)
	}
	stat, files, err := parseNumstat(out.String())
	if err != nil {
		return protocol.DiffStat{}, nil, err
	}

	untracked, err := untrackedFiles(ctx, worktree)
	if err != nil {
		return protocol.DiffStat{}, nil, err
	}
	for _, rel := range untracked {
		lines := countLines(filepath.Join(worktree, rel))
		stat.Files++
		stat.Insertions += lines
		files = append(files, rel)
	}
	return stat, files, nil
}

// untrackedFiles lists the worktree's untracked, non-ignored files — the ones
// git diff omits. The control-plane argus writes (.claude/argus, the generated
// settings) is excluded so it never counts toward the change under review.
func untrackedFiles(ctx context.Context, worktree string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "--others", "--exclude-standard") //nolint:gosec // fixed git binary; worktree is argus-derived
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing untracked files: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.TrimSpace(line)
		if f == "" || strings.HasPrefix(f, ".claude/argus/") || f == ".claude/settings.local.json" {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

// countLines returns the number of lines in a file, best-effort (0 if unreadable
// or binary-ish). It is only a magnitude for the gate, not an exact stat.
func countLines(path string) int {
	f, err := os.Open(path) //nolint:gosec // argus-derived worktree path
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		n++
	}
	return n
}

// parseNumstat turns `git diff --numstat` output into a DiffStat and the list of
// changed paths. Binary files report "-" for insertions/deletions; they count as
// a touched file but contribute no line counts.
func parseNumstat(out string) (protocol.DiffStat, []string, error) {
	var stat protocol.DiffStat
	files := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return protocol.DiffStat{}, nil, fmt.Errorf("unexpected numstat line %q", line)
		}
		stat.Files++
		files = append(files, fields[2])
		if fields[0] != "-" {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				stat.Insertions += n
			}
		}
		if fields[1] != "-" {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				stat.Deletions += n
			}
		}
	}
	return stat, files, nil
}
