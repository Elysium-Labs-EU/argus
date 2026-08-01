package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectShell_ReturnsBasename(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := detectShell(); got != "zsh" {
		t.Errorf("got %q, want %q", got, "zsh")
	}
}

func TestDetectShell_EmptyEnv(t *testing.T) {
	t.Setenv("SHELL", "")
	if got := detectShell(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestDetectShell_Bash(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/bash")
	if got := detectShell(); got != "bash" {
		t.Errorf("got %q, want %q", got, "bash")
	}
}

func TestCompletionTargetPath_Zsh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := completionTargetPath("zsh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, ".zsh", "completions", "_argus")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompletionTargetPath_Bash(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := completionTargetPath("bash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "bash-completion", "completions", "argus")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompletionTargetPath_Fish(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := completionTargetPath("fish")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, ".config", "fish", "completions", "argus.fish")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompletionTargetPath_Unsupported(t *testing.T) {
	if _, err := completionTargetPath("tcsh"); err == nil {
		t.Error("expected error for unsupported shell")
	}
}

func TestWriteCompletionScript_Zsh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completions", "_argus")

	if err := writeCompletionScript(rootCmd, "zsh", path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), "#compdef") {
		t.Errorf("zsh script missing #compdef header")
	}
}

func TestWriteCompletionScript_Bash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completions", "argus")

	if err := writeCompletionScript(rootCmd, "bash", path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if len(data) == 0 {
		t.Error("bash completion script is empty")
	}
}

func TestWriteCompletionScript_Fish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "completions", "argus.fish")

	if err := writeCompletionScript(rootCmd, "fish", path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), "complete") {
		t.Errorf("fish script missing 'complete'")
	}
}

func TestWriteCompletionScript_CreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "dir", "_argus")

	if err := writeCompletionScript(rootCmd, "zsh", path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteCompletionScript_UnsupportedShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argus")

	if err := writeCompletionScript(rootCmd, "tcsh", path); err == nil {
		t.Error("expected error for unsupported shell")
	}
}

func TestCompletionZshCmd_PrintsToStdout(t *testing.T) {
	cmd := newCompletionCmd(rootCmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"zsh"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "#compdef") {
		t.Errorf("expected #compdef in stdout, got: %s", out.String())
	}
}

func TestCompletionBashCmd_PrintsToStdout(t *testing.T) {
	cmd := newCompletionCmd(rootCmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"bash"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Len() == 0 {
		t.Error("expected bash completion script on stdout, got nothing")
	}
}

func TestCompletionFishCmd_PrintsToStdout(t *testing.T) {
	cmd := newCompletionCmd(rootCmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"fish"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "complete") {
		t.Errorf("expected 'complete' in fish output, got: %s", out.String())
	}
}

// These two drive rootCmd (not a standalone newCompletionCmd) because cobra's
// default Args validation (legacyArgs) only defers to a command's own RunE
// for unmatched positional args when the command has a parent; a parentless
// completion command would be rejected by cobra itself ("unknown command"),
// never reaching our own unsupported-shell error.
func TestCompletionCmd_UnsupportedShellArg(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"completion", "powershell"})

	err := rootCmd.Execute()
	want := `unsupported shell "powershell"; supported shells: bash, zsh, fish`
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v, want %q", err, want)
	}
}

func TestCompletionCmd_UnsupportedExtraArgs(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"completion", "foo", "bar", "baz"})

	err := rootCmd.Execute()
	want := `unsupported shell "foo"; supported shells: bash, zsh, fish`
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v, want %q", err, want)
	}
}

func TestCompletionInteractive_NoShell(t *testing.T) {
	t.Setenv("SHELL", "")
	cmd := newCompletionCmd(rootCmd)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "could not detect shell") {
		t.Errorf("expected shell detection hint in stderr, got: %s", errBuf.String())
	}
}

func TestCompletionInteractive_UnsupportedShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/tcsh")
	cmd := newCompletionCmd(rootCmd)
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errBuf.String(), "not supported") {
		t.Errorf("expected unsupported shell message, got: %s", errBuf.String())
	}
}

func TestCompletionInteractive_Decline(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := newCompletionCmd(rootCmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPath := filepath.Join(home, ".zsh", "completions", "_argus")
	if _, err := os.Stat(targetPath); err == nil {
		t.Error("completion file should not be written on decline")
	}
	if !strings.Contains(out.String(), "Skipped") {
		t.Errorf("expected 'Skipped' in output, got: %s", out.String())
	}
}

func TestCompletionInteractive_ConfirmZsh(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	home := t.TempDir()
	t.Setenv("HOME", home)

	cmd := newCompletionCmd(rootCmd)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	targetPath := filepath.Join(home, ".zsh", "completions", "_argus")
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("completion file not written: %v", err)
	}
	if !strings.Contains(string(data), "#compdef") {
		t.Errorf("written file missing #compdef header")
	}

	outStr := out.String()
	if !strings.Contains(outStr, "installed") {
		t.Errorf("expected 'installed' in output, got: %s", outStr)
	}

	zshrc, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatalf("~/.zshrc not written: %v", err)
	}
	if !strings.Contains(string(zshrc), ".zsh/completions") {
		t.Errorf("~/.zshrc missing fpath entry, got: %s", string(zshrc))
	}
	if !strings.Contains(string(zshrc), "compinit") {
		t.Errorf("~/.zshrc missing compinit, got: %s", string(zshrc))
	}
	if !strings.Contains(outStr, "patched") {
		t.Errorf("expected 'patched' in output, got: %s", outStr)
	}
}

