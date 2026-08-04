package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostRedirectTransport rewrites any request to hit addr over plain HTTP.
// Lets tests intercept the hardcoded github.com/api.github.com URLs in
// fetchLatestRelease and downloadFile.
type hostRedirectTransport struct{ addr string }

func (h *hostRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Host = h.addr
	r2.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(r2)
}

// useHTTPTestServer starts an httptest.Server and wires httpClient to route
// all requests to it. Restores the original client on test cleanup.
func useHTTPTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := httpClient
	httpClient = &http.Client{Transport: &hostRedirectTransport{addr: srv.Listener.Addr().String()}}
	t.Cleanup(func() {
		httpClient = orig
		srv.Close()
	})
}

func TestFetchLatestRelease(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tag_name": "v0.0.2",
			"assets": [
				{"name": "argus-linux-amd64", "browser_download_url": "https://example.com/argus-linux-amd64"},
				{"name": "sha256sums.txt", "browser_download_url": "https://example.com/sha256sums.txt"}
			]
		}`))
	})

	rel, err := fetchLatestRelease(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.0.2" {
		t.Errorf("TagName = %q, want %q", rel.TagName, "v0.0.2")
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("Assets = %+v, want 2 entries", rel.Assets)
	}
}

func TestFetchLatestReleaseNonOKStatus(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := fetchLatestRelease(context.Background(), false); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestFetchLatestReleaseIncludePre(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag_name": "v0.0.3-rc.1", "assets": []},
			{"tag_name": "v0.0.2", "assets": []}
		]`))
	})

	rel, err := fetchLatestRelease(context.Background(), true)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.0.3-rc.1" {
		t.Errorf("TagName = %q, want the newest (first) entry %q", rel.TagName, "v0.0.3-rc.1")
	}
}

func TestFetchLatestReleaseIncludePreOutOfOrder(t *testing.T) {
	// GitHub's /releases list is documented as newest-first but has been
	// observed live to return an entry out of order — the newest release
	// here, v0.3.0, sits in the middle of the list, not first. Trusting
	// list position (as the old releases[0] logic did)
	// would silently pick the stale v0.1.0.
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag_name": "v0.1.0", "prerelease": false, "assets": []},
			{"tag_name": "v0.3.0", "prerelease": false, "assets": []},
			{"tag_name": "v0.2.0", "prerelease": false, "assets": []}
		]`))
	})

	rel, err := fetchLatestRelease(context.Background(), true)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.3.0" {
		t.Errorf("TagName = %q, want the highest by semver %q", rel.TagName, "v0.3.0")
	}
}

func TestFetchLatestReleaseNonPreFallsBackToStableOn404(t *testing.T) {
	// /releases/latest 404s (e.g. GitHub is slow to promote a release, or
	// this repo's newest stable hasn't propagated yet); the fallback to
	// the full list must still prefer the stable release over a newer,
	// but pre-release, tag — matching install.sh's pick_latest_tag.
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag_name": "v0.2.0-rc.1", "prerelease": true, "assets": []},
			{"tag_name": "v0.1.0", "prerelease": false, "assets": []}
		]`))
	})

	rel, err := fetchLatestRelease(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.1.0" {
		t.Errorf("TagName = %q, want the stable release %q over the newer pre-release", rel.TagName, "v0.1.0")
	}
}

