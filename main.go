package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Elysium-Labs-EU/argus/cmd"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func main() {
	if err := cmd.Execute(); err != nil {
		var uerr *ui.UserError
		if errors.As(err, &uerr) {
			fmt.Fprintf(os.Stderr, "%s\n\n", uerr.Render())
		} else {
			fmt.Fprintf(os.Stderr, "%s %s\n\n", ui.LabelError.Render("error"), err)
		}
		os.Exit(1)
	}
}
