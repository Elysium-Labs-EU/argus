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
// and ship_lint — the one key that does run a command, controller-side,
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
// distinguish an absent key from. ShipLint is likewise a plain string: an
// empty value means ship runs no extra command, just its built-in hook
// detection (see supervisor.EnforceHooks/RunShipLint).
// VerifyCommand is not part of ReviewPolicy: it is not a pure policy check
// like the others, it is a shell command the gate executes in the worker's
// worktree (see supervisor.Config.VerifyCommand) — an empty string means the
// same "not configured, skip" as an absent key, so it needs no pointer
// either. VerifyCommand and ShipLint are deliberately distinct checks at
// different points in the pipeline: VerifyCommand runs in the gate, before a
// verdict is recorded, so a failure is an unwaivable escalation the reviewer
// sees; ShipLint (and EnforceHooks) runs controller-side at ship time, right
// before commit, as the last backstop regardless of how the verdict was
// reached.
type Config struct {
	BaseBranch      string
	WorkerPlacement string
	BriefNote       string
	ReviewNote      string
	ShipLint        string
	VerifyCommand   string
	ReviewEffort    string
	// Forge names the API shape ("gitlab" or "gitea") for a repo whose host
	// isn't one of forge.New's three auto-detected hosts (github.com,
	// gitlab.com, codeberg.org) — the same ambiguity ship/supervise/worktree
	// prune's own --forge flag resolves per invocation, set once here since a
	// repo's forge is a static fact, not something that varies invocation to
	// invocation. Empty behaves like forge.KindAuto: the hosted-forge
	// allowlist still applies, and any other host still refuses without an
	// explicit --forge. An explicit --forge flag always overrides this.
	Forge string
	// WorktreeDir overrides where a spawned worker's worktree is created,
	// same plain-string-means-unset shape as WorkerPlacement. Empty keeps
	// argus's default (<repo>/.claude/worktrees/<branch>); a relative value
	// (e.g. "..") is joined under the repo root — the escape hatch for a repo
	// whose own convention is a sibling directory next to the checkout rather
	// than a nested one; an absolute value is used as-is.
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
	MaxDiffLines        *int
	Allow               []string
	ProofRequiredPaths  []string
	AlwaysReviewPaths   []string
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