func TestFetchLatestReleaseNonPreFallsBackToPrereleaseWhenNoneStable(t *testing.T) {
	// Every published release is a pre-release, so /releases/latest 404s
	// and there's no stable release to prefer in the fallback list —
	// the highest pre-release should be used instead of erroring out.
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"tag_name": "v0.1.0-rc.1", "prerelease": true, "assets": []},
			{"tag_name": "v0.1.0-rc.2", "prerelease": true, "assets": []}
		]`))
	})

	rel, err := fetchLatestRelease(context.Background(), false)
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v0.1.0-rc.2" {
		t.Errorf("TagName = %q, want the highest pre-release %q", rel.TagName, "v0.1.0-rc.2")
	}
}

func TestFetchLatestReleaseIncludePreEmptyList(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	if _, err := fetchLatestRelease(context.Background(), true); err == nil {
		t.Fatal("expected an error when no releases exist")
	}
}

func TestFetchLatestReleaseIncludePreBadJSON(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})

	if _, err := fetchLatestRelease(context.Background(), true); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestReleaseAssetFor(t *testing.T) {
	rel := Release{
		Assets: []Asset{
			{Name: "argus-linux-amd64", DownloadURL: "https://github.com/x/amd64"},
			{Name: "argus-darwin-arm64", DownloadURL: "https://github.com/x/arm64"},
			{Name: "sha256sums.txt", DownloadURL: "https://github.com/x/sums"},
		},
	}

	a, ok := rel.AssetFor("linux-amd64")
	if !ok || a.Name != "argus-linux-amd64" {
		t.Fatalf("AssetFor(linux-amd64) = %+v, %v", a, ok)
	}

	if _, matched := rel.AssetFor("windows-amd64"); matched {
		t.Fatal("AssetFor(windows-amd64) should not match")
	}

	sums, ok := rel.ChecksumsAsset()
	if !ok || sums.Name != "sha256sums.txt" {
		t.Fatalf("ChecksumsAsset() = %+v, %v", sums, ok)
	}
}

func TestReleaseSignatureAsset(t *testing.T) {
	rel := Release{
		Assets: []Asset{
			{Name: "argus-linux-amd64", DownloadURL: "https://github.com/x/amd64"},
			{Name: "sha256sums.txt", DownloadURL: "https://github.com/x/sums"},
			{Name: "sha256sums.txt.sig", DownloadURL: "https://github.com/x/sig"},
		},
	}

	sig, ok := rel.SignatureAsset()
	if !ok || sig.Name != "sha256sums.txt.sig" {
		t.Fatalf("SignatureAsset() = %+v, %v", sig, ok)
	}

	unsigned := Release{Assets: []Asset{{Name: "sha256sums.txt"}}}
	if _, ok := unsigned.SignatureAsset(); ok {
		t.Error("SignatureAsset() should not match a release with no .sig asset")
	}
}

func generateTestSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test signing key: %v", err)
	}
	return key
}

func TestParseReleaseSigningPublicKey(t *testing.T) {
	pub, err := parseReleaseSigningPublicKey()
	if err != nil {
		t.Fatalf("parseReleaseSigningPublicKey: %v", err)
	}
	if pub.Curve != elliptic.P256() {
		t.Errorf("embedded key curve = %v, want P-256", pub.Curve)
	}
}

func TestVerifySignature(t *testing.T) {
	key := generateTestSigningKey(t)
	data := []byte("argus release checksums fixture")

	digest := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("signing fixture: %v", err)
	}

	if err := verifySignature(&key.PublicKey, data, sig); err != nil {
		t.Errorf("verifySignature with a valid signature: %v", err)
	}

	if err := verifySignature(&key.PublicKey, []byte("tampered data"), sig); err == nil {
		t.Error("expected an error when the signed data doesn't match")
	}

	otherKey := generateTestSigningKey(t)
	if err := verifySignature(&otherKey.PublicKey, data, sig); err == nil {
		t.Error("expected an error when the signature was made by a different key")
	}

	if err := verifySignature(&key.PublicKey, data, []byte("not a signature")); err == nil {
		t.Error("expected an error for a malformed signature")
	}
}

func TestVerifyChecksumsSignatureRejectsForgedSignature(t *testing.T) {
	// verifyChecksumsSignature always checks against the embedded production
	// public key, so a signature not produced by its (deliberately absent)
	// private key must be rejected regardless of content.
	if err := verifyChecksumsSignature([]byte("sha256sums.txt contents"), []byte("forged signature bytes")); err == nil {
		t.Error("expected an error for a signature not made by the production key")
	}
}

func TestVerifyReleaseSignatureMissingAssetFailsNowThatSigningIsEnforced(t *testing.T) {
	rel := Release{TagName: "v1.0.0"}
	buf := &bytes.Buffer{}

	err := verifyReleaseSignature(context.Background(), buf, rel, []byte("checksums"), t.TempDir())
	if err == nil {
		t.Fatal("verifyReleaseSignature: want error for a release with no sha256sums.txt.sig, got nil")
	}
	if !strings.Contains(err.Error(), "no sha256sums.txt.sig") {
		t.Errorf("err = %q, want a no-signature-asset error", err.Error())
	}
}

func TestVerifyReleaseSignaturePresentButInvalidFails(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "sha256sums.txt.sig") {
			t.Errorf("unexpected request to %s", r.URL.Path)
			return
		}
		_, _ = w.Write([]byte("not a real signature"))
	})

	rel := Release{
		TagName: "v1.0.0",
		Assets:  []Asset{{Name: "sha256sums.txt.sig", DownloadURL: "https://github.com/x/sha256sums.txt.sig"}},
	}
	buf := &bytes.Buffer{}

	err := verifyReleaseSignature(context.Background(), buf, rel, []byte("checksums"), t.TempDir())
	if err == nil {
		t.Fatal("expected an error for an invalid signature")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Errorf("error = %v, want a signature-verification-failed message", err)
	}
}

// withTestSigningKey generates a throwaway ECDSA P-256 keypair and points
// releaseSigningPubKeyFunc at its public half for the test's duration,
// restoring the real embedded key on cleanup. Lets tests exercise genuine
// signature verification without the production private key, which only
// ever lives in the RELEASE_SIGNING_KEY GitHub Actions secret.
func withTestSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	orig := releaseSigningPubKeyFunc
	releaseSigningPubKeyFunc = func() (*ecdsa.PublicKey, error) { return &priv.PublicKey, nil }
	t.Cleanup(func() { releaseSigningPubKeyFunc = orig })
	return priv
}

// signChecksums produces an ASN.1 DER ECDSA signature over data's SHA-256
// digest, the same shape `openssl dgst -sha256 -sign` produces in
// .github/workflows/release.yml.
func signChecksums(t *testing.T, priv *ecdsa.PrivateKey, data []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(data)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("SignASN1: %v", err)
	}
	return sig
}

func TestVerifyReleaseSignatureValidSucceeds(t *testing.T) {
	priv := withTestSigningKey(t)
	checksums := []byte("fake checksums content")
	sig := signChecksums(t, priv, checksums)

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "sha256sums.txt.sig") {
			t.Errorf("unexpected request to %s", r.URL.Path)
			return
		}
		_, _ = w.Write(sig)
	})

	rel := Release{
		TagName: "v1.0.0",
		Assets:  []Asset{{Name: "sha256sums.txt.sig", DownloadURL: "https://github.com/x/sha256sums.txt.sig"}},
	}
	buf := &bytes.Buffer{}

	if err := verifyReleaseSignature(context.Background(), buf, rel, checksums, t.TempDir()); err != nil {
		t.Fatalf("verifyReleaseSignature: %v", err)
	}
	if !strings.Contains(buf.String(), "signature verified") {
		t.Errorf("output = %q, want a signature-verified message", buf.String())
	}
}

func TestDownloadFileRejectsUntrustedHost(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	err := downloadFile(context.Background(), "https://evil.example.com/argus", dst)
	if err == nil {
		t.Fatal("expected an error for a non-github.com host")
	}
}

func TestDownloadFileRejectsNonHTTPS(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	err := downloadFile(context.Background(), "http://github.com/argus", dst)
	if err == nil {
		t.Fatal("expected an error for a non-https scheme")
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "argus-linux-amd64")
	if err := os.WriteFile(binPath, []byte("fake binary contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sum, err := sha256File(binPath)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}

	checksums := sum + "  argus-linux-amd64\nother  other-file\n"
	if err := verifyChecksum(binPath, checksums, "argus-linux-amd64"); err != nil {
		t.Errorf("verifyChecksum with matching sum: %v", err)
	}

	if err := verifyChecksum(binPath, "deadbeef  argus-linux-amd64\n", "argus-linux-amd64"); err == nil {
		t.Error("expected an error for a checksum mismatch")
	}

	if err := verifyChecksum(binPath, "deadbeef  other-file\n", "argus-linux-amd64"); err == nil {
		t.Error("expected an error when no entry matches the asset name")
	}
}

func TestCopyFileAndReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(src, []byte("new contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dst := filepath.Join(dir, "installed-binary")
	if err := os.WriteFile(dst, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := replaceBinary(context.Background(), src, dst, filepath.Join(dir, "no-such-backup")); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new contents" {
		t.Errorf("dst contents = %q, want %q", got, "new contents")
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("dst is not executable: mode %v", info.Mode())
	}

	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected the .tmp file to be gone after rename, got err=%v", err)
	}
}

// TestReplaceBinaryRollsBackOnResignFailure forces the post-rename resign
// step to fail and checks replaceBinary restores dstPath from backupPath
// instead of leaving the new (Gatekeeper-killed, on real darwin) binary in
// place with no working argus on disk.
func TestReplaceBinaryRollsBackOnResignFailure(t *testing.T) {
	orig := resignFunc
	t.Cleanup(func() { resignFunc = orig })
	resignErr := fmt.Errorf("codesign: boom")
	resignFunc = func(context.Context, string, string) error { return resignErr }

	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(src, []byte("new contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dst := filepath.Join(dir, "installed-binary")
	if err := os.WriteFile(dst, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	backup := filepath.Join(dir, "installed-binary.backup")
	if err := copyFile(dst, backup); err != nil {
		t.Fatalf("copyFile backup: %v", err)
	}

	err := replaceBinary(context.Background(), src, dst, backup)
	if err == nil {
		t.Fatal("replaceBinary: want error on resign failure, got nil")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("replaceBinary error = %q, want it to mention a rollback", err)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != "old contents" {
		t.Errorf("dst contents after rollback = %q, want the pre-update %q", got, "old contents")
	}
}

// TestReplaceBinaryNoRollbackWithoutBackup checks the no-backup-available
// error path: when backupPath doesn't exist, replaceBinary must say so
// rather than attempt (and fail) a rollback.
func TestReplaceBinaryNoRollbackWithoutBackup(t *testing.T) {
	orig := resignFunc
	t.Cleanup(func() { resignFunc = orig })
	resignFunc = func(context.Context, string, string) error { return fmt.Errorf("codesign: boom") }

	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(src, []byte("new contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dst := filepath.Join(dir, "installed-binary")
	if err := os.WriteFile(dst, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := replaceBinary(context.Background(), src, dst, filepath.Join(dir, "no-such-backup"))
	if err == nil {
		t.Fatal("replaceBinary: want error on resign failure, got nil")
	}
	if !strings.Contains(err.Error(), "no backup available") {
		t.Errorf("replaceBinary error = %q, want it to mention no backup available", err)
	}
}

func TestResignBinaryNoOpNonDarwin(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "argus")
	if err := os.WriteFile(dst, []byte("binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := resignBinary(context.Background(), "linux", dst); err != nil {
		t.Errorf("resignBinary(goos=linux) = %v, want nil no-op", err)
	}
}

// TestResignBinaryDarwin exercises the actual codesign re-signing step that
// replaceBinary was skipping entirely, by re-signing a copy of the running
// test binary and confirming codesign reports an ad-hoc signature where
// before there was none.
func TestResignBinaryDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign re-signing only applies on darwin")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign not on PATH")
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "argus")
	if err = copyFile(self, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if err = os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if err = resignBinary(context.Background(), runtime.GOOS, dst); err != nil {
		t.Fatalf("resignBinary: %v", err)
	}

	out, err := exec.CommandContext(context.Background(), "codesign", "-dv", dst).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign -dv %s: %v (%s)", dst, err, out)
	}
	if !strings.Contains(string(out), "adhoc") {
		t.Errorf("codesign -dv output = %q, want an adhoc signature", out)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Errorf("checkWritable(%s): %v", dir, err)
	}

	if err := checkWritable(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("expected an error for a non-existent directory")
	}
}

func TestRunUpdateAlreadyLatest(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "v0.1.0", "assets": []}`))
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(buf.String(), "already on the latest version") {
		t.Errorf("output = %q, want an already-latest message", buf.String())
	}
}

