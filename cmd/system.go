package cmd

import (
	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the argus version",
		Long:  `Print the current argus version, git commit hash, and build date.`,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println(buildinfo.Get())
		},
	}
}

func newSystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Manage the argus binary and check its version",
		Long:  `Manage the argus binary: check for updates, remove it, or print its version.`,
	}

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newUninstallCmd())

	return cmd
}

var systemCmd = newSystemCmd()
