package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"codeberg.org/Elysium_Labs/argus/internal/protocol"
)

// MeasureDiff computes the ground-truth diff of a worktree against base with
// `git diff --numstat`, read-only. It exists because argus must not trust a
// worker's self-reported DiffStat/FilesTouched when gating: the worker could be
// buggy or omit files. The measured numbers are what the gate and report use;
// status.json is only a hint. A worker leaves its change uncommitted, so a plain
// `git diff <base>` captures the working tree.
func MeasureDiff(ctx context.Context, worktree, base string) (protocol.DiffStat, []string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--numstat", base) //nolint:gosec // fixed git binary; worktree/base are argus-derived
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return protocol.DiffStat{}, nil, fmt.Errorf("measuring diff against %s: %w", base, err)
	}
	return parseNumstat(out.String())
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
