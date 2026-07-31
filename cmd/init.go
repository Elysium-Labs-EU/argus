package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newInitCmd() *cobra.Command {
	var (
		repo      string
		forgeKind string
		yes       bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write .argus/config.yml for this repo, prefilled from a toolchain guess",
		Long: `Init peeks for Taskfile.yml, Makefile, package.json, or go.mod (in that order,
first match wins) in the repo root to suggest a base_branch/allow/brief_note,
prints the suggestion, and asks you to confirm or edit each value before
writing .argus/config.yml (see internal/repoconfig). This is pure convenience:
argus itself has no built-in opinion on any repo's toolchain and never will —
guessing wrong, or a toolchain this version doesn't recognize, is not a bug to
file, just edit the YAML by hand. The forge key is the one setting init
can't guess at all: a self-hosted host is exactly the ambiguity
ship/supervise/worktree prune's own --forge flag exists to resolve, so
it's only ever set via --forge here or the interactive prompt.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, &initArgs{repo: repo, yes: yes, forgeKind: forgeKind})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo to write .argus/config.yml for")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "write the detected suggestion without prompting, and overwrite an existing config.yml without asking")
	cmd.Flags().StringVar(&forgeKind, "forge", "", "self-hosted forge kind for this repo's .argus/config.yml: \"gitlab\" or \"gitea\" (unset: prompted for interactively, or left unset with --yes, for a hosted or auto-detected forge)")
	return cmd
}

var initCmd = newInitCmd()

// initArgs holds newInitCmd's flag values so runInit can be tested directly,
// without going through cobra flag parsing.
type initArgs struct {
	repo      string
	forgeKind string
	yes       bool
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

	if _, statErr := os.Stat(path); statErr == nil && !a.yes {
		if !ui.Confirm(reader, out, fmt.Sprintf("%s already exists — overwrite?", path), false) {
			_, _ = fmt.Fprintln(out, "Canceled.")
			return nil
		}
	}

	suggested := detectRepoConfig(cmd.Context(), repoRoot)
	// --forge can't be detected the way base_branch/allow/brief_note are (see
	// detectRepoConfig): a self-hosted host is exactly the ambiguity forge.New
	// refuses to guess at, so it only ever comes from an explicit --forge or
	// the interactive prompt below, never a toolchain-marker-file guess.
	if a.forgeKind != "" {
		if _, err := parseForgeKind(a.forgeKind); err != nil {
			return err
		}
		suggested.Forge = a.forgeKind
	}
	cfg := suggested
	if !a.yes {
		cfg.BaseBranch = promptLine(reader, out, "base_branch", suggested.BaseBranch)
		cfg.Allow = promptList(reader, out, "allow", suggested.Allow)
		cfg.BriefNote = promptLine(reader, out, "brief_note", suggested.BriefNote)
		cfg.MaxDiffLines = promptOptionalInt(reader, out, "max_diff_lines", suggested.MaxDiffLines)
		cfg.ReworkBudget = promptOptionalInt(reader, out, "rework_budget (restart budget: total rework rounds a worktree may ever be dispatched for)", suggested.ReworkBudget)
		cfg.ProofRequiredPaths = promptList(reader, out, "proof_required_paths", suggested.ProofRequiredPaths)
		cfg.AlwaysReviewPaths = promptList(reader, out, "always_review_paths", suggested.AlwaysReviewPaths)
		cfg.WorkerPlacement = promptLine(reader, out, "worker_placement (workspace|tab)", suggested.WorkerPlacement)
		cfg.ShipVerifyCommand = promptLine(reader, out, "ship_verify_command (controller-side gate command run before commit)", suggested.ShipVerifyCommand)
		cfg.GateVerifyCommand = promptLine(reader, out, "gate_verify_command (gate: shell command re-run in a worker's worktree before a verdict is recorded)", suggested.GateVerifyCommand)
		cfg.WorktreeBootstrapCommand = promptLine(reader, out, "worktree_bootstrap_command (runs once in a fresh worktree, right after git worktree add, before the worker's agent starts)", suggested.WorktreeBootstrapCommand)
		cfg.ReviewEffort = promptLine(reader, out, "review_effort (low|medium|high|xhigh|max)", suggested.ReviewEffort)
		cfg.Launcher = promptLine(reader, out, "launcher (command started in each worker pane)", suggested.Launcher)
		cfg.WorktreeDir = promptLine(reader, out, "worktree_dir (blank for <repo>/.claude/worktrees/<branch>; \"..\" for a sibling-of-repo layout)", suggested.WorktreeDir)
		cfg.OwnerStaleAfter = promptLine(reader, out, "owner_stale_after (how long a worktree's owner-lease heartbeat may go quiet before a mismatched caller proceeds anyway, e.g. \"30m\"; blank for the built-in default)", suggested.OwnerStaleAfter)
		cfg.TitlePrefixTemplate = promptLine(reader, out, "title_prefix_template (required PR/commit title prefix, e.g. \"TICKET-{issue}: \")", suggested.TitlePrefixTemplate)
		cfg.ReviewNote = promptLine(reader, out, "review_note (free-text note appended to the reviewer's prompt)", suggested.ReviewNote)
		cfg.Forge = promptLine(reader, out, "forge (self-hosted only: gitlab|gitea, blank for hosted/auto-detected)", suggested.Forge)
		cfg.StatusPage = promptLine(reader, out, "status_page (status page URL to point at on a host-shaped forge/push failure; blank to use svcstatus's built-in map, which only covers github.com/gitlab.com/codeberg.org)", suggested.StatusPage)
	}

	if err := repoconfig.Save(path, &cfg); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s wrote %s\n", ui.LabelSuccess.Render("✓"), path)
	return nil
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

// detectRepoConfig peeks repoRoot for the marker files toolchainGuesses
// tracks (first match wins) to suggest allow/brief_note, and separately
// tries to detect the repo's default branch via origin/HEAD for
// base_branch. Every part is best-effort: an operator confirms or edits the
// result before anything is written (see runInit), so a wrong or empty
// guess here is not a bug, just a starting point.
func detectRepoConfig(ctx context.Context, repoRoot string) repoconfig.Config {
	var cfg repoconfig.Config
	for _, g := range toolchainGuesses {
		if _, err := os.Stat(filepath.Join(repoRoot, g.marker)); err == nil {
			cfg.Allow = g.allow
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
