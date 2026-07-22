package eventlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