// TestRunUpdateAlreadyLatestUnnormalizedTag guards against comparing a
// normalized currentVer against a raw tag_name: a v-less tag must still be
// recognized as equal to "v0.1.0", not silently treated as an invalid semver
// that skips the already-latest guard and forces a redundant reinstall.
func TestRunUpdateAlreadyLatestUnnormalizedTag(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "0.1.0", "assets": []}`))
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(buf.String(), "already on the latest version") {
		t.Errorf("output = %q, want an already-latest message", buf.String())
	}
}

func TestRunUpdateNewerAvailableButNoMatchingAsset(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "v9.9.9", "assets": []}`))
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false)
	if err == nil {
		t.Fatal("expected an error when the release has no matching asset")
	}
	if !strings.Contains(err.Error(), "no asset for") {
		t.Errorf("error = %v, want a missing-asset message", err)
	}
}

func TestRunUpdateIncludePreUsesReleasesList(t *testing.T) {
	var gotPath string
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"tag_name": "v0.2.0-rc.1", "assets": []}]`))
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "v0.1.0", true)
	if err == nil {
		t.Fatal("expected an error when the release has no matching asset")
	}
	if !strings.HasSuffix(gotPath, "/releases") {
		t.Errorf("request path = %q, want the releases-list endpoint", gotPath)
	}
	if !strings.Contains(buf.String(), "v0.1.0 -> v0.2.0-rc.1") {
		t.Errorf("output = %q, want it to mention the pre-release version", buf.String())
	}
}

func TestNormalizeSemver(t *testing.T) {
	tests := map[string]string{
		"0.1.0":  "v0.1.0",
		"v0.1.0": "v0.1.0",
		"":       "",
	}
	for in, want := range tests {
		if got := normalizeSemver(in); got != want {
			t.Errorf("normalizeSemver(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunUpdateFullSuccess(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	// runUpdate calls refreshInstalledCompletions after replaceBinary succeeds,
	// which resolves completion paths under $HOME; isolate it so this test
	// can't read or write the real host's completion files.
	t.Setenv("HOME", t.TempDir())
	priv := withTestSigningKey(t)
	assetName := "argus-" + platform
	binContents := []byte("new argus binary contents")
	sum := sha256.Sum256(binContents)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	sig := signChecksums(t, priv, []byte(checksums))

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
				{"name": %q, "browser_download_url": "https://github.com/x/%s"},
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"},
				{"name": "sha256sums.txt.sig", "browser_download_url": "https://github.com/x/sha256sums.txt.sig"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(binContents)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt.sig"):
			_, _ = w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt"):
			_, _ = w.Write([]byte(checksums))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	exePath := filepath.Join(dir, "argus")
	if err := os.WriteFile(exePath, []byte("old argus binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(binContents) {
		t.Errorf("exePath contents = %q, want the downloaded binary", got)
	}
	backup, err := os.ReadFile(exePath + ".backup")
	if err != nil {
		t.Fatalf("expected a backup of the old binary: %v", err)
	}
	if string(backup) != "old argus binary contents" {
		t.Errorf("backup contents = %q, want the pre-update binary", backup)
	}
	if !strings.Contains(buf.String(), "checksum verified") || !strings.Contains(buf.String(), "updated v0.1.0 -> v9.9.9") {
		t.Errorf("output = %q, want checksum-verified and updated messages", buf.String())
	}
}

func TestRunUpdateChecksumMismatch(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	assetName := "argus-" + platform

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
				{"name": %q, "browser_download_url": "https://github.com/x/%s"},
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write([]byte("new argus binary contents"))
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt"):
			_, _ = fmt.Fprintf(w, "deadbeef  %s\n", assetName)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	exePath := filepath.Join(dir, "argus")
	if err := os.WriteFile(exePath, []byte("old argus binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err == nil {
		t.Fatal("expected an error for a checksum mismatch")
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "old argus binary contents" {
		t.Errorf("exePath was modified despite a checksum mismatch: %q", got)
	}
}

func TestRunUpdateInvalidSignatureRefusesInstall(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	assetName := "argus-" + platform
	binContents := []byte("new argus binary contents")
	sum := sha256.Sum256(binContents)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
				{"name": %q, "browser_download_url": "https://github.com/x/%s"},
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"},
				{"name": "sha256sums.txt.sig", "browser_download_url": "https://github.com/x/sha256sums.txt.sig"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(binContents)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt.sig"):
			_, _ = w.Write([]byte("not a real signature"))
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt"):
			_, _ = w.Write([]byte(checksums))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	exePath := filepath.Join(dir, "argus")
	if err := os.WriteFile(exePath, []byte("old argus binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false)
	if err == nil {
		t.Fatal("expected an error for a release with an invalid signature")
	}
	if !strings.Contains(err.Error(), "signature verification failed") {
		t.Errorf("error = %v, want a signature-verification-failed message", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "old argus binary contents" {
		t.Errorf("exePath was modified despite an invalid signature: %q", got)
	}
}

func TestRunUpdateReleaseMissingChecksums(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	assetName := "argus-" + platform

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
			{"name": %q, "browser_download_url": "https://github.com/x/%s"}
		]}`, assetName, assetName)
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false)
	if err == nil {
		t.Fatal("expected an error when the release has no sha256sums.txt")
	}
	if !strings.Contains(err.Error(), "sha256sums.txt") {
		t.Errorf("error = %v, want a missing-checksums message", err)
	}
}

func TestRunUpdateNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through directory permissions; skip under root (e.g. some CI containers)")
	}
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	assetName := "argus-" + platform

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
			{"name": %q, "browser_download_url": "https://github.com/x/%s"},
			{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"}
		]}`, assetName, assetName)
	})

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	exePath := filepath.Join(dir, "argus")
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err == nil {
		t.Fatal("expected an error for a non-writable install directory")
	}
}

func TestHostPlatform(t *testing.T) {
	platform, err := hostPlatform()
	if err != nil {
		// Only linux/darwin amd64/arm64 are supported; this test environment
		// may not be one of them, which is itself a valid (if
		// untested-further) path.
		t.Skipf("hostPlatform: %v", err)
	}
	switch platform {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
	default:
		t.Errorf("hostPlatform() = %q, want one of the supported platforms", platform)
	}
}

// withReleaseSigningPublicKeyPEM temporarily swaps the embedded key material
// parseReleaseSigningPublicKey decodes, restoring the real production PEM on
// cleanup.
func withReleaseSigningPublicKeyPEM(t *testing.T, pemStr string) {
	t.Helper()
	orig := releaseSigningPublicKeyPEM
	releaseSigningPublicKeyPEM = pemStr
	t.Cleanup(func() { releaseSigningPublicKeyPEM = orig })
}

func TestParseReleaseSigningPublicKeyNoPEMBlock(t *testing.T) {
	withReleaseSigningPublicKeyPEM(t, "not a PEM block at all")

	if _, err := parseReleaseSigningPublicKey(); err == nil {
		t.Fatal("expected an error for a string with no PEM block")
	} else if !strings.Contains(err.Error(), "no PEM block found") {
		t.Errorf("error = %v, want a no-PEM-block message", err)
	}
}

func TestParseReleaseSigningPublicKeyInvalidDER(t *testing.T) {
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: []byte{0x00, 0x01, 0x02}}
	withReleaseSigningPublicKeyPEM(t, string(pem.EncodeToMemory(block)))

	if _, err := parseReleaseSigningPublicKey(); err == nil {
		t.Fatal("expected an error for a PEM block with invalid DER")
	} else if !strings.Contains(err.Error(), "parsing embedded release signing public key") {
		t.Errorf("error = %v, want a parse-failure message", err)
	}
}

func TestParseReleaseSigningPublicKeyWrongKeyType(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: der}
	withReleaseSigningPublicKeyPEM(t, string(pem.EncodeToMemory(block)))

	if _, err := parseReleaseSigningPublicKey(); err == nil {
		t.Fatal("expected an error for a non-ECDSA key")
	} else if !strings.Contains(err.Error(), "want ECDSA") {
		t.Errorf("error = %v, want a wrong-key-type message", err)
	}
}

func TestVerifyChecksumsSignatureKeyResolutionError(t *testing.T) {
	orig := releaseSigningPubKeyFunc
	t.Cleanup(func() { releaseSigningPubKeyFunc = orig })
	keyErr := fmt.Errorf("boom")
	releaseSigningPubKeyFunc = func() (*ecdsa.PublicKey, error) { return nil, keyErr }

	err := verifyChecksumsSignature([]byte("checksums"), []byte("sig"))
	if err == nil {
		t.Fatal("expected an error when the signing key fails to resolve")
	}
	if !strings.Contains(err.Error(), keyErr.Error()) {
		t.Errorf("error = %v, want it to surface the key-resolution error", err)
	}
}

func TestDoReleaseRequestInvalidURL(t *testing.T) {
	// A raw newline is an ASCII control character; http.NewRequestWithContext
	// rejects it before any network I/O happens.
	resp, err := doReleaseRequest(context.Background(), "https://example.com/\n")
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err == nil {
		t.Fatal("expected an error for a URL containing a control character")
	}
	if !strings.Contains(err.Error(), "building release request") {
		t.Errorf("error = %v, want a building-request message", err)
	}
}

// errTransport is an http.RoundTripper that always fails, for exercising
// httpClient.Do's own error path independently of server-side status codes.
type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestDoReleaseRequestTransportError(t *testing.T) {
	orig := httpClient
	t.Cleanup(func() { httpClient = orig })
	httpClient = &http.Client{Transport: errTransport{err: fmt.Errorf("connection refused")}}

	resp, err := doReleaseRequest(context.Background(), "https://api.github.com/repos/x/y")
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err == nil {
		t.Fatal("expected an error when the transport fails")
	}
	if !strings.Contains(err.Error(), "fetching latest release") {
		t.Errorf("error = %v, want a fetching-latest-release message", err)
	}
}

func TestListReleasesNonOKStatus(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := listReleases(context.Background()); err == nil {
		t.Fatal("expected an error for a non-200 response")
	} else if !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("error = %v, want an unexpected-status message", err)
	}
}

func TestLatestStableReleaseNonOKStatus(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, ok, err := latestStableRelease(context.Background())
	if err == nil {
		t.Fatal("expected an error for a non-200, non-404 response")
	}
	if ok {
		t.Error("ok = true, want false on error")
	}
	if !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("error = %v, want an unexpected-status message", err)
	}
}

func TestLatestStableReleaseMalformedJSON(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})

	if _, _, err := latestStableRelease(context.Background()); err == nil {
		t.Fatal("expected an error for malformed JSON")
	} else if !strings.Contains(err.Error(), "decoding release response") {
		t.Errorf("error = %v, want a decoding-response message", err)
	}
}

func TestHighestBySemverAllInvalid(t *testing.T) {
	releases := []Release{{TagName: "not-semver"}, {TagName: "also-bad"}}
	if _, err := highestBySemver(releases); err == nil {
		t.Fatal("expected an error when no release has a valid semver tag")
	} else if !strings.Contains(err.Error(), "no release with a valid semver tag found") {
		t.Errorf("error = %v, want a no-valid-semver message", err)
	}
}

func TestHighestBySemverMixedValidInvalid(t *testing.T) {
	releases := []Release{
		{TagName: "garbage"},
		{TagName: "v1.2.3"},
		{TagName: "not-a-version-either"},
		{TagName: "v1.0.0"},
	}
	best, err := highestBySemver(releases)
	if err != nil {
		t.Fatalf("highestBySemver: %v", err)
	}
	if best.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want the highest valid semver %q, invalid tags ignored", best.TagName, "v1.2.3")
	}
}

func TestDownloadFileParseURLError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	// A raw newline is an ASCII control character; url.Parse rejects it
	// before the github.com host check ever runs.
	err := downloadFile(context.Background(), "https://github.com/\n", dst)
	if err == nil {
		t.Fatal("expected an error for a URL containing a control character")
	}
	if !strings.Contains(err.Error(), "parsing download URL") {
		t.Errorf("error = %v, want a parsing-download-URL message", err)
	}
}

func TestDownloadFileGithubHostNonOKStatus(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	dst := filepath.Join(t.TempDir(), "out")
	err := downloadFile(context.Background(), "https://github.com/x/y", dst)
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if !strings.Contains(err.Error(), "unexpected status") {
		t.Errorf("error = %v, want an unexpected-status message", err)
	}
}

// shortBodyTransport reports a Content-Length longer than the bytes it
// actually returns, without erroring — the shape a caller sees when a server
// declares a length it doesn't deliver but still closes the body cleanly.
type shortBodyTransport struct {
	actualBody     string
	declaredLength int64
}

func (s shortBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: s.declaredLength,
		Body:          io.NopCloser(strings.NewReader(s.actualBody)),
		Header:        make(http.Header),
	}, nil
}

func TestDownloadFileTruncatedBody(t *testing.T) {
	orig := httpClient
	t.Cleanup(func() { httpClient = orig })
	httpClient = &http.Client{Transport: shortBodyTransport{declaredLength: 100, actualBody: "short"}}

	dst := filepath.Join(t.TempDir(), "out")
	err := downloadFile(context.Background(), "https://github.com/x/y", dst)
	if err == nil {
		t.Fatal("expected an error for a body shorter than Content-Length")
	}
	if !strings.Contains(err.Error(), "got 5 bytes, expected 100") {
		t.Errorf("error = %v, want a byte-count mismatch message", err)
	}
}

func TestDownloadFileCreateFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	// destPath's parent directory doesn't exist, so os.Create fails.
	dst := filepath.Join(t.TempDir(), "no-such-subdir", "out")
	err := downloadFile(context.Background(), "https://github.com/x/y", dst)
	if err == nil {
		t.Fatal("expected an error when the destination directory doesn't exist")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Errorf("error = %v, want a creating-file message", err)
	}
}

func TestVerifyChecksumOpenFailure(t *testing.T) {
	checksums := "deadbeef  argus-linux-amd64\n"
	err := verifyChecksum(filepath.Join(t.TempDir(), "does-not-exist"), checksums, "argus-linux-amd64")
	if err == nil {
		t.Fatal("expected an error when the binary path doesn't exist")
	}
	if !strings.Contains(err.Error(), "opening") {
		t.Errorf("error = %v, want an opening-file message", err)
	}
}

func TestVerifyChecksumBinaryModeLineNoMatch(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "argus-linux-amd64")
	if err := os.WriteFile(binPath, []byte("contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// sha256sum's binary-mode output ("<digest> *name") uses a single space
	// and an asterisk; verifyChecksum only recognizes the two-space
	// text-mode format, so this line is treated as not matching at all.
	err := verifyChecksum(binPath, "deadbeef *argus-linux-amd64\n", "argus-linux-amd64")
	if err == nil {
		t.Fatal("expected an error for a binary-mode checksum line")
	}
	if !strings.Contains(err.Error(), "no checksum entry") {
		t.Errorf("error = %v, want a no-checksum-entry message", err)
	}
}

func TestVerifyReleaseSignatureDownloadFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	rel := Release{
		TagName: "v1.0.0",
		Assets:  []Asset{{Name: "sha256sums.txt.sig", DownloadURL: "https://github.com/x/sha256sums.txt.sig"}},
	}
	buf := &bytes.Buffer{}

	err := verifyReleaseSignature(context.Background(), buf, rel, []byte("checksums"), t.TempDir())
	if err == nil {
		t.Fatal("expected an error when the signature asset fails to download")
	}
	if !strings.Contains(err.Error(), "downloading signature") {
		t.Errorf("error = %v, want a downloading-signature message", err)
	}
}

func TestVerifyReleaseSignatureReadFileFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("signature bytes"))
	})

	orig := readFileFunc
	t.Cleanup(func() { readFileFunc = orig })
	readFileFunc = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, "sha256sums.txt.sig") {
			return nil, fmt.Errorf("boom")
		}
		return os.ReadFile(name)
	}

	rel := Release{
		TagName: "v1.0.0",
		Assets:  []Asset{{Name: "sha256sums.txt.sig", DownloadURL: "https://github.com/x/sha256sums.txt.sig"}},
	}
	buf := &bytes.Buffer{}

	err := verifyReleaseSignature(context.Background(), buf, rel, []byte("checksums"), t.TempDir())
	if err == nil {
		t.Fatal("expected an error when reading the downloaded signature fails")
	}
	if !strings.Contains(err.Error(), "reading signature") {
		t.Errorf("error = %v, want a reading-signature message", err)
	}
}

func TestCopyFileStatFailure(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	err := copyFile(filepath.Join(t.TempDir(), "does-not-exist"), dst)
	if err == nil {
		t.Fatal("expected an error when src doesn't exist")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("error = %v, want a stat message", err)
	}
}

func TestCopyFileDstOpenFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// A directory can't be opened for writing.
	dstDir := filepath.Join(dir, "dst-is-a-dir")
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	err := copyFile(src, dstDir)
	if err == nil {
		t.Fatal("expected an error when dst is a directory")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Errorf("error = %v, want a creating message", err)
	}
}

// withFakeCodesign prepends a directory containing a fake "codesign" script
// (exiting with the given code) to PATH, so resignBinary's darwin branch is
// testable without a real macOS host or the real codesign binary.
func withFakeCodesign(t *testing.T, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake codesign script requires a POSIX shell")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	scriptPath := filepath.Join(dir, "codesign")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResignBinaryDarwinCodesignNonzeroExit(t *testing.T) {
	withFakeCodesign(t, 1)

	dst := filepath.Join(t.TempDir(), "argus")
	if err := os.WriteFile(dst, []byte("binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := resignBinary(context.Background(), "darwin", dst)
	if err == nil {
		t.Fatal("expected an error when codesign exits nonzero")
	}
	if !strings.Contains(err.Error(), "codesign "+dst) {
		t.Errorf("error = %v, want it to name the failing codesign invocation", err)
	}
}

func TestReplaceBinaryCopyFileFailureLeavesDstUntouched(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "installed-binary")
	if err := os.WriteFile(dst, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := replaceBinary(context.Background(), filepath.Join(dir, "does-not-exist"), dst, filepath.Join(dir, "no-backup"))
	if err == nil {
		t.Fatal("expected an error when newPath doesn't exist")
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != "old contents" {
		t.Errorf("dst was modified despite the copy failure: %q", got)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("expected no .tmp file to be left behind, got err=%v", statErr)
	}
}

func TestReplaceBinaryChmodFailure(t *testing.T) {
	orig := chmodFunc
	t.Cleanup(func() { chmodFunc = orig })
	chmodFunc = func(string, os.FileMode) error { return fmt.Errorf("boom") }

	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(src, []byte("new contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dst := filepath.Join(dir, "installed-binary")
	if err := os.WriteFile(dst, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := replaceBinary(context.Background(), src, dst, filepath.Join(dir, "no-backup"))
	if err == nil {
		t.Fatal("expected an error when chmod fails")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("error = %v, want a chmod message", err)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != "old contents" {
		t.Errorf("dst was modified despite the chmod failure: %q", got)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("expected the .tmp file to be removed after the chmod failure, got err=%v", statErr)
	}
}

func TestReplaceBinaryRenameFailureDstIsDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	if err := os.WriteFile(src, []byte("new contents"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Renaming a regular file over an existing directory always fails,
	// regardless of permissions — a reliable, portable way to force
	// os.Rename to fail without depending on filesystem ACL quirks.
	dst := filepath.Join(dir, "installed-binary")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	err := replaceBinary(context.Background(), src, dst, filepath.Join(dir, "no-backup"))
	if err == nil {
		t.Fatal("expected an error when the rename fails")
	}
	if !strings.Contains(err.Error(), "installing") {
		t.Errorf("error = %v, want an installing message", err)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("expected the .tmp file to be removed after the rename failure, got err=%v", statErr)
	}
}

func TestCurrentBinaryPathEvalSymlinksFailure(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "argus-link")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	orig := executableFunc
	t.Cleanup(func() { executableFunc = orig })
	executableFunc = func() (string, error) { return link, nil }

	_, err := currentBinaryPath()
	if err == nil {
		t.Fatal("expected an error for a dangling symlink")
	}
	if !strings.Contains(err.Error(), "resolving running binary path") {
		t.Errorf("error = %v, want a resolving-path message", err)
	}
}

// TestRunUpdateDevBuildInvalidSemver checks a dev build's non-semver current
// version (e.g. "dev") skips the already-latest guard instead of being
// silently (and wrongly) treated as up to date.
func TestRunUpdateDevBuildInvalidSemver(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name": "v9.9.9", "assets": []}`))
	})

	exePath := filepath.Join(t.TempDir(), "argus")
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "dev", false)
	if err == nil {
		t.Fatal("expected an error once past the already-latest guard (no matching asset)")
	}
	if strings.Contains(buf.String(), "already on the latest version") {
		t.Errorf("output = %q, an invalid-semver current version must not short-circuit as already latest", buf.String())
	}
	if !strings.Contains(buf.String(), "new version available: dev -> v9.9.9") {
		t.Errorf("output = %q, want it to proceed past the guard", buf.String())
	}
}

