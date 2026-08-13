package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/config"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// newConfigCmd is argus's persisted-config surface: today just credential
// name -> env var overrides (see internal/config and internal/credential),
// written only by `config set`, never hand-authored — the config schema's
// discoverability problem goes away when the only writer validates as it
// goes.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage argus's persisted config (~/.argus/config.toml)",
		Long: `Config manages the small operator config file argus falls back to when a
--credential-env flag isn't repeated on every invocation. Today the only
supported key namespace is credential.<name>, mapping a credential name (a
forge host like github.com, or an agent-key name like anthropic) to the
environment variable argus should read it from instead of its own built-in
default.`,
	}
	cmd.AddCommand(newConfigSetCmd())
	cmd.AddCommand(newConfigGetCmd())
	cmd.AddCommand(configCheckCmd)
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a persisted config value",
		Long: `Set writes one key/value pair to ~/.argus/config.toml. The only supported key
namespace today is credential.<name>, e.g.:

  argus config set credential.github.com MY_GH_TOKEN
  argus config set credential.anthropic MY_CLAUDE_KEY

This tells argus to read the named credential from the given environment
variable instead of its built-in default — see --credential-env on supervise/
ship/rebase for the equivalent one-off override.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			if serr := cfg.Set(args[0], args[1]); serr != nil {
				return &ui.UserError{Err: serr}
			}
			if serr := config.Save(path, cfg); serr != nil {
				return serr
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s set %s in %s\n", ui.LabelSuccess.Render("✓"), args[0], path)
			return nil
		},
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <key>",
		Aliases: []string{"show"},
		Short:   "Read back a persisted config value",
		Long: `Get prints the value persisted for <key> in ~/.argus/config.toml — the same
key namespace "argus config set" writes to, e.g.:

  argus config get credential.github.com
  argus config show credential.anthropic

Exits non-zero with a "not set" message if <key> has no persisted value.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			value, found, gerr := cfg.Get(args[0])
			if gerr != nil {
				return &ui.UserError{Err: gerr}
			}
			if !found {
				return &ui.UserError{Err: fmt.Errorf("%s is not set in %s", args[0], path)}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), value)
			return nil
		},
	}
}

var configCmd = newConfigCmd()
