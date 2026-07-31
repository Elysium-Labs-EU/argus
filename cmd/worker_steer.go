package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newWorkerSteerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "steer <worktree> <text>",
		Short: "Inject a follow-up note into a worker that is still working",
		Long: `Steer lets a supervisor correct a worker's direction without waiting for it
to reach a terminal phase and re-dispatching via rework. It records TEXT onto
the worktree's status.json as a durable trace (a new entry in Steers), then
delivers it as a chat message into the worker's live pane — the worker keeps
its existing context and plan; the note augments its current turn rather than
starting a fresh one.

<worktree> must currently be reporting phase "working". This is deliberately
narrower than "not yet terminal": a worker in planning has no live turn to
steer yet, and one in self_test/blocked/awaiting_review already has a
first-class path (report a fresh phase, or ` + "`argus worker answer`" + `).

Steer does not itself change the worker's reported phase; the worker still
reports its next phase once it acts on the note, the same as any other
report. Each working leg gets at most MaxSteersPerWorking notes — a worker's
own next phase report resets that budget, so repeated steering can't become a
silent substitute for the phase-transition table itself.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, closeLog := openRunLog(cmd, "worker-steer")
			defer closeLog()
			return runWorkerSteer(cmd, herdr.New(), logger, args[0], args[1], time.Now)
		},
	}
	return cmd
}

// runWorkerSteer is newWorkerSteerCmd's RunE body: it validates the target
// worker is actually working and under its steer budget, appends a Steer to
// status.json, then delivers it into the worker's live pane. Split out of the
// RunE closure so it is directly testable without cobra flag parsing,
// mirroring runWorkerAnswer.
func runWorkerSteer(cmd *cobra.Command, client herdr.Client, logger *eventlog.Logger, worktree, text string, now func() time.Time) error {
	if worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus worker steer <worktree> <text>"}
	}
	if text == "" {
		return &ui.UserError{Err: fmt.Errorf("no follow-up text given"), Hint: "argus worker steer <worktree> <text>"}
	}
	abs, err := supervisor.ResolveWorktree(worktree)
	if err != nil {
		return err
	}
	worktree = abs

	cur, err := protocol.Load(protocol.StatusPath(worktree))
	if err != nil {
		return &ui.UserError{
			Err:  fmt.Errorf("loading status for %s: %w", worktree, err),
			Hint: "the worker must have reported at least once before it can be steered",
		}
	}
	if cur.Phase != protocol.PhaseWorking {
		return &ui.UserError{
			Err:  fmt.Errorf("%s is not working (phase %q)", worktree, cur.Phase),
			Hint: "worker steer only applies to a worker currently reporting phase \"working\" — use `argus worker answer` for a blocked worker, or wait for a terminal phase",
		}
	}
	if len(cur.Steers) >= protocol.MaxSteersPerWorking {
		return &ui.UserError{
			Err:  fmt.Errorf("%s already received %d steer messages this working phase", worktree, len(cur.Steers)),
			Hint: "wait for the worker to reach its next phase (which resets the budget), or let it run to a terminal phase and use `argus rework` instead of steering further",
		}
	}

	cur.Steers = append(cur.Steers, protocol.Steer{Text: text, DeliveredAt: now()})
	cur.UpdatedAt = now()
	if werr := protocol.Write(protocol.StatusPath(worktree), &cur); werr != nil {
		return fmt.Errorf("recording steer for %s: %w", worktree, werr)
	}

	ctx := cmd.Context()
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return fmt.Errorf("steer recorded, but resolving repo root for %s to deliver it: %w", worktree, err)
	}
	wt, err := client.WorktreeOpen(ctx, repoRoot, worktree)
	if err != nil {
		return fmt.Errorf("steer recorded, but could not open %s's pane to deliver it: %w", worktree, err)
	}
	if wt.RootPaneID == "" {
		return fmt.Errorf("steer recorded, but herdr opened no pane for %s to deliver it to", worktree)
	}

	message := supervisor.SteerMessage(text)
	if err := deliverPaneMessage(ctx, logger, client, wt.RootPaneID, worktree, "steer", message); err != nil {
		return fmt.Errorf("steer recorded, but delivery failed: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s steer recorded and delivered to %s\n", ui.LabelSuccess.Render("✓"), worktree)
	return nil
}
