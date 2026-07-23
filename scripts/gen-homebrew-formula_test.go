// Package scripts_test exercises gen-homebrew-formula.sh, which renders
// Formula/argus.rb for the (not yet created) homebrew-tap repo from a
// release's version tag and sha256sums.txt. It shells out to the real
// script rather than reimplementing the rendering in Go.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	darwinArm64Sha = "1111111111111111111111111111111111111111111111111111111111111111"
	darwinAmd64Sha = "2222222222222222222222222222222222222222222222222222222222222222"
	linuxArm64Sha  = "3333333333333333333333333333333333333333333333333333333333333333"
	linuxAmd64Sha  = "4444444444444444444444444444444444444444444444444444444444444444"
)

func writeSums(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sha256sums.txt")
	content := strings.Join([]string{
		darwinArm64Sha + "  argus-darwin-arm64",
		darwinAmd64Sha + "  argus-darwin-amd64",
		linuxArm64Sha + "  argus-linux-arm64",
		linuxAmd64Sha + "  argus-linux-amd64",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing sha256sums.txt: %v", err)
	}
	return path
}

func genFormula(t *testing.T, args ...string) (string, error) {
	t.Helper()

	scriptPath, err := filepath.Abs("gen-homebrew-formula.sh")
	if err != nil {
		t.Fatalf("resolving gen-homebrew-formula.sh path: %v", err)
	}

	cmd := exec.Command("sh", append([]string{scriptPath}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGenFormulaRendersAllPlatforms(t *testing.T) {
	sumsPath := writeSums(t)

	out, err := genFormula(t, "v1.2.3", sumsPath)
	if err != nil {
		t.Fatalf("gen-homebrew-formula.sh failed: %v\n%s", err, out)
	}

	for _, want := range []string{
		"class Argus < Formula",
		`version "1.2.3"`,
		`url "https://github.com/Elysium-Labs-EU/argus/releases/download/v1.2.3/argus-darwin-arm64"`,
		`sha256 "` + darwinArm64Sha + `"`,
		`url "https://github.com/Elysium-Labs-EU/argus/releases/download/v1.2.3/argus-darwin-amd64"`,
		`sha256 "` + darwinAmd64Sha + `"`,
		`url "https://github.com/Elysium-Labs-EU/argus/releases/download/v1.2.3/argus-linux-arm64"`,
		`sha256 "` + linuxArm64Sha + `"`,
		`url "https://github.com/Elysium-Labs-EU/argus/releases/download/v1.2.3/argus-linux-amd64"`,
		`sha256 "` + linuxAmd64Sha + `"`,
		`bin.install Dir["argus-*"].first => "argus"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formula output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestGenFormulaStripsLeadingVFromVersion(t *testing.T) {
	out, err := genFormula(t, "v9.9.9", writeSums(t))
	if err != nil {
		t.Fatalf("gen-homebrew-formula.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `version "9.9.9"`) {
		t.Errorf("formula version not stripped of leading v, got:\n%s", out)
	}
	if !strings.Contains(out, "download/v9.9.9/") {
		t.Errorf("formula download URL should keep the vX.Y.Z tag form, got:\n%s", out)
	}
}

func TestGenFormulaReadsSumsFromStdin(t *testing.T) {
	sumsPath := writeSums(t)
	content, err := os.ReadFile(sumsPath)
	if err != nil {
		t.Fatalf("reading fixture sums: %v", err)
	}

	scriptPath, err := filepath.Abs("gen-homebrew-formula.sh")
	if err != nil {
		t.Fatalf("resolving gen-homebrew-formula.sh path: %v", err)
	}

	cmd := exec.Command("sh", scriptPath, "v1.0.0", "-")
	cmd.Stdin = strings.NewReader(string(content))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gen-homebrew-formula.sh - failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `sha256 "`+linuxAmd64Sha+`"`) {
		t.Errorf("formula from stdin missing expected sha256, got:\n%s", out)
	}
}

func TestGenFormulaMissingAssetFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sha256sums.txt")
	if err := os.WriteFile(path, []byte(darwinArm64Sha+"  argus-darwin-arm64\n"), 0o644); err != nil {
		t.Fatalf("writing partial sums: %v", err)
	}

	out, err := genFormula(t, "v1.2.3", path)
	if err == nil {
		t.Fatalf("expected failure for missing asset sha256, got output:\n%s", out)
	}
}

func TestGenFormulaMissingFileFails(t *testing.T) {
	out, err := genFormula(t, "v1.2.3", filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err == nil {
		t.Fatalf("expected failure for missing sums file, got output:\n%s", out)
	}
}

func TestGenFormulaWrongArgCountFails(t *testing.T) {
	out, err := genFormula(t, "v1.2.3")
	if err == nil {
		t.Fatalf("expected failure for missing sums-file arg, got output:\n%s", out)
	}
}
