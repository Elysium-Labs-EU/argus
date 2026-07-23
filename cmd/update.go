package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/Elysium-Labs-EU/argus/internal/buildinfo"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

const argusRepo = "Elysium-Labs-EU/argus"

var httpClient = &http.Client{
	Timeout: 15 * time.Second,
}

// Asset is one file attached to a GitHub release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// Release is the subset of GitHub's release API response argus needs.
type Release struct {
	TagName    string  `json:"tag_name"`
	Assets     []Asset `json:"assets"`
	Prerelease bool    `json:"prerelease"`
}

// AssetFor returns the release asset for argus on platform (a
// "<goos>-<goarch>" pair such as "linux-amd64" or "darwin-arm64").
func (r Release) AssetFor(platform string) (Asset, bool) {
	want := fmt.Sprintf("argus-%s", platform)
	for _, a := range r.Assets {
		if a.Name == want {
			return a, true
		}
	}
	return Asset{}, false
}

// ChecksumsAsset returns the sha256sums.txt asset, if the release has one.
func (r Release) ChecksumsAsset() (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == "sha256sums.txt" {
			return a, true
		}
	}
	return Asset{}, false
}

// doReleaseRequest issues a GET against reqURL, a hardcoded GitHub API URL.
func doReleaseRequest(ctx context.Context, reqURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building release request: %w", err)
	}

	resp, err := httpClient.Do(req) // #nosec G704 -- URL is constructed from a hardcoded GitHub API base, not user input
	if err != nil {
		return nil, fmt.Errorf("fetching latest release: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("fetching latest release: nil response")
	}
	return resp, nil
}

// listReleases fetches every release from GitHub's list endpoint. The list
// is documented as newest-first but has been observed live to return an
// entry out of order (a freshly created release landed 3rd, not 1st), so
// callers must never trust list position and instead pick by semver (see
// highestBySemver).
func listReleases(ctx context.Context) ([]Release, error) {
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/releases", argusRepo)
	resp, err := doReleaseRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching latest release: unexpected status %s", resp.Status)
	}

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding release response: %w", err)
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("no releases found")
	}
	return releases, nil
}

