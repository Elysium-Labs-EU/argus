package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/permission"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newInitCmd() *cobra.Command {
	var (
		repo      string
		forgeKind string
		yes       bool
		refresh   bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write .argus/config.yml for this repo, prefilled from a toolchain guess",
		Long: `Init peeks for Taskfile.yml, Makefile, package.json, or go.mod (in that order,
first match wins) in the repo root to suggest phases.*.allow/brief_note/
ship_verify_command, prints the suggestion, and asks you to confirm or edit
each value before writing .argus/config.yml (see internal/repoconfig). The
toolchain suggestion only ever populates phases.working.allow/
phases.self_test.allow, never the phase-independent top-level allow — that
key stays purely operator-authored, since a repo-wide grant would defeat the
whole point of scoping build/test commands to the phases that actually run
them. This is pure convenience: argus itself has no built-in opinion on any
repo's toolchain and never will — guessing wrong, or a toolchain this version
doesn't recognize, is not a bug to file, just edit the YAML by hand.
base_branch is guessed separately, from the local refs/remotes/origin/HEAD
ref rather than any toolchain marker — a ref a plain "git clone" sets up but
a bare "git init" or a shallow/single-branch clone won't, so base_branch is
left unset as often as not; that's a safe default; ResolveBase falls through
to origin/HEAD and then "main" at ship/rebase time anyway. The forge key is
the one setting init can't guess at all: a self-hosted host is exactly the
ambiguity ship/supervise/worktree prune's own --forge flag exists to
resolve, so it's only ever set via --forge here or the interactive prompt.

--refresh re-materializes only the phases.*.allow suggestion from the
current toolchain-detection default, preserving every other existing key
(including a hand-authored top-level allow) — for a repo whose config.yml
predates an improved default set in a newer argus.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, &initArgs{repo: repo, yes: yes, forgeKind: forgeKind, refresh: refresh})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo to write .argus/config.yml for")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "write the detected suggestion without prompting, and overwrite an existing config.yml without asking")
	cmd.Flags().StringVar(&forgeKind, "forge", "", "self-hosted forge kind for this repo's .argus/config.yml: \"gitlab\" or \"gitea\" (unset: prompted for interactively, or left unset with --yes, for a hosted or auto-detected forge)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "load the existing .argus/config.yml and re-materialize only its phases.*.allow suggestion from the current toolchain-detection default, leaving every other key (including a hand-authored top-level allow) untouched (skips the overwrite confirmation)")
	return cmd
}

var initCmd = newInitCmd()

// initArgs holds newInitCmd's flag values so runInit can be tested directly,
// without going through cobra flag parsing.
type initArgs struct {
	repo      string
	forgeKind string
	yes       bool
	refresh   bool
}

func runInit(cmd *cobra.Command, a *initArgs) error {
	repoRoot, err := supervisor.ResolveWorktree(a.repo)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	path := repoconfig.Path(repoRoot)

	// One shared bufio.Reader for every possible prompt below (see
	// cmd/uninstall.go's ui.Confirm usage): Confirm and the line prompts
	// below all read ahead past the first "\n", so a fresh reader per call
	// would strand an already-buffered later answer when several are
	// typed/piped together.
	reader := bufio.NewReader(cmd.InOrStdin())

	// A plain init over an existing file asks to overwrite first (unless
	// --yes); --refresh never asks — loading and updating in place is the
	// whole point of the flag, not a destructive surprise.
	if !a.refresh {
		if _, statErr := os.Stat(path); statErr == nil && !a.yes {
			if !ui.Confirm(reader, out, fmt.Sprintf("%s already exists — overwrite?", path), false) {
				_, _ = fmt.Fprintln(out, "Canceled.")
				return nil
			}
		}
	}

	cfg, err := startingConfig(cmd.Context(), repoRoot, path, a)
	if err != nil {
		return err
	}

	// --forge can't be detected the way base_branch/allow/brief_note are (see
	// detectRepoConfig): a self-hosted host is exactly the ambiguity forge.New
	// refuses to guess at, so it only ever comes from an explicit --forge or
	// the interactive prompt below, never a toolchain-marker-file guess.
	if a.forgeKind != "" {
		if _, err := parseForgeKind(a.forgeKind); err != nil {
			return err
		}
		cfg.Forge = a.forgeKind
	}
	if !a.yes {
		promptAllFields(reader, out, cfg)
	}

	if err := repoconfig.Save(path, cfg); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s wrote %s\n", ui.LabelSuccess.Render("✓"), path)
	printNextSteps(out)
	// The offer is interactive-only: --yes is the non-interactive path, and the
	// step it runs mutates .claude/settings.json, so it must never fire off an
	// unattended write.
	if !a.yes {
		offerConfigCheck(cmd, reader, out, repoRoot)
	}
	return nil
}

// startingConfig resolves the Config runInit prompts against and eventually
// saves: a fresh toolchain-detected suggestion (the plain-init path), or —
// with --refresh — the existing config.yml with only its phases.*.allow
// suggestion re-materialized from the current toolchain-detection default,
// every other key (including a hand-authored top-level allow) left exactly
// as loaded. Any overwrite confirmation is
// runInit's own concern, resolved before this is ever called. repoRoot is
// runInit's own already-resolved root (supervisor.ResolveWorktree), not
// re-derived here — a second, independent resolution of the same path is
// exactly the kind of duplicated ambient lookup that can silently diverge
// from the first.
func startingConfig(ctx context.Context, repoRoot, path string, a *initArgs) (*repoconfig.Config, error) {
	suggested := detectRepoConfig(ctx, repoRoot)
	if !a.refresh {
		return &suggested, nil
	}
	existing, err := repoconfig.Load(path)
	if err != nil {
		return nil, err
	}
	// suggested.Allow is always empty (detectRepoConfig never populates the
	// phase-independent top-level list — see its own comment), so there is
	// nothing to re-materialize there; only phases.*.allow is toolchain-
	// derived, and only it gets refreshed. existing.Allow, whatever an
	// operator hand-authored, is left untouched.
	existing.Phases = mergeAllowIntoPhases(existing.Phases, suggested.Phases)
	return &existing, nil
}

// promptAllFields walks the operator through every interactive prompt,
// mutating cfg in place with each answer (bare Enter keeps cfg's own current
// value — the toolchain suggestion on a fresh init, or the preserved
// existing value under --refresh).
func promptAllFields(reader *bufio.Reader, out io.Writer, cfg *repoconfig.Config) {
	cfg.BaseBranch = promptLine(reader, out, "base_branch", cfg.BaseBranch)
	cfg.Allow = promptList(reader, out, "allow", cfg.Allow)
	cfg.Phases = promptPhaseAllow(reader, out, cfg.Phases)
	cfg.BriefNote = promptLine(reader, out, "brief_note", cfg.BriefNote)
	cfg.MaxDiffLines = promptOptionalInt(reader, out, "review.max_diff_lines", cfg.MaxDiffLines)
	cfg.ReworkBudget = promptOptionalInt(reader, out, "rework.budget (restart budget: total rework rounds a worktree may ever be dispatched for)", cfg.ReworkBudget)
	cfg.MaxReworkRounds = promptOptionalInt(reader, out, "rework.max_rounds (per-invocation dispatch-and-judge loop ceiling, distinct from rework.budget)", cfg.MaxReworkRounds)
	cfg.ProofRequiredPaths = promptList(reader, out, "review.proof_required_paths", cfg.ProofRequiredPaths)
	cfg.AlwaysReviewPaths = promptList(reader, out, "review.always_review_paths", cfg.AlwaysReviewPaths)
	cfg.WorkerPlacement = promptLine(reader, out, "worker_placement (workspace|tab)", cfg.WorkerPlacement)
	cfg.ShipVerifyCommand = promptLine(reader, out, "ship.verify_command (controller-side gate command run before commit)", cfg.ShipVerifyCommand)
	cfg.GateVerifyCommand = promptLine(reader, out, "review.gate_verify_command (gate: shell command re-run in a worker's worktree before a verdict is recorded)", cfg.GateVerifyCommand)
	cfg.WorktreeBootstrapCommand = promptLine(reader, out, "worktree_bootstrap_command (runs once in a fresh worktree, right after git worktree add, before the worker's agent starts)", cfg.WorktreeBootstrapCommand)
	cfg.ReviewEffort = promptLine(reader, out, "review.review_effort (low|medium|high|xhigh|max)", cfg.ReviewEffort)
	cfg.Launcher = promptLine(reader, out, "launcher (command started in each worker pane)", cfg.Launcher)
	cfg.WorktreeDir = promptLine(reader, out, "worktree_dir (blank for <repo>/.claude/worktrees/<branch>; \"..\" for a sibling-of-repo layout)", cfg.WorktreeDir)
	cfg.OwnerStaleAfter = promptLine(reader, out, "owner_stale_after (how long a worktree's owner-lease heartbeat may go quiet before a mismatched caller proceeds anyway, e.g. \"30m\"; blank for the built-in default)", cfg.OwnerStaleAfter)
	cfg.TitlePrefixTemplate = promptLine(reader, out, "ship.title_prefix_template (required PR/commit title prefix, e.g. \"TICKET-{issue}: \")", cfg.TitlePrefixTemplate)
	cfg.ReviewNote = promptLine(reader, out, "review.review_note (free-text note appended to the reviewer's prompt)", cfg.ReviewNote)
	cfg.Forge = promptLine(reader, out, "forge (self-hosted only: gitlab|gitea, blank for hosted/auto-detected)", cfg.Forge)
	cfg.StatusPage = promptLine(reader, out, "status_page (status page URL to point at on a host-shaped forge/push failure; blank to use svcstatus's built-in map, which only covers github.com/gitlab.com/codeberg.org)", cfg.StatusPage)
}

// printNextSteps names the two onboarding actions init deliberately can't
// perform for you: config check --write lives in .claude/settings.json, which
// is untracked and per-clone (so it can't be folded into the config.yml init
// just wrote — every fresh clone would otherwise dead-end at a manual approval
// prompt on the first argus call), and a first --dry-run is the safe way to see
// a run before it spawns any workers.
func printNextSteps(out io.Writer) {
	_, _ = fmt.Fprintf(out, "\n%s\n", ui.TextBold.Render("Next steps:"))
	_, _ = fmt.Fprintf(out, "  1. %s  %s\n",
		ui.TextCommand.Render("argus config check --write"),
		ui.TextMuted.Render("allowlist argus in this clone's .claude/settings.json"))
	_, _ = fmt.Fprintf(out, "  2. %s  %s\n",
		ui.TextCommand.Render("argus supervise <task> --dry-run"),
		ui.TextMuted.Render("preview a run before spawning workers"))
}

// offerConfigCheck offers to run `config check --write` inline. It defaults to
// no because, unlike everything else init does, running it mutates
// .claude/settings.json — the same reason `config check` itself needs an
// explicit --write rather than fixing by default. A failure here is only
// warned about: the config.yml write already succeeded, so init should not
// exit non-zero over the optional follow-up.
func offerConfigCheck(cmd *cobra.Command, reader *bufio.Reader, out io.Writer, repoRoot string) {
	if !ui.Confirm(reader, out, "Run `argus config check --write` now?", false) {
		return
	}
	if err := runConfigCheck(cmd, &configCheckArgs{repo: repoRoot, entry: permission.DefaultAllowEntry, write: true}); err != nil {
		_, _ = fmt.Fprintf(out, "%s config check: %v\n", ui.LabelWarning.Render("!"), err)
	}
}

// toolchainGuess is one entry in detectRepoConfig's ordered detection table:
// a marker file to peek for, and the allow/brief_note it suggests.
type toolchainGuess struct {
	marker            string
	briefNote         string
	shipVerifyCommand string
	allow             []string
}

// toolchainGuesses is checked in order, first match wins — a repo carrying
// several marker files (e.g. a Makefile that just wraps `go test`) most
// likely wants the more specific runner's own allow pattern, not go's.
var toolchainGuesses = []toolchainGuess{
	{
		marker:            "Taskfile.yml",
		allow:             []string{"Bash(task *)"},
		briefNote:         "Add a focused test and keep task ci green.",
		shipVerifyCommand: "task lint",
	},
	{
		marker:            "Makefile",
		allow:             []string{"Bash(make *)"},
		briefNote:         "Add a focused test and keep make ci green. Follow the repo STYLE.md.",
		shipVerifyCommand: "make lint",
	},
	{
		marker:            "package.json",
		allow:             []string{"Bash(npm *)"},
		briefNote:         "Add a focused test and keep npm test green.",
		shipVerifyCommand: "npm run lint",
	},
	{
		marker:            "go.mod",
		allow:             []string{"Bash(go build *)", "Bash(go test *)", "Bash(go vet *)"},
		briefNote:         "Add a focused test and keep go test ./... green.",
		shipVerifyCommand: "golangci-lint run",
	},
}

// phasesForToolchainAllow turns a toolchain guess's flat allow list into a
// starting per-phase suggestion: working and self_test — the phases where a
// worker actually builds/runs its own change — get the full toolchain allow;
// planning, awaiting_review, and blocked get nothing beyond the structural
// floor, since there is no legitimate toolchain command to run before a plan
// exists or once a diff is already up for review. Returns nil for an empty
// allow (no marker file matched), matching every other "not configured"
// zero value here.
func phasesForToolchainAllow(allow []string) protocol.PhaseConfig {
	if len(allow) == 0 {
		return nil
	}
	return protocol.PhaseConfig{
		protocol.PhaseWorking:  {Allow: slices.Clone(allow)},
		protocol.PhaseSelfTest: {Allow: slices.Clone(allow)},
	}
}

// mergeAllowIntoPhases overlays suggested's own Allow onto existing,
// preserving every phase's own Deny/Skip untouched and leaving any phase
// present only in existing (e.g. a hand-authored planning.deny) exactly as
// it was — --refresh only ever re-materializes the toolchain-derived allow
// suggestion, never anything else a repo owner configured by hand.
func mergeAllowIntoPhases(existing, suggested protocol.PhaseConfig) protocol.PhaseConfig {
	merged := protocol.PhaseConfig{}
	maps.Copy(merged, existing)
	for p, sp := range suggested {
		policy := merged[p]
		policy.Allow = sp.Allow
		merged[p] = policy
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// promptPhaseAllow walks the operator through every protocol.
// ConfigurablePhases value's own allow list — the interactive co-authoring
// step: showing the proposed allowed actions (from toolchain detection, or
// from an existing config's own phases.<name>.allow under --refresh) and
// letting the operator add or remove entries, one phase at a time. Each
// phase's own Deny/Skip (if any) is preserved untouched; only Allow is ever
// prompted here — hand-edit the YAML directly for deny/skip, the same as
// before this prompt existed.
func promptPhaseAllow(reader *bufio.Reader, out io.Writer, suggested protocol.PhaseConfig) protocol.PhaseConfig {
	result := protocol.PhaseConfig{}
	maps.Copy(result, suggested)
	for _, p := range protocol.ConfigurablePhases {
		policy := result[p]
		policy.Allow = promptList(reader, out, fmt.Sprintf("phases.%s.allow", p), policy.Allow)
		if len(policy.Allow) > 0 || len(policy.Deny) > 0 || policy.Skip {
			result[p] = policy
		} else {
			delete(result, p)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// detectRepoConfig peeks repoRoot for the marker files toolchainGuesses
// tracks (first match wins) to suggest phases.*.allow/brief_note, and
// separately tries to detect the repo's default branch via origin/HEAD for
// base_branch. Every part is best-effort: an operator confirms or edits the
// result before anything is written (see runInit), so a wrong or empty
// guess here is not a bug, just a starting point.
func detectRepoConfig(ctx context.Context, repoRoot string) repoconfig.Config {
	var cfg repoconfig.Config
	for _, g := range toolchainGuesses {
		if _, err := os.Stat(filepath.Join(repoRoot, g.marker)); err == nil {
			// cfg.Allow (the phase-independent top-level list) is
			// deliberately left unset here: ResolvedAllowForPhase unions it
			// into every phase, so setting it to the toolchain guess would
			// grant build/test commands in planning/awaiting_review/blocked
			// too — silently defeating phasesForToolchainAllow's own
			// working/self_test-only restriction. The toolchain suggestion
			// belongs solely in cfg.Phases; cfg.Allow stays purely
			// operator-authored.
			cfg.Phases = phasesForToolchainAllow(g.allow)
			cfg.BriefNote = g.briefNote
			cfg.ShipVerifyCommand = g.shipVerifyCommand
			break
		}
	}
	if base, err := supervisor.DetectDefaultBase(ctx, repoRoot); err == nil {
		cfg.BaseBranch = base
	}
	return cfg
}

// promptLine prints label and def, reads one line from reader, and returns
// it trimmed — an empty answer (bare Enter) keeps def.
func promptLine(reader *bufio.Reader, out io.Writer, label, def string) string {
	shown := def
	if shown == "" {
		shown = "unset"
	}
	_, _ = fmt.Fprintf(out, "%s [%s]: ", label, shown)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptOptionalInt works like promptLine but for max_diff_lines, whose
// value is a *int: a bare Enter keeps def (including a nil "unset", which is
// not the same as 0 — 0 explicitly disables the diff ceiling), and an
// unparseable answer keeps def rather than aborting the whole init, matching
// the rest of init's best-effort, edit-the-YAML-later stance.
func promptOptionalInt(reader *bufio.Reader, out io.Writer, label string, def *int) *int {
	shown := "unset"
	if def != nil {
		shown = strconv.Itoa(*def)
	}
	_, _ = fmt.Fprintf(out, "%s [%s]: ", label, shown)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  %q is not a number, keeping %s\n", line, shown)
		return def
	}
	return &n
}

// promptList works like promptLine but for a comma-separated list, the
// shape `allow` takes.
func promptList(reader *bufio.Reader, out io.Writer, label string, def []string) []string {
	shown := "none"
	if len(def) > 0 {
		shown = strings.Join(def, ", ")
	}
	_, _ = fmt.Fprintf(out, "%s (comma-separated) [%s]: ", label, shown)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	parts := strings.Split(line, ",")
	items := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			items = append(items, p)
		}
	}
	return items
}
