package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"codeberg.org/Elysium_Labs/argus/internal/codeberg"
	"codeberg.org/Elysium_Labs/argus/internal/supervisor"
	"codeberg.org/Elysium_Labs/argus/internal/ui"
)

func newShipCmd() *cobra.Command {
	var (
		worktree string
		base     string
		title    string
		repo     string
		issue    int
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "ship",
		Short: "Commit a worktree's change, push it, and open a Codeberg PR",
		Long: `Ship commits any uncommitted change in a worktree, pushes the branch to origin,
and opens a Codeberg pull request. It is Milestone C of argus: the deterministic
final step once a change has been reviewed. Repo owner/name and branch are derived
from the worktree unless overridden. Requires CODEBERG_TOKEN in the environment.`,
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
			owner, name, err := resolveRepo(ctx, repo, worktree)
			if err != nil {
				return err
			}
			commitMsg, prTitle, prBody := shipText(title, issue, branch)

			if dryRun {
				_, _ = fmt.Fprintf(out, "%s ship plan (dry run)\n", ui.LabelInfo.Render("i"))
				_, _ = fmt.Fprintf(out, "  repo:   %s/%s\n  branch: %s -> %s\n  commit: %s\n  PR:     %s\n",
					owner, name, branch, base, commitMsg, prTitle)
				return nil
			}

			token := os.Getenv("CODEBERG_TOKEN")
			if token == "" {
				return &ui.UserError{Err: fmt.Errorf("CODEBERG_TOKEN not set"), Hint: "export CODEBERG_TOKEN=<token>"}
			}

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

			pr, err := codeberg.New(token).OpenPR(ctx, &codeberg.PRRequest{
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
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be committed and opened, without doing it")
	return cmd
}

var shipCmd = newShipCmd()

func resolveRepo(ctx context.Context, override, worktree string) (owner, name string, err error) {
	if override != "" {
		owner, name, ok := splitOwnerRepo(override)
		if !ok {
			return "", "", &ui.UserError{Err: fmt.Errorf("--repo must be owner/name, got %q", override)}
		}
		return owner, name, nil
	}
	return supervisor.RemoteOwnerRepo(ctx, worktree)
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

func shipText(title string, issue int, branch string) (commitMsg, prTitle, prBody string) {
	subject := title
	if subject == "" {
		subject = "fix: " + branch
	}
	closes := ""
	if issue > 0 {
		closes = fmt.Sprintf("\n\nCloses #%d", issue)
	}
	commitMsg = subject + closes
	prTitle = subject
	prBody = "Reviewed and shipped by argus." + closes
	return commitMsg, prTitle, prBody
}
