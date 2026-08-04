package cmd

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newFleetCmd() *cobra.Command {
	var repo string
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "List every worktree linked to this repo with its phase/owner/lifecycle",
		Long: `Fleet is a read-only aggregate: one row per worktree linked to this repo
(git worktree list), joining the ground truth already scattered across each
worktree's own status.json (phase/title), verdict.json (approved),
lifecycle.json (post-ship state/PR number), and owner.json (owner label,
heartbeat). It tracks nothing new and writes nothing — supervise's own poll
loop already re-stamps heartbeat_at on the files fleet only reads.

Phase/lifecycle/PR is a worktree's health; owner + heartbeat age is a
separate liveness signal. A finished-but-unmerged worktree has a stale
heartbeat and is still fine, so a stale heartbeat is never itself shown as a
problem.

A control-plane file that exists but fails to decode (malformed JSON, or the
literal value "null" some of these loaders silently accept as a zero value)
renders as "unreadable", never as a blank phase — a blank phase would
otherwise look identical to a worktree that simply hasn't reported yet.

--json emits the same rows structured, so a controller (e.g. the session
driving argus) can correlate each worktree to its own task/todo list.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFleet(cmd, repo, jsonOut)
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose linked worktrees to list")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON instead of a table")
	return cmd
}

var fleetCmd = newFleetCmd()

// runFleet is newFleetCmd's RunE body, extracted so it's testable without
// going through cobra flag parsing.
func runFleet(cmd *cobra.Command, repo string, jsonOut bool) error {
	ctx := cmd.Context()
	resolvedRepo, err := supervisor.ResolveWorktree(repo)
	if err != nil {
		return err
	}
	repoRoot, err := supervisor.RepoRoot(ctx, resolvedRepo)
	if err != nil {
		return fmt.Errorf("resolving repo root for %s: %w", resolvedRepo, err)
	}

	rows, err := supervisor.BuildFleet(ctx, repoRoot, time.Now())
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fmt.Errorf("encoding fleet rows: %w", err)
		}
		return nil
	}
	renderFleet(cmd, rows)
	return nil
}

// renderFleet prints one table row per worktree. Split out of runFleet so
// the rendering logic is independently testable against a canned []FleetRow,
// without a real git repo.
func renderFleet(cmd *cobra.Command, rows []supervisor.FleetRow) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "%s no worktrees linked to this repo\n", ui.TextMuted.Render("i"))
		return
	}

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BRANCH\tPHASE\tTITLE\tAPPROVED\tLIFECYCLE\tPR\tOWNER\tHEARTBEAT\tPATH")
	for i := range rows {
		r := &rows[i]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(r.Branch), fleetField(r.StatusFile, string(r.Status.Phase)), fleetField(r.StatusFile, r.Status.Title),
			approvedCell(r), fleetField(r.LifecycleFile, string(r.Lifecycle.State)), prCell(r),
			fleetField(r.OwnerFile, r.Owner.OwnerLabel), heartbeatCell(r), r.Path)
	}
	_ = w.Flush()
}

// fleetField renders one status-gated cell: "-" for an absent file or an
// empty value, "unreadable" for a file that exists but failed to decode
// (including the null-JSON fail-open case), otherwise the value itself.
func fleetField(status supervisor.FileStatus, value string) string {
	if status == supervisor.FileUnreadable {
		return "unreadable"
	}
	return orDash(value)
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func approvedCell(r *supervisor.FleetRow) string {
	switch r.VerdictFile {
	case supervisor.FileOK:
		if r.Verdict.Approved {
			return "y"
		}
		return "n"
	case supervisor.FileUnreadable:
		return "unreadable"
	case supervisor.FileAbsent:
		return "-"
	}
	return "-"
}

func prCell(r *supervisor.FleetRow) string {
	if r.LifecycleFile != supervisor.FileOK || r.Lifecycle.PRNumber == 0 {
		return fleetField(r.LifecycleFile, "")
	}
	return fmt.Sprintf("#%d", r.Lifecycle.PRNumber)
}

func heartbeatCell(r *supervisor.FleetRow) string {
	if r.OwnerFile != supervisor.FileOK {
		return fleetField(r.OwnerFile, "")
	}
	return r.HeartbeatAge.Round(time.Second).String() + " ago"
}
