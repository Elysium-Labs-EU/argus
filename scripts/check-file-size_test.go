// check-file-size_test.go exercises the CI gate that fails the build if a
// diff adds or modifies a file above the size ceiling without routing it
// through Git LFS, so an accidental large binary can't quietly bloat every
// future clone of the repo's history.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoAtBase creates a repo with one commit on main, then checks out a
// "work" branch so main stays pinned at the base commit while the test adds
// further commits the gate can diff against it.
func initRepoAtBase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "base.txt")
	runGit(t, dir, "commit", "-q", "-m", "base")
	runGit(t, dir, "checkout", "-q", "-b", "work")
	return dir
}

func runFileSizeGate(t *testing.T, dir, base, maxBytes string) (string, error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-file-size.sh")
	if err != nil {
		t.Fatalf("resolving check-file-size.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath, dir)
	cmd.Env = append(os.Environ(),
		"CHECK_FILE_SIZE_BASE="+base,
		"CHECK_FILE_SIZE_MAX_BYTES="+maxBytes,
	)
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

func TestFileSizeGatePassesUnderThreshold(t *testing.T) {
	dir := initRepoAtBase(t)
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte(strings.Repeat("a", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "small.txt")
	runGit(t, dir, "commit", "-q", "-m", "add small file")

	out, err := runFileSizeGate(t, dir, "main", "1024")
	if err != nil {
		t.Fatalf("gate should pass, got err: %v\n%s", err, out)
	}
}

func TestFileSizeGateFailsOverThreshold(t *testing.T) {
	dir := initRepoAtBase(t)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(strings.Repeat("a", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "big.bin")
	runGit(t, dir, "commit", "-q", "-m", "add big file")

	out, err := runFileSizeGate(t, dir, "main", "1024")
	if err == nil {
		t.Fatalf("gate should fail on an oversized file, got output:\n%s", out)
	}
}

func TestFileSizeGateExemptsLFSTrackedFile(t *testing.T) {
	dir := initRepoAtBase(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("big.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(strings.Repeat("a", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".gitattributes", "big.bin")
	runGit(t, dir, "commit", "-q", "-m", "add LFS-tracked big file")

	out, err := runFileSizeGate(t, dir, "main", "1024")
	if err != nil {
		t.Fatalf("gate should exempt an LFS-tracked file, got err: %v\n%s", err, out)
	}
}

func TestFileSizeGatePassesOnRealRepo(t *testing.T) {
	out, err := runFileSizeGate(t, "..", "origin/main", "1048576")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
