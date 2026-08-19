package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/jira"
)

// runDoctorArgs drives runDoctor with a throwaway command carrying buf as its
// output, returning what doctor printed plus the exit error.
func runDoctorArgs(t *testing.T, a *doctorArgs) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	err := runDoctor(cmd, a)
	return buf.String(), err
}

// found/notFound are lookPath stubs for the two binary checks.
func found(string) (string, error)    { return "/usr/local/bin/x", nil }
func notFound(string) (string, error) { return "", errors.New("not found") }

// writeAllowlist drops a .claude/settings.json into repo that covers argus.
func writeAllowlist(t *testing.T, repo string) {
	t.Helper()
	dir := filepath.Join(repo, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(argus *)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRepoConfig drops a .argus/config.yml into repo.
func writeRepoConfig(t *testing.T, repo string) {
	t.Helper()
	dir := filepath.Join(repo, ".argus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("base_branch: main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorAllChecksPass(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "1")

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       found,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("expected no error when every check passes, got %v", err)
	}
	if n := strings.Count(out, "✓"); n != 6 {
		t.Errorf("expected 6 passing checks, got %d in:\n%s", n, out)
	}
	if strings.Contains(out, "fix:") {
		t.Errorf("expected no fix hints when all pass, got:\n%s", out)
	}
	for _, want := range []string{"herdr on PATH", "claude on PATH", "forge token resolvable (github.com)", "allowlisted", "config.yml present", "CLAUDE_CODE_ENABLE_TODO_TOOLS=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorHardFailBothBinariesMissing(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       notFound,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
	})
	if err == nil {
		t.Fatal("expected a non-nil error when hard prerequisites fail")
	}
	if !strings.Contains(err.Error(), "2 hard prerequisite check(s) failed") {
		t.Errorf("expected error to count both hard failures, got %v", err)
	}
	if !strings.Contains(out, "✗ herdr on PATH") || !strings.Contains(out, "✗ claude on PATH") {
		t.Errorf("expected a cross mark for each missing binary, got:\n%s", out)
	}
	if !strings.Contains(out, "put it on your PATH") {
		t.Errorf("expected a PATH fix hint, got:\n%s", out)
	}
}

func TestDoctorOneHardFail(t *testing.T) {
	repo := t.TempDir()
	// herdr present, claude missing.
	out, err := runDoctorArgs(t, &doctorArgs{
		repo: repo,
		lookPath: func(name string) (string, error) {
			if name == "herdr" {
				return "/usr/local/bin/herdr", nil
			}
			return "", errors.New("not found")
		},
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
	})
	if err == nil || !strings.Contains(err.Error(), "1 hard prerequisite check(s) failed") {
		t.Fatalf("expected exactly one hard failure, got %v", err)
	}
	if !strings.Contains(out, "✓ herdr on PATH") || !strings.Contains(out, "✗ claude on PATH") {
		t.Errorf("expected herdr pass + claude fail, got:\n%s", out)
	}
}

func TestDoctorSoftFailuresKeepExitZero(t *testing.T) {
	repo := t.TempDir() // no settings.json, no config.yml

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:        repo,
		lookPath:    found,
		resolveRepo: func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		// token empty: forge-token check fails soft.
		tokenForHost:   func(string) string { return "" },
		jiraConfigured: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("soft failures must not change the exit code, got %v", err)
	}
	if !strings.Contains(out, "! forge token resolvable") || !strings.Contains(out, "no token for github.com") {
		t.Errorf("expected a soft warning naming the tokenless host, got:\n%s", out)
	}
	for _, hint := range []string{"argus config check --write", "argus init"} {
		if !strings.Contains(out, hint) {
			t.Errorf("expected soft fix hint %q, got:\n%s", hint, out)
		}
	}
}

func TestDoctorForgeTokenNoRemote(t *testing.T) {
	repo := t.TempDir()

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:     repo,
		lookPath: found,
		resolveRepo: func(context.Context, string) (string, string, string, error) {
			return "", "", "", errors.New("no git remote")
		},
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("a missing remote is a soft failure, got %v", err)
	}
	if !strings.Contains(out, "! forge token resolvable") || !strings.Contains(out, "no forge remote detected") {
		t.Errorf("expected a soft warning naming the remote-detection failure, got:\n%s", out)
	}
}

func TestDoctorAllowlistAndConfigPass(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       found,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "✓ argus allowlisted") || !strings.Contains(out, "Bash(argus *)") {
		t.Errorf("expected the allowlist check to pass and name the covering entry, got:\n%s", out)
	}
	if !strings.Contains(out, "✓ .argus/config.yml present") {
		t.Errorf("expected the config.yml check to pass, got:\n%s", out)
	}
}

func TestDoctorAllowlistCheckReportsParseError(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := checkAllowlist(repo)
	if r.ok {
		t.Fatal("expected the allowlist check to fail on unparseable settings.json")
	}
	if r.detail == "" {
		t.Errorf("expected the parse error surfaced as detail, got empty")
	}
}

