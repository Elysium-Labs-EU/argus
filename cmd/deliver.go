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
func deliverPaneMessage(ctx context.Context, logger *eventlog.Logger, client herdr.Client, paneID, worktree, action, message string) error {
	_, live, err := client.AgentGet(ctx, paneID)
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
	if !errors.Is(perr, herdr.ErrAgentPromptStalled) {
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