// TestRunUpdateBackupFailureWarnsAndProceeds forces the pre-install backup
// copy to fail (by pre-occupying its destination with a directory) and
// checks runUpdate warns but still installs the update rather than aborting.
func TestRunUpdateBackupFailureWarnsAndProceeds(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	t.Setenv("HOME", t.TempDir())
	priv := withTestSigningKey(t)
	assetName := "argus-" + platform
	binContents := []byte("new argus binary contents")
	sum := sha256.Sum256(binContents)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	sig := signChecksums(t, priv, []byte(checksums))

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
				{"name": %q, "browser_download_url": "https://github.com/x/%s"},
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"},
				{"name": "sha256sums.txt.sig", "browser_download_url": "https://github.com/x/sha256sums.txt.sig"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(binContents)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt.sig"):
			_, _ = w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt"):
			_, _ = w.Write([]byte(checksums))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	exePath := filepath.Join(dir, "argus")
	if err := os.WriteFile(exePath, []byte("old argus binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Pre-occupy the backup path with a directory so copyFile's OpenFile
	// step fails there specifically, without touching replaceBinary's own
	// (differently-named) tmp file.
	if err := os.Mkdir(exePath+".backup", 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	buf := &bytes.Buffer{}

	if err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false); err != nil {
		t.Fatalf("runUpdate: %v, want the backup failure to only warn", err)
	}
	if !strings.Contains(buf.String(), "could not create backup") {
		t.Errorf("output = %q, want a could-not-create-backup warning", buf.String())
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(binContents) {
		t.Errorf("exePath contents = %q, want the update to proceed despite the backup failure", got)
	}
}

// TestRunUpdateReplaceBinaryFailure forces the post-download install step
// (replaceBinary) to fail and checks runUpdate wraps it as an
// installing-new-binary error rather than swallowing it.
func TestRunUpdateReplaceBinaryFailure(t *testing.T) {
	platform, perr := hostPlatform()
	if perr != nil {
		t.Skipf("hostPlatform: %v", perr)
	}
	t.Setenv("HOME", t.TempDir())
	orig := resignFunc
	t.Cleanup(func() { resignFunc = orig })
	resignFunc = func(context.Context, string, string) error { return fmt.Errorf("codesign: boom") }

	priv := withTestSigningKey(t)
	assetName := "argus-" + platform
	binContents := []byte("new argus binary contents")
	sum := sha256.Sum256(binContents)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	sig := signChecksums(t, priv, []byte(checksums))

	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name": "v9.9.9", "assets": [
				{"name": %q, "browser_download_url": "https://github.com/x/%s"},
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"},
				{"name": "sha256sums.txt.sig", "browser_download_url": "https://github.com/x/sha256sums.txt.sig"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(binContents)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt.sig"):
			_, _ = w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/sha256sums.txt"):
			_, _ = w.Write([]byte(checksums))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	exePath := filepath.Join(dir, "argus")
	if err := os.WriteFile(exePath, []byte("old argus binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := &bytes.Buffer{}

	err := runUpdate(context.Background(), buf, exePath, "v0.1.0", false)
	if err == nil {
		t.Fatal("expected an error when replaceBinary fails")
	}
	if !strings.Contains(err.Error(), "installing new binary") {
		t.Errorf("error = %v, want an installing-new-binary message", err)
	}
}

func TestDownloadAndVerifyUpdateChecksumsDownloadFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "argus-linux-amd64"):
			_, _ = w.Write([]byte("binary contents"))
		case strings.HasSuffix(r.URL.Path, "sha256sums.txt"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	rel := Release{TagName: "v1.0.0"}
	asset := Asset{Name: "argus-linux-amd64", DownloadURL: "https://github.com/x/argus-linux-amd64"}
	checksums := Asset{Name: "sha256sums.txt", DownloadURL: "https://github.com/x/sha256sums.txt"}
	buf := &bytes.Buffer{}

	_, err := downloadAndVerifyUpdate(context.Background(), buf, rel, asset, checksums, t.TempDir())
	if err == nil {
		t.Fatal("expected an error when the checksums file fails to download")
	}
	if !strings.Contains(err.Error(), "downloading checksums") {
		t.Errorf("error = %v, want a downloading-checksums message", err)
	}
}

func TestDownloadAndVerifyUpdateReadFileFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "argus-linux-amd64"):
			_, _ = w.Write([]byte("binary contents"))
		case strings.HasSuffix(r.URL.Path, "sha256sums.txt"):
			_, _ = w.Write([]byte("deadbeef  argus-linux-amd64\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	tmpDir := t.TempDir()
	checksumsTmp := filepath.Join(tmpDir, "sha256sums.txt")
	orig := readFileFunc
	t.Cleanup(func() { readFileFunc = orig })
	readFileFunc = func(name string) ([]byte, error) {
		if name == checksumsTmp {
			return nil, fmt.Errorf("boom")
		}
		return os.ReadFile(name)
	}

	rel := Release{TagName: "v1.0.0"}
	asset := Asset{Name: "argus-linux-amd64", DownloadURL: "https://github.com/x/argus-linux-amd64"}
	checksums := Asset{Name: "sha256sums.txt", DownloadURL: "https://github.com/x/sha256sums.txt"}
	buf := &bytes.Buffer{}

	_, err := downloadAndVerifyUpdate(context.Background(), buf, rel, asset, checksums, tmpDir)
	if err == nil {
		t.Fatal("expected an error when reading the downloaded checksums file fails")
	}
	if !strings.Contains(err.Error(), "reading checksums") {
		t.Errorf("error = %v, want a reading-checksums message", err)
	}
}

func TestDownloadAndVerifyUpdateBinaryDownloadFailure(t *testing.T) {
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	rel := Release{TagName: "v1.0.0"}
	asset := Asset{Name: "argus-linux-amd64", DownloadURL: "https://github.com/x/argus-linux-amd64"}
	checksums := Asset{Name: "sha256sums.txt", DownloadURL: "https://github.com/x/sha256sums.txt"}
	buf := &bytes.Buffer{}

	_, err := downloadAndVerifyUpdate(context.Background(), buf, rel, asset, checksums, t.TempDir())
	if err == nil {
		t.Fatal("expected an error when the binary fails to download")
	}
	if !strings.Contains(err.Error(), "downloading update") {
		t.Errorf("error = %v, want a downloading-update message", err)
	}
}

func TestNewUpdateCmdCurrentBinaryPathError(t *testing.T) {
	orig := executableFunc
	t.Cleanup(func() { executableFunc = orig })
	pathErr := fmt.Errorf("boom")
	executableFunc = func() (string, error) { return "", pathErr }

	cmd := newUpdateCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs(nil)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when currentBinaryPath fails")
	}
	if !strings.Contains(err.Error(), pathErr.Error()) {
		t.Errorf("error = %v, want it to surface the currentBinaryPath error", err)
	}
}

// TestNewUpdateCmdPreFlagWiring checks --pre actually reaches runUpdate as
// includePre=true, by observing that the releases-list endpoint (not
// releases/latest) gets hit.
func TestNewUpdateCmdPreFlagWiring(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "argus-under-test")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	orig := executableFunc
	t.Cleanup(func() { executableFunc = orig })
	executableFunc = func() (string, error) { return exe, nil }

	var gotPath string
	useHTTPTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"tag_name": "v0.2.0-rc.1", "assets": []}]`))
	})

	cmd := newUpdateCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--pre"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error (no matching asset in the fixture release)")
	}
	if !strings.HasSuffix(gotPath, "/releases") {
		t.Errorf("request path = %q, want the releases-list endpoint, confirming --pre wired includePre=true", gotPath)
	}
}
