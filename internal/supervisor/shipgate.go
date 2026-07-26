package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// shipGateTimeout bounds one hook-manager or ship_lint invocation, so a hung
// lint/build command escalates ship instead of hanging it forever — the same
// role testVerifyTimeout plays for a worker's own re-run test command.
const shipGateTimeout = 5 * time.Minute

// hookManager is one repo-local hook/lint tool EnforceHooks can run directly,
// independent of whether it happens to be wired into .git/hooks: lefthook and
// the Python pre-commit framework both require an explicit `<tool> install`
// to register themselves there, and a fresh worktree checkout has no reason
// to have ever run that — a hook that silently never ran is indistinguishable
// from one that ran and passed.
type hookManager struct {
	name        string
	binary      string
	args        []string
	configFiles []string
}

var hookManagers = []hookManager{
	{
		name:        "lefthook",
		binary:      "lefthook",
		args:        []string{"run", "pre-commit"},
		configFiles: []string{"lefthook.yml", "lefthook.yaml", "lefthook.toml", "lefthook.json"},
	},
	{
		name:        "pre-commit",
		binary:      "pre-commit",
		args:        []string{"run", "--all-files"},
		configFiles: []string{".pre-commit-config.yaml", ".pre-commit-config.yml"},
	},
}

// EnforceHooks runs every hook manager it finds configured in worktree,
// failing ship loud on a non-zero exit rather than letting the caller
// discover the failure at CI. A manager whose config file is present but
// whose binary is missing from PATH also fails loud: silently skipping it
// would recreate exactly the invisible bypass this closes, just one step
// removed from a human typing --no-verify.
//
// Native .git/hooks and husky (which wires itself in via core.hooksPath) need
// no entry here: both dispatch off git's own pre-commit/pre-push machinery,
// which CommitAll and Push already invoke — neither ever passes --no-verify
// or -n, so those hooks already run on every ship.
func EnforceHooks(ctx context.Context, worktree string) error {
	for _, m := range hookManagers {
		matched := firstExisting(worktree, m.configFiles)
		if matched == "" {
			continue
		}
		if _, err := exec.LookPath(m.binary); err != nil {
			return fmt.Errorf("%s is configured (%s present) but %q is not on PATH: install it, or set ship_lint in .argus/config.yml instead", m.name, matched, m.binary)
		}
		if err := runGateCommand(ctx, worktree, m.binary, m.args...); err != nil {
			return fmt.Errorf("%s hook failed: %w", m.name, err)
		}
	}
	return nil
}

// RunShipLint runs a repo's optional ship_lint command (.argus/config.yml)
// controller-side, before commit. Unlike EnforceHooks this does not depend on
// the repo having any hook manager wired up at all — the intended use is
// running the same pinned command CI runs (e.g. "make ci"), so a local
// toolchain/version mismatch that a hook might paper over still fails ship
// instead of only showing up after the PR is already open. An empty command
// (the default: no ship_lint key) is a no-op.
func RunShipLint(ctx context.Context, worktree, command string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	if err := runGateCommand(ctx, worktree, "sh", "-c", command); err != nil {
		return fmt.Errorf("ship_lint %q failed: %w", command, err)
	}
	return nil
}

// runGateCommand runs one hook/lint command inside worktree, under
// shipGateTimeout, folding a failure's output (tailed, matching
// testverify.go's own cap) into the returned error so the operator sees why
// ship refused rather than just an exit status.
func runGateCommand(ctx context.Context, worktree, name string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, shipGateTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...) //nolint:gosec // name/args are argus-selected (hookManagers) or an operator's own repo-config command, not attacker input
	cmd.Dir = worktree
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("exceeded %s and was killed", shipGateTimeout)
	}
	return fmt.Errorf("%w\n%s", err, tail(buf.Bytes(), maxCapturedOutput))
}

// firstExisting returns the first of names present in dir, or "" if none are.
func firstExisting(dir string, names []string) string {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	return ""
}