// latestStableRelease fetches GitHub's "latest" endpoint, which only ever
// returns a stable (non-prerelease) release. ok is false when the endpoint
// 404s, which happens whenever every published release is a pre-release.
func latestStableRelease(ctx context.Context) (rel Release, ok bool, err error) {
	reqURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", argusRepo)
	resp, err := doReleaseRequest(ctx, reqURL)
	if err != nil {
		return Release{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Release{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, false, fmt.Errorf("fetching latest release: unexpected status %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, false, fmt.Errorf("decoding release response: %w", err)
	}
	return rel, true, nil
}

// highestBySemver returns the release with the highest tag_name among
// releases, comparing by semver rather than list order. Releases whose
// tag_name isn't a valid semver are ignored.
func highestBySemver(releases []Release) (Release, error) {
	var best Release
	found := false
	for _, r := range releases {
		v := normalizeSemver(r.TagName)
		if !semver.IsValid(v) {
			continue
		}
		if !found || semver.Compare(v, normalizeSemver(best.TagName)) > 0 {
			best, found = r, true
		}
	}
	if !found {
		return Release{}, fmt.Errorf("no release with a valid semver tag found")
	}
	return best, nil
}

// fetchLatestRelease fetches the latest argus release from GitHub.
//
// When includePre is true, pre-releases are eligible candidates: it lists
// every release and picks the highest by semver, stable or not.
//
// When includePre is false, it prefers GitHub's "latest" endpoint, which
// excludes pre-releases entirely. That endpoint 404s if every published
// release is a pre-release; when it does, this falls back to the full
// release list and picks the highest stable release by semver, or the
// highest pre-release if no stable release exists at all.
func fetchLatestRelease(ctx context.Context, includePre bool) (Release, error) {
	if includePre {
		releases, err := listReleases(ctx)
		if err != nil {
			return Release{}, err
		}
		return highestBySemver(releases)
	}

	if rel, ok, err := latestStableRelease(ctx); err != nil {
		return Release{}, err
	} else if ok {
		return rel, nil
	}

	releases, err := listReleases(ctx)
	if err != nil {
		return Release{}, err
	}
	var stable []Release
	for _, r := range releases {
		if !r.Prerelease {
			stable = append(stable, r)
		}
	}
	if len(stable) > 0 {
		return highestBySemver(stable)
	}
	return highestBySemver(releases)
}

// downloadFile fetches downloadURL to destPath. It refuses anything but a
// plain https://github.com URL, since this is used to fetch and then
// execute-in-place a new argus binary.
func downloadFile(ctx context.Context, downloadURL, destPath string) error {
	u, err := url.Parse(downloadURL)
	if err != nil {
		return fmt.Errorf("parsing download URL: %w", err)
	}
	if u.Scheme != "https" || u.Host != "github.com" {
		return fmt.Errorf("refusing to download from untrusted host %q", u.Host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("building download request: %w", err)
	}

	resp, err := httpClient.Do(req) // #nosec G704 -- downloadURL is validated above to be https://github.com
	if err != nil {
		return fmt.Errorf("downloading %s: %w", downloadURL, err)
	}
	if resp == nil {
		return fmt.Errorf("downloading %s: nil response", downloadURL)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: unexpected status %s", downloadURL, resp.Status)
	}

	out, err := os.Create(destPath) //nolint:gosec // destPath is a caller-controlled temp path
	if err != nil {
		return fmt.Errorf("creating %s: %w", destPath, err)
	}
	defer func() { _ = out.Close() }()

	n, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return fmt.Errorf("downloading %s: got %d bytes, expected %d", downloadURL, n, resp.ContentLength)
	}
	return nil
}

// verifyChecksum checks binaryPath's sha256 against the entry for assetName
// in a sha256sums.txt file's contents (the standard `sha256sum` output
// format: "<hex digest>  <filename>" per line).
func verifyChecksum(binaryPath, checksumsContent, assetName string) error {
	var want string
	for line := range strings.SplitSeq(checksumsContent, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum entry for %s", assetName)
	}

	f, err := os.Open(binaryPath) //nolint:gosec // binaryPath is a caller-controlled temp path
	if err != nil {
		return fmt.Errorf("opening %s: %w", binaryPath, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing %s: %w", binaryPath, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, want, got)
	}
	return nil
}

// copyFile copies src to dst, creating or truncating dst, preserving src's
// file mode.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	in, err := os.Open(src) //nolint:gosec // caller-controlled paths
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode()) //nolint:gosec // caller-controlled paths
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying to %s: %w", dst, err)
	}
	return nil
}

// replaceBinary installs newPath over dstPath, which may be the currently
// running executable: it copies to a same-directory temp file, chmods it
// executable, then renames over dstPath. The rename is atomic on the same
// filesystem, and the OS keeps the old inode open for any process (e.g. the
// one calling this function) that's already running it.
func replaceBinary(newPath, dstPath string) error {
	tmp := dstPath + ".tmp"
	if err := copyFile(newPath, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil { //nolint:gosec // installed binary must be executable
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing %s: %w", dstPath, err)
	}
	return nil
}

// checkWritable verifies dir is writable by creating and removing a probe
// file in it.
func checkWritable(dir string) error {
	probe := filepath.Join(dir, ".argus-write-check")
	f, err := os.Create(probe) //nolint:gosec // fixed probe filename in a caller-controlled dir
	if err != nil {
		return err
	}
	_ = f.Close()
	return os.Remove(probe)
}

// currentBinaryPath returns the resolved path of the running argus binary.
func currentBinaryPath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating running binary: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", fmt.Errorf("resolving running binary path: %w", err)
	}
	return exePath, nil
}

// hostPlatform maps the running GOOS/GOARCH to the platform suffix used in
// release asset names (see .github/workflows/release.yml's build matrix).
func hostPlatform() (string, error) {
	switch runtime.GOOS + "-" + runtime.GOARCH {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
		return runtime.GOOS + "-" + runtime.GOARCH, nil
	default:
		return "", &ui.UserError{Err: fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)}
	}
}

// normalizeSemver prefixes a bare "0.0.1"-style version with "v" so it's
// valid input for golang.org/x/mod/semver, which requires the "v" prefix.
func normalizeSemver(v string) string {
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
}

