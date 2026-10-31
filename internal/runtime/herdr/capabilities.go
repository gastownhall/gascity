package herdr

import (
	"context"
	"fmt"
	"slices"
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
// probe. Start's startup delivery proceeds either way, but records WHY a
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
	outcome, _ := p.waitForIdleVerdict(ctx, name, timeout)
	return outcome
}

// waitForIdleVerdict is waitForIdleOutcome with the herdr failure behind every
// verdict but idleWaitReached. herdr's timeout is read from the channels only
// herdr writes (herdrAnswerCode): a non-zero exit carries it on stderr.
func (p *Provider) waitForIdleVerdict(ctx context.Context, name string, timeout time.Duration) (idleWaitOutcome, error) {
	ms := int(timeout / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	// A Claude pane that has finished a turn reports "done", not "idle";
	// waiting for idle alone never returns for it.
	_, err := p.c.run(ctx, "agent", "wait", herdrAgentName(name), "--until", "idle", "--until", "done", "--timeout", strconv.Itoa(ms))
	switch {
	case err == nil:
		return idleWaitReached, nil
	case herdrAnswerCode(err) == "timeout":
		return idleWaitTimeout, err
	case p.c.targetHasNoNamedAgent(ctx, herdrAgentName(name), err):
		return idleWaitNoAgent, err
	default:
		return idleWaitError, err
	}
}

// idleWaitBusyStates are the agent_status readings that show a turn running:
// working, or blocked on a dialog the turn opened.
var idleWaitBusyStates = []string{"working", "blocked"}

// idleWaitStatusReserve is the slice of a WaitForIdle bound kept back from
// herdr's native wait for the one status read a timeout needs, so that read
// still lands inside the bound.
func idleWaitStatusReserve(timeout time.Duration) time.Duration {
	return min(timeout/5, 2*time.Second)
}

// WaitForIdle blocks until herdr reports the agent idle (or done) or the
// timeout elapses; the timeout is a hard bound. An unregistered session (a raw
// shell pane has no agent to wait on) returns nil at once. A timeout returns
// an error wrapping context.DeadlineExceeded, and runtime.ErrWaitUnobserved as
// well unless the agent was read busy (RUNTIME-CONTRACT-016). A herdr that
// cannot answer returns an error wrapping runtime.ErrRuntimeUnavailable, and a
// canceled ctx returns its error.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outcome, err := p.waitForIdleVerdict(waitCtx, name, timeout-idleWaitStatusReserve(timeout))
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	switch {
	case outcome == idleWaitReached, outcome == idleWaitNoAgent:
		return nil
	case outcome == idleWaitTimeout:
		return p.idleWaitTimedOut(waitCtx, name, timeout)
	case waitCtx.Err() != nil:
		// The bound ended herdr's wait before herdr answered: nothing was read.
		return idleWaitTimeoutError(name, timeout, false)
	default:
		return fmt.Errorf("herdr: waiting for %q to idle: %w: %w", name, runtime.ErrRuntimeUnavailable, err)
	}
}

// idleWaitTimedOut classifies a timed-out herdr wait. herdr reports only that
// the agent never reached idle or done, not what it read meanwhile, so one
// status read inside the bound tells a running turn from an agent herdr cannot
// classify ("unknown") or that could not be read at all.
func (p *Provider) idleWaitTimedOut(ctx context.Context, name string, timeout time.Duration) error {
	a, ok, err := p.c.getAgent(ctx, herdrAgentName(name))
	if err != nil || !ok {
		return idleWaitTimeoutError(name, timeout, false)
	}
	switch state := normalizeAgentState(a.AgentStatus); {
	case slices.Contains(idleWaitBusyStates, state):
		return idleWaitTimeoutError(name, timeout, true)
	case state == agentStateIdle || state == "done":
		return nil
	default:
		return idleWaitTimeoutError(name, timeout, false)
	}
}

// idleWaitTimeoutError is WaitForIdle's timeout: context.DeadlineExceeded,
// wrapping runtime.ErrWaitUnobserved as well unless the agent was read busy.
func idleWaitTimeoutError(name string, timeout time.Duration, sawBusy bool) error {
	if sawBusy {
		return fmt.Errorf("herdr: %q not idle within %s: %w", name, timeout, context.DeadlineExceeded)
	}
	return fmt.Errorf("herdr: %q not idle within %s: %w: %w", name, timeout, context.DeadlineExceeded, runtime.ErrWaitUnobserved)
}

// NudgeNow injects input immediately. herdr's send/run already deliver without a
// wait-idle heuristic, so this is the same delivery path as Nudge.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	return p.Nudge(name, content)
}
