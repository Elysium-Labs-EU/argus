package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Aggregate the run logs under ~/.argus/runs into supervision metrics",
		Long: `Stats reads every run log argus has written (~/.argus/runs/*.jsonl) and prints
deterministic aggregates: how often the gate escalated instead of auto-approving,
how often a review reply had to be re-asked, the terminal-phase breakdown, and
tokens spent per task. It is the analysis half of the log->analyze->improve loop:
plain code over typed events, no LLM.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolving home dir: %w", err)
			}
			dir := filepath.Join(home, ".argus", "runs")
			events, err := eventlog.ReadDir(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(events) == 0 {
				_, _ = fmt.Fprintf(out, "%s no run logs yet under %s\n", ui.TextMuted.Render("i"), dir)
				return nil
			}
			stats := eventlog.Summarize(events)
			renderStats(cmd, &stats)
			return nil
		},
	}
	return cmd
}

var statsCmd = newStatsCmd()

func renderStats(cmd *cobra.Command, s *eventlog.Stats) {
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
		for _, task := range sortedTokenKeys(s.TokensByTask) {
			_, _ = fmt.Fprintf(out, "               · %s: %d\n", task, s.TokensByTask[task])
		}
	}
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
