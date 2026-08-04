package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmitRoundTripsFromJSONL(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "supervise", "abcd1234", nil)
	l.Action("gate", "fix #144", "escalate", "diff 650 lines exceeds max 400")
	l.Emit(&Event{Action: "review", Target: "fix #144", Outcome: "approve", Fields: map[string]any{"tokens": 3500000}})

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d: %q", len(lines), buf.String())
	}

	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decoding line 0: %v", err)
	}
	if first.Action != "gate" || first.Outcome != "escalate" || first.Target != "fix #144" {
		t.Errorf("unexpected first event: %+v", first)
	}
	if first.Command != "supervise" || first.Run != "abcd1234" {
		t.Errorf("event not tagged with command/run: %+v", first)
	}
	if first.Time.IsZero() {
		t.Error("event time not stamped")
	}

	var second Event
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decoding line 1: %v", err)
	}
	tokens, ok := second.Fields["tokens"].(float64)
	if !ok || tokens != 3500000 {
		t.Errorf("fields not preserved: %+v", second.Fields)
	}
}

func TestNilLoggerIsNoOp(t *testing.T) {
	var l *Logger
	// Must not panic — a nil logger is the "logging disabled" path callers rely on.
	l.Action("spawn", "t", "ok", "")
	l.Fail("spawn", "t", os.ErrNotExist)
	l.Emit(&Event{Action: "x"})
}

func TestOpenWritesRunLogFile(t *testing.T) {
	l, path, closer := OpenForTest(t)
	l.Action("review", "t", "approve", "looks correct")
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	if !strings.Contains(path, filepath.Join(".argus", "runs")) {
		t.Errorf("run log not under .argus/runs: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading run log: %v", err)
	}
	var e Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
		t.Fatalf("run log line not valid JSON: %v (%q)", err, data)
	}
	if e.Command != "test" || e.Outcome != "approve" {
		t.Errorf("unexpected persisted event: %+v", e)
	}
}

// Open is package-private-tested here (unqualified, not eventlog.Open) so the
// scripts/check-eventlog-open.sh gate — which only greps for callers outside this
// package — doesn't fire; these two tests exist specifically to reach Open's
// error branches, which OpenForTest's happy path never touches.
func TestOpenMkdirAllFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding blocking file: %v", err)
	}

	l, path, closer, err := Open(home, "test", nil)
	if err == nil {
		t.Fatal("want error when home is a regular file, got nil")
	}
	if !strings.Contains(err.Error(), "creating run-log dir") {
		t.Errorf("error %q missing MkdirAll context", err)
	}
	if l != nil || path != "" || closer != nil {
		t.Errorf("want zero values on error, got logger=%v path=%q closer!=nil=%v", l, path, closer != nil)
	}
}

func TestOpenOpenFileFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permission")
	}
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict directory writes on windows")
	}

	home := t.TempDir()
	dir := filepath.Join(home, ".argus", "runs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("seeding runs dir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("removing write permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	l, path, closer, err := Open(home, "test", nil)
	if err == nil {
		t.Fatal("want error when runs dir is not writable, got nil")
	}
	if !strings.Contains(err.Error(), "opening run log") {
		t.Errorf("error %q missing OpenFile context", err)
	}
	if l != nil || path != "" || closer != nil {
		t.Errorf("want zero values on error, got logger=%v path=%q closer!=nil=%v", l, path, closer != nil)
	}
}

func TestResolveActorWith(t *testing.T) {
	cases := []struct {
		name        string
		currentUser func() (*user.User, error)
		getenv      func(string) string
		want        string
	}{
		{
			name:        "os user available",
			currentUser: func() (*user.User, error) { return &user.User{Username: "alice"}, nil },
			getenv:      func(string) string { return "should-not-be-used" },
			want:        "alice",
		},
		{
			name:        "os user lookup fails",
			currentUser: func() (*user.User, error) { return nil, errors.New("no passwd entry") },
			getenv:      func(k string) string { return map[string]string{"USER": "bob"}[k] },
			want:        "bob",
		},
		{
			name:        "os user has empty username",
			currentUser: func() (*user.User, error) { return &user.User{Username: ""}, nil },
			getenv:      func(k string) string { return map[string]string{"USER": "carol"}[k] },
			want:        "carol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveActorWith(tc.currentUser, tc.getenv); got != tc.want {
				t.Errorf("resolveActorWith() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewRunIDWithRandFailureFallsBackToRun(t *testing.T) {
	id := newRunIDWith(func([]byte) (int, error) { return 0, errors.New("entropy exhausted") })
	if id != "run" {
		t.Errorf("newRunIDWith() = %q, want \"run\"", id)
	}
}

func TestEmitTeesToDebugWriter(t *testing.T) {
	var main, debug bytes.Buffer
	l := New(&main, "supervise", "run1", &debug)
	l.Emit(&Event{Action: "gate"})

	if main.String() == "" || main.String() != debug.String() {
		t.Errorf("debug writer did not receive an identical copy: main=%q debug=%q", main.String(), debug.String())
	}
}

func TestEmitDropsEventOnMarshalFailure(t *testing.T) {
	var main, debug bytes.Buffer
	l := New(&main, "supervise", "run1", &debug)
	l.Emit(&Event{Action: "gate", Fields: map[string]any{"x": make(chan int)}})

	if main.Len() != 0 || debug.Len() != 0 {
		t.Errorf("want nothing written when Fields can't marshal, got main=%q debug=%q", main.String(), debug.String())
	}
}

func splitLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			out = append(out, sc.Text())
		}
	}
	return out
}
