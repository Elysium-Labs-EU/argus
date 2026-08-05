package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/jira"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// jiraTokenURL is where a scoped Jira API token is created — printed by
// `argus jira setup` and referenced from check's failure hints so the fix is
// one click away, not a search.
const jiraTokenURL = "https://id.atlassian.com/manage-profile/security/api-tokens"

// jiraScopeSummary is the minimal-scopes line shared by --jira-issues' flag
// help, `argus jira setup`, and `argus jira check`'s failure hints — see
// docs/jira.md for the full table and how each scope was verified.
const jiraScopeSummary = "minimal token scopes: read:jira-work, write:jira-work, read:jira-user (see docs/jira.md)"

func newJiraCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jira",
		Short: "Set up and validate Jira Cloud credentials",
		Long: `Jira groups the setup and diagnostic surface around the credentials
--jira-issues (see supervise), Transition, Comment, and Assign all share:
resolved from JIRA_BASE_URL/JIRA_EMAIL/JIRA_API_TOKEN, else $JIRA_CONFIG_FILE
or ~/.argus/jira.json.`,
	}
	cmd.AddCommand(newJiraCheckCmd())
	cmd.AddCommand(newJiraSetupCmd())
	return cmd
}

var jiraCmd = newJiraCmd()

// jiraWhoamier is the one method `argus jira check`/`setup`/doctor need from
// a Jira client — defined here at the point of use (see STYLE.md) rather
// than depending on the concrete *jira.Client, so tests can supply a fake
// that never touches the network. *jira.Client already satisfies it.
type jiraWhoamier interface {
	Whoami(ctx context.Context) (jira.WhoamiResult, error)
}

// jiraCredentialCategory classifies a failed credential check as far as the
// HTTP response allows — see checkJiraCredentials.
type jiraCredentialCategory int

const (
	jiraOK jiraCredentialCategory = iota
	jiraMisconfigured
	jiraDeadToken
	jiraMissingScope
)

// jiraCredentialResult is checkJiraCredentials' outcome: a category plus the
// detail string `argus jira check`, `argus jira setup`, and `argus doctor`
// all render the same way.
type jiraCredentialResult struct {
	err      error
	detail   string
	category jiraCredentialCategory
}

// checkJiraCredentials is the live check `argus jira check`, `argus jira
// setup`, and `argus doctor` all share: it resolves a client via newClient,
// then performs a real GET /rest/api/3/myself (through Whoami) so tenant
// resolution and auth are both exercised, not just checked for non-empty
// fields. newClient's own error (a missing field, an unresolvable
// /_edge/tenant_info, a bad base_url) always lands in jiraMisconfigured — it
// never reaches Jira at all, so there is no status code to classify by. A
// 403, or a 401 whose body mentions "scope", is classified as
// jiraMissingScope; a bare 401 is classified as jiraDeadToken. This is a
// best-effort split: the client cannot truly distinguish a dead token from a
// missing scope from a bare 401 with an empty body (see authHint in
// internal/jira), so a bare 401 defaults to the more common case rather than
// claiming certainty.
func checkJiraCredentials(ctx context.Context, newClient func() (jiraWhoamier, error)) jiraCredentialResult {
	client, err := newClient()
	if err != nil {
		return jiraCredentialResult{category: jiraMisconfigured, detail: err.Error(), err: err}
	}

	who, err := client.Whoami(ctx)
	if err != nil {
		if apiErr, ok := errors.AsType[*jira.APIError](err); ok {
			switch {
			case apiErr.StatusCode == http.StatusForbidden, apiErr.StatusCode == http.StatusUnauthorized && jiraScopeShaped(apiErr.Message):
				return jiraCredentialResult{category: jiraMissingScope, detail: err.Error(), err: err}
			case apiErr.StatusCode == http.StatusUnauthorized:
				return jiraCredentialResult{category: jiraDeadToken, detail: err.Error(), err: err}
			}
		}
		return jiraCredentialResult{category: jiraMisconfigured, detail: err.Error(), err: err}
	}

	detail := who.DisplayName
	if detail == "" {
		detail = who.AccountID
	}
	return jiraCredentialResult{
		category: jiraOK,
		detail:   fmt.Sprintf("%s (%s) via %s", detail, who.AccountID, who.APIBase),
	}
}

// jiraScopeShaped reports whether a 401's own error message reads like a
// missing-scope rejection rather than a dead/revoked token — the one signal
// available to tell them apart when the status code alone (unlike a 403) is
// ambiguous.
func jiraScopeShaped(message string) bool {
	return strings.Contains(strings.ToLower(message), "scope")
}

// newJiraFromEnv adapts jira.NewFromEnv to checkJiraCredentials' newClient
// shape (jiraWhoamier, not the concrete *jira.Client) — the production path
// both `argus jira check` and `argus doctor` use.
func newJiraFromEnv() (jiraWhoamier, error) {
	return jira.NewFromEnv(nil)
}

func newJiraCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate Jira credentials with a live GET /rest/api/3/myself",
		Long: `Check resolves Jira credentials the same way --jira-issues (and Transition/
Comment/Assign) do — JIRA_BASE_URL/JIRA_EMAIL/JIRA_API_TOKEN, else
$JIRA_CONFIG_FILE or ~/.argus/jira.json — then performs a real GET
/rest/api/3/myself through the same resolve+auth+tenant path every other
Jira call uses, not just a check that the fields are non-empty.

On success it prints the resolved account and the
api.atlassian.com/ex/jira/{cloudId} base, confirming tenant resolution too.
On failure it reports one of three categories: misconfigured (missing field,
bad base_url, an unresolvable tenant lookup), a dead/revoked token (401), or
the wrong site / a missing scope (403, or a 401 whose body reads
scope-shaped) — see docs/jira.md for the minimal scopes each argus call
needs.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJiraCheck(cmd, newJiraFromEnv)
		},
	}
}

func runJiraCheck(cmd *cobra.Command, newClient func() (jiraWhoamier, error)) error {
	result := checkJiraCredentials(cmd.Context(), newClient)
	return printJiraCheckResult(cmd.OutOrStdout(), result)
}

// printJiraCheckResult renders a jiraCredentialResult the way `argus jira
// check` and `argus jira setup` (after writing its file) both need: a
// success line on stdout, or a *ui.UserError carrying a category-specific
// fix hint so main.go's error rendering shows it without this command
// needing its own error-printing path.
func printJiraCheckResult(out io.Writer, result jiraCredentialResult) error {
	if result.category == jiraOK {
		_, _ = fmt.Fprintf(out, "%s %s\n", ui.LabelSuccess.Render("✓"), result.detail)
		return nil
	}
	if result.category == jiraDeadToken {
		return &ui.UserError{
			Err:  fmt.Errorf("jira token appears dead/revoked: %w", result.err),
			Hint: "create a new token at " + jiraTokenURL + " and run `argus jira setup`",
		}
	}
	if result.category == jiraMissingScope {
		return &ui.UserError{
			Err:  fmt.Errorf("wrong site or missing scope: %w", result.err),
			Hint: jiraScopeSummary + "; recreate the token with the right scopes/site and run `argus jira setup`",
		}
	}
	// jiraMisconfigured, the only category left.
	return &ui.UserError{
		Err:  fmt.Errorf("jira misconfigured: %w", result.err),
		Hint: "argus jira setup",
	}
}

func newJiraSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactively write ~/.argus/jira.json and validate it",
		Long: `Setup prompts for base_url (a bare acme.atlassian.net is fine — argus resolves
the api.atlassian.com/ex/jira/{cloudId} form itself, see internal/jira),
email, and an API token, writes them to ~/.argus/jira.json (0600), then
immediately runs the same live check as ` + "`argus jira check`" + ` and prints its
result — so a bad token is caught here, not minutes into a real dispatch.

Create a token at ` + jiraTokenURL + `.
Prefer a scoped API token (site-bound, explicit scopes) over a classic token
(full account access): ` + jiraScopeSummary + `. Neither
token type can be rotated in place — an expired or compromised token means
create a new one and delete the old one, not "renew".`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJiraSetup(cmd, &jiraSetupArgs{})
		},
	}
}

// jiraSetupArgs holds runJiraSetup's dependencies so it is testable without
// touching the real ~/.argus/jira.json, the real clock, or the network — the
// same pattern doctorArgs uses for runDoctor. A nil field falls back to the
// real implementation in withDefaults.
type jiraSetupArgs struct {
	now        func() time.Time
	newClient  func(cfg jira.Config) (jiraWhoamier, error)
	configPath string
}

func (a *jiraSetupArgs) withDefaults() {
	if a.now == nil {
		a.now = time.Now
	}
	if a.newClient == nil {
		a.newClient = func(cfg jira.Config) (jiraWhoamier, error) {
			return jira.New(cfg.BaseURL, cfg.Email, cfg.APIToken, nil), nil
		}
	}
}

func runJiraSetup(cmd *cobra.Command, a *jiraSetupArgs) error {
	a.withDefaults()
	out := cmd.OutOrStdout()
	reader := bufio.NewReader(cmd.InOrStdin())

	_, _ = fmt.Fprintf(out, "Create a token at %s\n", ui.TextCommand.Render(jiraTokenURL))
	_, _ = fmt.Fprintf(out, "%s\n\n", ui.TextMuted.Render(jiraScopeSummary))

	baseURL := promptRequiredLine(reader, out, "base_url (e.g. acme.atlassian.net or https://acme.atlassian.net)")
	email := promptRequiredLine(reader, out, "email")
	token := promptRequiredLine(reader, out, "api_token")
	if baseURL == "" || email == "" || token == "" {
		return &ui.UserError{
			Err:  fmt.Errorf("jira setup: base_url, email, and api_token are all required"),
			Hint: "argus jira setup",
		}
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + baseURL
	}

	path := a.configPath
	if path == "" {
		p, err := jira.DefaultConfigPath()
		if err != nil {
			return err
		}
		path = p
	}

	cfg := jira.Config{
		BaseURL:   baseURL,
		Email:     email,
		APIToken:  token,
		CreatedAt: a.now().UTC().Format(time.RFC3339),
	}
	if err := jira.SaveConfig(path, cfg); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%s wrote %s\n", ui.LabelSuccess.Render("✓"), path)

	result := checkJiraCredentials(cmd.Context(), func() (jiraWhoamier, error) { return a.newClient(cfg) })
	return printJiraCheckResult(out, result)
}

// promptRequiredLine reads one non-blank line from reader, re-prompting on a
// blank one, and returns it trimmed. A read error (EOF from a closed or
// scripted stdin) ends the loop instead of spinning forever, returning "" so
// the caller's own validation surfaces a clear error rather than hanging.
func promptRequiredLine(reader *bufio.Reader, out io.Writer, label string) string {
	for {
		_, _ = fmt.Fprintf(out, "%s: ", label)
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
		if err != nil {
			return ""
		}
		_, _ = fmt.Fprintf(out, "  %s\n", ui.TextMuted.Render("required"))
	}
}
