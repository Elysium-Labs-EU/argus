package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
)

// ReviewRequest is the scoped payload argus hands a reviewer when the
// deterministic gate escalates. It is deliberately small: the task, why the gate
// flagged it, and the actual diff — not the worker's whole scrollback.
type ReviewRequest struct {
	Task     string
	Branch   string
	Worktree string
	Diff     string
	Reasons  []string
}

// ReviewResult is a reviewer's verdict. Decision is one of "approve",
// "request-changes", or "needs-human" (the reviewer abstains to a person).
type ReviewResult struct {
	Decision string   `json:"decision"`
	Summary  string   `json:"summary"`
	Findings []string `json:"findings"`
}

// Reviewer turns an escalated change into a verdict. This is the one place an LLM
// re-enters argus's loop — and only for the risky minority the gate couldn't
// clear, with a scoped prompt rather than a hot agent.
type Reviewer interface {
	Review(ctx context.Context, req *ReviewRequest) (ReviewResult, error)
}

// reviewRunner execs a command in workdir with stdin and returns stdout. The one
// effectful seam in the CLI reviewer; tests substitute a fake. workdir is the
// worktree the reviewer runs inside, so it can read the real files (not just the
// diff); an empty workdir runs in the current directory.
type reviewRunner func(ctx context.Context, workdir, stdin string, args ...string) ([]byte, error)

// CLIReviewer asks a headless `claude -p` for a verdict. It is the default
// Milestone-B reviewer: a one-shot, not a session.
type CLIReviewer struct {
	run   reviewRunner
	log   *eventlog.Logger
	model string
}

// NewCLIReviewer returns a reviewer backed by the real claude CLI. model may be
// empty to use claude's default.
func NewCLIReviewer(model string) CLIReviewer {
	return CLIReviewer{run: claudeRunner(), model: model}
}

// NewReviewerWithRunner returns a CLIReviewer backed by a caller-supplied runner,
// for tests.
func NewReviewerWithRunner(run reviewRunner) CLIReviewer {
	return CLIReviewer{run: run}
}

// WithLog returns a copy of the reviewer that records its re-asks to log, so the
// analysis pass can measure how often the model's first reply failed to parse.
func (r CLIReviewer) WithLog(log *eventlog.Logger) CLIReviewer {
	r.log = log
	return r
}

// DiffFor returns the worker's change as a diff against base, read-only via
// git -C (argus never edits a worktree; it reviews via git). The worker leaves
// its change uncommitted, so a plain `git diff <base>` captures the working tree.
func DiffFor(ctx context.Context, worktree, base string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", base) //nolint:gosec // fixed git binary; worktree/base are argus-derived
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("diffing worktree against %s: %w", base, err)
	}
	return out.String(), nil
}

func claudeRunner() reviewRunner {
	return func(ctx context.Context, workdir, stdin string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "claude", args...) //nolint:gosec // fixed "claude" binary, argus-composed args
		cmd.Dir = workdir                                  // review inside the worktree so read tools see the real files
		cmd.Stdin = strings.NewReader(stdin)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("running claude reviewer: %w", err)
		}
		return out.Bytes(), nil
	}
}

// Review sends the scoped prompt to claude -p and parses its verdict. claude's
// JSON envelope wraps the model's text in .result; the model is instructed to
// answer with only our verdict JSON, which we then extract from that text.
func (r CLIReviewer) Review(ctx context.Context, req *ReviewRequest) (ReviewResult, error) {
	// Read-only tools only: the reviewer inspects the checkout to verify symbols
	// and behavior, but must not edit — its job is a verdict, not a change.
	args := []string{"-p", "--output-format", "json", "--allowedTools", "Read,Grep,Glob"}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	out, err := r.run(ctx, req.Worktree, reviewPrompt(req), args...)
	if err != nil {
		return ReviewResult{}, err
	}
	res, perr := parseReviewOutput(out)
	if perr == nil {
		return res, nil
	}
	// The model answered but not as a clean verdict (e.g. unquoted keys, stray
	// prose). Re-ask exactly once for strict JSON before giving up — a one-shot
	// review shouldn't die on a formatting slip. The re-ask feeds the prior reply
	// back so the model reformats its own judgment rather than re-reviewing.
	r.log.Action("review_reask", req.Task, "parse-retry", perr.Error())
	out, err = r.run(ctx, req.Worktree, reAskPrompt(out), args...)
	if err != nil {
		return ReviewResult{}, err
	}
	return parseReviewOutput(out)
}

