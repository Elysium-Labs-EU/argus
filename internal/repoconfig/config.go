// Package repoconfig reads a repo's own optional .argus/config.yml — the
// declarative contract a repo owner can write once instead of fighting
// argus's toolchain-neutral defaults with --allow/--base on every
// invocation. argus itself assigns no semantics to
// any value here beyond the narrow places that read it back
// (agentadapter's base allow-list, supervise/rebase/ship's base-branch
// resolution, the brief_note appended verbatim to a generated brief, the
// review_note appended verbatim to the reviewer's prompt, cmd/gatepolicy.go's
// review-gate precedence, supervise's --worker-placement default, the gate's
// own re-run of verify_command in the worktree before a verdict is recorded,
// rework's own cumulative restart budget (rework_budget), and ship_lint —
// the one key that does run a command, controller-side,
// before ship commits) — the two exceptions to "argus runs no build/test/
// lint command of its own" are verify_command and ship_lint themselves,
// since a repo owner opts into each specific command by setting the key;
// argus still hardcodes no toolchain guess of what either command should be.
package repoconfig

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config is the in-memory shape of .argus/config.yml. All fields are
// optional; a missing file is equivalent to a zero Config. The gate keys
// mirror supervisor.ReviewPolicy field-for-field so a repo owner sets gate
// policy once instead of repeating --max-diff-lines/--proof-required-path/
// --always-review-path on every supervise/rework invocation.
// MaxDiffLines is a pointer because 0 is a legal value (disables the diff
// ceiling entirely) and must stay distinguishable from "key not present".
// WorkerPlacement stays a plain string (not a pointer) because its empty
// value already means "unset" — supervise treats "" identically to the
// --worker-placement default "workspace", so there is nothing to
// distinguish an absent key from. ShipVerifyCommand is likewise a plain
// string: an empty value means ship runs no extra command, just its
// built-in hook detection (see supervisor.EnforceHooks/RunShipVerifyCommand).
// GateVerifyCommand is not part of ReviewPolicy: it is not a pure policy
// check like the others, it is a shell command the gate executes in the
// worker's worktree (see supervisor.Config.GateVerifyCommand) — an empty
// string means the same "not configured, skip" as an absent key, so it
// needs no pointer either. GateVerifyCommand and ShipVerifyCommand are
// deliberately distinct checks at different points in the pipeline:
// GateVerifyCommand runs in the gate, before a verdict is recorded, so a
// failure is an unwaivable escalation the reviewer sees; ShipVerifyCommand
// (and EnforceHooks) runs controller-side at ship time, right before
// commit, as the last backstop regardless of how the verdict was reached.
// gate_verify_command is also the one general-purpose escape hatch for a
// repo-specific mechanical rule supervisor.ReviewPolicy has no field for:
// MaxDiffLines/ProofRequiredPaths/AlwaysReviewPaths are a closed, fixed set
// argus itself knows how to run, but any rule expressible as a script that
// exits non-zero on violation — a custom lint, a forbidden-import check, a
// schema-drift check — becomes an unwaivable HardReason via this one key,
// with no argus code change. Its one limitation: it only runs once a worker
// reaches a terminal phase, right before a verdict is recorded — not
// continuously during planning/working — so a violation surfaces at review
// time, not the moment the worker introduces it.
// WorktreeBootstrapCommand is a third, earlier shell command: it runs once
// in a freshly created worktree, right after `git worktree add` succeeds
// and before the worker's agent is spawned (see
// supervisor.RunWorktreeBootstrapCommand), so a repo whose task depends on
// gitignored per-developer local config (env files, local settings) that
// only exists in the original checkout can bootstrap it into every
// worktree instead of a worker hitting a silent, confusing file-not-found
// failure. Empty means no command is configured — the prior behavior, a
// bare `git worktree add` with no bootstrap step.
type Config struct {
	BaseBranch        string
	WorkerPlacement   string
	BriefNote         string
	ReviewNote        string
	ShipVerifyCommand string
	GateVerifyCommand string
	// WorktreeBootstrapCommand runs once, synchronously, in a freshly created
	// worktree, right after `git worktree add` succeeds and before the
	// worker's agent is spawned (see supervisor.RunWorktreeBootstrapCommand) — with
	// cwd already at the resolved WorktreeDir location, since `git worktree
	// add` already succeeded there. It must never attempt to create or
	// relocate the worktree itself (git will refuse). A script that
	// hardcodes a relative hop count back to the original checkout (e.g. "cp
	// ../../.env .env", assuming the default two-levels-deep layout) breaks
	// if WorktreeDir changes the nesting depth; prefer deriving the repo
	// root at runtime (`git rev-parse --show-toplevel`) over a hardcoded hop
	// count.
	WorktreeBootstrapCommand string
	ReviewEffort             string
	// Launcher is the command started in each spawned worker pane, mirroring
	// supervise's own --launcher flag. Empty behaves like supervisor.
	// DefaultLauncher was chosen: the same "not configured, skip" shape as
	// GateVerifyCommand/ShipVerifyCommand, no pointer needed. An explicit --launcher flag
	// still overrides this.
	Launcher string
	// Forge names the API shape ("gitlab" or "gitea") for a repo whose host
	// isn't one of forge.New's three auto-detected hosts (github.com,
	// gitlab.com, codeberg.org) — the same ambiguity ship/supervise/worktree
	// prune's own --forge flag resolves per invocation, set once here since a
	// repo's forge is a static fact, not something that varies invocation to
	// invocation. Empty behaves like forge.KindAuto: the hosted-forge
	// allowlist still applies, and any other host still refuses without an
	// explicit --forge. An explicit --forge flag always overrides this.
	Forge string
	// StatusPage overrides the status page internal/svcstatus points at when a
	// forge request or push fails in a host-shaped way (see
	// svcstatus.WorthMentioning). svcstatus's built-in map only knows the three
	// hosted forges' own pages; a self-hosted host has no built-in entry, and
	// there is no way to guess where an operator's own Statuspage/Cachet/static
	// page lives, so this is the same static per-repo fact as Forge, set once
	// instead of repeated. Checked before svcstatus's built-in map, so it also
	// lets a repo owner point at a mirror instead of a known host's own page.
	// An explicit --status-page-url flag (ship only) always overrides this.
	StatusPage string
	// WorktreeDir overrides where a spawned worker's worktree is created,
	// same plain-string-means-unset shape as WorkerPlacement. Empty keeps
	// argus's default (<repo>/.claude/worktrees/<branch>); a relative value
	// (e.g. "..") is joined under the repo root — the escape hatch for a repo
	// whose own convention is a sibling directory next to the checkout rather
	// than a nested one; an absolute value is used as-is. Changing this
	// shifts the worktree's nesting depth relative to the original checkout
	// — see WorktreeBootstrapCommand's own comment if that command hardcodes a
	// relative hop count back to it.
	WorktreeDir string
	// TitlePrefixTemplate, when set, is a required prefix ship mechanically
	// enforces on the PR/commit title it ends up using — worker-reported
	// (protocol.Status.Title), forge-fetched, branch-derived, or an explicit
	// --title override — before opening the PR. A repo's own title
	// convention (e.g. a ticket-key prefix) previously lived only in
	// brief_note prose a worker could get wrong like any other instruction;
	// this key gives the same convention a mechanical, unbypassable check.
	// The literal substring "{issue}" is replaced with --jira-issue's key if
	// set, else "#<--issue>" if --issue is set, else the empty string. A
	// title that already starts with the rendered prefix is left alone;
	// otherwise the prefix is prepended. Empty means no enforcement, the
	// same "not configured" default every other key here has.
	TitlePrefixTemplate string
	// OwnerStaleAfter overrides how long a worktree's owner-lease heartbeat
	// (see internal/ownership) may go quiet before a mismatched caller is let
	// through instead of refused, same "not configured, skip" plain-string
	// shape as VerifyCommand/ShipLint — empty falls back to
	// ownership.DefaultStaleAfter. Stored as a Go duration string (e.g.
	// "30m") rather than a parsed time.Duration so a malformed value is only
	// ever an error at the one place it's consumed (resolveOwnerStaleAfter),
	// not at Load time for every command that merely reads other keys. An
	// explicit --owner-stale-after flag always overrides this.
	OwnerStaleAfter string
	MaxDiffLines    *int
	// ReworkBudget overrides how many rework rounds a worktree may be
	// dispatched for in total, across every separate `argus rework`
	// invocation over its lifetime — not the same knob as rework's own
	// --max-rounds, which only bounds one invocation's internal loop. A
	// pointer for the same reason as MaxDiffLines: 0 is a legal value
	// (disables the budget entirely) that must stay distinguishable from
	// "key not present". See supervisor.DefaultMaxReworkBudget for the
	// default when neither this nor --max-rework-budget is set.
	ReworkBudget *int
	Allow        []string
	// ProofRequiredPaths, when set, entirely replaces
	// supervisor.DefaultReviewPolicy's own built-in list rather than merging
	// with it — the same "config wins outright, no additive merge" shape
	// every other gate-policy key here has (see resolveGatePolicy).
	ProofRequiredPaths []string
	// AlwaysReviewPaths, when set, entirely replaces
	// supervisor.DefaultReviewPolicy's own built-in list rather than merging
	// with it, same as ProofRequiredPaths. The one exception is
	// .argus/config.yml itself: supervisor.Assess checks it unconditionally,
	// independent of this list, so a repo's own AlwaysReviewPaths can never
	// silently drop that check by omission — see supervisor's selfConfigPath.
	AlwaysReviewPaths []string
	// Deprecated is populated only by Load/parseYAML reading an old-named key
	// (see deprecatedKeyAliases) — never set by anything that constructs a
	// Config directly, such as runInit's own suggested/cfg values.
	Deprecated []DeprecatedKeyUse
}

