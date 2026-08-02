package forge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/credential"
)

// Detect parses a git remote URL into the forge host and the owner/repo. It
// handles the SSH scp form (git@host:owner/repo.git), ssh:// URLs, and https://
// URLs, which is enough for Codeberg, GitHub, and self-hosted Gitea.
func Detect(remoteURL string) (host, owner, repo string, err error) {
	s := strings.TrimSuffix(strings.TrimSpace(remoteURL), ".git")

	switch {
	case strings.Contains(s, "://"):
		// scheme://[user@]host/owner/repo
		rest := s[strings.Index(s, "://")+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		var path string
		var ok bool
		host, path, ok = strings.Cut(rest, "/")
		if !ok {
			return "", "", "", fmt.Errorf("cannot parse host/path from remote %q", remoteURL)
		}
		owner, repo, err = splitOwnerRepo(path)
	case strings.Contains(s, ":"):
		// scp form: [user@]host:owner/repo
		hostPart, path, ok := strings.Cut(s, ":")
		if !ok {
			return "", "", "", fmt.Errorf("unrecognized remote URL %q", remoteURL)
		}
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = hostPart
		owner, repo, err = splitOwnerRepo(path)
	default:
		return "", "", "", fmt.Errorf("unrecognized remote URL %q", remoteURL)
	}
	if err != nil {
		return "", "", "", err
	}
	// A host may carry a port (host:22); strip it for API base construction.
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "", "", "", fmt.Errorf("no host in remote %q", remoteURL)
	}
	return host, owner, repo, nil
}

// splitOwnerRepo takes the trailing path of a remote (possibly nested, e.g.
// group/subgroup/repo) and returns the last two segments as owner and repo.
func splitOwnerRepo(path string) (owner, repo string, err error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-1] == "" || parts[len(parts)-2] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from %q", path)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

// TokenForHost returns the API token for a forge host, checking an operator
// override first, then the environment, then falling back to the same
// credential tooling a human would already have configured. overrides maps
// host -> an alternate env var name that takes priority over the built-in
// list (see internal/credential and cmd's --credential-env /
// `argus config set credential.<host>`); it may be nil. github.com uses
// GITHUB_TOKEN or GH_TOKEN; codeberg.org uses CODEBERG_TOKEN; gitlab.com uses
// GITLAB_TOKEN; any other host uses <HOST>_TOKEN (non-alphanumerics as
// underscores) then FORGE_TOKEN. If none of those env vars is set, it shells
// out to gh/glab/git-credential (see tokenFromHelper) so a caller never has
// to pre-export the secret into argus's own process env — the same
// non-interactive path gh and glab use for their own commands.
func TokenForHost(host string, overrides map[string]string) string {
	vars := credential.EnvVars(host, overrides, tokenVarsForHost(host))
	if v := credential.Lookup(vars); v != "" {
		return v
	}
	return tokenFromHelper(host)
}

// tokenFromHelper is TokenForHost's env-var-miss fallback, split into a
// package var so tests can stub the subprocess boundary without shelling out
// for real. Production wiring is credentialHelperToken.
var tokenFromHelper = credentialHelperToken

// credentialHelperTimeout bounds how long argus waits on gh/glab/git-credential
// subprocesses. A helper with nothing configured for a host should fail fast,
// not stall a ship/supervise invocation.
const credentialHelperTimeout = 3 * time.Second

// credentialHelperToken resolves a token via the credential tooling a human
// running gh/glab already has set up, so an LLM-driven caller never needs to
// materialize the secret into its own shell first. It never prompts
// (GIT_TERMINAL_PROMPT=0 plus a bounded timeout), so an unauthenticated
// caller gets an empty string quickly instead of a hang.
func credentialHelperToken(host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), credentialHelperTimeout)
	defer cancel()

	if host == "github.com" {
		if tok := runTrimmed(ctx, "gh", "auth", "token"); tok != "" {
			return tok
		}
	} else if tok := runTrimmed(ctx, "glab", "config", "get", "token", "--host", host); tok != "" {
		// glab has no "auth token" subcommand; "config get token --host" is
		// the documented way to read what "glab auth login" stored. Tried for
		// every non-GitHub host, not just gitlab.com, since glab logs into
		// self-hosted GitLab the same way and --host disambiguates.
		return tok
	}
	// Covers Codeberg/Gitea (no standard CLI token-export command), a glab
	// login that used --use-keyring (config get won't surface it), and any
	// other self-hosted forge: ask git's own credential-helper chain
	// (osxkeychain, libsecret, the store gh/glab themselves register with, ...).
	return gitCredentialFill(ctx, host)
}

// newFixedCmd builds a command for name with args, GIT_TERMINAL_PROMPT
// disabled so nothing blocks on an interactive credential prompt. name is
// always a fixed literal ("gh", "glab", "git") from this file's own call
// sites, never attacker- or user-controlled input.
func newFixedCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is a fixed literal from this file's own call sites, not user input
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// runTrimmed runs name with args and returns its trimmed stdout, or "" on any
// failure (binary missing, not authenticated, non-zero exit).
func runTrimmed(ctx context.Context, name string, args ...string) string {
	out, err := newFixedCmd(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitCredentialFill asks `git credential fill` for a password on host via
// git's credential.helper chain and returns it, or "" if no helper answers.
func gitCredentialFill(ctx context.Context, host string) string {
	cmd := newFixedCmd(ctx, "git", "credential", "fill")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("protocol=https\nhost=%s\n\n", host))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseCredentialPassword(string(out))
}

// parseCredentialPassword extracts the password= value from `git credential
// fill` output (git's key=value-per-line protocol). Split out from
// gitCredentialFill so the parsing logic is unit-testable without a subprocess.
func parseCredentialPassword(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(line, "password="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// tokenVarsForHost is the ordered list of environment variables TokenForHost
// consults for host — the single place that knows which variables hold a forge
// token.
func tokenVarsForHost(host string) []string {
	switch host {
	case "github.com":
		return []string{"GITHUB_TOKEN", "GH_TOKEN"}
	case "codeberg.org":
		return []string{"CODEBERG_TOKEN"}
	case "gitlab.com":
		return []string{"GITLAB_TOKEN"}
	default:
		return []string{envKey(host) + "_TOKEN", "FORGE_TOKEN"}
	}
}

// StandardTokenVars are the credential environment variables argus knows by
// name and a worker never needs: forge API tokens for the hosts it supports
// (GitHub, Codeberg, GitLab, plus the generic self-hosted fallback) and the
// Jira issue-source API token. It is the authority the supervisor uses to
// scrub these secrets from a worker's environment — argus itself does the
// forge API calls (fetching issues, opening PRs) and the Jira issue fetch on
// the host, never inside the worker pane, so the safe default is to withhold
// them. Host-specific custom variables (<HOST>_TOKEN for a self-hosted Gitea)
// are deliberately not included — they are unknowable here without the
// remote, and the generic FORGE_TOKEN covers the common self-hosted case.
// JIRA_BASE_URL and JIRA_EMAIL are not included: they are non-secret config,
// not credentials.
func StandardTokenVars() []string {
	return []string{"CODEBERG_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "FORGE_TOKEN", "JIRA_API_TOKEN"}
}

// envKey turns a host into an environment-variable-safe upper-case fragment:
// gitea.example.com -> GITEA_EXAMPLE_COM.
func envKey(host string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(host) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
