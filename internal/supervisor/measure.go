package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	stat, files, err := parseNumstat(dropControlPlaneNumstat(out.String()))
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

// ContentHash returns a deterministic digest of files' current on-disk bytes
// under worktree, keyed by path. Callers pass MeasureDiff's own file list so
// the hash is bound to the same set the gate already measured, rather than
// re-deriving "what changed" a second, potentially divergent way. A verdict
// recorded against this hash catches an edit landing after approval even
// when it happens to leave the diff's line counts unchanged (e.g. content
// swapped for other content of the same size).
func ContentHash(worktree string, files []string) (string, error) {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, rel := range sorted {
		data, err := os.ReadFile(filepath.Join(worktree, rel)) //nolint:gosec // rel comes from git's own file list, worktree is argus-derived
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("hashing %s: %w", rel, err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(data)) //nolint:errcheck // hash.Hash.Write never returns an error
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
	for line := range strings.SplitSeq(out.String(), "\n") {
		f := strings.TrimSpace(line)
		if f == "" || isControlPlanePath(f) {
			continue
		}
		files = append(files, f)
	}
	return files, nil
}

// dropControlPlaneNumstat removes numstat lines for control-plane paths (the
// same ones ship unstages before opening a PR, isControlPlanePath in
// prune.go) from raw `git diff --numstat` output, before it reaches
// parseNumstat. A pre-existing or in-session edit to .claude/argus/status.json
// is a normal tracked change like any other, so git diff --numstat reports it
// unless filtered here — unlike untracked files, which git already omits.
func dropControlPlaneNumstat(out string) string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && isControlPlanePath(fields[2]) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
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
	_ = sc.Err() // best-effort magnitude for the gate; a scan error still keeps the count seen so far
	return n
}

// parseNumstat turns `git diff --numstat` output into a DiffStat and the list of
// changed paths. Binary files report "-" for insertions/deletions; they count as
// a touched file but contribute no line counts.
func parseNumstat(out string) (protocol.DiffStat, []string, error) {
	var stat protocol.DiffStat
	files := []string{}
	for line := range strings.SplitSeq(out, "\n") {
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
