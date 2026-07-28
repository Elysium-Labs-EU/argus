// Package eventlog records what argus actually did, as typed JSONL. Every action
// argus takes deterministically — spawning a worker, a gate verdict, a review
// outcome, a ship, a rebase — is one Event appended to a per-run log under
// ~/.argus/runs/. The log is the raw corpus a later analysis pass (escalation
// rate, review parse-fail rate, tokens per task) reads to answer "is argus
// improving?" without an LLM re-reading scrollback.
package eventlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"time"
)

// Event is one recorded action. It is deliberately flat and typed so an analysis
// pass can filter on Action/Outcome without parsing free text. Fields carries the
// action-specific numbers (diff size, tokens, pane id) that don't warrant a column.
type Event struct {
	Time    time.Time      `json:"time"`
	Fields  map[string]any `json:"fields,omitempty"`
	Run     string         `json:"run"`
	Command string         `json:"command"`
	Actor   string         `json:"actor,omitempty"`
	Action  string         `json:"action"`
	Target  string         `json:"target,omitempty"`
	Outcome string         `json:"outcome,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Err     string         `json:"err,omitempty"`
}

// Logger appends Events to a run log (and optionally tees them to a debug writer).
// A nil *Logger is a valid no-op logger, so callers thread it unconditionally and
// tests that don't care about logging pass nil. All methods are safe for the
// concurrent watch goroutines.
type Logger struct {
	now     func() time.Time
	w       io.Writer
	debug   io.Writer
	run     string
	command string
	actor   string
	mu      sync.Mutex
}

// New returns a Logger writing JSONL to w. debug, when non-nil, receives a copy of
// every event (argus points it at stderr under --debug). command and run tag every
// event so a shared log can be sliced by invocation.
func New(w io.Writer, command, run string, debug io.Writer) *Logger {
	return &Logger{now: time.Now, w: w, debug: debug, run: run, command: command}
}

// Open creates ~/.argus/runs/<timestamp>-<command>-<run>.jsonl and returns a
// Logger writing to it. The returned closer flushes and closes the file. Logging
// must never break a run, so callers treat an error here as "log to nowhere".
func Open(home, command string, debug io.Writer) (logger *Logger, path string, closer func() error, err error) {
	run := newRunID()
	dir := filepath.Join(home, ".argus", "runs")
	if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
		return nil, "", nil, fmt.Errorf("creating run-log dir: %w", mkErr)
	}
	name := fmt.Sprintf("%s-%s-%s.jsonl", time.Now().Format("20060102T150405"), command, run)
	path = filepath.Join(dir, name)
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path built from home+".argus/runs", not attacker-controlled
	if openErr != nil {
		return nil, "", nil, fmt.Errorf("opening run log: %w", openErr)
	}
	l := New(f, command, run, debug)
	l.actor = resolveActor()
	return l, path, f.Close, nil
}

// resolveActor identifies who is running argus, for the audit trail. It tries the
// OS user first since that works even without git configured; $USER covers the
// rare environment where os/user can't resolve (e.g. no matching /etc/passwd entry).
func resolveActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// Emit records one event. Time, Run, and Command are stamped here so callers only
// supply the action-specific fields. A nil Logger drops the event.
func (l *Logger) Emit(e *Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Time = l.now()
	e.Run = l.run
	e.Command = l.command
	e.Actor = l.actor
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	if l.w != nil {
		_, _ = l.w.Write(b)
	}
	if l.debug != nil {
		_, _ = l.debug.Write(b)
	}
}

// Action records a completed action and its outcome (e.g. "gate"/"escalate").
func (l *Logger) Action(action, target, outcome, detail string) {
	l.Emit(&Event{Action: action, Target: target, Outcome: outcome, Detail: detail})
}

// Fail records an action that errored, keeping the error text for the analysis pass.
func (l *Logger) Fail(action, target string, err error) {
	l.Emit(&Event{Action: action, Target: target, Outcome: "error", Err: err.Error()})
}

func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run"
	}
	return hex.EncodeToString(b[:])
}
