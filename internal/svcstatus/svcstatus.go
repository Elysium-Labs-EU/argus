// Package svcstatus turns a failed git-host request into a pointer at that
// host's public status page, so an operator who hits a 5xx or a dropped
// connection knows where to look instead of guessing it is an argus bug.
//
// It deliberately does no health-checking of its own: it maps a host to its
// status page URL and decides which failures are worth mentioning it for.
// Whether the host is actually down is a question the operator answers by
// opening the page — cheaper and more reliable than argus trying to parse
// someone's status dashboard.
package svcstatus

import (
	"fmt"
	"strings"
)

// statusPages maps a git host to its public status page.
var statusPages = map[string]string{
	"github.com":   "https://www.githubstatus.com",
	"gitlab.com":   "https://status.gitlab.com",
	"codeberg.org": "https://status.codeberg.org/status/codeberg",
}

// pageURL returns the status page for host, or "" if none is known. A
// subdomain of a known host (e.g. ci.codeberg.org) resolves to the same page.
func pageURL(host string) string {
	if u, ok := statusPages[host]; ok {
		return u
	}
	for h, u := range statusPages {
		if strings.HasSuffix(host, "."+h) {
			return u
		}
	}
	return ""
}

// WorthMentioning reports whether a non-2xx statusCode looks host-shaped
// rather than caller-shaped. Only the 5xx range qualifies — a 400 or 404 is
// the caller's problem (bad request, wrong path) and a status page won't
// explain it. Network-level failures (no response at all) are always
// host-shaped and skip this check entirely; it exists only to gate the
// non-2xx case.
func WorthMentioning(statusCode int) bool {
	return statusCode >= 500 && statusCode < 600
}

// Note returns a clause to append to an error message, pointing at host's
// status page, or "" when host has no known page. It does not claim the host
// is down — only that the page is where to confirm whether it is, since a
// request failure this points at (a dropped connection, a rejected push) can
// just as easily be local.
func Note(host string) string {
	u := pageURL(host)
	if u == "" {
		return ""
	}
	return fmt.Sprintf(" (check %s if this persists)", u)
}
