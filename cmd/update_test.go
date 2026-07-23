package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	// observed live to return an entry out of order (issue #74) — the
	// newest release here, v0.3.0, sits in the middle of the list, not
	// first. Trusting list position (as the old releases[0] logic did)
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

func TestVerifyReleaseSignatureMissingAssetWarnsAndContinues(t *testing.T) {
	rel := Release{TagName: "v1.0.0"}
	buf := &bytes.Buffer{}

	if err := verifyReleaseSignature(context.Background(), buf, rel, []byte("checksums"), t.TempDir()); err != nil {
		t.Fatalf("verifyReleaseSignature: %v", err)
	}
	if !strings.Contains(buf.String(), "has no signature") {
		t.Errorf("output = %q, want a no-signature warning", buf.String())
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

	if err := replaceBinary(context.Background(), src, dst); err != nil {
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

func TestResignBinaryNoOpNonDarwin(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "argus")
	if err := os.WriteFile(dst, []byte("binary contents"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := resignBinary(context.Background(), "linux", dst); err != nil {
		t.Errorf("resignBinary(goos=linux) = %v, want nil no-op", err)
	}
}

// TestResignBinaryDarwin exercises the actual signing path issue #124 needs
// (replaceBinary was skipping this entirely) by re-signing a copy of the
// running test binary and confirming codesign reports an ad-hoc signature
// where before there was none.
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
				{"name": "sha256sums.txt", "browser_download_url": "https://github.com/x/sha256sums.txt"}
			]}`, assetName, assetName)
		case strings.HasSuffix(r.URL.Path, "/"+assetName):
			_, _ = w.Write(binContents)
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
