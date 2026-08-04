package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, home, sessionID string, lines []string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-some-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(join(lines)), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
}

func join(lines []string) string {
	var out strings.Builder
	for _, l := range lines {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return out.String()
}

func TestTokensForSessionSumsTranscript(t *testing.T) {
	home := t.TempDir()
	sess := "abc-123"
	writeTranscript(t, home, sess, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":50}}}`,
		`{"type":"user","message":{"role":"user"}}`, // no usage, skipped
		`not even json`, // malformed, skipped
		`{"type":"assistant","message":{"usage":{"input_tokens":200,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":100}}}`,
	})

	usage, known, err := TokensForSession(home, sess)
	if err != nil {
		t.Fatalf("TokensForSession: %v", err)
	}
	if !known {
		t.Fatal("want known=true when transcript exists")
	}
	if usage.Input != 300 || usage.Output != 30 || usage.CacheCreation != 5 || usage.CacheRead != 150 {
		t.Errorf("sum wrong: %+v", usage)
	}
	// Total excludes cumulative cache reads (300+30+5), which would otherwise be
	// inflated by the 150 repeated cache-read tokens.
	if got, want := usage.Total(), 335; got != want {
		t.Errorf("Total: got %d want %d", got, want)
	}
}

func TestTokensForSessionUnknownWhenMissing(t *testing.T) {
	home := t.TempDir()
	_, known, err := TokensForSession(home, "nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if known {
		t.Fatal("want known=false when no transcript exists")
	}
}

func TestTokensForSessionEmptyID(t *testing.T) {
	_, known, err := TokensForSession(t.TempDir(), "")
	if err != nil || known {
		t.Fatalf("empty session id should be unknown, no error; got known=%v err=%v", known, err)
	}
}

func TestSumTranscriptMissingFile(t *testing.T) {
	usage, err := sumTranscript(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != (TokenUsage{}) {
		t.Errorf("want zero usage for a missing file, got %+v", usage)
	}
}

func TestSumTranscriptLineTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	// One line with no newline, past the 8MB scanner buffer cap in sumTranscript.
	big := strings.Repeat("a", 9*1024*1024)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	_, err := sumTranscript(path)
	if err == nil || !strings.Contains(err.Error(), "scanning session transcript") {
		t.Fatalf("want a scanning error, got %v", err)
	}
}

// skipIfRoot skips permission-based negative tests when running as root, since
// root bypasses the filesystem permission bits the test relies on to force a
// read failure.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission-denial tests can't force a failure")
	}
}

func TestTokensForSessionPermissionError(t *testing.T) {
	skipIfRoot(t)
	home := t.TempDir()
	sess := "perm-sess"
	writeTranscript(t, home, sess, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	})
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sess+".jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("setup: want 1 match, got %v err=%v", matches, err)
	}
	if chmodErr := os.Chmod(matches[0], 0o000); chmodErr != nil {
		t.Fatalf("chmod: %v", chmodErr)
	}
	t.Cleanup(func() { _ = os.Chmod(matches[0], 0o644) })

	_, known, err := TokensForSession(home, sess)
	if err == nil {
		t.Fatal("want an error when the transcript file is unreadable")
	}
	if known {
		t.Error("want known=false when TokensForSession errors")
	}
}

func TestTokensForSessionMultipleMatchesUsesFirst(t *testing.T) {
	home := t.TempDir()
	sess := "dup-sess"
	// filepath.Glob sorts matches lexically, so writeTranscript's fixed
	// "-some-project" dir sorts before "-zzz-project" and wins as matches[0].
	writeTranscript(t, home, sess, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
	})
	dirB := filepath.Join(home, ".claude", "projects", "-zzz-project")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line := `{"type":"assistant","message":{"usage":{"input_tokens":999,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dirB, sess+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write second match: %v", err)
	}

	usage, known, err := TokensForSession(home, sess)
	if err != nil {
		t.Fatalf("TokensForSession: %v", err)
	}
	if !known {
		t.Fatal("want known=true")
	}
	if usage.Input != 10 {
		t.Errorf("want the first glob match's usage (input=10), got %+v", usage)
	}
}
