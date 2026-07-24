package runtime

import (
	"strings"
	"testing"
)

// Realistic tmux pane tails captured after a provider transport failure aborts
// a turn and the CLI returns to its idle prompt. These mirror incident ci-emg
// (Codex WebSockets→HTTPS fallback timeout and HTTPS stream disconnect) and the
// Claude ENOTFOUND DNS failure named in the ga-qox acceptance criteria.
const (
	codexWSFallbackInertPane = `● I'll start by reading the requirements file.

⚠ stream error: Falling back from WebSockets to HTTPS transport. request timed out
⚠ stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)

› `

	codexWSFallbackOnlyInertPane = `● Starting the request.

⚠ stream error: Falling back from WebSockets to HTTPS transport. request timed out

› `

	codexDNSInertPane = `● Working on the task.

Falling back from WebSockets to HTTPS transport. stream disconnected before completion: failed to lookup address information: nodename nor servname provided, or not known
stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)

› `

	claudeENOTFOUNDInertPane = `● Let me fetch the upstream branch.

API Error: request to https://api.anthropic.com/v1/messages failed, reason: getaddrinfo ENOTFOUND api.anthropic.com

❯ `

	// Mid-turn: the transport error printed but the agent is still working
	// (a live spinner footer, no input prompt). Must NOT be classified — an
	// active turn is not restarted.
	codexTransportFailureMidTurnPane = `● Retrying the request.

stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)
· Herding the model… (2m 28s · esc to interrupt)`

	// Ordinary idle prompt after a clean turn — no transport failure at all.
	ordinaryIdlePane = `● Done. The change is committed.

⎿ committed 3 files

› `

	// A WebSocket failure followed by a successful HTTPS response is not an
	// inert turn. The warning remains in the recent pane tail, but healthy
	// output after it proves the fallback completed normally.
	codexSuccessfulFallbackPane = `⚠ stream error: Falling back from WebSockets to HTTPS transport. request timed out

● The HTTPS retry succeeded and the requested change is complete.

› `

	// A prior input prompt can remain in the tmux tail while the current turn is
	// still active. The classifier must require the idle prompt after the
	// terminal failure, not merely any prompt somewhere in the tail.
	codexPriorPromptMidTurnPane = `› Continue the implementation.

● Retrying the request.

stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)
· Herding the model… (2m 28s · esc to interrupt)`

	// The prior turn itself ended on an error, but a newer turn is now active
	// after that prompt. The old terminal block must not be recovered again.
	codexPriorFailedTurnThenActivePane = `stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)

› Retry the task.

● Retrying the request.
stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)
· Herding the model… (2m 28s · esc to interrupt)`

	// Provider status chrome can render below the prompt. It does not make a
	// terminal failure non-idle.
	claudeENOTFOUNDWithPromptChromePane = `API Error: request to https://api.anthropic.com/v1/messages failed, reason: getaddrinfo ENOTFOUND api.anthropic.com

❯
────────────────────────────────────────────────────────────────────────────────
⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt`

	// tmux capture-pane may split a long terminal error across physical lines.
	// The contiguous terminal block immediately before the prompt still counts.
	codexWrappedTerminalFailurePane = `● Working on the task.

stream disconnected before completion: failed to lookup address
information: nodename nor servname provided, or not known

› `
)

func TestClassifyInertTransportFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		content         string
		wantRecoverable bool
		wantReason      string
	}{
		{
			name:            "codex websocket fallback timeout at idle prompt",
			content:         codexWSFallbackInertPane,
			wantRecoverable: true,
			wantReason:      transportFailureStreamDisconnected,
		},
		{
			name:            "codex terminal websocket fallback timeout at idle prompt",
			content:         codexWSFallbackOnlyInertPane,
			wantRecoverable: true,
			wantReason:      transportFailureWSFallback,
		},
		{
			name:            "codex dns lookup failure at idle prompt",
			content:         codexDNSInertPane,
			wantRecoverable: true,
			wantReason:      transportFailureDNSLookup,
		},
		{
			name:            "claude enotfound at idle prompt",
			content:         claudeENOTFOUNDInertPane,
			wantRecoverable: true,
			wantReason:      transportFailureDNSLookup,
		},
		{
			name:            "transport failure mid-turn is not recoverable",
			content:         codexTransportFailureMidTurnPane,
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "ordinary idle prompt is not a transport failure",
			content:         ordinaryIdlePane,
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "successful websocket fallback is not an inert turn",
			content:         codexSuccessfulFallbackPane,
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "prompt from prior turn does not make current active turn inert",
			content:         codexPriorPromptMidTurnPane,
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "failed prior turn is not recovered while newer turn is active",
			content:         codexPriorFailedTurnThenActivePane,
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "prompt footer chrome preserves terminal failure classification",
			content:         claudeENOTFOUNDWithPromptChromePane,
			wantRecoverable: true,
			wantReason:      transportFailureDNSLookup,
		},
		{
			name:            "wrapped terminal transport error at idle prompt",
			content:         codexWrappedTerminalFailurePane,
			wantRecoverable: true,
			wantReason:      transportFailureDNSLookup,
		},
		{
			name:            "partial phrase without the full signature does not match",
			content:         "● The stream disconnected briefly but reconnected fine.\n\n› ",
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "healthy final explanation quoting exact error is not terminal failure",
			content:         "● The log contained `stream disconnected before completion: error sending request for url`; the fix is complete.\n\n› ",
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "rate limit screen is a different lane, not transport",
			content:         "Usage limit reached for your plan.\n\n› ",
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "shell prompt after provider exit is not a live agent prompt",
			content:         "stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)\n\nuser@host % ",
			wantRecoverable: false,
			wantReason:      "",
		},
		{
			name:            "empty pane",
			content:         "",
			wantRecoverable: false,
			wantReason:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotRecoverable, gotReason := ClassifyInertTransportFailure(tt.content)
			if gotRecoverable != tt.wantRecoverable {
				t.Errorf("ClassifyInertTransportFailure() recoverable = %v, want %v\ncontent:\n%s",
					gotRecoverable, tt.wantRecoverable, tt.content)
			}
			if gotReason != tt.wantReason {
				t.Errorf("ClassifyInertTransportFailure() reason = %q, want %q", gotReason, tt.wantReason)
			}
		})
	}
}

// TestClassifyInertTransportFailure_RecentFailureFollowedBySuccess proves the
// bounded-tail check is not sufficient by itself: even when the old failure is
// still within the last 20 lines, later healthy turn output makes it historical
// rather than terminal.
func TestClassifyInertTransportFailure_RecentFailureFollowedBySuccess(t *testing.T) {
	t.Parallel()
	content := `stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)

● The retry succeeded.
⎿ All requested files were updated.

› `

	recoverable, reason := ClassifyInertTransportFailure(content)
	if recoverable {
		t.Errorf("recent historical error followed by success must not be recoverable (reason=%q)", reason)
	}
}

// TestClassifyInertTransportFailure_HistoricalTextOutsideTail proves pasted or
// scrolled-back error text far above the current prompt cannot false-trigger
// recovery: only a failure within the bounded recent suffix counts.
func TestClassifyInertTransportFailure_HistoricalTextOutsideTail(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	// A real-looking transport error, but buried far up in scrollback.
	b.WriteString("stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)\n")
	// Plenty of subsequent, healthy activity pushes it out of the recent tail.
	for i := 0; i < 60; i++ {
		b.WriteString("● Completed step, moving on to the next file.\n")
	}
	b.WriteString("\n› ")

	recoverable, reason := ClassifyInertTransportFailure(b.String())
	if recoverable {
		t.Errorf("historical error outside the recent tail must not be recoverable (reason=%q)", reason)
	}
}

// TestClassifyInertTransportFailure_StableReason proves the reason is
// deterministic for a given pane so the recovery state machine can dedup an
// ongoing failure episode by fingerprint instead of re-arming every tick.
func TestClassifyInertTransportFailure_StableReason(t *testing.T) {
	t.Parallel()
	_, first := ClassifyInertTransportFailure(codexDNSInertPane)
	_, second := ClassifyInertTransportFailure(codexDNSInertPane)
	if first == "" || first != second {
		t.Fatalf("reason not stable: first=%q second=%q", first, second)
	}
}
