package forge

import (
	"fmt"
	"os"
	"strings"
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
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "", "", "", fmt.Errorf("cannot parse host/path from remote %q", remoteURL)
		}
		host = rest[:slash]
		owner, repo, err = splitOwnerRepo(rest[slash+1:])
	case strings.Contains(s, ":"):
		// scp form: [user@]host:owner/repo
		colon := strings.IndexByte(s, ':')
		if colon < 0 {
			return "", "", "", fmt.Errorf("unrecognized remote URL %q", remoteURL)
		}
		hostPart := s[:colon]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = hostPart
		owner, repo, err = splitOwnerRepo(s[colon+1:])
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

// TokenForHost returns the API token for a forge host from the environment,
// checking host-specific variables before a generic fallback. github.com uses
// GITHUB_TOKEN or GH_TOKEN; codeberg.org uses CODEBERG_TOKEN; gitlab.com uses
// GITLAB_TOKEN; any other host uses <HOST>_TOKEN (non-alphanumerics as
// underscores) then FORGE_TOKEN.
func TokenForHost(host string) string {
	for _, key := range tokenVarsForHost(host) {
		if v := os.Getenv(key); v != "" {
			return v
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
