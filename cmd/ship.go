package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/jira"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/repoconfig"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/svcstatus"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newShipCmd() *cobra.Command {
	var (
		worktree       string
		base           string
		title          string
		repo           string
		issue          int
		force          bool
		dryRun         bool
		credentialEnv  map[string]string
		jiraIssue      string
		jiraTransition string
		jiraAssignee   string
		forgeKind      string
	)

	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Commit a worktree's change, push it, and open a pull request",
		Long: `Ship commits any uncommitted change in a worktree, pushes the branch to origin,
and opens a pull request. It is Milestone C of argus: the deterministic final step
once a change has been reviewed. The forge (Codeberg/Gitea, GitHub, or GitLab) is
detected from the worktree's origin remote and the matching token is read from the
environment (CODEBERG_TOKEN, GITHUB_TOKEN, GITLAB_TOKEN, ...). Auto-detection only
covers the exact hosts github.com, gitlab.com, and codeberg.org; a self-hosted
GitLab, Gitea, or Forgejo instance needs an explicit --forge gitlab or --forge
gitea, since a host name alone can't say which REST shape it speaks. Repo
owner/name and branch are derived from the worktree unless overridden.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			overrides, err := resolveCredentialOverrides(credentialEnv)
			if err != nil {
				return err
			}
			return runShip(cmd, &shipArgs{
				worktree: worktree, base: base, baseIsDefault: !cmd.Flags().Changed("base"), title: title, repo: repo,
				issue: issue, force: force, dryRun: dryRun, credentialEnv: overrides,
				jiraIssue: jiraIssue, jiraTransition: jiraTransition, jiraAssignee: jiraAssignee,
				forgeKind: forgeKind, forgeKindExplicit: cmd.Flags().Changed("forge"),
			})
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree to ship")
	cmd.Flags().StringVar(&base, "base", "main", "PR base branch")
	cmd.Flags().IntVar(&issue, "issue", 0, "issue number this change closes")
	cmd.Flags().StringVar(&title, "title", "", "PR title (default derived from the branch/issue)")
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name override (default: parsed from the worktree's origin remote)")
	cmd.Flags().BoolVar(&force, "force", false, "ship even without an approving argus verdict (skips the gate/review check)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be committed and opened, without doing it")
	cmd.Flags().StringToStringVar(&credentialEnv, "credential-env", nil, credentialEnvFlagHelp)
	cmd.Flags().StringVar(&jiraIssue, "jira-issue", "", "Jira issue key (e.g. PROJ-123) to update once the PR is open; unset by default, which skips the Jira post-ship hook entirely. Requires JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN, or a JSON config file (see jira.Config) at $JIRA_CONFIG_FILE or ~/.argus/jira.json")
	cmd.Flags().StringVar(&jiraTransition, "jira-transition", "", "with --jira-issue: transition name or ID to move the issue to (e.g. \"In Review\"); no transition is made if unset")
	cmd.Flags().StringVar(&jiraAssignee, "jira-assignee", "", "with --jira-issue: Jira accountID to assign the issue to; not reassigned if unset")
	cmd.Flags().StringVar(&forgeKind, "forge", "", "force the forge API shape for a self-hosted host: \"gitlab\" or \"gitea\" (default: auto-detect, which only recognizes github.com/gitlab.com/codeberg.org and refuses every other host). Without this flag, this repo's .argus/config.yml forge key wins, then auto-detect")
	return cmd
}

var shipCmd = newShipCmd()

// shipArgs holds newShipCmd's flag values so runShip can be tested directly,
// without going through cobra flag parsing.
type shipArgs struct {
	credentialEnv  map[string]string
	worktree       string
	base           string
	title          string
	repo           string
	jiraIssue      string
	jiraTransition string
	jiraAssignee   string
	forgeKind      string
	issue          int
	force          bool
	dryRun         bool
	// forgeKindExplicit is true only when --forge was actually passed
	// (cmd.Flags().Changed("forge")): an operator-given flag always wins over
	// this repo's .argus/config.yml forge key, the same explicit-flag-wins
	// precedence baseIsDefault gives --base. Any caller building shipArgs
	// directly (tests) leaves this false, which means "fall back to the
	// repo's config, then auto-detect" — the pre-this-change behavior when no
	// config key is set.
	forgeKindExplicit bool
	// baseIsDefault is true only when --base was left at its unset CLI
	// default (cmd.Flags().Changed("base") == false): runShip then resolves
	// the real base via supervisor.ResolveBase instead of trusting the
	// flag's literal "main" default (see internal/repoconfig, issue
	// #160/#161). Any caller building shipArgs directly (tests,
	// shipChange's other callers) leaves this false, which means "trust
	// base as given" — the pre-#161 behavior.
	baseIsDefault bool
}

// shipTarget is the forge/branch/PR identity runShip resolves before deciding
// whether to print a dry-run plan or actually ship.
type shipTarget struct {
	host, owner, name string
	branch            string
	prTitle           string
	commitMsg         string
}

// currentBranch is a var, not a plain call to supervisor.CurrentBranch, so a
// test driving runShip through cmd.SetArgs + cmd.Execute can intercept the
// worktree path this — the first supervisor call runShip makes — receives,
// without needing a real git remote/push.
var currentBranch = supervisor.CurrentBranch

// shipContext is what resolveShipContext resolves before runShip decides
// whether to print a dry-run plan or actually ship: everything derived from
// the worktree/flags, independent of whether a forge token is available.
type shipContext struct {
	branch            string
	host, owner, name string
	kind              forge.Kind
}

// resolveShipContext runs runShip's validation/resolution steps — worktree
// path, base branch, current branch, approval gate, repo/forge identity — so
// runShip itself only has to branch on the outcome. Split out because this is
// the bulk of runShip's own decision points; isolating them here keeps both
// functions independently testable and each under the CRAP gate.
func resolveShipContext(ctx context.Context, a *shipArgs) (*shipContext, error) {
	// See supervisor.ResolveWorktree: a --worktree given relative to argus's
	// own cwd must be resolved before it reaches CurrentBranch/CommitAll/Push
	// or protocol.Load/LoadApproval, so every downstream call agrees on the
	// same absolute path.
	resolved, err := supervisor.ResolveWorktree(a.worktree)
	if err != nil {
		return nil, err
	}
	a.worktree = resolved
	if a.baseIsDefault {
		a.base = supervisor.ResolveBase(ctx, a.worktree, a.base, false)
	}

	branch, err := currentBranch(ctx, a.worktree)
	if err != nil {
		return nil, err
	}
	if verr := checkApproved(ctx, a.worktree, "origin/"+a.base, a.force); verr != nil {
		return nil, verr
	}
	host, owner, name, err := resolveRepo(ctx, a.repo, a.worktree)
	if err != nil {
		return nil, err
	}
	kind, err := parseForgeKind(resolveForgeKindValue(a.forgeKindExplicit, a.forgeKind, forgeConfigDefault(ctx, a.worktree)))
	if err != nil {
		return nil, err
	}
	// Validated with no token so this also runs (and can fail) under --dry-run:
	// a clean dry-run plan previously proved nothing about whether the real API
	// call would even hit the right forge shape for the host.
	if _, verr := forge.New(host, "", nil, kind); verr != nil {
		return nil, verr
	}
	return &shipContext{branch: branch, host: host, owner: owner, name: name, kind: kind}, nil
}

// runShip is newShipCmd's RunE body, extracted so the decision logic (commit,
// push, open PR) is independently testable and the constructor itself stays
// flag-registration-only.
func runShip(cmd *cobra.Command, a *shipArgs) error {
	if a.worktree == "" {
		return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus ship --worktree <path> --issue <n>"}
	}
	ctx := cmd.Context()
	sc, err := resolveShipContext(ctx, a)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()

	if a.dryRun {
		// No token/forge client yet at this point (see below) — the issue-title
		// fetch fallback is simply skipped for the preview; status.Title and the
		// old branch/issue default need no network and still apply.
		prTitle, terr := resolvePRTitle(ctx, nil, reader, out, a, sc.owner, sc.name, sc.branch)
		if terr != nil {
			return terr
		}
		target := &shipTarget{host: sc.host, owner: sc.owner, name: sc.name, branch: sc.branch, prTitle: prTitle, commitMsg: prTitle + closesLine(a.issue)}
		renderShipPlan(out, target, a.base)
		return nil
	}

	token := forge.TokenForHost(sc.host, a.credentialEnv)
	if token == "" {
		return &ui.UserError{
			Err:  fmt.Errorf("no API token for %s", sc.host),
			Hint: "set the token env var for this host (e.g. CODEBERG_TOKEN, GITHUB_TOKEN, or GITLAB_TOKEN), or run `gh auth login` / `glab auth login`",
		}
	}
	f, ferr := forge.New(sc.host, token, nil, sc.kind)
	if ferr != nil {
		return ferr
	}
	prTitle, terr := resolvePRTitle(ctx, f, reader, out, a, sc.owner, sc.name, sc.branch)
	if terr != nil {
		return terr
	}
	target := &shipTarget{host: sc.host, owner: sc.owner, name: sc.name, branch: sc.branch, prTitle: prTitle, commitMsg: prTitle + closesLine(a.issue)}
	return shipChange(cmd, f, a, target)
}

func renderShipPlan(out io.Writer, target *shipTarget, base string) {
	_, _ = fmt.Fprintf(out, "%s ship plan (dry run)\n", ui.LabelInfo.Render("i"))
	_, _ = fmt.Fprintf(out, "  forge:  %s\n  repo:   %s/%s\n  branch: %s -> %s\n  commit: %s\n  PR:     %s\n",
		target.host, target.owner, target.name, target.branch, base, target.commitMsg, target.prTitle)
}

// shipChange commits, pushes, and opens the PR for an already-resolved
// shipTarget. Split out of runShip so the resolve/decide logic above (which
// branches many times on flags and lookups) stays independently testable from
// this side-effecting happy path.
func shipChange(cmd *cobra.Command, f forge.Forge, a *shipArgs, target *shipTarget) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	logger, closeLog := openRunLog(cmd, "ship")
	defer closeLog()

	if gerr := enforceShipGate(ctx, a.worktree); gerr != nil {
		logger.Fail("ship_gate", target.branch, gerr)
		return gerr
	}

	if cerr := supervisor.CommitAll(ctx, a.worktree, target.commitMsg); cerr != nil && !errors.Is(cerr, supervisor.ErrNothingToCommit) {
		logger.Fail("commit", target.branch, cerr)
		return cerr
	}
	if perr := supervisor.Push(ctx, a.worktree, target.branch); perr != nil {
		logger.Fail("push", target.branch, perr)
		// A push can just as easily fail for local reasons (a rejected pre-push
		// hook, a non-fast-forward) as for the host being down, so this is only a
		// hint, not a claim.
		return fmt.Errorf("%w%s", perr, svcstatus.Note(target.host))
	}

	prBody := buildPRBody(ctx, f, a.worktree, a.base, a.issue, target.owner, target.name)
	pr, err := f.OpenPR(ctx, &forge.PRRequest{
		Owner: target.owner, Repo: target.name,
		Title: target.prTitle, Body: prBody,
		Head: target.branch, Base: a.base,
	})
	if err != nil {
		logger.Fail("open_pr", target.branch, err)
		return err
	}
	logger.Action("open_pr", target.branch, "ok", pr.HTMLURL)
	_, _ = fmt.Fprintf(out, "%s opened PR #%d: %s\n", ui.LabelSuccess.Render("✓"), pr.Number, pr.HTMLURL)

	// Recorded best-effort: a write failure here must not undo an already-opened
	// PR. Without it, `argus worktree prune` still works (it falls back to
	// forge.FindPR by branch), just without the exact PR number pre-resolved.
	if lerr := protocol.WriteLifecycle(a.worktree, &protocol.Lifecycle{
		State: protocol.LifecycleShipped, Host: target.host, Owner: target.owner, Repo: target.name,
		Branch: target.branch, PRURL: pr.HTMLURL, PRNumber: pr.Number,
	}); lerr != nil {
		logger.Fail("record_lifecycle", target.branch, lerr)
		_, _ = fmt.Fprintf(out, "%s recording worktree lifecycle: %v\n", ui.LabelWarning.Render("!"), lerr)
	}

	if a.jiraIssue != "" {
		postShipJira(ctx, out, logger, a, pr)
	}
	return nil
}

// newJiraClient is a var so tests can inject a fake jiraIssueWriter without a
// real JIRA_BASE_URL/EMAIL/API_TOKEN or network; non-test callers get
// jira.NewFromEnv unchanged.
var newJiraClient = func() (jiraIssueWriter, error) { return jira.NewFromEnv(nil) }

// jiraIssueWriter is the subset of *jira.Client postShipJira needs, so it is
// testable without a network.
type jiraIssueWriter interface {
	Transition(ctx context.Context, key, idOrName string) error
	Comment(ctx context.Context, key, body string) error
	Assign(ctx context.Context, key, accountID string) error
}

// postShipJira closes the loop back to Jira once a PR has actually been
// opened: optionally moves the issue to a new status, optionally reassigns
// it, and always leaves a comment linking the PR — so an operator using
// --jira-issues as work input (see cmd/supervise.go) doesn't have to update
// the ticket by hand afterward. It only runs when --jira-issue is set (see
// shipChange) and is entirely best-effort: a failure here is logged and
// printed as a warning but never undoes the ship, which has already
// succeeded by the time this runs.
func postShipJira(ctx context.Context, out io.Writer, logger *eventlog.Logger, a *shipArgs, pr forge.PR) {
	c, err := newJiraClient()
	if err != nil {
		warnJiraPostShip(out, logger, a.jiraIssue, err)
		return
	}
	if a.jiraTransition != "" {
		if terr := c.Transition(ctx, a.jiraIssue, a.jiraTransition); terr != nil {
			warnJiraPostShip(out, logger, a.jiraIssue, terr)
		} else {
			logger.Action("jira_transition", a.jiraIssue, "ok", a.jiraTransition)
		}
	}
	if a.jiraAssignee != "" {
		if aerr := c.Assign(ctx, a.jiraIssue, a.jiraAssignee); aerr != nil {
			warnJiraPostShip(out, logger, a.jiraIssue, aerr)
		} else {
			logger.Action("jira_assign", a.jiraIssue, "ok", a.jiraAssignee)
		}
	}
	if cerr := c.Comment(ctx, a.jiraIssue, "Opened "+pr.HTMLURL); cerr != nil {
		warnJiraPostShip(out, logger, a.jiraIssue, cerr)
		return
	}
	logger.Action("jira_comment", a.jiraIssue, "ok", pr.HTMLURL)
}

func warnJiraPostShip(out io.Writer, logger *eventlog.Logger, key string, err error) {
	logger.Fail("jira_post_ship", key, err)
	_, _ = fmt.Fprintf(out, "%s jira post-ship for %s: %v\n", ui.LabelWarning.Render("!"), key, err)
}

// enforceShipGate runs this repo's hook/lint enforcement before shipChange
// commits anything: any lefthook/pre-commit-framework config found in the
// worktree (supervisor.EnforceHooks), then the repo's own optional ship_lint
// command from .argus/config.yml (supervisor.RunShipLint). It is unconditional
// — unlike checkApproved, --force does not skip it — because the point is to
// close the --no-verify bypass even for a human who has decided to ship an
// unreviewed change; letting --force also skip this would just relocate the
// bypass rather than close it.
func enforceShipGate(ctx context.Context, worktree string) error {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return fmt.Errorf("resolving repo root for ship gate: %w", err)
	}
	if herr := supervisor.EnforceHooks(ctx, worktree); herr != nil {
		return herr
	}
	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return fmt.Errorf("loading %s: %w", repoconfig.Path(repoRoot), err)
	}
	return supervisor.RunShipLint(ctx, worktree, rc.ShipLint)
}

// checkApproved refuses to ship a worktree that argus never cleared. supervise
// records a verdict.json per worker; ship enforces it so a gate escalation or a
// reviewer's request-changes actually blocks the PR instead of being advisory.
// It also refuses a stale verdict: nothing stops a worker's session (or plain
// continued activity) from touching the worktree again after approval, so an
// approved verdict only counts if the content it was measured against is
// still what's on disk right now. --force overrides both checks for the
// human who has decided to ship anyway.
func checkApproved(ctx context.Context, worktree, base string, force bool) error {
	if force {
		return nil
	}
	approval, found, err := protocol.LoadApproval(worktree)
	if err != nil {
		return err
	}
	if !found {
		return &ui.UserError{
			Err:  fmt.Errorf("no argus verdict for this worktree"),
			Hint: "run `argus supervise --review` (or `argus review`) first, or pass --force to ship anyway",
		}
	}
	if !approval.Approved {
		return &ui.UserError{
			Err:  fmt.Errorf("argus did not approve this change (%s): %s", approval.Source, approval.Summary),
			Hint: "address the findings and re-review, or pass --force to override",
		}
	}
	_, files, err := supervisor.MeasureDiff(ctx, worktree, base)
	if err != nil {
		return fmt.Errorf("re-measuring worktree before ship: %w", err)
	}
	hash, err := supervisor.ContentHash(worktree, files)
	if err != nil {
		return fmt.Errorf("hashing worktree content before ship: %w", err)
	}
	if hash != approval.ContentHash {
		return &ui.UserError{
			Err:  fmt.Errorf("worktree content has changed since argus approved this change"),
			Hint: "re-run `argus supervise --review` (or `argus review`) to re-verify the current diff, or pass --force to ship anyway",
		}
	}
	return nil
}

// resolveRepo detects the forge host and owner/repo from the worktree's origin
// remote. --repo overrides only the owner/name; the host still comes from the
// remote, so argus ships to whichever forge the worktree actually points at.
func resolveRepo(ctx context.Context, override, worktree string) (host, owner, name string, err error) {
	remote, err := supervisor.RemoteURL(ctx, worktree)
	if err != nil {
		return "", "", "", err
	}
	host, owner, name, err = forge.Detect(remote)
	if err != nil {
		return "", "", "", err
	}
	if override != "" {
		o, n, ok := splitOwnerRepo(override)
		if !ok {
			return "", "", "", &ui.UserError{Err: fmt.Errorf("--repo must be owner/name, got %q", override)}
		}
		owner, name = o, n
	}
	return host, owner, name, nil
}

// forgeConfigDefault reads worktree's repo .argus/config.yml forge key,
// best-effort like supervisor.ResolveBase's own repoconfig lookup: a
// worktree outside any repo, or with no config file, simply has no default
// to offer, falling through to whatever the caller does next (an explicit
// --forge, or auto-detection).
func forgeConfigDefault(ctx context.Context, worktree string) string {
	repoRoot, err := supervisor.RepoRoot(ctx, worktree)
	if err != nil {
		return ""
	}
	rc, err := repoconfig.Load(repoconfig.Path(repoRoot))
	if err != nil {
		return ""
	}
	return rc.Forge
}

// resolveForgeKindValue applies --forge > this repo's .argus/config.yml
// forge key > the flag's own default (""), the same explicit-flag-wins
// precedence resolveWorkerPlacement/resolveReviewEffort (cmd/supervise.go)
// use for their own config keys. explicit is true only when --forge was
// actually passed on the command line.
func resolveForgeKindValue(explicit bool, flagValue, configValue string) string {
	if explicit {
		return flagValue
	}
	if configValue != "" {
		return configValue
	}
	return flagValue
}

// parseForgeKind turns --forge's raw flag value into a forge.Kind, rejecting
// anything but the values forge.New itself understands so a typo surfaces
// immediately instead of silently falling back to auto-detection.
func parseForgeKind(raw string) (forge.Kind, error) {
	switch forge.Kind(raw) {
	case forge.KindAuto, forge.KindGitLab, forge.KindGitea:
		return forge.Kind(raw), nil
	default:
		return "", &ui.UserError{Err: fmt.Errorf("--forge must be %q or %q, got %q", forge.KindGitLab, forge.KindGitea, raw)}
	}
}

func splitOwnerRepo(s string) (owner, name string, ok bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return "", "", false
			}
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// prTitleMaxLen is the hard cap ship enforces on every PR/commit title except
// an explicit --title, which always wins untouched. Forges truncate or wrap
// long titles inconsistently in PR lists and notification emails, so ship
// picks one length and enforces it up front rather than letting each forge
// mangle it differently.
const prTitleMaxLen = 72

// isStdinInteractive is a var, not a plain call, so a test can force either
// branch of resolvePRTitle's too-long handling deterministically — go-isatty
// always reports false under `go test`, which already matches the headless
// case (see internal/ui/spinner.go's isStderrInteractive for the same
// pattern).
var isStdinInteractive = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}

// resolvePRTitle picks the PR/commit title ship uses to open the PR and to
// title the commit it makes, in priority order: an explicit --title (exempt
// from the length rule below — the human asked for exactly this), the
// worker's own status.Title (internal/protocol.Status, written via `argus
// worker report`, informed by the worker's actual diff rather than the issue
// title verbatim), the linked issue's fetched title (f is nil during
// --dry-run, which has no forge client yet — the fetch is then simply
// skipped), and finally the old branch/issue default as a last resort.
// Whichever of the last three wins is then subject to the 72-char rule.
func resolvePRTitle(ctx context.Context, f forge.Forge, in *bufio.Reader, out io.Writer, a *shipArgs, owner, name, branch string) (string, error) {
	if a.title != "" {
		return a.title, nil
	}
	status, _ := protocol.Load(protocol.StatusPath(a.worktree))
	title := status.Title
	if title == "" && a.issue > 0 && f != nil {
		if iss, err := f.FetchIssue(ctx, owner, name, a.issue); err == nil {
			title = iss.Title
		}
	}
	if title == "" {
		title = defaultPRTitle(a.issue, branch)
	}
	return enforceTitleLength(title, in, out)
}