// DeprecatedKeyUse records one old-named .argus/config.yml key parseYAML
// mapped to its current name, so a caller with access to user-facing output
// can warn about it — argus is young enough that key names are still being
// corrected, and an old name that silently keeps working forever gives an
// operator no signal to migrate off it.
type DeprecatedKeyUse struct {
	Old string
	New string
}

// pathEnvVar overrides the default <repo>/.argus/config.yml location for
// tests, mirroring internal/config's ARGUS_CONFIG_FILE.
const pathEnvVar = "ARGUS_REPO_CONFIG_FILE"

// Path resolves the config file location for repoRoot: $ARGUS_REPO_CONFIG_FILE
// if set, else <repoRoot>/.argus/config.yml.
func Path(repoRoot string) string {
	if p := os.Getenv(pathEnvVar); p != "" {
		return p
	}
	return filepath.Join(repoRoot, ".argus", "config.yml")
}

// Load reads and parses the config file at path. A missing file is not an
// error — it returns a zero Config, the same "nothing configured" state as
// an empty file, since every key is optional.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from a repo root argus already trusts, or an operator-set test env var
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	cfg, err := parseYAML(string(data))
	if err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to path as YAML, creating its parent directory if needed.
// cfg is a pointer solely to avoid copying the struct at the call site; Save
// does not mutate it.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // .argus/ dir inside a repo checkout, standard perms
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(encodeYAML(cfg)), 0o644); err != nil { //nolint:gosec // repo-tracked config file, meant to be readable
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}
