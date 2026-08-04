package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/eventlog"
	"github.com/Elysium-Labs-EU/argus/internal/herdr"
)

const deliverTestPaneID = "w1:p1"

// deliverFakeConfig controls how each herdr call in the fake runner behaves,
// letting a single table exercise every branch in deliverPaneMessage.
type deliverFakeConfig struct {
	agentGetErr     error
	agentPromptErr  error
	paneRunErr      error
	paneSendKeysErr error
	agentWaitErr    error
	agentLive       bool
}

func fakeDeliverClient(cfg *deliverFakeConfig) herdr.Client {
	return herdr.NewWithRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[0] == "agent" && args[1] == "get":
			if cfg.agentGetErr != nil {
				return nil, cfg.agentGetErr
			}
			if !cfg.agentLive {
				return nil, fmt.Errorf("herdr agent get: %w", herdr.ErrAgentNotFound)
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, deliverTestPaneID), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "prompt":
			if cfg.agentPromptErr != nil {
				return nil, cfg.agentPromptErr
			}
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "run":
			if cfg.paneRunErr != nil {
				return nil, cfg.paneRunErr
			}
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "pane" && args[1] == "send-keys":
			if cfg.paneSendKeysErr != nil {
				return nil, cfg.paneSendKeysErr
			}
			return []byte(`{"result":{}}`), nil
		case len(args) > 1 && args[0] == "agent" && args[1] == "wait":
			if cfg.agentWaitErr != nil {
				return nil, cfg.agentWaitErr
			}
			return fmt.Appendf(nil, `{"result":{"agent":{"pane_id":%q,"agent":"claude","agent_status":"working"}}}`, deliverTestPaneID), nil
		default:
			return []byte(`{"result":{}}`), nil
		}
	})
}

// loggedActions decodes a Logger's JSONL output into just the outcome strings,
// in emission order, so a test can assert which branch fired without parsing
// full Event structs.
func loggedActions(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var outcomes []string
	dec := json.NewDecoder(buf)
	for dec.More() {
		var e eventlog.Event
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decoding logged event: %v", err)
		}
		outcomes = append(outcomes, e.Outcome)
	}
	return outcomes
}

func TestDeliverPaneMessage(t *testing.T) {
	cases := []struct {
		name        string
		cfg         deliverFakeConfig
		wantErr     string
		wantActions []string
	}{
		{
			name:    "agent get error propagates",
			cfg:     deliverFakeConfig{agentGetErr: errors.New("herdr: socket unavailable")},
			wantErr: "socket unavailable",
		},
		{
			name:    "no live agent rejected",
			cfg:     deliverFakeConfig{agentLive: false},
			wantErr: "has no live agent",
		},
		{
			name:        "prompt success delivers directly",
			cfg:         deliverFakeConfig{agentLive: true},
			wantActions: []string{"delivered"},
		},
		{
			name:    "non-stalled prompt error propagates without fallback",
			cfg:     deliverFakeConfig{agentLive: true, agentPromptErr: errors.New("herdr: socket unavailable")},
			wantErr: "socket unavailable",
		},
		{
			name: "stalled prompt falls back but pane-run also fails",
			cfg: deliverFakeConfig{
				agentLive:      true,
				agentPromptErr: fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled),
				paneRunErr:     errors.New("herdr: pane run refused"),
			},
			wantErr:     "pane-run fallback also failed",
			wantActions: []string{"prompt-stalled-fallback-pane-run"},
		},
		{
			name: "stalled prompt falls back but submit keystroke fails",
			cfg: deliverFakeConfig{
				agentLive:       true,
				agentPromptErr:  fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled),
				paneSendKeysErr: errors.New("herdr: send-keys refused"),
			},
			wantErr:     "submit keystroke failed",
			wantActions: []string{"prompt-stalled-fallback-pane-run"},
		},
		{
			name: "stalled prompt falls back but agent never starts working",
			cfg: deliverFakeConfig{
				agentLive:      true,
				agentPromptErr: fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled),
				agentWaitErr:   herdr.ErrWaitTimeout,
			},
			wantErr:     "never started working",
			wantActions: []string{"prompt-stalled-fallback-pane-run"},
		},
		{
			name: "stalled prompt falls back and fully succeeds",
			cfg: deliverFakeConfig{
				agentLive:      true,
				agentPromptErr: fmt.Errorf("herdr agent: exit status 1: %w", herdr.ErrAgentPromptStalled),
			},
			wantActions: []string{"prompt-stalled-fallback-pane-run", "delivered-via-fallback"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := eventlog.New(&buf, "deliver-test", "test-run", nil)
			client := fakeDeliverClient(&tc.cfg)

			err := deliverPaneMessage(context.Background(), logger, client, deliverTestPaneID, "wt", "steer", "note")

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("deliverPaneMessage: want success, got %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("deliverPaneMessage error = %v, want it to contain %q", err, tc.wantErr)
				}
			}

			gotActions := loggedActions(t, &buf)
			if len(gotActions) != len(tc.wantActions) {
				t.Fatalf("logged outcomes = %v, want %v", gotActions, tc.wantActions)
			}
			for i, want := range tc.wantActions {
				if gotActions[i] != want {
					t.Errorf("logged outcome[%d] = %q, want %q", i, gotActions[i], want)
				}
			}
		})
	}
}
