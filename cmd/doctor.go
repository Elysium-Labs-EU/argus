package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/permission"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// doctorCmd is a package-level var (like configCheckCmd) so skill_lint_test.go
// can cross-check its flags against SKILL.md the same way.
var doctorCmd = newDoctorCmd()

func newDoctorCmd() *cobra.Command {
	var repo string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check argus's prerequisites and print a pass/fail checklist",
		Long: `Doctor is the onboarding pre-flight: it walks the prerequisites a first argus
run depends on and prints one pass/fail line per check, each failure carrying
the exact command or step that fixes it.

Two checks are hard prerequisites — herdr and the worker launcher (claude) on
PATH — because nothing runs without them; a hard failure exits non-zero. The
rest are soft: a forge token (needed for ship and supervise --issues, not a
basic run), the Bash allowlist entry argus needs to run without a per-call
approval prompt, and this repo's .argus/config.yml. A soft failure prints a
warning and its fix hint but does not change the exit code, so doctor stays
green on a box that can supervise locally without ever opening a PR.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, &doctorArgs{repo: repo})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", ".", "repo whose prerequisites to check")
	return cmd
}

// doctorArgs holds newDoctorCmd's flag values plus the ambient boundaries
// doctor touches, so runDoctor is testable without a real herdr/claude binary
// or a git remote on the box. A nil boundary field falls back to the real
// implementation in withDefaults; tests set them to drive each pass/fail path.
type doctorArgs struct {
	lookPath     func(string) (string, error)
	resolveRepo  func(ctx context.Context, worktree string) (host, owner, name string, err error)
	tokenForHost func(host string) string
	repo         string
}

func (a *doctorArgs) withDefaults() {
	if a.lookPath == nil {
		a.lookPath = exec.LookPath
	}
	if a.resolveRepo == nil {
		// The same remote-to-forge resolution ship uses, with no --repo
		// override: doctor only wants the host to resolve a token for.
		a.resolveRepo = func(ctx context.Context, worktree string) (string, string, string, error) {
			return resolveRepo(ctx, "", worktree)
		}
	}
	if a.tokenForHost == nil {
		// Reuse ship/supervise's own credential resolution: operator override
		// (argus config set credential.<host>), then env, then the gh/glab/
		// git-credential helper chain forge.TokenForHost already walks.
		a.tokenForHost = func(host string) string {
			overrides, _ := resolveCredentialOverrides(nil)
			return forge.TokenForHost(host, overrides)
		}
	}
}

// checkResult is one line of doctor's checklist: a name, whether it passed,
// whether its failure is hard (exit non-zero) or soft (warn only), an optional
// detail shown after the name, and the fix hint printed on failure.
type checkResult struct {
	name   string
	detail string
	hint   string
	ok     bool
	hard   bool
}

func runDoctor(cmd *cobra.Command, a *doctorArgs) error {
	a.withDefaults()
	out := cmd.OutOrStdout()

	repoRoot, err := supervisor.ResolveWorktree(a.repo)
	if err != nil {
		return err
	}

	results := []checkResult{
		checkBinary(a.lookPath, binHerdr),
		checkBinary(a.lookPath, binClaude),
		checkForgeToken(cmd.Context(), a, repoRoot),
		checkAllowlist(repoRoot),
		checkRepoConfig(repoRoot),
	}

	hardFailures := 0
	for _, r := range results {
		printCheck(out, r)
		if !r.ok && r.hard {
			hardFailures++
		}
	}
	if hardFailures > 0 {
		return fmt.Errorf("%d hard prerequisite check(s) failed", hardFailures)
	}
	return nil
}

// checkBinary reports whether name resolves on PATH, pulling its fix hint from
// installHints — the same source of truth the commands' own upfront presence
// checks use. Both binaries doctor checks this way (herdr, claude) are hard
// prerequisites.
func checkBinary(lookPath func(string) (string, error), name string) checkResult {
	r := checkResult{name: name + " on PATH", hard: true, hint: installHints[name]}
	if path, err := lookPath(name); err == nil {
		r.ok = true
		r.detail = path
	}
	return r
}

// checkForgeToken resolves the repo's git remote to a forge host and reports
// whether a token for that host is resolvable. Soft: a basic supervise run
// needs no token, only ship and supervise --issues do.
func checkForgeToken(ctx context.Context, a *doctorArgs, repoRoot string) checkResult {
	r := checkResult{
		name: "forge token resolvable",
		hint: "set this host's token env var (CODEBERG_TOKEN / GITHUB_TOKEN / GITLAB_TOKEN, ...) or run `gh auth login` / `glab auth login`",
	}
	host, _, _, err := a.resolveRepo(ctx, repoRoot)
	if err != nil {
		r.detail = fmt.Sprintf("no forge remote detected: %v", err)
		return r
	}
	if a.tokenForHost(host) == "" {
		r.detail = "no token for " + host
		return r
	}
	r.ok = true
	r.detail = host
	return r
}

// checkAllowlist reports whether .claude/settings.json already allowlists
// argus, using the exact check `argus config check` performs. Soft: a missing
// entry only means every argus call prompts for manual approval.
func checkAllowlist(repoRoot string) checkResult {
	r := checkResult{
		name: "argus allowlisted in .claude/settings.json",
		hint: "argus config check --write",
	}
	covered, matches, err := permission.Check(permission.SettingsPath(repoRoot))
	if err != nil {
		r.detail = err.Error()
		return r
	}
	if covered {
		r.ok = true
		r.detail = strings.Join(matches, ", ")
	}
	return r
}

// checkRepoConfig reports whether this repo has a .argus/config.yml. Soft:
// every config.yml key is optional, so argus runs on defaults without one.
func checkRepoConfig(repoRoot string) checkResult {
	path := repoconfig.Path(repoRoot)
	r := checkResult{
		name: ".argus/config.yml present",
		hint: "argus init",
	}
	if _, err := os.Stat(path); err == nil {
		r.ok = true
		r.detail = path
	}
	return r
}

// printCheck renders one checklist line with the ui styles init and config
// check use: a success tick, or a hard-fail cross / soft-fail bang followed by
// the fix hint.
func printCheck(out io.Writer, r checkResult) {
	if r.ok {
		_, _ = fmt.Fprintf(out, "%s %s%s\n", ui.LabelSuccess.Render("✓"), r.name, detailSuffix(r.detail))
		return
	}
	mark := ui.LabelWarning.Render("!")
	if r.hard {
		mark = ui.LabelError.Render("✗")
	}
	_, _ = fmt.Fprintf(out, "%s %s%s\n", mark, r.name, detailSuffix(r.detail))
	_, _ = fmt.Fprintf(out, "  %s %s\n", ui.TextMuted.Render("fix:"), ui.TextCommand.Render(r.hint))
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " " + ui.TextMuted.Render("("+detail+")")
}
