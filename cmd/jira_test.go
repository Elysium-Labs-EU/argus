package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/jira"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

// fakeJiraWhoamier is the jiraWhoamier test double every check/setup/doctor
// test in this package shares — it never touches the network, unlike a real
// *jira.Client.
type fakeJiraWhoamier struct {
	err error
	who jira.WhoamiResult
}

func (f *fakeJiraWhoamier) Whoami(context.Context) (jira.WhoamiResult, error) {
	return f.who, f.err
}

func TestCheckJiraCredentialsSuccess(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", DisplayName: "Dev Person", APIBase: "https://api.atlassian.com/ex/jira/cloud-1"}}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraOK {
		t.Fatalf("category = %v, want jiraOK", result.category)
	}
	if !strings.Contains(result.detail, "Dev Person") || !strings.Contains(result.detail, "acc-1") || !strings.Contains(result.detail, "cloud-1") {
		t.Errorf("detail = %q, want account + api base", result.detail)
	}
}

func TestCheckJiraCredentialsSuccessNoDisplayName(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", APIBase: "https://api.atlassian.com/ex/jira/cloud-1"}}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraOK {
		t.Fatalf("category = %v, want jiraOK", result.category)
	}
	if !strings.Contains(result.detail, "acc-1") {
		t.Errorf("detail = %q, want accountID fallback", result.detail)
	}
}

func TestCheckJiraCredentialsMisconfigured(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return nil, errNewFromEnv
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraMisconfigured {
		t.Fatalf("category = %v, want jiraMisconfigured", result.category)
	}
	if !strings.Contains(result.detail, errNewFromEnv.Error()) {
		t.Errorf("detail = %q, want the underlying error", result.detail)
	}
}

func TestCheckJiraCredentialsDeadToken(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{err: &jira.APIError{StatusCode: 401, Prefix: "jira", Status: "401 Unauthorized", Message: ""}}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraDeadToken {
		t.Fatalf("category = %v, want jiraDeadToken", result.category)
	}
}

func TestCheckJiraCredentialsForbidden(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{err: &jira.APIError{StatusCode: 403, Prefix: "jira", Status: "403 Forbidden", Message: "not permitted"}}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraMissingScope {
		t.Fatalf("category = %v, want jiraMissingScope", result.category)
	}
}

func TestCheckJiraCredentialsScopeShaped401(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{err: &jira.APIError{StatusCode: 401, Prefix: "jira", Status: "401 Unauthorized", Message: "insufficient_scope: requires read:jira-user"}}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraMissingScope {
		t.Fatalf("category = %v, want jiraMissingScope for a scope-shaped 401 body", result.category)
	}
}

// TestCheckJiraCredentialsNonAPIError covers Whoami failing with an error
// that isn't a *jira.APIError at all (e.g. resolvedBase's tenant_info/
// bad-base_url failure) landing in jiraMisconfigured, same as newClient's
// own error.
func TestCheckJiraCredentialsNonAPIError(t *testing.T) {
	newClient := func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{err: errNewFromEnv}, nil
	}
	result := checkJiraCredentials(context.Background(), newClient)
	if result.category != jiraMisconfigured {
		t.Fatalf("category = %v, want jiraMisconfigured for a non-APIError failure", result.category)
	}
}

// errNewFromEnv is a stand-in "credentials didn't even resolve" error shared
// by several tests above.
var errNewFromEnv = &testError{"jira: set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func TestPrintJiraCheckResult(t *testing.T) {
	tests := []struct {
		wantHint    string
		result      jiraCredentialResult
		wantSuccess bool
	}{
		{result: jiraCredentialResult{category: jiraOK, detail: "Dev (acc-1) via https://api.atlassian.com/ex/jira/cloud-1"}, wantSuccess: true},
		{result: jiraCredentialResult{category: jiraDeadToken, err: errNewFromEnv}, wantHint: jiraTokenURL},
		{result: jiraCredentialResult{category: jiraMissingScope, err: errNewFromEnv}, wantHint: "docs/jira.md"},
		{result: jiraCredentialResult{category: jiraMisconfigured, err: errNewFromEnv}, wantHint: "argus jira setup"},
	}
	for _, tc := range tests {
		buf := &bytes.Buffer{}
		err := printJiraCheckResult(buf, tc.result)
		if tc.wantSuccess {
			if err != nil {
				t.Errorf("category %v: want no error, got %v", tc.result.category, err)
			}
			if !strings.Contains(buf.String(), tc.result.detail) {
				t.Errorf("category %v: output = %q, want detail %q", tc.result.category, buf.String(), tc.result.detail)
			}
			continue
		}
		if err == nil {
			t.Fatalf("category %v: want an error", tc.result.category)
		}
		uerr, ok := errors.AsType[*ui.UserError](err)
		if !ok {
			t.Fatalf("category %v: want *ui.UserError, got %T", tc.result.category, err)
		}
		if tc.wantHint != "" && !strings.Contains(uerr.Hint, tc.wantHint) {
			t.Errorf("category %v: hint = %q, want to contain %q", tc.result.category, uerr.Hint, tc.wantHint)
		}
	}
}

