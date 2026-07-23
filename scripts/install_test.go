// Package scripts_test exercises install.sh's install-directory selection,
// the piece of the installer the "sudo mv ... /usr/local/bin" bug lived in.
// It shells out to the real script (via --print-install-dir, which is
// side-effect free) rather than reimplementing the logic in Go.
package scripts_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
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

// runInstallSh runs install.sh with args and returns its combined output and
// error, so signature-verification tests can assert on both exit status and
// the log lines the script prints.
func runInstallSh(t *testing.T, args ...string) (string, error) {
	t.Helper()

	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("resolving install.sh path: %v", err)
	}

	cmd := exec.Command("sh", append([]string{scriptPath}, args...)...)
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// writeTestSigningKeyFixture generates a throwaway ECDSA P-256 keypair,
// writes its PEM-encoded public key to <dir>/pubkey.pem, and returns the
// private key so callers can sign fixture data with it. install.sh's
// signature functions are keyed off an explicit pubkey_file argument (not
// the real embedded RELEASE_SIGNING_PUBKEY), so tests never need the actual
// production private key, which is never checked into this repo.
func writeTestSigningKeyFixture(t *testing.T, dir string) (pubkeyPath string, priv *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test signing key: %v", err)
	}
	derBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshaling test public key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes})

	pubkeyPath = filepath.Join(dir, "pubkey.pem")
	if err := os.WriteFile(pubkeyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("writing pubkey fixture: %v", err)
	}
	return pubkeyPath, key
}

// signFileFixture signs dataPath's contents with priv (as
// `openssl dgst -sha256 -sign` would) and writes the ASN.1 DER signature to
// <dir>/sig.bin, returning its path.
func signFileFixture(t *testing.T, dir string, priv *ecdsa.PrivateKey, dataPath string) string {
	t.Helper()

	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("reading data fixture: %v", err)
	}
	digest := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("signing data fixture: %v", err)
	}

	sigPath := filepath.Join(dir, "sig.bin")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatalf("writing sig fixture: %v", err)
	}
	return sigPath
}

func requireOpenSSL(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available")
	}
}

