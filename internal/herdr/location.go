package herdr

import "os"

// Location is the caller's own herdr coordinate: which workspace, tab, and
// pane the current process is running inside of. It is distinct from a
// worker pane's location (returned by herdr at spawn, via Pane) — this is
// always the invoking orchestrator's own position, resolved from its
// environment.
type Location struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// CurrentLocation reads HERDR_WORKSPACE_ID, HERDR_TAB_ID, and HERDR_PANE_ID
// once and returns them as a Location. herdr sets all three in every pane's
// environment, but a caller running outside herdr (a plain terminal, CI) has
// none of them — fields are "" in that case, and it is up to each caller to
// decide whether an empty field is fatal.
func CurrentLocation() Location {
	return Location{
		WorkspaceID: os.Getenv("HERDR_WORKSPACE_ID"),
		TabID:       os.Getenv("HERDR_TAB_ID"),
		PaneID:      os.Getenv("HERDR_PANE_ID"),
	}
}
