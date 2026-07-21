// Package scripts_test exercises install.sh's install-directory selection,
// the piece of the installer the "sudo mv ... /usr/local/bin" bug lived in.
// It shells out to the real script (via --print-install-dir, which is
// side-effect free) rather than reimplementing the logic in Go.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestStripQuarantine exercises the fix for issue #47: a binary carrying the
// macOS "com.apple.quarantine" xattr gets SIGKILLed by Gatekeeper on first
// run. On Darwin it verifies the attribute is actually removed; on other
// platforms it verifies the same invocation is a safe no-op (so CI, which
// runs on ubuntu-latest, still exercises the code path without erroring).
func TestStripQuarantine(t *testing.T) {
	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolving install.sh path: %v", err)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "argus-binary")
	if err := os.WriteFile(target, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}

	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("xattr"); err != nil {
			t.Skip("xattr not available")
		}
		if out, err := exec.Command("xattr", "-w", "com.apple.quarantine", "0081;00000000;Safari;", target).CombinedOutput(); err != nil {
			t.Fatalf("setting quarantine attribute: %v\n%s", err, out)
		}
	}

	cmd := exec.Command("bash", scriptPath, "--strip-quarantine", target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh --strip-quarantine failed: %v\n%s", err, out)
	}

	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("xattr", "-p", "com.apple.quarantine", target).CombinedOutput(); err == nil {
			t.Fatalf("quarantine attribute still present after strip: %s", out)
		}
	}
}

func TestStripQuarantineMissingArg(t *testing.T) {
	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolving install.sh path: %v", err)
	}

	cmd := exec.Command("bash", scriptPath, "--strip-quarantine")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("expected install.sh --strip-quarantine with no path to fail, got: %s", out)
	}
}