func TestVerifySignatureFlagAcceptsValidSignature(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(dataPath, []byte("argus release checksums fixture\n"), 0o600); err != nil {
		t.Fatalf("writing data fixture: %v", err)
	}

	pubkeyPath, priv := writeTestSigningKeyFixture(t, dir)
	sigPath := signFileFixture(t, dir, priv, dataPath)

	out, err := runInstallSh(t, "--verify-signature", pubkeyPath, sigPath, dataPath)
	if err != nil {
		t.Fatalf("--verify-signature with a valid signature failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "signature valid") {
		t.Errorf("output = %q, want a signature-valid message", out)
	}
}

func TestVerifySignatureFlagRejectsTamperedData(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(dataPath, []byte("argus release checksums fixture\n"), 0o600); err != nil {
		t.Fatalf("writing data fixture: %v", err)
	}

	pubkeyPath, priv := writeTestSigningKeyFixture(t, dir)
	sigPath := signFileFixture(t, dir, priv, dataPath)

	// Tamper with the data after signing — the signature no longer matches.
	if err := os.WriteFile(dataPath, []byte("tampered contents\n"), 0o600); err != nil {
		t.Fatalf("tampering data fixture: %v", err)
	}

	out, err := runInstallSh(t, "--verify-signature", pubkeyPath, sigPath, dataPath)
	if err == nil {
		t.Fatalf("expected --verify-signature to fail for tampered data, got:\n%s", out)
	}
	if !strings.Contains(out, "signature invalid") {
		t.Errorf("output = %q, want a signature-invalid message", out)
	}
}

func TestVerifySignatureFlowMissingSigSoftFailsAndPasses(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(dataPath, []byte("fake checksums\n"), 0o600); err != nil {
		t.Fatalf("writing checksums fixture: %v", err)
	}
	pubkeyPath, _ := writeTestSigningKeyFixture(t, dir)
	missingSigPath := filepath.Join(dir, "does-not-exist.sig")

	out, err := runInstallSh(t, "--verify-signature-flow", pubkeyPath, missingSigPath, dataPath)
	if err != nil {
		t.Fatalf("--verify-signature-flow with a missing sig should soft-fail (pass), got err: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has no sha256sums.txt.sig") && !strings.Contains(out, "checksum-only integrity") {
		t.Errorf("output = %q, want a checksum-only-integrity warning", out)
	}
}

func TestVerifySignatureFlowEmptySigSoftFailsAndPasses(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(dataPath, []byte("fake checksums\n"), 0o600); err != nil {
		t.Fatalf("writing checksums fixture: %v", err)
	}
	pubkeyPath, _ := writeTestSigningKeyFixture(t, dir)
	emptySigPath := filepath.Join(dir, "empty.sig")
	if err := os.WriteFile(emptySigPath, nil, 0o600); err != nil {
		t.Fatalf("writing empty sig fixture: %v", err)
	}

	out, err := runInstallSh(t, "--verify-signature-flow", pubkeyPath, emptySigPath, dataPath)
	if err != nil {
		t.Fatalf("--verify-signature-flow with an empty sig should soft-fail (pass), got err: %v\n%s", err, out)
	}
	if !strings.Contains(out, "checksum-only integrity") {
		t.Errorf("output = %q, want a checksum-only-integrity warning", out)
	}
}

func TestVerifySignatureFlowInvalidSigHardFails(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(dataPath, []byte("fake checksums\n"), 0o600); err != nil {
		t.Fatalf("writing checksums fixture: %v", err)
	}
	pubkeyPath, _ := writeTestSigningKeyFixture(t, dir)
	badSigPath := filepath.Join(dir, "bad.sig")
	if err := os.WriteFile(badSigPath, []byte("not a real signature"), 0o600); err != nil {
		t.Fatalf("writing bad sig fixture: %v", err)
	}

	out, err := runInstallSh(t, "--verify-signature-flow", pubkeyPath, badSigPath, dataPath)
	if err == nil {
		t.Fatalf("--verify-signature-flow with an invalid signature should hard-fail, got:\n%s", out)
	}
	if !strings.Contains(out, "signature does not match") {
		t.Errorf("output = %q, want a signature-mismatch message", out)
	}
}

func TestVerifySignatureFlowValidSigPasses(t *testing.T) {
	requireOpenSSL(t)
	dir := t.TempDir()

	dataPath := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(dataPath, []byte("fake checksums\n"), 0o600); err != nil {
		t.Fatalf("writing checksums fixture: %v", err)
	}
	pubkeyPath, priv := writeTestSigningKeyFixture(t, dir)
	sigPath := signFileFixture(t, dir, priv, dataPath)

	out, err := runInstallSh(t, "--verify-signature-flow", pubkeyPath, sigPath, dataPath)
	if err != nil {
		t.Fatalf("--verify-signature-flow with a valid signature should pass, got err: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Signature verified") {
		t.Errorf("output = %q, want a signature-verified message", out)
	}
}

// unreachableURL returns a URL to a local address nothing is listening on,
// by opening a TCP listener and immediately closing it — connections to the
// freed port are refused immediately, unlike a routable-but-silent address,
// which would hang for the OS/curl connect timeout.
func unreachableURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opening throwaway listener: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing throwaway listener: %v", err)
	}
	return "http://" + addr + "/sha256sums.txt.sig"
}

func TestFetchReleaseSignaturePassesOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fake signature bytes"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "sha256sums.txt.sig")
	out, err := runInstallSh(t, "--fetch-signature", srv.URL, dest)
	if err != nil {
		t.Fatalf("--fetch-signature on a 200 should pass, got err: %v\n%s", err, out)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("dest file should exist after a 200: %v", rerr)
	}
	if string(got) != "fake signature bytes" {
		t.Errorf("dest contents = %q, want the response body", got)
	}
}

func TestFetchReleaseSignatureSoftFailsOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "sha256sums.txt.sig")
	out, err := runInstallSh(t, "--fetch-signature", srv.URL, dest)
	if err != nil {
		t.Fatalf("--fetch-signature on a 404 should soft-fail (pass), got err: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest file should not exist after a 404, stat err = %v", statErr)
	}
}

func TestFetchReleaseSignatureHardFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "sha256sums.txt.sig")
	out, err := runInstallSh(t, "--fetch-signature", srv.URL, dest)
	if err == nil {
		t.Fatalf("--fetch-signature on a 500 should hard-fail, got:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 500") {
		t.Errorf("output = %q, want it to mention HTTP 500", out)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest file should not exist after a hard failure, stat err = %v", statErr)
	}
}

func TestFetchReleaseSignatureHardFailsOnConnectionRefused(t *testing.T) {
	// The security gap this covers: a network error or an attacker blocking
	// just the .sig URL must never be silently treated the same as a 404
	// ("release predates signing") — that would let an attacker force the
	// checksum-only fallback by MITMing or dropping only this one request.
	dest := filepath.Join(t.TempDir(), "sha256sums.txt.sig")
	out, err := runInstallSh(t, "--fetch-signature", unreachableURL(t), dest)
	if err == nil {
		t.Fatalf("--fetch-signature against an unreachable host should hard-fail, got:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 000") {
		t.Errorf("output = %q, want it to mention HTTP 000 (no response received)", out)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest file should not exist after a hard failure, stat err = %v", statErr)
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
