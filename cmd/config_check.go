package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/permission"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// configCheckCmd is a package-level var (like worktreePruneCmd) so
// skill_lint_test.go can cross-check its flags against SKILL.md directly.
var configCheckCmd = newConfigCheckCmd()

func newConfigCheckCmd() *cobra.Command {
	var (
		repo  string
		entry string
		write bool
	)

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check (and optionally fix) the Bash allow/deny entries argus itself needs",
		Long: `Every argus invocation (supervise, ship, review, rebase, ...) goes through the
calling agent's own Bash-tool permission gate — a separate layer from argus's
own gate/verdict system, and one argus can't reach into itself. Without an
allow entry in this repo's .claude/settings.json, every argus call prompts
for manual approval, defeating the point of using argus for the mechanical
half of supervision.

check reports whether an entry already covers argus (the broad "Bash(argus
*)" or a narrower per-subcommand entry like "Bash(argus ship *)" both count).
Pass --write to add one for you; check never touches any other key in the
file. This is not a blanket bypass of judgment calls the calling agent still
needs the user's explicit say-so for (e.g. ship --force) — those remain the
agent's responsibility regardless of what's allowlisted here.

check also reports (and --write adds) a permissions.deny entry for the raw
"herdr pane send-text"/"send-keys"/"run" calls: they return as soon as herdr
accepts the text, whether or not a live agent turn ever reads it, so calling
them directly instead of "argus worker steer"/"answer" gets no delivery
confirmation at all. Read-only pane list/read/get are left alone.

.claude/settings.json is untracked and per-clone, not per-repo — run
"config check --write" once from every clone you supervise from; a worker's
own worktree and a teammate's clone each start with none of this.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigCheck(cmd, &configCheckArgs{repo: repo, entry: entry, write: write})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose .claude/settings.json to check")
	cmd.Flags().StringVar(&entry, "entry", permission.DefaultAllowEntry, "allow entry to add with --write, e.g. a narrower \"Bash(argus ship *)\"")
	cmd.Flags().BoolVar(&write, "write", false, "add the entry to .claude/settings.json if none already covers argus")
	return cmd
}

type configCheckArgs struct {
	repo, entry string
	write       bool
}

func runConfigCheck(cmd *cobra.Command, a *configCheckArgs) error {
	path := permission.SettingsPath(a.repo)
	out := cmd.OutOrStdout()

	allowErr := checkOrWriteAllow(out, path, a)
	denyErr := checkOrWriteDeny(out, path, a.repo, a.write)
	if allowErr != nil {
		return allowErr
	}
	return denyErr
}

func checkOrWriteAllow(out io.Writer, path string, a *configCheckArgs) error {
	covered, matches, err := permission.Check(path)
	if err != nil {
		return err
	}
	if covered {
		_, _ = fmt.Fprintf(out, "%s argus is allowlisted in %s (%s)\n",
			ui.LabelSuccess.Render("✓"), path, strings.Join(matches, ", "))
		warnIfCoversShipForce(out, matches)
		return nil
	}

	if !a.write {
		_, _ = fmt.Fprintf(out, "%s no Bash permission allowlist entry for argus in %s\n", ui.LabelWarning.Render("!"), path)
		_, _ = fmt.Fprintf(out, "  add this to fix it:\n  {\n    \"permissions\": {\n      \"allow\": [\"%s\"]\n    }\n  }\n", a.entry)
		warnIfCoversShipForce(out, []string{a.entry})
		return &ui.UserError{
			Err:  fmt.Errorf("argus is not allowlisted in %s", path),
			Hint: fmt.Sprintf("argus config check --repo %s --write", a.repo),
		}
	}

	if _, err := permission.Ensure(path, a.entry); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s added %s to %s\n", ui.LabelSuccess.Render("✓"), a.entry, path)
	warnIfCoversShipForce(out, []string{a.entry})
	return nil
}

// checkOrWriteDeny reports (and, with write, closes) the gap left by raw
// `herdr pane send-text`/`send-keys`/`run` calls: unlike `argus worker
// steer`/`answer`, herdr accepts that text and returns immediately whether
// or not a live agent turn ever reads it, so a supervising session bypassing
// argus's own dispatch gets no delivery confirmation at all — success and a
// silently missed prompt look identical.
func checkOrWriteDeny(out io.Writer, path, repo string, write bool) error {
	if !write {
		missing, err := permission.CheckDeny(path)
		if err != nil {
			return err
		}
		if len(missing) == 0 {
			_, _ = fmt.Fprintf(out, "%s raw herdr pane-mutation calls are denied in %s\n", ui.LabelSuccess.Render("✓"), path)
			return nil
		}
		_, _ = fmt.Fprintf(out, "%s raw herdr pane-mutation calls are not denied in %s: %s\n",
			ui.LabelWarning.Render("!"), path, strings.Join(missing, ", "))
		_, _ = fmt.Fprintf(out, "  add this to fix it (argus worker steer/answer cover the same need with delivery confirmation):\n  {\n    \"permissions\": {\n      \"deny\": %s\n    }\n  }\n",
			jsonStringArray(missing))
		return &ui.UserError{
			Err:  fmt.Errorf("raw herdr pane mutation is not denied in %s", path),
			Hint: fmt.Sprintf("argus config check --repo %s --write", repo),
		}
	}

	added, err := permission.EnsureDeny(path)
	if err != nil {
		return err
	}
	if len(added) == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s added %s to %s (raw herdr pane mutation bypasses argus's delivery-confirmed dispatch; use `argus worker steer`/`answer` instead — revert this if you have a real reason to keep raw access)\n",
		ui.LabelSuccess.Render("✓"), strings.Join(added, ", "), path)
	_, _ = fmt.Fprintf(out, "  %s scopes the orchestrating session's own Bash tool only — a worker's Bash permissions come from its own per-worktree settings.local.json and never read this file\n",
		path)
	return nil
}

func jsonStringArray(entries []string) string {
	quoted := make([]string, len(entries))
	for i, e := range entries {
		quoted[i] = fmt.Sprintf("%q", e)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// warnIfCoversShipForce flags any entry broad enough to also authorize
// `argus ship --force` without a prompt: the harness's own Bash-tool gate is
// the only thing standing between a compromised context (e.g. prompt
// injection) and a --force ship that skips argus's approval gate outright.
// Bash allow-glob can't exclude a single flag from a scoped prefix, so the
// only fix is scoping the entry away from ship entirely.
func warnIfCoversShipForce(out io.Writer, entries []string) {
	for _, entry := range entries {
		if permission.CoversShipForce(entry) {
			_, _ = fmt.Fprintf(out, "%s %s also authorizes `argus ship --force` without a prompt — scope it away from ship (e.g. \"Bash(argus supervise *)\") if --force should always need explicit say-so\n",
				ui.LabelWarning.Render("!"), entry)
			return
		}
	}
}
