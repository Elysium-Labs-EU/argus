package cmd

import (
	"fmt"
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
		Short: "Check (and optionally fix) the Bash allowlist entry argus itself needs",
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
agent's responsibility regardless of what's allowlisted here.`,
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

	covered, matches, err := permission.Check(path)
	if err != nil {
		return err
	}
	if covered {
		_, _ = fmt.Fprintf(out, "%s argus is allowlisted in %s (%s)\n",
			ui.LabelSuccess.Render("✓"), path, strings.Join(matches, ", "))
		return nil
	}

	if !a.write {
		_, _ = fmt.Fprintf(out, "%s no Bash permission allowlist entry for argus in %s\n", ui.LabelWarning.Render("!"), path)
		_, _ = fmt.Fprintf(out, "  add this to fix it:\n  {\n    \"permissions\": {\n      \"allow\": [\"%s\"]\n    }\n  }\n", a.entry)
		return &ui.UserError{
			Err:  fmt.Errorf("argus is not allowlisted in %s", path),
			Hint: fmt.Sprintf("argus config check --repo %s --write", a.repo),
		}
	}

	if _, err := permission.Ensure(path, a.entry); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s added %s to %s\n", ui.LabelSuccess.Render("✓"), a.entry, path)
	return nil
}
