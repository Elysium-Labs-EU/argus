// Package scripts_test exercises install.sh's install-directory selection,
// the piece of the installer the "sudo mv ... /usr/local/bin" bug lived in.
// It shells out to the real script (via --print-install-dir, which is
// side-effect free) rather than reimplementing the logic in Go.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func printInstallDir(t *testing.T, env map[string]string) string {
	t.Helper()

	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolving install.sh path: %v", err)
	}

	cmd := exec.Command("bash", scriptPath, "--print-install-dir")
	cmd.Env = append(os.Environ(), "ARGUS_INSTALL_DIR=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh --print-install-dir failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSelectInstallDir(t *testing.T) {
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "ARGUS_INSTALL_DIR override wins even when .local/bin is on PATH",
			env: map[string]string{
				"HOME":              home,
				"PATH":              localBin + ":/usr/bin",
				"ARGUS_INSTALL_DIR": filepath.Join(home, "custom"),
			},
			want: filepath.Join(home, "custom"),
		},
		{
			name: "falls back to ~/.local/bin when it is on PATH",
			env: map[string]string{
				"HOME": home,
				"PATH": localBin + ":/usr/bin",
			},
			want: localBin,
		},
		{
			name: "falls back to /usr/local/bin when .local/bin is not on PATH",
			env: map[string]string{
				"HOME": home,
				"PATH": "/usr/bin:/bin",
			},
			want: "/usr/local/bin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := printInstallDir(t, tt.env); got != tt.want {
				t.Errorf("install dir = %q, want %q", got, tt.want)
			}
		})
	}
}
