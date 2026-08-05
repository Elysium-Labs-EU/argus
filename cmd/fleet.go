package cmd

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newFleetCmd() *cobra.Command {
	var repo string
	var jsonOut bool
	var all bool
	var includeIdle bool
	var owner string

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

By default fleet only shows worktrees owned by this invocation's own
resolved identity (--owner, then $ARGUS_OWNER_ID, then $HERDR_WORKSPACE_ID —
the same chain supervise itself uses) plus any unowned worktree (no
owner.json at all can't be attributed away, so it stays visible) — a repo
that accumulates worktrees across many sessions is mostly noise otherwise.
--all restores every linked worktree regardless of owner. Within that scope,
a worktree with no status.json yet (nothing has happened there) is hidden by
default too; --include-idle restores those.

--json emits the same rows structured inside an envelope carrying
generated_at/scope/controller_id/count and how many rows each filter
dropped (excluded_foreign_count, excluded_idle_count), so a controller
(e.g. the session driving argus) can correlate each worktree to its own
task/todo list and tell "nothing here" apart from "filtered out."`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFleet(cmd, &fleetArgs{repo: repo, jsonOut: jsonOut, all: all, includeIdle: includeIdle, owner: owner})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose linked worktrees to list")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON instead of a table")
	cmd.Flags().BoolVar(&all, "all", false, "show every linked worktree regardless of owner (default: only this invocation's own + unowned worktrees)")
	cmd.Flags().BoolVar(&includeIdle, "include-idle", false, "also show worktrees with no status.json yet (default: hidden, since nothing has happened there)")
	cmd.Flags().StringVar(&owner, "owner", "", "override this invocation's resolved identity used to scope the default view (default: $ARGUS_OWNER_ID, then $HERDR_WORKSPACE_ID, then a generated id — the same resolution argus supervise itself uses)")
	return cmd
}

var fleetCmd = newFleetCmd()

// fleetArgs holds newFleetCmd's flag values so runFleet is testable directly,
// without going through cobra flag parsing.
type fleetArgs struct {
	repo        string
	owner       string
	jsonOut     bool
	all         bool
	includeIdle bool
}

// fleetEnvelope is --json's top-level shape: the filtered rows plus enough
// accounting (count and each filter's own excluded total) that a caller can
// always tell an empty fleet apart from one that's just been filtered down —
// silently shrinking the list with no trace would make the two
// indistinguishable to a controller parsing this.
type fleetEnvelope struct {
	GeneratedAt          time.Time             `json:"generated_at"`
	Scope                string                `json:"scope"`
	ControllerID         string                `json:"controller_id"`
	Worktrees            []supervisor.FleetRow `json:"worktrees"`
	Count                int                   `json:"count"`
	ExcludedForeignCount int                   `json:"excluded_foreign_count"`
	ExcludedIdleCount    int                   `json:"excluded_idle_count"`
}

// buildFleet is a var, not a plain call to supervisor.BuildFleet, so a test
// driving runFleet can inject a failure after resolvedRepo/repoRoot have
// already resolved successfully — mirrors cmd/ship.go's currentBranch var.
var buildFleet = supervisor.BuildFleet

// runFleet is newFleetCmd's RunE body, extracted so it's testable without
// going through cobra flag parsing.
func runFleet(cmd *cobra.Command, a *fleetArgs) error {
	ctx := cmd.Context()
	resolvedRepo, err := supervisor.ResolveWorktree(a.repo)
	if err != nil {
		return err
	}
	repoRoot, err := supervisor.RepoRoot(ctx, resolvedRepo)
	if err != nil {
		return fmt.Errorf("resolving repo root for %s: %w", resolvedRepo, err)
	}

	now := time.Now()
	rows, err := buildFleet(ctx, repoRoot, now)
	if err != nil {
		return err
	}

	controllerID := ownership.ResolveOwnerID(a.owner)
	filtered := supervisor.FilterFleet(rows, controllerID, a.all, a.includeIdle)

	if a.jsonOut {
		return encodeFleetJSON(cmd, &filtered, controllerID, a.all, now)
	}
	renderFleet(cmd, filtered.Rows, now)
	return nil
}

// fleetScope renders --all's effect as --json's own "scope" string, so a
// consumer doesn't have to re-derive it from --all's absence.
func fleetScope(all bool) string {
	if all {
		return "all"
	}
	return "mine"
}

// encodeFleetJSON writes filtered as --json's envelope (see fleetEnvelope).
func encodeFleetJSON(cmd *cobra.Command, filtered *supervisor.FleetFilterResult, controllerID string, all bool, now time.Time) error {
	envelope := fleetEnvelope{
		GeneratedAt:          now,
		Scope:                fleetScope(all),
		ControllerID:         controllerID,
		Count:                len(filtered.Rows),
		ExcludedForeignCount: filtered.ExcludedForeignCount,
		ExcludedIdleCount:    filtered.ExcludedIdleCount,
		Worktrees:            filtered.Rows,
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(envelope); err != nil {
		return fmt.Errorf("encoding fleet envelope: %w", err)
	}
	return nil
}

// renderFleet prints one table row per worktree. Split out of runFleet so
// the rendering logic is independently testable against a canned []FleetRow,
// without a real git repo. now is the same clock BuildFleet computed
// HeartbeatAge against, reused here for the phase-age column so both ages
// are relative to one consistent instant.
func renderFleet(cmd *cobra.Command, rows []supervisor.FleetRow, now time.Time) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(out, "%s no worktrees linked to this repo\n", ui.TextMuted.Render("i"))
		return
	}

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "BRANCH\tPHASE\tAGE\tTITLE\tAPPROVED\tLIFECYCLE\tPR\tOWNER\tHEARTBEAT\tPATH")
	for i := range rows {
		r := &rows[i]
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(r.Branch), fleetField(r.StatusFile, string(r.Status.Phase)), phaseAgeCell(r, now), titleCell(r),
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

// titleCell renders the TITLE column, falling back to the resolved PR title
// ship persisted into lifecycle.json (protocol.Lifecycle.Title) when the
// worker's own status.json Title was left blank — status.json's Title is
// worker-supplied and optional, so a worktree whose worker never self-titled
// would otherwise show blank forever, even after a successful ship. An
// unreadable status.json still renders "unreadable" first, matching every
// other status-gated cell — a corrupt file is a real anomaly the lifecycle
// fallback must not paper over.
func titleCell(r *supervisor.FleetRow) string {
	if r.StatusFile == supervisor.FileUnreadable {
		return "unreadable"
	}
	title := r.Status.Title
	if title == "" && r.LifecycleFile == supervisor.FileOK {
		title = r.Lifecycle.Title
	}
	return orDash(title)
}

// prCell shows the full PR URL when lifecycle.json recorded one — the
// drill-down surface --json already exposes as pr_url, now in the table too
// — falling back to a bare "#N" for a legacy lifecycle record with a PR
// number but no URL.
func prCell(r *supervisor.FleetRow) string {
	if r.LifecycleFile != supervisor.FileOK || r.Lifecycle.PRNumber == 0 {
		return fleetField(r.LifecycleFile, "")
	}
	if r.Lifecycle.PRURL != "" {
		return r.Lifecycle.PRURL
	}
	return fmt.Sprintf("#%d", r.Lifecycle.PRNumber)
}

func heartbeatCell(r *supervisor.FleetRow) string {
	if r.OwnerFile != supervisor.FileOK {
		return fleetField(r.OwnerFile, "")
	}
	return r.HeartbeatAge.Round(time.Second).String() + " ago"
}

// phaseAgeCell renders how long ago status.json's Status.UpdatedAt was last
// written — a separate axis from heartbeatCell's owner/process liveness (see
// newFleetCmd's Long help): this is how long the current phase has been
// sitting, not whether anyone is still around to advance it. now is
// BuildFleet's same injected clock. UpdatedAt is "last report time," which
// only equals phase-entry time if nothing re-reported the same phase since —
// an acceptable staleness proxy, not an exact phase-entry timestamp.
func phaseAgeCell(r *supervisor.FleetRow, now time.Time) string {
	if r.StatusFile != supervisor.FileOK {
		return fleetField(r.StatusFile, "")
	}
	return now.Sub(r.Status.UpdatedAt).Round(time.Second).String() + " ago"
}