func TestPatchZshrc_WritesWhenMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	patched, err := patchZshrc("/home/user/.zsh/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patched {
		t.Error("expected patched=true for new .zshrc")
	}

	data, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatalf(".zshrc not written: %v", err)
	}
	if !strings.Contains(string(data), "fpath=(/home/user/.zsh/completions $fpath)") {
		t.Errorf("fpath line missing, got: %s", string(data))
	}
	if !strings.Contains(string(data), "compinit") {
		t.Errorf("compinit missing, got: %s", string(data))
	}
}

func TestPatchZshrc_SkipsIfAlreadyPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	zshrc := filepath.Join(home, ".zshrc")
	existing := "fpath=(/home/user/.zsh/completions $fpath)\nautoload -Uz compinit && compinit\n"
	if err := os.WriteFile(zshrc, []byte(existing), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	patched, err := patchZshrc("/home/user/.zsh/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patched {
		t.Error("expected patched=false when entry already present")
	}

	data, _ := os.ReadFile(zshrc)
	if string(data) != existing {
		t.Errorf("file was modified when it should not have been")
	}
}

func TestPatchZshrc_AppendsToExistingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	zshrc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(zshrc, []byte("export PATH=$PATH:/usr/local/bin\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	patched, err := patchZshrc("/home/user/.zsh/completions")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patched {
		t.Error("expected patched=true")
	}

	data, _ := os.ReadFile(zshrc)
	content := string(data)
	if !strings.Contains(content, "export PATH") {
		t.Error("existing content was lost")
	}
	if !strings.Contains(content, "fpath=(/home/user/.zsh/completions $fpath)") {
		t.Errorf("fpath line not appended, got: %s", content)
	}
}

func writeFakeCompletionBinary(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-argus")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

func TestRefreshInstalledCompletions_SkipsNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var out bytes.Buffer
	fakeBinary := writeFakeCompletionBinary(t, t.TempDir(), `echo "NEWSCRIPT"`)

	refreshInstalledCompletions(t.Context(), &out, fakeBinary)

	if out.Len() != 0 {
		t.Errorf("expected no output when no shells have completion installed, got: %s", out.String())
	}
}

func TestRefreshInstalledCompletions_RefreshesInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	zshPath, err := completionTargetPath("zsh")
	if err != nil {
		t.Fatalf("resolving zsh target path: %v", err)
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(zshPath), 0o750); mkdirErr != nil {
		t.Fatalf("preparing zsh completion dir: %v", mkdirErr)
	}
	if writeErr := os.WriteFile(zshPath, []byte("#compdef old\n"), 0o600); writeErr != nil {
		t.Fatalf("seeding old zsh completion: %v", writeErr)
	}

	var out bytes.Buffer
	fakeBinary := writeFakeCompletionBinary(t, t.TempDir(), `echo "NEWSCRIPT"`)

	refreshInstalledCompletions(t.Context(), &out, fakeBinary)

	data, err := os.ReadFile(zshPath)
	if err != nil {
		t.Fatalf("reading refreshed zsh completion: %v", err)
	}
	if !strings.Contains(string(data), "NEWSCRIPT") {
		t.Errorf("expected refreshed zsh completion content, got: %s", string(data))
	}
	if !strings.Contains(out.String(), "refreshed zsh completion") {
		t.Errorf("expected refresh confirmation message, got: %s", out.String())
	}

	for _, shell := range []string{"bash", "fish"} {
		p, pathErr := completionTargetPath(shell)
		if pathErr != nil {
			t.Fatalf("resolving %s target path: %v", shell, pathErr)
		}
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("did not expect %s completion to be created, it was never installed", shell)
		}
	}
}

func TestRefreshInstalledCompletions_WarnsOnExecFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	zshPath, err := completionTargetPath("zsh")
	if err != nil {
		t.Fatalf("resolving zsh target path: %v", err)
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(zshPath), 0o750); mkdirErr != nil {
		t.Fatalf("preparing zsh completion dir: %v", mkdirErr)
	}
	if writeErr := os.WriteFile(zshPath, []byte("#compdef old\n"), 0o600); writeErr != nil {
		t.Fatalf("seeding old zsh completion: %v", writeErr)
	}

	var out bytes.Buffer
	fakeBinary := writeFakeCompletionBinary(t, t.TempDir(), `exit 1`)

	refreshInstalledCompletions(t.Context(), &out, fakeBinary)

	if !strings.Contains(out.String(), "could not refresh zsh completion") {
		t.Errorf("expected warning about failed refresh, got: %s", out.String())
	}

	data, err := os.ReadFile(zshPath)
	if err != nil {
		t.Fatalf("reading zsh completion: %v", err)
	}
	if !strings.Contains(string(data), "#compdef old") {
		t.Errorf("expected old completion content to remain untouched, got: %s", string(data))
	}
}
