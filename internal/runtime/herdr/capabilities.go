package herdr

import (
	"context"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Optional capability interfaces herdr supports natively. (Relaunch,
// ProcessTableScanner, InterruptBoundaryWait, and DialogProvider are
// deliberately omitted from the first cut — the reconciler degrades gracefully
// when a provider lacks them: Relaunch falls back to Stop+Start, the others to
// no-op/default behavior.) SessionEventProvider is implemented in events.go
// over the socket API's events.subscribe stream.
var (
	_ runtime.IdleWaitProvider       = (*Provider)(nil)
	_ runtime.ImmediateNudgeProvider = (*Provider)(nil)
	// LivenessObserver lets the reconciler read aliveness from herdr's own
	// agent-status instead of the host process-table walk (see
	// provider.go ObserveLiveness).
	_ runtime.LivenessObserver = (*Provider)(nil)
	// SessionEventProvider is implemented in events.go over the socket API's
	// events.subscribe stream (see #4217 herdr-first-class).
	_ runtime.SessionEventProvider = (*Provider)(nil)
)

// idleWaitOutcome is the legible verdict of one `agent wait --until idle`
// probe. The interface-facing WaitForIdle keeps its proceed-either-way
// contract, but internal callers (Start's startup delivery) need to see WHY a
// wait did not confirm: the live city measured 0 ok / 8 timeout / 2 error
// over 20h with every verdict silently discarded (gas-90h).
type idleWaitOutcome string

const (
	idleWaitReached idleWaitOutcome = "idle"     // herdr observed the agent idle
	idleWaitTimeout idleWaitOutcome = "timeout"  // bound elapsed without idle
	idleWaitNoAgent idleWaitOutcome = "no_agent" // no registered agent (raw shell pane)
	idleWaitError   idleWaitOutcome = "error"    // transport or unexpected herdr error
)

// waitForIdleOutcome blocks until herdr reports the agent idle or the timeout
// elapses, via herdr's native `agent wait --until idle` (the ≥0.7.5 flag
// spelling) — vs the pane-polling tmux does — and returns the verdict
// distinctly instead of discarding it.
func (p *Provider) waitForIdleOutcome(ctx context.Context, name string, timeout time.Duration) idleWaitOutcome {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	_, err := p.c.run(ctx, "agent", "wait", herdrAgentName(name), "--until", "idle", "--timeout", strconv.Itoa(ms))
	switch {
	case err == nil:
		return idleWaitReached
	case herdrErrorCode(err) == "timeout":
		return idleWaitTimeout
	case isAgentNotFound(err):
		return idleWaitNoAgent
	default:
		return idleWaitError
	}
}

// WaitForIdle blocks until herdr reports the agent idle or the timeout
// elapses. Either outcome (idle reached or timed out) means the caller may
// proceed — as does an unregistered session (raw shell panes have no agent to
// wait on) — so only context cancellation surfaces as an error; the timeout
// is a hard bound.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	_ = p.waitForIdleOutcome(ctx, name, timeout)
	return ctx.Err()
}

// NudgeNow injects input immediately. herdr's send/run already deliver without a
// wait-idle heuristic, so this is the same delivery path as Nudge.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	return p.Nudge(name, content)
}