func defaultPRTitle(issue int, branch string) string {
	if issue > 0 {
		return fmt.Sprintf("fix: %s (#%d)", branch, issue)
	}
	return "fix: " + branch
}

// enforceTitleLength applies the 72-char rule to a title that did not come
// from --title. A title that fits passes through untouched. Over the limit
// with a TTY attached, the operator gets a chance to type a shorter one or
// hit Enter to auto-truncate; over the limit with no TTY errors instead of
// blocking on stdin that a headless spawn (a worker's own `ship` call, CI)
// will never answer.
func enforceTitleLength(title string, in *bufio.Reader, out io.Writer) (string, error) {
	n := utf8.RuneCountInString(title)
	if n <= prTitleMaxLen {
		return title, nil
	}
	if !isStdinInteractive() {
		return "", &ui.UserError{
			Err:  fmt.Errorf("PR title is %d chars, over the %d-char limit: %q", n, prTitleMaxLen, title),
			Hint: "re-run with --title to set one under the limit",
		}
	}
	_, _ = fmt.Fprintf(out, "%s PR title is %d chars (max %d): %q\n", ui.LabelWarning.Render("!"), n, prTitleMaxLen, title)
	_, _ = fmt.Fprintf(out, "%s enter a shorter title, or press Enter to auto-truncate: ", ui.LabelWarning.Render("?"))
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return string([]rune(title)[:prTitleMaxLen]), nil
	}
	if ln := utf8.RuneCountInString(line); ln > prTitleMaxLen {
		return "", &ui.UserError{
			Err:  fmt.Errorf("shortened title is still %d chars, over the %d-char limit: %q", ln, prTitleMaxLen, line),
			Hint: "re-run with --title to set one under the limit",
		}
	}
	return line, nil
}

