package herdr

import "testing"

func TestCurrentLocation(t *testing.T) {
	tests := []struct {
		name        string
		workspaceID string
		tabID       string
		paneID      string
		want        Location
	}{
		{
			name: "unset",
			want: Location{},
		},
		{
			name:        "fully set",
			workspaceID: "w1",
			tabID:       "t1",
			paneID:      "p1",
			want:        Location{WorkspaceID: "w1", TabID: "t1", PaneID: "p1"},
		},
		{
			name:        "partially set",
			workspaceID: "w1",
			want:        Location{WorkspaceID: "w1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HERDR_WORKSPACE_ID", tt.workspaceID)
			t.Setenv("HERDR_TAB_ID", tt.tabID)
			t.Setenv("HERDR_PANE_ID", tt.paneID)

			if got := CurrentLocation(); got != tt.want {
				t.Errorf("CurrentLocation() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