// runUpdate implements `argus system update` against an explicit exePath, so
// it can be exercised in tests without touching the test binary itself
// (os.Executable() under `go test` is the test binary).
func runUpdate(ctx context.Context, out io.Writer, exePath, currentVersion string, includePre bool) error {
	var rel Release
	err := ui.WithSpinner("Checking for updates...", func() error {
		var err error
		rel, err = fetchLatestRelease(ctx, includePre)
		return err
	})
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	currentVer, latestVer := normalizeSemver(currentVersion), rel.TagName
	if semver.IsValid(currentVer) && semver.IsValid(latestVer) && semver.Compare(currentVer, latestVer) >= 0 {
		_, _ = fmt.Fprintf(out, "%s already on the latest version (%s)\n", ui.LabelSuccess.Render("✓"), currentVersion)
		return nil
	}

	_, _ = fmt.Fprintf(out, "%s new version available: %s -> %s\n", ui.LabelInfo.Render("i"), currentVersion, latestVer)

	platform, err := hostPlatform()
	if err != nil {
		return err
	}
	asset, ok := rel.AssetFor(platform)
	if !ok {
		return &ui.UserError{Err: fmt.Errorf("release %s has no asset for %s", latestVer, platform)}
	}
	checksums, ok := rel.ChecksumsAsset()
	if !ok {
		return &ui.UserError{Err: fmt.Errorf("release %s is missing sha256sums.txt", latestVer)}
	}

	if writeErr := checkWritable(filepath.Dir(exePath)); writeErr != nil {
		return &ui.UserError{
			Err:  fmt.Errorf("%s is not writable: %w", filepath.Dir(exePath), writeErr),
			Hint: "sudo argus system update",
		}
	}

	tmpDir, err := os.MkdirTemp("", "argus-update")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binTmp := filepath.Join(tmpDir, "argus")
	err = ui.WithSpinner(fmt.Sprintf("Downloading %s...", latestVer), func() error {
		return downloadFile(ctx, asset.DownloadURL, binTmp)
	})
	if err != nil {
		return fmt.Errorf("downloading update: %w", err)
	}

	checksumsTmp := filepath.Join(tmpDir, "sha256sums.txt")
	if dlErr := downloadFile(ctx, checksums.DownloadURL, checksumsTmp); dlErr != nil {
		return fmt.Errorf("downloading checksums: %w", dlErr)
	}
	checksumsData, err := os.ReadFile(checksumsTmp) //nolint:gosec // fixed name in an argus-owned temp dir
	if err != nil {
		return fmt.Errorf("reading checksums: %w", err)
	}
	if verifyErr := verifyChecksum(binTmp, string(checksumsData), asset.Name); verifyErr != nil {
		return &ui.UserError{Err: verifyErr}
	}
	_, _ = fmt.Fprintf(out, "%s checksum verified\n", ui.LabelSuccess.Render("✓"))

	backupPath := exePath + ".backup"
	if backupErr := copyFile(exePath, backupPath); backupErr != nil {
		_, _ = fmt.Fprintf(out, "%s could not create backup of the current binary: %v\n", ui.LabelWarning.Render("warning"), backupErr)
	} else {
		_, _ = fmt.Fprintf(out, "%s backed up current binary to %s\n", ui.TextMuted.Render("i"), backupPath)
	}

	if replaceErr := replaceBinary(binTmp, exePath); replaceErr != nil {
		return fmt.Errorf("installing new binary: %w", replaceErr)
	}

	_, _ = fmt.Fprintf(out, "%s updated %s -> %s\n", ui.LabelSuccess.Render("✓"), currentVersion, latestVer)
	return nil
}

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "update",
		Short:   "Download and install the latest argus release",
		Example: "  argus system update        # check and apply latest stable release\n  argus system update --pre  # include pre-releases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			exePath, err := currentBinaryPath()
			if err != nil {
				return err
			}
			includePre, err := cmd.Flags().GetBool("pre")
			if err != nil {
				return err
			}
			return runUpdate(cmd.Context(), cmd.OutOrStdout(), exePath, buildinfo.GetVersionOnly(), includePre)
		},
	}
	cmd.Flags().Bool("pre", false, "include pre-releases in update check")
	return cmd
}

// systemUpdateCmd is a package-level var (like shipCmd, rebaseCmd, worktreePruneCmd,
// ...) so skill_lint_test.go can cross-check its flags against SKILL.md directly,
// the same way it already does for every other documented subcommand.
var systemUpdateCmd = newUpdateCmd()
