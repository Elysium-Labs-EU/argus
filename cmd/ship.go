package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Elysium-Labs-EU/argus/internal/forge"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
	"github.com/Elysium-Labs-EU/argus/internal/supervisor"
	"github.com/Elysium-Labs-EU/argus/internal/ui"
)

func newShipCmd() *cobra.Command {
	var (
		worktree string
		base     string
		title    string
		repo     string
		issue    int
		force    bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Commit a worktree's change, push it, and open a pull request",
		Long: `Ship commits any uncommitted change in a worktree, pushes the branch to origin,
and opens a pull request. It is Milestone C of argus: the deterministic final step
once a change has been reviewed. The forge (Codeberg/Gitea or GitHub) is detected
from the worktree's origin remote and the matching token is read from the
environment (CODEBERG_TOKEN, GITHUB_TOKEN, ...). Repo owner/name and branch are
derived from the worktree unless overridden.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if worktree == "" {
				return &ui.UserError{Err: fmt.Errorf("no worktree given"), Hint: "argus ship --worktree <path> --issue <n>"}
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			branch, err := supervisor.CurrentBranch(ctx, worktree)
			if err != nil {
				return err
			}
			if verr := checkApproved(worktree, force); verr != nil {
				return verr
			}
			host, owner, name, err := resolveRepo(ctx, repo, worktree)
			if err != nil {
				return err
			}
			prTitle := prTitleFor(title, issue, branch)
			commitMsg := prTitle + closesLine(issue)

			if dryRun {
				_, _ = fmt.Fprintf(out, "%s ship plan (dry run)\n", ui.LabelInfo.Render("i"))
				_, _ = fmt.Fprintf(out, "  forge:  %s\n  repo:   %s/%s\n  branch: %s -> %s\n  commit: %s\n  PR:     %s\n",
					host, owner, name, branch, base, commitMsg, prTitle)
				return nil
			}

			token := forge.TokenForHost(host)
			if token == "" {
				return &ui.UserError{
					Err:  fmt.Errorf("no API token for %s", host),
					Hint: "set the token env var for this host (e.g. CODEBERG_TOKEN or GITHUB_TOKEN)",
				}
			}
			f := forge.New(host, token, nil)

			logger, closeLog := openRunLog(cmd, "ship")
			defer closeLog()

			if cerr := supervisor.CommitAll(ctx, worktree, commitMsg); cerr != nil && !errors.Is(cerr, supervisor.ErrNothingToCommit) {
				logger.Fail("commit", branch, cerr)
				return cerr
			}
			if perr := supervisor.Push(ctx, worktree, branch); perr != nil {
				logger.Fail("push", branch, perr)
				return perr
			}

			prBody := buildPRBody(ctx, f, worktree, base, issue, owner, name)
			pr, err := f.OpenPR(ctx, &forge.PRRequest{
				Owner: owner, Repo: name,
				Title: prTitle, Body: prBody,
				Head: branch, Base: base,
			})
			if err != nil {
				logger.Fail("open_pr", branch, err)
				return err
			}
			logger.Action("open_pr", branch, "ok", pr.HTMLURL)
			_, _ = fmt.Fprintf(out, "%s opened PR #%d: %s\n", ui.LabelSuccess.Render("✓"), pr.Number, pr.HTMLURL)
			return nil
		},
	}

	cmd.Flags().StringVar(&worktree, "worktree", "", "worktree to ship")
	cmd.Flags().StringVar(&base, "base", "main", "PR base branch")
	cmd.Flags().IntVar(&issue, "issue", 0, "issue number this change closes")
	cmd.Flags().StringVar(&title, "title", "", "PR title (default derived from the branch/issue)")
	cmd.Flags().StringVar(&repo, "repo", "", "owner/name override (default: parsed from the worktree's origin remote)")
	cmd.Flags().BoolVar(&force, "force", false, "ship even without an approving argus verdict (skips the gate/review check)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be committed and opened, without doing it")
	return cmd
}

var shipCmd = newShipCmd()

// checkApproved refuses to ship a worktree that argus never cleared. supervise
// records a verdict.json per worker; ship enforces it so a gate escalation or a
// reviewer's request-changes actually blocks the PR instead of being advisory.
// --force overrides for the human who has decided to ship anyway.
func checkApproved(worktree string, force bool) error {
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

func prTitleFor(title string, issue int, branch string) string {
	if title != "" {
		return title
	}
	if issue > 0 {
		return fmt.Sprintf("fix: %s (#%d)", branch, issue)
	}
	return "fix: " + branch
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

	b.WriteString("## Target\n\n")
	if issue > 0 {
		line := fmt.Sprintf("Closes #%d", issue)
		if iss, err := f.FetchIssue(ctx, owner, repo, issue); err == nil && iss.Title != "" {
			line += " — " + iss.Title
		}
		b.WriteString(line + "\n\n")
	} else {
		b.WriteString("General fix (no linked issue).\n\n")
	}

	b.WriteString("## Change\n\n")
	if ds, files, err := supervisor.MeasureDiff(ctx, worktree, "origin/"+base); err == nil {
		fmt.Fprintf(&b, "%d file(s), +%d/-%d\n", ds.Files, ds.Insertions, ds.Deletions)
		for _, fp := range files {
			fmt.Fprintf(&b, "- `%s`\n", fp)
		}
		b.WriteString("\n")
	}

	if status, err := protocol.Load(protocol.StatusPath(worktree)); err == nil {
		passed, total := 0, 0
		for i := range status.Tests {
			total++
			if status.Tests[i].Result == protocol.ResultPass {
				passed++
			}
		}
		if total > 0 {
			fmt.Fprintf(&b, "## Verification\n\nTests: %d/%d passed (worker-reported).\n", passed, total)
		}
		if status.RealWorldProof != "" {
			fmt.Fprintf(&b, "Real-world proof: %s\n", status.RealWorldProof)
		}
		b.WriteString("\n")
	}

	if approval, found, err := protocol.LoadApproval(worktree); err == nil && found {
		verdict := "not approved"
		if approval.Approved {
			verdict = "approved"
		}
		fmt.Fprintf(&b, "## Verdict\n\nargus %s via %s", verdict, approval.Source)
		if approval.Summary != "" {
			fmt.Fprintf(&b, ": %s", approval.Summary)
		}
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String())
}
