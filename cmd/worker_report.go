package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Commands a worker agent runs against its own worktree",
	}
	cmd.AddCommand(newWorkerReportCmd())
	cmd.AddCommand(newWorkerAnswerCmd())
	return cmd
}

var workerCmd = newWorkerCmd()

// reportablePhases is every phase a worker may name as the <phase> argument to
// `argus worker report`. protocol.PhaseDone is deliberately excluded: see
// internal/protocol/transition.go — a worker report can never set it, only
// argus's own ship path does.
var reportablePhases = []protocol.Phase{
	protocol.PhasePlanning,
	protocol.PhaseWorking,
	protocol.PhaseSelfTest,
	protocol.PhaseAwaitingReview,
	protocol.PhaseBlocked,
}

func newWorkerReportCmd() *cobra.Command {
	var (
		worktree string
		file     string
	)

	cmd := &cobra.Command{
		Use:   "report <phase>",
		Short: "Report a worker's status, gated by the legal phase-transition table",
		Long: `Report replaces a worker writing status.json directly: it
loads the worktree's current status, checks that <phase> is a legal move from
that phase against the same table pollStatus enforces, and only if legal
stamps the timestamp itself and persists the rest of the status body read from
stdin (or --file). Neither a worker's own clock nor a worker's own
claim of what phase comes next is trusted; argus decides both.

<phase> is one of: planning, working, self_test, awaiting_review, blocked.
"done" is never accepted here — only argus's ship path sets it.

The body is the same JSON shape status.json used to be written as directly
(task, branch, real_world_proof, pr_url, blocked_reason, files_touched, plan,
tests, diff_stat) minus updated_at, which is ignored even if present.

Reporting "planning" with an empty (or missing) plan array is accepted, but the
next report is not: moving from planning to working is rejected unless the
planning report already on file carried a non-empty plan array.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wt := worktree
			if wt == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				wt = cwd
			}
			phase, err := parseReportablePhase(args[0])
			if err != nil {
				return err
			}
			body, err := readReportBody(cmd, file)
			if err != nil {
				return err
			}
			var rest protocol.Status
			if err := json.Unmarshal(body, &rest); err != nil {
				return &ui.UserError{
					Err:  fmt.Errorf("decoding status body: %w", err),
					Hint: "the body must be the same JSON shape status.json used",
				}
			}
			if err := runWorkerReport(wt, phase, &rest, time.Now); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s reported %s -> %s\n", ui.LabelSuccess.Render("✓"), wt, phase)
			return nil
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree whose status to report (default: current directory)")
	cmd.Flags().StringVar(&file, "file", "", "read the status body from this file instead of stdin")
	return cmd
}

// parseReportablePhase validates a worker-supplied phase string against
// reportablePhases, so a typo or an attempt to report "done" fails with a clear
// message instead of silently falling through to the transition-table check
// with an unrecognized Phase value.
func parseReportablePhase(s string) (protocol.Phase, error) {
	p := protocol.Phase(s)
	if slices.Contains(reportablePhases, p) {
		return p, nil
	}
	return "", &ui.UserError{
		Err:  fmt.Errorf("unknown phase %q", s),
		Hint: "one of: planning, working, self_test, awaiting_review, blocked",
	}
}

// readReportBody reads the status JSON body from --file when given, otherwise
// from stdin — the worker pipes the same body it used to write to status.json
// directly.
func readReportBody(cmd *cobra.Command, file string) ([]byte, error) {
	if file != "" {
		data, err := os.ReadFile(file) //nolint:gosec // worker-supplied path, same trust level as --worktree
		if err != nil {
			return nil, fmt.Errorf("reading status body: %w", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return nil, fmt.Errorf("reading status body from stdin: %w", err)
	}
	if len(data) == 0 {
		return nil, &ui.UserError{
			Err:  fmt.Errorf("empty status body"),
			Hint: "pipe the status JSON on stdin, or pass --file",
		}
	}
	return data, nil
}

// runWorkerReport is the guarded transition + write at the heart of `argus
// worker report`: it loads the worktree's current status, rejects
// next if it is not a legal move from the current phase, rejects planning ->
// working if the on-file planning report carries no plan evidence, and only
// then stamps UpdatedAt with now() — argus's clock, never the
// caller's — before persisting. Split out of the RunE closure so it is
// directly testable without going through cobra flag parsing or stdin.
func runWorkerReport(worktree string, next protocol.Phase, rest *protocol.Status, now func() time.Time) error {
	cur, _ := protocol.Load(protocol.StatusPath(worktree))
	if !protocol.IsLegalTransition(cur.Phase, next) {
		return &ui.UserError{
			Err:  fmt.Errorf("illegal status transition %q -> %q", cur.Phase, next),
			Hint: fmt.Sprintf("legal from %q: %v", cur.Phase, protocol.LegalNext(cur.Phase)),
		}
	}
	if protocol.RequiresPlanEvidence(cur.Phase, next) && len(cur.Plan) == 0 {
		return &ui.UserError{
			Err:  fmt.Errorf("cannot move %q -> %q: no plan/todo evidence reported during planning", cur.Phase, next),
			Hint: `report phase "planning" again with a non-empty "plan" array listing your todo items before moving to "working"`,
		}
	}
	rest.Phase = next
	rest.UpdatedAt = now()
	// Base is set once by supervise at worktree-creation time (see
	// internal/repoconfig), never by the worker — its reported JSON body has
	// no "base" key, so unmarshaling it into rest always leaves rest.Base
	// empty. Carry the prior value forward instead of losing it on every
	// report.
	rest.Base = cur.Base
	// Answer is only ever set by `argus worker answer`, never by a worker's
	// own report body — carry it (and the Question it resolved) forward the
	// same way, unless this report names a fresh Question, which means a new
	// blocked cycle started and any earlier answer no longer applies.
	if rest.Question == nil {
		rest.Question = cur.Question
		rest.Answer = cur.Answer
	}
	// A worker that doesn't set title in this report (e.g. a rework round
	// describing only that round's fix) must not wipe out a title an earlier
	// report already set — only a report that explicitly names a new title
	// gets to replace it.
	if rest.Title == "" {
		rest.Title = cur.Title
	}
	return protocol.Write(protocol.StatusPath(worktree), rest)
}
