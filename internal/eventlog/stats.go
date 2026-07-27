package eventlog

import (
	"bufio"
	"encoding/json"
	"fmt"
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
// dir is not an error — it means no runs have been recorded yet.
func ReadDir(dir string) ([]Event, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("globbing run logs: %w", err)
	}
	sort.Strings(matches)
	var events []Event
	for _, path := range matches {
		fileEvents, rerr := readFile(path)
		if rerr != nil {
			return nil, rerr
		}
		events = append(events, fileEvents...)
	}
	return events, nil
}

func readFile(path string) ([]Event, error) {
	f, err := os.Open(path) //nolint:gosec // path came from a glob under the run-log dir
	if err != nil {
		return nil, fmt.Errorf("opening run log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // skip a malformed line rather than fail the whole read
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning run log: %w", err)
	}
	return events, nil
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
