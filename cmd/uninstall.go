package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// argusDataDir returns ~/.argus, which holds the run logs under
// ~/.argus/runs (see internal/eventlog). argus has no daemon or systemd
// unit to stop, unlike eos/theia — it's a CLI invoked directly.
func argusDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".argus"), nil
}

// runUninstall implements `argus system uninstall` against explicit paths so
// it can be exercised in tests without touching the real installed binary or
// os.Executable() (which, under `go test`, is the test binary itself).
func runUninstall(_ context.Context, in io.Reader, out io.Writer, exePath, dataDir string, yes, purge bool) error {
	if !yes && !ui.Confirm(in, out, fmt.Sprintf("Remove argus (%s)?", exePath), false) {
		_, _ = fmt.Fprintln(out, "Canceled.")
		return nil
	}

	if err := os.Remove(exePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", exePath, err)
	}
	_, _ = fmt.Fprintf(out, "%s removed %s\n", ui.LabelSuccess.Render("✓"), exePath)

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		return nil
	}

	removeData := purge
	if !removeData && !yes {
		removeData = ui.Confirm(in, out, fmt.Sprintf("Also remove argus data (%s)?", dataDir), false)
	}

	if removeData {
		if err := os.RemoveAll(dataDir); err != nil {
			return fmt.Errorf("removing %s: %w", dataDir, err)
		}
		_, _ = fmt.Fprintf(out, "%s removed %s\n", ui.LabelSuccess.Render("✓"), dataDir)
	} else {
		_, _ = fmt.Fprintf(out, "%s data left in place — remove manually: %s\n",
			ui.TextMuted.Render("i"), ui.TextCommand.Render("rm -rf "+dataDir))
	}
	return nil
}

func newUninstallCmd() *cobra.Command {
	var yes bool
	var purge bool

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the argus binary",
		Long: `Remove the argus binary.

By default ~/.argus (run logs under ~/.argus/runs) is left in place and a
manual cleanup hint is printed. Pass --purge to remove it too.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exePath, err := currentBinaryPath()
			if err != nil {
				return err
			}
			dataDir, err := argusDataDir()
			if err != nil {
				return fmt.Errorf("resolving argus data dir: %w", err)
			}
			return runUninstall(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), exePath, dataDir, yes, purge)
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the binary-removal confirmation prompt")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove ~/.argus without prompting")
	return cmd
}
