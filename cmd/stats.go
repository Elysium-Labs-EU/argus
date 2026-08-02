package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// defaultTaskLimit bounds the "tokens per task" list to a quick read instead
// of an itemized dump — a real run history runs into the thousands of tasks.
const defaultTaskLimit = 20

// maxTaskLabelWidth is the printed width of a task label before it gets
// ellipsis-truncated. Kept independent of taskLabel's own 60-char cut in
// internal/supervisor/loop.go: older run logs predate that cut and can carry
// a task's entire multi-line brief, so stats must re-bound at render time
// regardless of what's actually stored on disk.
const maxTaskLabelWidth = 60

func newStatsCmd() *cobra.Command {
	var export bool
	var limit int
	var all bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Aggregate the run logs under ~/.argus/runs into supervision metrics",
		Long: `Stats reads every run log argus has written (~/.argus/runs/*.jsonl) and prints
deterministic aggregates: how often the gate escalated instead of auto-approving,
how often a review reply had to be re-asked, the terminal-phase breakdown, and
tokens spent per task. It is the analysis half of the log->analyze->improve loop:
plain code over typed events, no LLM.

The "tokens per task" list is capped at --limit highest-spend tasks (default
20); pass --all to print every task instead.

--export switches to a CSV of one row per task, joining token spend (with the
cache-read/input ratio) to the model/effort it ran with and the gate/review/
verdict/phase outcome for that same task — the input a manual dispatch-tuning
pass reads instead of hand-correlating separate event types.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !all && limit <= 0 {
				return fmt.Errorf("--limit must be positive (use --all to show every task)")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolving home dir: %w", err)
			}
			dir := filepath.Join(home, ".argus", "runs")
			var debug io.Writer
			if debugLog {
				debug = cmd.ErrOrStderr()
			}
			events, err := eventlog.ReadDir(dir, debug)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(events) == 0 {
				_, _ = fmt.Fprintf(out, "%s no run logs yet under %s\n", ui.TextMuted.Render("i"), dir)
				return nil
			}
			if export {
				return writeTaskCSV(out, eventlog.JoinTasks(events))
			}
			stats := eventlog.Summarize(events)
			taskLimit := limit
			if all {
				taskLimit = -1
			}
			renderStats(cmd, &stats, taskLimit)
			return nil
		},
	}
	cmd.Flags().BoolVar(&export, "export", false, "print one CSV row per task (tokens, model, effort, gate/review/verdict/phase) instead of the aggregate summary")
	cmd.Flags().IntVar(&limit, "limit", defaultTaskLimit, "cap the \"tokens per task\" list to the N highest-spend tasks")
	cmd.Flags().BoolVar(&all, "all", false, "show every task in \"tokens per task\" instead of capping at --limit")
	return cmd
}

// writeTaskCSV renders JoinTasks' per-task rows as CSV so they can be piped
// into a spreadsheet or a notebook for the manual dispatch-tuning analysis
// this export exists for.
func writeTaskCSV(out io.Writer, rows []eventlog.TaskRow) error {
	w := csv.NewWriter(out)
	header := []string{
		"run", "task", "model", "effort",
		"tokens_total", "tokens_input", "tokens_output", "cache_creation", "cache_read", "cache_read_ratio",
		"gate", "review", "verdict", "phase",
	}
	if err := w.Write(header); err != nil {
		return fmt.Errorf("writing csv header: %w", err)
	}
	for i := range rows {
		r := &rows[i]
		record := []string{
			r.Run, r.Task, r.Model, r.Effort,
			strconv.FormatInt(r.TokensTotal, 10), strconv.FormatInt(r.TokensInput, 10), strconv.FormatInt(r.TokensOutput, 10),
			strconv.FormatInt(r.CacheCreation, 10), strconv.FormatInt(r.CacheRead, 10), strconv.FormatFloat(r.CacheReadRatio, 'f', 4, 64),
			r.GateOutcome, r.ReviewOutcome, r.Verdict, r.Phase,
		}
		if err := w.Write(record); err != nil {
			return fmt.Errorf("writing csv row for %s/%s: %w", r.Run, r.Task, err)
		}
	}
	w.Flush()
	return w.Error()
}

var statsCmd = newStatsCmd()

func renderStats(cmd *cobra.Command, s *eventlog.Stats, taskLimit int) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "\n%s argus stats — %d run(s), %d worker(s)\n\n",
		ui.LabelInfo.Render("i"), s.Runs, s.Workers)

	_, _ = fmt.Fprintf(out, "  gate:      %d auto-approve, %d escalate  (escalation rate %.0f%%)\n",
		s.GateAutoApprove, s.GateEscalate, s.EscalationRate()*100)
	_, _ = fmt.Fprintf(out, "  review:    %d run, %d re-asked  (parse-fail rate %.0f%%)\n",
		s.Reviews, s.ReviewReAsks, s.ReAskRate()*100)
	for _, d := range sortedKeys(s.ReviewDecisions) {
		_, _ = fmt.Fprintf(out, "               · %s: %d\n", d, s.ReviewDecisions[d])
	}
	_, _ = fmt.Fprintf(out, "  verdicts:  %d approved, %d not approved\n", s.Approved, s.NotApproved)

	if len(s.Phases) > 0 {
		_, _ = fmt.Fprintf(out, "  phases:\n")
		for _, p := range sortedKeys(s.Phases) {
			if p == "blocked" && s.BlockedOnQuestion > 0 {
				_, _ = fmt.Fprintf(out, "               · %s: %d (%d on a structured question)\n", p, s.Phases[p], s.BlockedOnQuestion)
				continue
			}
			_, _ = fmt.Fprintf(out, "               · %s: %d\n", p, s.Phases[p])
		}
	}

	if len(s.TokensByTask) > 0 {
		_, _ = fmt.Fprintf(out, "  tokens per task:\n")
		keys := sortedTokenKeys(s.TokensByTask)
		shown, omitted := keys, 0
		if taskLimit >= 0 && len(keys) > taskLimit {
			shown, omitted = keys[:taskLimit], len(keys)-taskLimit
		}
		for _, task := range shown {
			_, _ = fmt.Fprintf(out, "               · %s: %d\n", truncateTaskLabel(task, maxTaskLabelWidth), s.TokensByTask[task])
		}
		if omitted > 0 {
			_, _ = fmt.Fprintf(out, "               … %d more task(s) omitted (--limit %d or --all to see them)\n", omitted, len(keys))
		}
	}
}

// truncateTaskLabel bounds task to a single line of at most width runes,
// appending an unambiguous "…" marker when it cuts — a bare cut is
// indistinguishable from a task whose own text happens to end at that byte,
// and task descriptions routinely contain colons themselves.
func truncateTaskLabel(task string, width int) string {
	if nl := strings.IndexByte(task, '\n'); nl >= 0 {
		task = task[:nl]
	}
	task = strings.TrimSpace(task)
	runes := []rune(task)
	if len(runes) <= width {
		return task
	}
	return string(runes[:width]) + "…"
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedTokenKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	return keys
}
