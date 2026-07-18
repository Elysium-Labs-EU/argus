package supervisor

import (
	"os"
	"path/filepath"
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
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
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
	if got, want := usage.Total(), 485; got != want {
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