func closesLine(issue int) string {
	if issue > 0 {
		return fmt.Sprintf("\n\nCloses #%d", issue)
	}
	return ""
}

// buildPRBody assembles an informative PR description instead of a one-liner: what
// the change targets (the issue title, fetched from the forge when --issue is
// set), the measured diff, the worker's test result, and argus's verdict. Every
// part is best-effort; a missing piece is simply omitted so ship never fails on
// reporting.
func buildPRBody(ctx context.Context, f forge.Forge, worktree, base string, issue int, owner, repo string) string {
	var b strings.Builder
	writePRTargetSection(ctx, &b, f, issue, owner, repo)
	writePRChangeSection(ctx, &b, worktree, base)
	writePRVerificationSection(&b, worktree)
	writePRVerdictSection(&b, worktree)
	return strings.TrimSpace(b.String())
}

func writePRTargetSection(ctx context.Context, b *strings.Builder, f forge.Forge, issue int, owner, repo string) {
	b.WriteString("## Target\n\n")
	if issue <= 0 {
		b.WriteString("General fix (no linked issue).\n\n")
		return
	}
	line := fmt.Sprintf("Closes #%d", issue)
	if iss, err := f.FetchIssue(ctx, owner, repo, issue); err == nil && iss.Title != "" {
		line += " — " + iss.Title
	}
	fmt.Fprintf(b, "%s\n\n", line)
}

