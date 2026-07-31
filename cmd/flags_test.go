package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestBindWorktreeFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var dst string
	bindWorktreeFlag(cmd, &dst, "help text")

	f := cmd.Flags().Lookup("worktree")
	if f == nil {
		t.Fatal("expected --worktree flag to be registered")
	}
	if f.DefValue != "" {
		t.Errorf("default = %q, want empty", f.DefValue)
	}
	if f.Usage != "help text" {
		t.Errorf("usage = %q, want %q", f.Usage, "help text")
	}
	if f.Value.Type() != "string" {
		t.Errorf("type = %q, want string", f.Value.Type())
	}
}

func TestBindBaseFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var dst string
	bindBaseFlag(cmd, &dst, "origin/main", "help text")

	f := cmd.Flags().Lookup("base")
	if f == nil {
		t.Fatal("expected --base flag to be registered")
	}
	if f.DefValue != "origin/main" {
		t.Errorf("default = %q, want origin/main", f.DefValue)
	}
	if f.Usage != "help text" {
		t.Errorf("usage = %q, want %q", f.Usage, "help text")
	}
}

func TestBindBaseFlagDifferentDefaults(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var dst string
	bindBaseFlag(cmd, &dst, "main", "help text")

	f := cmd.Flags().Lookup("base")
	if f == nil {
		t.Fatal("expected --base flag to be registered")
	}
	if f.DefValue != "main" {
		t.Errorf("default = %q, want main", f.DefValue)
	}
}

func TestBindRepoFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var dst string
	bindRepoFlag(cmd, &dst, "help text")

	f := cmd.Flags().Lookup("repo")
	if f == nil {
		t.Fatal("expected --repo flag to be registered")
	}
	if f.DefValue != "" {
		t.Errorf("default = %q, want empty", f.DefValue)
	}
	if f.Usage != "help text" {
		t.Errorf("usage = %q, want %q", f.Usage, "help text")
	}
}

func TestBindVerifyCmdFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var dst string
	bindVerifyCmdFlag(cmd, &dst, "help text")

	f := cmd.Flags().Lookup("verify-cmd")
	if f == nil {
		t.Fatal("expected --verify-cmd flag to be registered")
	}
	if f.DefValue != "" {
		t.Errorf("default = %q, want empty", f.DefValue)
	}
	if f.Usage != "help text" {
		t.Errorf("usage = %q, want %q", f.Usage, "help text")
	}
}
