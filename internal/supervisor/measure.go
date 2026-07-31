package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// `git diff --numstat -z <base>`, plus every untracked, non-ignored file counted
// as added lines. -z (not cosmetic) makes git emit a rename as two separate
// NUL-terminated path tokens instead of an abbreviated, ambiguous
// "old => new" / "prefix{old => new}suffix" text form — parseNumstat resolves
// a rename to its current (post-change) path, so a worker can't smuggle an
// edit to a control-plane or self-protection path past FilesTouched-keyed
// checks by renaming an unrelated file onto it.
//
// Control-plane paths (isControlPlanePath, prune.go) are filtered out of the
// parsed records here, after path resolution, rather than by pattern-matching
// raw numstat text beforehand — the same resolved path is what both the
// filter and the returned file list use, so the two can't diverge.
func MeasureDiff(ctx context.Context, worktree, base string) (protocol.DiffStat, []string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--numstat", "-z", base) //nolint:gosec // fixed git binary; worktree/base are argus-derived
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return protocol.DiffStat{}, nil, fmt.Errorf("measuring diff against %s: %w", base, err)
	}
	records, err := parseNumstat(out.String())
	if err != nil {
		return protocol.DiffStat{}, nil, err
	}
	var stat protocol.DiffStat
	files := []string{}
	for _, r := range records {
		if isControlPlanePath(r.path) {
			continue
		}
		stat.Files++
		stat.Insertions += r.insertions
		stat.Deletions += r.deletions
		files = append(files, r.path)
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

// countLines returns the number of lines in a file, best-effort (0 if unreadable
// or binary). It is only a magnitude for the gate, not an exact stat. Binary
// files report 0 lines, matching how parseNumstat treats tracked binary files
// (git diff --numstat reports "-" for them, contributing no line counts) —
// without this, a raw newline scan over a PDF/PNG/font's bytes can produce
// hundreds of spurious "lines" and trip the gate's unwaivable under-report check.
func countLines(path string) int {
	if isBinaryFile(path) {
		return 0
	}
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

// binarySniffLen is how many leading bytes isBinaryFile inspects for a NUL
// byte, matching git's own is_binary heuristic (git caps its scan at the
// first 8000 bytes regardless of core.bigFileThreshold).
const binarySniffLen = 8000

// isBinaryFile reports whether path looks binary using the same heuristic
// git uses to decide when to print "Binary files differ" / report "-" in
// --numstat: a NUL byte anywhere in the first binarySniffLen bytes. It
// fails open (false) on read errors so a missing/unreadable file falls
// through to countLines' own best-effort handling.
func isBinaryFile(path string) bool {
	f, err := os.Open(path) //nolint:gosec // argus-derived worktree path
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, binarySniffLen)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// numstatFile is one `git diff --numstat -z` record: a file's line counts and
// its current (post-change) path. Binary files report "-" for insertions/
// deletions; they count as a touched file but contribute no line counts.
type numstatFile struct {
	path       string
	insertions int
	deletions  int
}

// parseNumstat parses `git diff --numstat -z` output into one record per
// changed file. Each record is normally a single NUL-terminated token
// "ins\tdel\tpath". For a rename or copy, git instead emits "ins\tdel\t"
// (an empty path field) followed by two more NUL-terminated tokens — the old
// path, then the new one — rather than folding both into one abbreviated,
// ambiguous "old => new" text field the way plain --numstat (without -z)
// does; a rename's resolved path here is always the new one, since that is
// where the file's content now lives.
func parseNumstat(out string) ([]numstatFile, error) {
	tokens := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(tokens) == 1 && tokens[0] == "" {
		return nil, nil
	}
	var records []numstatFile
	for i := 0; i < len(tokens); i++ {
		fields := strings.SplitN(tokens[i], "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected numstat token %q", tokens[i])
		}
		ins, del, path := fields[0], fields[1], fields[2]
		if path == "" {
			if i+2 >= len(tokens) {
				return nil, fmt.Errorf("truncated rename record after %q", tokens[i])
			}
			path = tokens[i+2]
			i += 2
		}
		rec := numstatFile{path: path}
		if ins != "-" {
			if n, err := strconv.Atoi(ins); err == nil {
				rec.insertions = n
			}
		}
		if del != "-" {
			if n, err := strconv.Atoi(del); err == nil {
				rec.deletions = n
			}
		}
		records = append(records, rec)
	}
	return records, nil
}