// TestDoctorDefaultsWired exercises the real boundaries withDefaults installs
// when a doctorArgs leaves them nil, so the default closures are covered.
func TestDoctorDefaultsWired(t *testing.T) {
	a := &doctorArgs{}
	a.withDefaults()
	if a.lookPath == nil || a.resolveRepo == nil || a.tokenForHost == nil || a.jiraConfigured == nil || a.jiraNewClient == nil {
		t.Fatal("withDefaults left a boundary nil")
	}
	// A non-git temp dir makes the default resolveRepo return an error.
	if _, _, _, err := a.resolveRepo(context.Background(), t.TempDir()); err == nil {
		t.Error("expected the default resolveRepo to fail outside a git repo")
	}
	// An unknown host with no configured credential resolves to no token.
	if tok := a.tokenForHost("doctor.invalid.example"); tok != "" {
		t.Errorf("expected no token for an unknown host, got %q", tok)
	}
	// No JIRA_* env vars and no real ~/.argus/jira.json in a test environment.
	t.Setenv("JIRA_BASE_URL", "")
	t.Setenv("JIRA_EMAIL", "")
	t.Setenv("JIRA_API_TOKEN", "")
	t.Setenv("JIRA_CONFIG_FILE", filepath.Join(t.TempDir(), "does-not-exist.json"))
	if a.jiraConfigured() {
		t.Error("expected the default jiraConfigured to report false with nothing set")
	}
}

// TestDoctorJiraConfiguredHealthy covers a working Jira credential folding a
// passing line into doctor's checklist.
func TestDoctorJiraConfiguredHealthy(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       found,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return true },
		jiraNewClient: func() (jiraWhoamier, error) {
			return &fakeJiraWhoamier{who: jira.WhoamiResult{AccountID: "acc-1", DisplayName: "Dev", APIBase: "https://api.atlassian.com/ex/jira/cloud-1"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("a healthy Jira credential must not fail doctor, got %v", err)
	}
	if !strings.Contains(out, "✓ Jira credentials") || !strings.Contains(out, "Dev (acc-1)") {
		t.Errorf("expected a passing Jira credentials line, got:\n%s", out)
	}
}

// TestDoctorJiraConfiguredUnhealthy covers a dead Jira token folding a soft
// (non-exit-code-changing) warning into doctor's checklist, pointing at
// `argus jira check`.
func TestDoctorJiraConfiguredUnhealthy(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       found,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return true },
		jiraNewClient: func() (jiraWhoamier, error) {
			return &fakeJiraWhoamier{err: &jira.APIError{StatusCode: 401, Prefix: "jira", Status: "401 Unauthorized"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("a dead Jira token is a soft failure, got %v", err)
	}
	if !strings.Contains(out, "! Jira credentials") {
		t.Errorf("expected a soft warning for the dead Jira token, got:\n%s", out)
	}
	if !strings.Contains(out, "argus jira check") {
		t.Errorf("expected the fix hint to point at argus jira check, got:\n%s", out)
	}
}

// TestDoctorJiraNotConfiguredNoLine covers doctor staying silent about Jira
// entirely when nothing is configured — no extra line, passing or failing.
func TestDoctorJiraNotConfiguredNoLine(t *testing.T) {
	repo := t.TempDir()
	writeAllowlist(t, repo)
	writeRepoConfig(t, repo)

	out, err := runDoctorArgs(t, &doctorArgs{
		repo:           repo,
		lookPath:       found,
		resolveRepo:    func(context.Context, string) (string, string, string, error) { return "github.com", "o", "n", nil },
		tokenForHost:   func(string) string { return "tok" },
		jiraConfigured: func() bool { return false },
		jiraNewClient: func() (jiraWhoamier, error) {
			t.Fatal("jiraNewClient must not be called when jiraConfigured is false")
			return nil, errors.New("unreachable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Not a bare "Jira" substring check: t.TempDir() embeds this test's own
	// name in its path, and that name itself contains "Jira" — checkRepoConfig's
	// passing detail line below would false-positive on that.
	if strings.Contains(out, "Jira credentials") {
		t.Errorf("expected no Jira credentials line when unconfigured, got:\n%s", out)
	}
}

// TestDoctorTodoToolsEnv covers checkTodoToolsEnv's three outcomes directly:
// enabled, unset, and set to something other than "1".
func TestDoctorTodoToolsEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "1")
	if r := checkTodoToolsEnv(); !r.ok {
		t.Errorf("expected CLAUDE_CODE_ENABLE_TODO_TOOLS=1 to pass, got %+v", r)
	}

	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "")
	if r := checkTodoToolsEnv(); r.ok || r.hard || r.detail != "unset" {
		t.Errorf("expected an unset var to fail soft with detail \"unset\", got %+v", r)
	}

	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "0")
	if r := checkTodoToolsEnv(); r.ok || r.hard || r.detail != "0" {
		t.Errorf("expected CLAUDE_CODE_ENABLE_TODO_TOOLS=0 to fail soft with detail \"0\", got %+v", r)
	}
}

// TestDoctorCommandEndToEnd drives newDoctorCmd through cobra with fake herdr
// and claude on PATH, covering the constructor, its RunE, and the real default
// lookPath/resolveRepo. The temp --repo is not a git repo, so the forge-token
// check fails soft and the run still exits zero.
func TestDoctorCommandEndToEnd(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"herdr", "claude"} {
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)

	repo := t.TempDir()
	cmd := newDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetArgs([]string{"--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected exit zero with both binaries on PATH, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "✓ herdr on PATH") || !strings.Contains(out, "✓ claude on PATH") {
		t.Errorf("expected both binary checks to pass via the fake PATH, got:\n%s", out)
	}
	if !strings.Contains(out, "! forge token resolvable") {
		t.Errorf("expected the forge-token check to fail soft outside a git repo, got:\n%s", out)
	}
}
