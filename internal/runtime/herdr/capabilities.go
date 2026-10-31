package herdr

import (
	"context"
	"encoding/json"
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
	idleWaitBlocked idleWaitOutcome = "blocked"  // herdr reads the agent as waiting at a dialog
	idleWaitTimeout idleWaitOutcome = "timeout"  // bound elapsed without idle
	idleWaitNoAgent idleWaitOutcome = "no_agent" // no registered agent (raw shell pane)
	idleWaitError   idleWaitOutcome = "error"    // transport or unexpected herdr error
)

// agentStateBlocked is herdr's agent_status for an agent waiting at a dialog;
// herdr ≥0.8.2 reads Claude's workspace-trust dialog this way.
const agentStateBlocked = "blocked"

// waitForIdleOutcome blocks until herdr reports the agent idle or the timeout
// elapses, via herdr's native `agent wait --until idle` (the ≥0.7.5 flag
// spelling) — vs the pane-polling tmux does — and returns the verdict
// distinctly instead of discarding it.
func (p *Provider) waitForIdleOutcome(ctx context.Context, name string, timeout time.Duration) idleWaitOutcome {
	// A Claude pane that has finished a turn reports "done", not "idle";
	// waiting for idle alone never returns for it.
	return p.waitForAgentStatus(ctx, name, timeout, "idle", "done")
}

// waitForStartupReadiness is Start's readiness wait: waitForIdleOutcome that
// also ends when herdr reads the agent as blocked. herdr matches only the
// states a wait names, so without blocked an agent parked on a dialog — a
// trust cursor no pass could move, a dialog the matcher does not know, or
// dialog acceptance turned off — holds the wait for its whole bound. That
// outlasts Start's own deadline under the default 60s [session]
// startup_timeout, and Start would fail before the dialog pass, the warning
// and the withheld or refused first turn that follow the wait.
func (p *Provider) waitForStartupReadiness(ctx context.Context, name string, timeout time.Duration) idleWaitOutcome {
	return p.waitForAgentStatus(ctx, name, timeout, "idle", "done", agentStateBlocked)
}

// waitForAgentStatus runs `agent wait --until <status>…` and classifies the
// verdict; a blocked match is reported apart from idle or done.
func (p *Provider) waitForAgentStatus(ctx context.Context, name string, timeout time.Duration, until ...string) idleWaitOutcome {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	args := []string{"agent", "wait", herdrAgentName(name)}
	for _, s := range until {
		args = append(args, "--until", s)
	}
	args = append(args, "--timeout", strconv.Itoa(ms))
	res, err := p.c.run(ctx, args...)
	switch {
	case err == nil:
		return matchedStatusOutcome(res)
	case herdrErrorCode(err) == "timeout":
		return idleWaitTimeout
	case p.c.targetHasNoNamedAgent(ctx, herdrAgentName(name), err):
		return idleWaitNoAgent
	default:
		return idleWaitError
	}
}

// matchedStatusOutcome classifies a successful `agent wait` by the agent
// status herdr matched. A reply that does not decode is an unexpected herdr
// answer, not a reached idle.
func matchedStatusOutcome(res json.RawMessage) idleWaitOutcome {
	var wrap struct {
		Agent agentInfo `json:"agent"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return idleWaitError
	}
	if normalizeAgentState(wrap.Agent.AgentStatus) == agentStateBlocked {
		return idleWaitBlocked
	}
	return idleWaitReached
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