func writePRChangeSection(ctx context.Context, b *strings.Builder, worktree, base string) {
	b.WriteString("## Change\n\n")
	ds, files, err := supervisor.MeasureDiff(ctx, worktree, "origin/"+base)
	if err != nil {
		return
	}
	fmt.Fprintf(b, "%d file(s), +%d/-%d\n", ds.Files, ds.Insertions, ds.Deletions)
	for _, fp := range files {
		fmt.Fprintf(b, "- `%s`\n", fp)
	}
	b.WriteString("\n")
}

func writePRVerificationSection(b *strings.Builder, worktree string) {
	status, err := protocol.Load(protocol.StatusPath(worktree))
	if err != nil {
		return
	}
	passed, total := 0, 0
	for i := range status.Tests {
		total++
		if status.Tests[i].Result == protocol.ResultPass {
			passed++
		}
	}
	if total > 0 {
		fmt.Fprintf(b, "## Verification\n\nTests: %d/%d passed (worker-reported).\n", passed, total)
	}
	if status.RealWorldProof != "" {
		fmt.Fprintf(b, "Real-world proof: %s\n", status.RealWorldProof)
	}
	b.WriteString("\n")
}

func writePRVerdictSection(b *strings.Builder, worktree string) {
	approval, found, err := protocol.LoadApproval(worktree)
	if err != nil || !found {
		return
	}
	verdict := "not approved"
	if approval.Approved {
		verdict = "approved"
	}
	fmt.Fprintf(b, "## Verdict\n\nargus %s via %s", verdict, approval.Source)
	if approval.Summary != "" {
		fmt.Fprintf(b, ": %s", approval.Summary)
	}
	b.WriteString("\n")
}
