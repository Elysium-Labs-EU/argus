package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Stats is the aggregate of many run logs — the answer to "is argus improving?"
// computed deterministically from the JSONL, no LLM. It is what `argus stats`
// prints and what a future auto-tune step would read.
type Stats struct {
	TokensByTask    map[string]int64
	Phases          map[string]int
	ReviewDecisions map[string]int
	Runs            int
	Workers         int
	GateAutoApprove int
	GateEscalate    int
	Reviews         int
	ReviewReAsks    int
	Approved        int
	NotApproved     int
	// BlockedOnQuestion is how many blocked workers (see Phases["blocked"] for
	// the total) carried a structured Question rather than only a freeform
	// BlockedReason — the same distinction `argus supervise`'s own report
	// draws per-run, aggregated here across every run log.
	BlockedOnQuestion int
}

// EscalationRate is the fraction of gate decisions that escalated (needed review
// or a human) rather than auto-approving. A high rate means the gate is spending
// review budget widely — either the work is risky or the policy is too strict.
func (s *Stats) EscalationRate() float64 {
	total := s.GateAutoApprove + s.GateEscalate
	if total == 0 {
		return 0
	}
	return float64(s.GateEscalate) / float64(total)
}

// ReAskRate is the fraction of reviews whose first reply failed to parse and had
// to be re-asked. It measures reviewer-output fragility, not code quality.
func (s *Stats) ReAskRate() float64 {
	if s.Reviews == 0 {
		return 0
	}
	return float64(s.ReviewReAsks) / float64(s.Reviews)
}

// ReadDir reads every *.jsonl run log under dir into a flat event slice. A missing
// dir is not an error — it means no runs have been recorded yet. A malformed line
// is skipped rather than failing the whole read (a partially-written line from a
// crash mid-write shouldn't take down every other event in the file); when debug
// is non-nil, each file with skipped lines reports the count so that kind of
// corruption doesn't go unnoticed indefinitely.
func ReadDir(dir string, debug io.Writer) ([]Event, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("globbing run logs: %w", err)
	}
	sort.Strings(matches)
	var events []Event
	for _, path := range matches {
		fileEvents, skipped, rerr := readFile(path)
		if rerr != nil {
			return nil, rerr
		}
		if skipped > 0 && debug != nil {
			_, _ = fmt.Fprintf(debug, "skipped %d malformed line(s) in %s\n", skipped, path)
		}
		events = append(events, fileEvents...)
	}
	return events, nil
}

func readFile(path string) ([]Event, int, error) {
	f, err := os.Open(path) //nolint:gosec // path came from a glob under the run-log dir
	if err != nil {
		return nil, 0, fmt.Errorf("opening run log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var events []Event
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			skipped++
			continue
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("scanning run log: %w", err)
	}
	return events, skipped, nil
}

// Summarize folds events into Stats. It counts distinct run ids, tallies gate and
// review outcomes, sums tokens per task, and buckets terminal phases.
func Summarize(events []Event) Stats {
	s := Stats{
		TokensByTask:    map[string]int64{},
		Phases:          map[string]int{},
		ReviewDecisions: map[string]int{},
	}
	runs := map[string]struct{}{}
	for i := range events {
		e := &events[i]
		if e.Run != "" {
			runs[e.Run] = struct{}{}
		}
		switch e.Action {
		case "gate":
			switch e.Outcome {
			case "auto-approve":
				s.GateAutoApprove++
			case "escalate":
				s.GateEscalate++
			}
		case "review":
			s.Reviews++
			s.ReviewDecisions[e.Outcome]++
		case "review_reask":
			s.ReviewReAsks++
		case "verdict":
			switch e.Outcome {
			case "approved":
				s.Approved++
			case "not-approved":
				s.NotApproved++
			}
		case "phase":
			s.Phases[e.Outcome]++
		case "run_summary":
			s.Workers += intField(e.Fields, "workers")
			s.BlockedOnQuestion += intField(e.Fields, "blocked_on_question")
		case "tokens":
			s.TokensByTask[e.Target] += int64Field(e.Fields, "total")
		}
	}
	s.Runs = len(runs)
	return s
}

func intField(f map[string]any, key string) int {
	return int(int64Field(f, key))
}

func int64Field(f map[string]any, key string) int64 {
	v, ok := f[key]
	if !ok {
		return 0
	}
	// JSON numbers decode into float64 through map[string]any.
	if n, ok := v.(float64); ok {
		return int64(n)
	}
	return 0
}

func stringField(f map[string]any, key string) string {
	v, _ := f[key].(string)
	return v
}

// TaskRow is one task's cost-and-outcome record, joined by Run+Target across
// the tokens, spawn, gate, review, verdict, and phase events that same task
// produced. This is the row a manual dispatch-tuning pass reads to correlate
// spend against quality — the join `argus stats`'s flat TokensByTask tally
// discards by collapsing across runs and never touching outcome at all.
type TaskRow struct {
	Effort         string
	GateOutcome    string
	Phase          string
	Task           string
	ReviewOutcome  string
	Run            string
	Model          string
	Verdict        string
	TokensInput    int64
	TokensTotal    int64
	CacheReadRatio float64
	CacheRead      int64
	CacheCreation  int64
	TokensOutput   int64
}

// JoinTasks folds events into one TaskRow per Run+Target. Row order follows
// each task's first appearance in events, which ReadDir already returns
// sorted by run-log filename (so oldest run first) — deterministic output
// without a separate sort pass.
//
// Rows live in a plain slice, indexed by a map[string]int rather than
// map[string]*TaskRow: a pointer pulled out of a map read requires a nil
// check nilaway's static analysis can't otherwise discharge, where a slice
// index is provably in range the moment it's looked up.
func JoinTasks(events []Event) []TaskRow {
	index := map[string]int{}
	out := make([]TaskRow, 0, len(events))
	for i := range events {
		e := &events[i]
		if e.Target == "" {
			continue
		}
		key := e.Run + "\x00" + e.Target
		idx, ok := index[key]
		if !ok {
			idx = len(out)
			index[key] = idx
			out = append(out, TaskRow{Run: e.Run, Task: e.Target})
		}
		row := &out[idx]
		switch e.Action {
		case "tokens":
			row.TokensTotal += int64Field(e.Fields, "total")
			row.TokensInput += int64Field(e.Fields, "input")
			row.TokensOutput += int64Field(e.Fields, "output")
			row.CacheCreation += int64Field(e.Fields, "cache_creation")
			row.CacheRead += int64Field(e.Fields, "cache_read")
			if m := stringField(e.Fields, "model"); m != "" {
				row.Model = m
			}
			if ef := stringField(e.Fields, "effort"); ef != "" {
				row.Effort = ef
			}
		case "spawn":
			if m := stringField(e.Fields, "model"); m != "" {
				row.Model = m
			}
			if ef := stringField(e.Fields, "effort"); ef != "" {
				row.Effort = ef
			}
		case "gate":
			row.GateOutcome = e.Outcome
		case "review":
			row.ReviewOutcome = e.Outcome
		case "verdict":
			row.Verdict = e.Outcome
		case "phase":
			row.Phase = e.Outcome
		}
	}
	for i := range out {
		row := &out[i]
		if row.TokensInput > 0 {
			row.CacheReadRatio = float64(row.CacheRead) / float64(row.TokensInput)
		}
	}
	return out
}