// reAskPrompt asks the model to reformat its previous, unparseable reply into a
// strict verdict object. It carries the prior text so no re-review is needed.
func reAskPrompt(prior []byte) string {
	var b strings.Builder
	b.WriteString("Your previous reply was not a single valid JSON verdict object.\n")
	b.WriteString("Reply with ONLY a JSON object — double-quoted keys and string values,\n")
	b.WriteString("no prose, no code fence — of the form:\n")
	b.WriteString(`{"decision":"approve|request-changes|needs-human","summary":"one sentence","findings":["..."]}` + "\n\n")
	b.WriteString("Base it on your prior assessment:\n")
	b.Write(prior)
	return b.String()
}

func reviewPrompt(req *ReviewRequest) string {
	var b strings.Builder
	b.WriteString("You are a code reviewer. A deterministic gate flagged this change for review.\n")
	b.WriteString("Judge only correctness, parity with existing code, and test adequacy.\n\n")
	if req.Worktree != "" {
		b.WriteString("You are running inside the change's worktree. Use your Read/Grep/Glob tools\n")
		b.WriteString("to verify the diff against the ACTUAL files: confirm referenced symbols and\n")
		b.WriteString("imports exist, that call sites match, and that the change fits the surrounding\n")
		b.WriteString("code — do not judge from the diff alone. Do not edit anything.\n\n")
	}
	fmt.Fprintf(&b, "Task: %s\nBranch: %s\n\n", req.Task, req.Branch)
	b.WriteString("The gate escalated for these reasons:\n")
	for _, reason := range req.Reasons {
		fmt.Fprintf(&b, "  - %s\n", reason)
	}
	b.WriteString("\nDiff:\n```diff\n")
	b.WriteString(req.Diff)
	b.WriteString("\n```\n\n")
	b.WriteString(`When done, reply with ONLY a JSON object, no prose, of the form:
{"decision":"approve|request-changes|needs-human","summary":"one sentence","findings":["..."]}
Use "approve" only if the change is correct and adequately tested; "request-changes" for a concrete defect; "needs-human" if you cannot tell.`)
	return b.String()
}

// claudeEnvelope is the subset of `claude -p --output-format json` output we read.
type claudeEnvelope struct {
	Result string `json:"result"`
}

func parseReviewOutput(out []byte) (ReviewResult, error) {
	// First unwrap claude's JSON envelope to get the model's text; fall back to
	// treating stdout as the verdict directly if it isn't the envelope shape.
	text := string(out)
	var env claudeEnvelope
	if err := json.Unmarshal(out, &env); err == nil && env.Result != "" {
		text = env.Result
	}

	obj := extractJSONObject(text)
	if obj == "" {
		return ReviewResult{}, fmt.Errorf("reviewer output had no JSON verdict: %.200s", text)
	}
	var res ReviewResult
	if err := json.Unmarshal([]byte(obj), &res); err != nil {
		return ReviewResult{}, fmt.Errorf("decoding reviewer verdict: %w", err)
	}
	if res.Decision == "" {
		return ReviewResult{}, fmt.Errorf("reviewer verdict missing decision")
	}
	return res, nil
}

// extractJSONObject returns the first balanced {...} run in s, so a verdict still
// parses even if the model wraps it in stray prose or a code fence.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
