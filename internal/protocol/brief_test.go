package protocol

import (
	"strings"
	"testing"
)

// TestWriterBriefDiffsAgainstGivenBase pins the issue-299 fix: the worker
// instruction must diff against the same base MeasureDiff gates against, not
// the literal "HEAD" — HEAD only equals base before a worker's first commit.
func TestWriterBriefDiffsAgainstGivenBase(t *testing.T) {
	got := WriterBrief("origin/main")
	want := "`git diff --stat origin/main`"
	if !strings.Contains(got, want) {
		t.Errorf("WriterBrief(%q) missing %q, got:\n%s", "origin/main", want, got)
	}
	if strings.Contains(got, "git diff --stat HEAD") {
		t.Errorf("WriterBrief must not instruct diffing against HEAD, got:\n%s", got)
	}
}

func TestWriterBriefVariesWithBase(t *testing.T) {
	a := WriterBrief("origin/main")
	b := WriterBrief("origin/develop")
	if a == b {
		t.Errorf("WriterBrief(origin/main) and WriterBrief(origin/develop) should differ in their diff_stat instruction")
	}
	if !strings.Contains(b, "`git diff --stat origin/develop`") {
		t.Errorf("WriterBrief(origin/develop) missing its own base in the diff_stat instruction, got:\n%s", b)
	}
}

// TestNeverRunBriefNamesEveryCommand confirms the shared clause names every
// command it's given, so a brief using it and settingsFor's Ask list (built
// from the same AskGatedCommands slice — see internal/supervisor/agentadapter.go)
// can never list different commands from each other.
func TestNeverRunBriefNamesEveryCommand(t *testing.T) {
	got := NeverRunBrief(AskGatedCommands)
	for _, cmd := range AskGatedCommands {
		if !strings.Contains(got, cmd) {
			t.Errorf("NeverRunBrief(%v) = %q, missing command %q", AskGatedCommands, got, cmd)
		}
	}
}

// TestNeverRunBriefEmptyCommandsRendersNothing confirms a brief that mandates
// no ask-gated command at all gets no clause to compose around.
func TestNeverRunBriefEmptyCommandsRendersNothing(t *testing.T) {
	if got := NeverRunBrief(nil); got != "" {
		t.Errorf("NeverRunBrief(nil) = %q, want empty", got)
	}
}
