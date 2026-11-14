package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the un-welded start's tail (proc.provision): the non-tmux
// `launch` op (proc.launch), and the first turn gc delivers once the agent is
// launched. `provision` runs before any agent exists and `launch` types
// nothing, so on this path the start config's nudge, which carries a pack
// runtime's startup prompt, is gc's to deliver: once, after the dialog pass
// and a readiness wait, through the path every later nudge takes. A turn the
// runtime refused is handed back to the Start's caller
// ([runtime.StartupNudgeHandbackProvider]), which queues it.

// launchOp is the op that starts the agent in a provisioned box.
const launchOp = "launch"

// firstTurnReadyBound caps the readiness wait before the first turn.
const firstTurnReadyBound = 60 * time.Second

// launchDeclared reports whether the pack declared the `launch` op.
func (p *Provider) launchDeclared() bool {
	return p.handshakeCapability(runtime.ProtocolCapabilityLaunch)
}

// launchConfig is the start config the `launch` op receives: the config
// `provision` received, without the nudge gc delivers itself (and, for a pack
// sharing the host filesystem, without the host work gc does itself).
func (p *Provider) launchConfig(cfg runtime.Config) ([]byte, error) {
	cfg.Nudge = ""
	marshal := marshalStartConfig
	if p.sharesHostFS() {
		marshal = marshalHostStagedStartConfig
	}
	data, err := marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("exec provider: marshaling launch config: %w", err)
	}
	return data, nil
}

// runLaunchOp starts the agent in name's box through the `launch` op. A pack
// that can fence has the previous incarnation's fence cleared before the op
// and this one's published after it, as a welded start does.
func (p *Provider) runLaunchOp(ctx context.Context, name string, cfg runtime.Config) error {
	data, err := p.launchConfig(cfg)
	if err != nil {
		return err
	}
	p.clearInputFence(ctx, name)
	if _, err := p.runWithContext(ctx, p.startTimeout, data, launchOp, name); err != nil {
		return fmt.Errorf("exec provider: launching agent in %q: %w", name, err)
	}
	return p.publishInputFence(ctx, name, cfg)
}

// withoutNudge is cfg without its first turn, which a separable launch
// delivers itself (finishSeparableLaunch), so `provision` is never sent it.
func withoutNudge(cfg runtime.Config) runtime.Config {
	cfg.Nudge = ""
	return cfg
}

// finishSeparableLaunch runs what follows a separable launch of name, in
// order: when there is a first turn, a readiness wait; the host
// session_setup when hostSetup; then the first turn. A welded pack's start
// op already did all of it, so there it does nothing.
func (p *Provider) finishSeparableLaunch(ctx context.Context, name string, cfg runtime.Config, hostSetup bool) error {
	if !p.supportsSeparableLaunch() {
		return nil
	}
	if cfg.Nudge != "" {
		if err := p.awaitFirstTurnReady(ctx, name, cfg); err != nil {
			return err
		}
	}
	if hostSetup {
		p.runHostSessionSetup(ctx, name, cfg)
	}
	if cfg.Nudge == "" {
		return nil
	}
	return p.deliverFirstTurn(name, cfg.Nudge)
}

// awaitFirstTurnReady waits, bounded by ctx and firstTurnReadyBound, for the
// launched agent to take input: the selected carrier's idle wait when the
// pack declares an idle boundary, otherwise the config's ready_delay_ms. A
// wait that runs out is not a failure; a canceled or expired ctx is.
func (p *Provider) awaitFirstTurnReady(ctx context.Context, name string, cfg runtime.Config) error {
	bound := firstTurnReadyBound
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < bound {
		bound = time.Until(deadline)
	}
	if p.idleBoundaryDeclared() {
		_ = p.WaitForIdle(ctx, name, bound) // a timeout is not a failure; ctx is checked below
	} else if delay := time.Duration(cfg.ReadyDelayMs) * time.Millisecond; delay > 0 {
		timer := time.NewTimer(min(delay, bound))
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("exec provider: waiting for %q to take its first turn: %w", name, err)
	}
	return nil
}

// deliverFirstTurn sends nudge to name once, through the selected carrier
// (Provider.Nudge), and reads its outcome. A delivered answer, delivered
// unobserved included, is final. An answer that may have been delivered
// (unconfirmed, or an untyped failure from a pack without input-outcome) is
// a warning and is not handed back, which could deliver it twice. A turn the
// runtime did not take (busy, pending_interaction, rejected, not_sent,
// unavailable) is handed back. Gone is the agent dying during startup.
func (p *Provider) deliverFirstTurn(name, nudge string) error {
	err := p.Nudge(name, runtime.TextContent(nudge))
	switch {
	case runtime.NudgeDelivered(err):
		return nil
	case errors.Is(err, runtime.ErrSessionNotFound):
		return fmt.Errorf("%w: exec provider: %q was gone when its first turn was sent: %w", runtime.ErrSessionDiedDuringStartup, name, err)
	case firstTurnNotTaken(err):
		p.handbacks.set(name, nudge)
		return nil
	default:
		fmt.Fprintf(p.warnings(), "gc: exec provider: %q's first turn may not have been delivered: %v\n", name, err) //nolint:errcheck // best-effort warning
		return nil
	}
}

// firstTurnNotTaken reports a nudge outcome that says the runtime typed
// nothing, so handing the turn back cannot deliver it twice.
func firstTurnNotTaken(err error) bool {
	for _, sentinel := range []error{
		runtime.ErrSessionBusy, runtime.ErrNudgeRefusedPendingInteraction, runtime.ErrNudgeRejected,
		runtime.ErrNudgeNotSent, runtime.ErrRuntimeUnavailable,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// startupHandbacks holds, per session, the first turn a separable launch
// handed back, until the Start's caller takes it. Each launch clears its
// session's entry first, so an untaken one never outlives the next start.
type startupHandbacks struct {
	mu     sync.Mutex
	nudges map[string]string
}

// set records nudge for session; an empty nudge clears it.
func (h *startupHandbacks) set(session, nudge string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if nudge == "" {
		delete(h.nudges, session)
		return
	}
	if h.nudges == nil {
		h.nudges = make(map[string]string)
	}
	h.nudges[session] = nudge
}

// take returns and forgets session's handed-back turn.
func (h *startupHandbacks) take(session string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nudge, ok := h.nudges[session]
	delete(h.nudges, session)
	return nudge, ok
}

// TakeStartupNudgeHandback implements [runtime.StartupNudgeHandbackProvider]:
// the first turn the last separable launch of name sent and the runtime did
// not take, once.
func (p *Provider) TakeStartupNudgeHandback(name string) (string, bool) {
	return p.handbacks.take(name)
}

var _ runtime.StartupNudgeHandbackProvider = (*Provider)(nil)
