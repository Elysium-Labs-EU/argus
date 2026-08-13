package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
)

// deliverPaneMessage submits message into paneID's live agent, so a
// supervisor command can resume or redirect a worker without a human
// free-typing into herdr by hand. There is no spawn-a-fresh-agent fallback: a
// pane with no live agent means the original worker's session is gone, and
// starting a brand-new one with no context beyond this one message would not
// actually resume the original task — the caller needs `argus rework`/`argus
// rebase` instead, which re-dispatch with a full brief.
//
// action names the eventlog entries this call emits (e.g. "answer", "steer")
// so different callers' deliveries stay distinguishable in the run log.
//
// AgentPrompt's own confirmation wait can come back non-nil two ways that
// look identical from here — herdr's dedicated ErrAgentPromptStalled (a
// prompt it positively observed land with no subsequent state change) and
// the generic ErrWaitTimeout its ordinary wait mechanism also uses — and
// both can mean either "genuinely never picked up" or "picked up, just
// slower than the wait window" depending on what the pane was doing before
// the prompt was ever sent. That prior state is the one signal that
// disambiguates them: a pane already "working" before AgentPrompt was
// called is mid an unrelated turn that will silently drop both the prompt
// and any retyped fallback text the same way, so retrying into it would
// only let the fallback's own AgentWait rediscover that pre-existing busy
// state and misreport it as a fresh pickup (AgentWait is level-triggered —
// see herdr.Client.AgentWait). A pane that was idle or done before the
// prompt, by contrast, has nothing else to attribute a later "working" to,
// so the fallback's confirmation there is trustworthy.
func deliverPaneMessage(ctx context.Context, logger *eventlog.Logger, client herdr.Client, paneID, worktree, action, message string) error {
	pane, live, err := client.AgentGet(ctx, paneID)
	if err != nil {
		return fmt.Errorf("checking whether pane %s has a live agent: %w", paneID, err)
	}
	if !live {
		return fmt.Errorf("pane %s has no live agent — the worker's session is gone", paneID)
	}

	timeout := defaultLivenessTimeout
	perr := client.AgentPrompt(ctx, paneID, message, timeout)
	if perr == nil {
		logger.Action(action, worktree, "delivered", paneID)
		return nil
	}
	recoverable := errors.Is(perr, herdr.ErrAgentPromptStalled) || errors.Is(perr, herdr.ErrWaitTimeout)
	if !recoverable || pane.AgentStatus == "working" {
		return fmt.Errorf("delivering %s to pane %s: %w", action, paneID, perr)
	}

	logger.Action(action, worktree, "prompt-stalled-fallback-pane-run", paneID)
	if rerr := client.PaneRun(ctx, paneID, message); rerr != nil {
		return fmt.Errorf("delivering %s to pane %s: %w (pane-run fallback also failed: %w)", action, paneID, perr, rerr)
	}
	if kerr := client.PaneSendKeys(ctx, paneID, "enter"); kerr != nil {
		return fmt.Errorf("delivering %s to pane %s: %w (pane-run fallback's submit keystroke failed: %w)", action, paneID, perr, kerr)
	}
	if _, werr := client.AgentWait(ctx, paneID, []string{"working"}, timeout); werr != nil {
		return fmt.Errorf("delivering %s to pane %s: %w (pane-run fallback sent, but agent never started working: %w)", action, paneID, perr, werr)
	}
	logger.Action(action, worktree, "delivered-via-fallback", paneID)
	return nil
}
