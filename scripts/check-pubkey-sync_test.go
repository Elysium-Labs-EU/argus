// check-pubkey-sync_test.go exercises the CI gate that fails the build if
// the release-signing public key embedded in cmd/update.go and
// scripts/install.sh ever drifts apart — the mistake that silently broke
// signature verification in eos and themis after a key rotation touched
// only one of the two copies.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runPubkeySyncGate(t *testing.T, target string) (out string, err error) {
	t.Helper()
	scriptPath, err := filepath.Abs("check-pubkey-sync.sh")
	if err != nil {
		t.Fatalf("resolving check-pubkey-sync.sh path: %v", err)
	}
	cmd := exec.Command("bash", scriptPath, target)
	raw, runErr := cmd.CombinedOutput()
	return string(raw), runErr
}

const testPubkeyA = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEKixhYiZA8bWtyh5sBs0hLdOhVXj3
zHHnA3f89l/hPJOQljhWQPOWUcVWnxpVkiIfMPfvxuH4CxnRfFL2azqr8A==
-----END PUBLIC KEY-----`

const testPubkeyB = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEByucQHF5ASSSrPSu6Gb5fvAuWdMw
BNAGlV57YMjkCdpcq8HHRXYXHXqy3cvfIzHYE2UHfftsk83lrSXPkxMyZg==
-----END PUBLIC KEY-----`

func writeFixture(t *testing.T, dir, goPEM, shPEM string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	goSrc := "package cmd\n\nconst releaseSigningPublicKeyPEM = `" + goPEM + "\n`\n"
	if err := os.WriteFile(filepath.Join(dir, "cmd", "update.go"), []byte(goSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	shSrc := "#!/bin/sh\nRELEASE_SIGNING_PUBKEY='" + shPEM + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte(shSrc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPubkeySyncGatePassesWhenKeysMatch(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, testPubkeyA, testPubkeyA)

	out, err := runPubkeySyncGate(t, dir)
	if err != nil {
		t.Fatalf("gate should pass when both copies match, got err: %v\n%s", err, out)
	}
}

func TestPubkeySyncGateFailsOnDrift(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, testPubkeyA, testPubkeyB)

	out, err := runPubkeySyncGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when the two embedded pubkeys differ, got output:\n%s", out)
	}
}

func TestPubkeySyncGateFailsOnMissingBlock(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, testPubkeyA, testPubkeyA)
	// Strip the PEM block entirely from the sh copy.
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte("#!/bin/sh\necho no key here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runPubkeySyncGate(t, dir)
	if err == nil {
		t.Fatalf("gate should fail when a PEM block is missing, got output:\n%s", out)
	}
}

func TestPubkeySyncGatePassesOnRealRepo(t *testing.T) {
	out, err := runPubkeySyncGate(t, "..")
	if err != nil {
		t.Fatalf("gate should pass on the real repo, got err: %v\n%s", err, out)
	}
}