func TestRunJiraCheckSuccess(t *testing.T) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetContext(context.Background())

	err := runJiraCheck(cmd, func() (jiraWhoamier, error) {
		return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", DisplayName: "Dev", APIBase: "https://api.atlassian.com/ex/jira/cloud-1"}}, nil
	})
	if err != nil {
		t.Fatalf("runJiraCheck: %v", err)
	}
	if !strings.Contains(buf.String(), "Dev (acc-1)") {
		t.Errorf("output = %q, want the resolved account", buf.String())
	}
}

func TestRunJiraCheckFailure(t *testing.T) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetContext(context.Background())

	err := runJiraCheck(cmd, func() (jiraWhoamier, error) {
		return nil, errNewFromEnv
	})
	if err == nil {
		t.Fatal("want an error for a misconfigured client")
	}
}

// TestJiraCheckCommandEndToEnd drives newJiraCheckCmd through cobra with the
// real newJiraFromEnv wiring and no credentials configured, covering the
// constructor and its RunE.
func TestJiraCheckCommandEndToEnd(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))

	cmd := newJiraCheckCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	if err := cmd.Execute(); err == nil {
		t.Fatal("want an error with no Jira credentials configured")
	}
}

func TestPromptRequiredLine(t *testing.T) {
	t.Run("returns the first non-blank line", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("  hello  \n"))
		buf := &bytes.Buffer{}
		got := promptRequiredLine(reader, buf, "label")
		if got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("re-prompts past a blank line", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader("\n\nvalue\n"))
		buf := &bytes.Buffer{}
		got := promptRequiredLine(reader, buf, "label")
		if got != "value" {
			t.Errorf("got %q, want %q", got, "value")
		}
		if strings.Count(buf.String(), "required") != 2 {
			t.Errorf("output = %q, want two re-prompts", buf.String())
		}
	})

	t.Run("EOF on a blank line returns empty instead of hanging", func(t *testing.T) {
		reader := bufio.NewReader(strings.NewReader(""))
		buf := &bytes.Buffer{}
		got := promptRequiredLine(reader, buf, "label")
		if got != "" {
			t.Errorf("got %q, want empty on EOF", got)
		}
	})
}

func TestRunJiraSetupWritesConfigAndRunsCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jira.json")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	cmd.SetContext(context.Background())

	fixedNow := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	var gotCfg jira.Config
	a := &jiraSetupArgs{
		configPath: path,
		now:        func() time.Time { return fixedNow },
		newClient: func(cfg jira.Config) (jiraWhoamier, error) {
			gotCfg = cfg
			return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", DisplayName: "Dev", APIBase: "https://api.atlassian.com/ex/jira/cloud-1"}}, nil
		},
	}

	if err := runJiraSetup(cmd, a); err != nil {
		t.Fatalf("runJiraSetup: %v", err)
	}

	if gotCfg.BaseURL != "https://acme.atlassian.net" {
		t.Errorf("BaseURL = %q, want https:// prepended to the bare domain", gotCfg.BaseURL)
	}
	if gotCfg.Email != "dev@example.com" || gotCfg.APIToken != "secret-token" {
		t.Errorf("cfg = %+v", gotCfg)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	var onDisk jira.Config
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("unmarshaling written config: %v", err)
	}
	if onDisk.CreatedAt != fixedNow.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", onDisk.CreatedAt, fixedNow.Format(time.RFC3339))
	}

	if !strings.Contains(out.String(), "wrote "+path) {
		t.Errorf("output = %q, want confirmation of the write", out.String())
	}
	if !strings.Contains(out.String(), "Dev (acc-1)") {
		t.Errorf("output = %q, want the inline check's success line", out.String())
	}
}

func TestRunJiraSetupAcceptsFullURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jira.json")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("https://acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	cmd.SetContext(context.Background())

	var gotCfg jira.Config
	a := &jiraSetupArgs{
		configPath: path,
		newClient: func(cfg jira.Config) (jiraWhoamier, error) {
			gotCfg = cfg
			return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", APIBase: "b"}}, nil
		},
	}
	if err := runJiraSetup(cmd, a); err != nil {
		t.Fatalf("runJiraSetup: %v", err)
	}
	if gotCfg.BaseURL != "https://acme.atlassian.net" {
		t.Errorf("BaseURL = %q, want the URL kept as typed", gotCfg.BaseURL)
	}
}

func TestRunJiraSetupSurfacesCheckFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jira.json")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nbad-token\n"))
	cmd.SetContext(context.Background())

	a := &jiraSetupArgs{
		configPath: path,
		newClient: func(cfg jira.Config) (jiraWhoamier, error) {
			return &fakeJiraWhoamier{err: &jira.APIError{StatusCode: 401, Prefix: "jira", Status: "401 Unauthorized"}}, nil
		},
	}
	err := runJiraSetup(cmd, a)
	if err == nil {
		t.Fatal("want the inline check's failure surfaced")
	}
	// The file must still be written even though the inline check failed —
	// setup's job is to persist what the operator typed, not to gate the
	// write on a live network call succeeding.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("expected the config file to be written despite the check failing: %v", statErr)
	}
}

func TestRunJiraSetupRequiresAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jira.json")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("")) // EOF immediately: every prompt comes back blank
	cmd.SetContext(context.Background())

	a := &jiraSetupArgs{configPath: path}
	if err := runJiraSetup(cmd, a); err == nil {
		t.Fatal("want an error when required fields are left blank")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("expected no config file written when required fields are missing")
	}
}

// TestRunJiraSetupDefaultConfigPathError covers jira.DefaultConfigPath
// itself failing (no $HOME, no $JIRA_CONFIG_FILE) surfacing as runJiraSetup's
// own error, before any write is attempted.
func TestRunJiraSetupDefaultConfigPathError(t *testing.T) {
	t.Setenv("JIRA_CONFIG_FILE", "")
	t.Setenv("HOME", "")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	cmd.SetContext(context.Background())

	a := &jiraSetupArgs{}
	if err := runJiraSetup(cmd, a); err == nil {
		t.Fatal("want an error when DefaultConfigPath itself fails")
	}
}

// TestRunJiraSetupSaveConfigError covers jira.SaveConfig failing (a path
// component that already exists as a regular file) surfacing as
// runJiraSetup's own error.
func TestRunJiraSetupSaveConfigError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	path := filepath.Join(blocker, "jira.json")

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	cmd.SetContext(context.Background())

	a := &jiraSetupArgs{configPath: path}
	if err := runJiraSetup(cmd, a); err == nil {
		t.Fatal("want an error when SaveConfig fails")
	}
}

// TestRunJiraSetupDefaultConfigPath covers runJiraSetup falling back to
// jira.DefaultConfigPath when configPath is left unset, by pointing
// JIRA_CONFIG_FILE at a temp file so it never touches a real ~/.argus.
func TestRunJiraSetupDefaultConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jira.json")
	t.Setenv("JIRA_CONFIG_FILE", path)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	cmd.SetContext(context.Background())

	a := &jiraSetupArgs{
		newClient: func(cfg jira.Config) (jiraWhoamier, error) {
			return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", APIBase: "b"}}, nil
		},
	}
	if err := runJiraSetup(cmd, a); err != nil {
		t.Fatalf("runJiraSetup: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the default-resolved config path to be written: %v", err)
	}
}

// TestJiraSetupDefaultsWired exercises withDefaults' real newClient closure
// against a live-but-unreachable base_url, covering the default without
// hitting the real network for a real credential.
func TestJiraSetupDefaultsWired(t *testing.T) {
	a := &jiraSetupArgs{}
	a.withDefaults()
	if a.now == nil || a.newClient == nil {
		t.Fatal("withDefaults left a boundary nil")
	}
	client, err := a.newClient(jira.Config{BaseURL: "https://example.invalid", Email: "e", APIToken: "t"})
	if err != nil {
		t.Fatalf("default newClient: %v", err)
	}
	if client == nil {
		t.Fatal("default newClient returned a nil client")
	}
}

// TestJiraSetupCommandEndToEnd drives newJiraSetupCmd through cobra, covering
// the constructor and its RunE, writing to a temp JIRA_CONFIG_FILE so it
// never touches a real ~/.argus.
func TestJiraSetupCommandEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jira.json")
	t.Setenv("JIRA_CONFIG_FILE", path)

	cmd := newJiraSetupCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetIn(strings.NewReader("acme.atlassian.net\ndev@example.com\nsecret-token\n"))
	// The real newClient hits the network; a bogus host resolves to a
	// misconfigured/failed inline check, which is fine — this test only
	// covers the constructor and flag wiring, not a live Jira call.
	_ = cmd.Execute()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the config file to be written: %v", err)
	}
}

// TestNewJiraCmdRegistersSubcommands covers newJiraCmd wiring both
// subcommands, and newJiraFromEnv adapting jira.NewFromEnv to jiraWhoamier.
func TestNewJiraCmdRegistersSubcommands(t *testing.T) {
	cmd := newJiraCmd()
	names := map[string]bool{}
	for _, c := range cmd.Commands() {
		names[c.Name()] = true
	}
	if !names["check"] || !names["setup"] {
		t.Errorf("subcommands = %v, want check and setup", names)
	}
}

func TestNewJiraFromEnv(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "https://acme.atlassian.net")
	t.Setenv("JIRA_EMAIL", "dev@example.com")
	t.Setenv("JIRA_API_TOKEN", "secret-token")
	client, err := newJiraFromEnv()
	if err != nil {
		t.Fatalf("newJiraFromEnv: %v", err)
	}
	if client == nil {
		t.Fatal("newJiraFromEnv returned a nil client")
	}
}

func TestJiraScopeShaped(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{"insufficient_scope", true},
		{"Scope missing", true},
		{"", false},
		{"not authenticated", false},
	}
	for _, tc := range tests {
		if got := jiraScopeShaped(tc.message); got != tc.want {
			t.Errorf("jiraScopeShaped(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}
