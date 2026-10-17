package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports how the pack's sessions take fenced input: atomic for
// a pack that declares the exec connection (proc.exec), whose input the tmux
// carrier guards with the tmux provider's if-shell on the agent pane's fence;
// none for a pack driven by the dedicated wire ops, which cannot condition
// input on an incarnation. Like the pack's other traits it is read from the
// handshake, so it is static per provider.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	if p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec) {
		return runtime.InputFencingAtomic
	}
	return runtime.InputFencingNone
}

// fencedCarrier is the carrier for fenced input to name, or
// ErrInputFenceUnsupported for a pack that cannot fence.
func (p *Provider) fencedCarrier(name string) (runtime.Carrier, error) {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return nil, fmt.Errorf("session %q: %w", name, runtime.ErrInputFenceUnsupported)
	}
	return p.carrier(), nil
}

// fencedResult is what a carrier-guarded op on name reports for err. A fence
// read the pack's exec op did not answer (its own non-zero exit, as for a
// box that does not exist) cannot tell absence from an outage, so the pack's
// is-running decides: a definite "false" is a miss finding the box absent;
// anything else stays ErrRuntimeUnavailable.
func (p *Provider) fencedResult(name string, err error) error {
	if !errors.Is(err, runtime.ErrCarrierFenceUnread) {
		return err
	}
	if occupied, definite := p.boxOccupancy(name); definite && !occupied {
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundAbsent}
	}
	return err
}

// NudgeFenced delivers content as Nudge does, through the carrier guard. The
// exec provider has no idle wait to skip and cannot deduplicate, so
// n.Immediate and n.DeliveryKey change nothing.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return p.fencedResult(name, c.NudgeFenced(ctx, name, content, n.Fence))
}

// SendKeysFenced sends keys as SendKeys does, through the carrier guard.
func (p *Provider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return p.fencedResult(name, c.SendKeysFenced(ctx, name, fence, keys...))
}

// InterruptFenced interrupts as Interrupt does, through the carrier guard.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return p.fencedResult(name, c.InterruptFenced(ctx, name, fence))
}

// clearInputFence removes the agent pane's fence before a welded start op,
// for a pack that can fence, so a pack whose start reuses a pane never leaves
// the previous incarnation's fence naming the new agent. Best-effort: a box
// that does not exist yet has no pane to clear, and the publish after the
// start op is required.
func (p *Provider) clearInputFence(ctx context.Context, name string) {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return
	}
	_, _, _ = p.Exec(ctx, name, append([]string{"tmux"}, runtime.TmuxClearInputFenceArgs(execTmuxSession)...))
}

// publishInputFence records the incarnation cfg.Env names on the agent pane
// a welded start op created, for a pack that can fence. A pane left without
// its fence would be unproven to every fenced op, so a failed publish fails
// the start.
func (p *Provider) publishInputFence(ctx context.Context, name string, cfg runtime.Config) error {
	f, ok := runtime.InputFenceFromEnv(cfg.Env)
	if !ok || p.InputFencing(name) == runtime.InputFencingNone {
		return nil
	}
	if err := p.carrier().PublishInputFence(ctx, name, f); err != nil {
		return fmt.Errorf("exec provider: %w", err)
	}
	return nil
}

// launchFenceArgs is the tmux command, joined to the launch's own as one
// command list, that sets the launched agent pane's fence to the
// incarnation cfg.Env names, or clears it when cfg.Env names none: a
// respawned pane keeps its option, so its predecessor's fence must never
// name it. Nil for a pack that cannot fence.
func (p *Provider) launchFenceArgs(name string, cfg runtime.Config) []string {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return nil
	}
	if f, ok := runtime.InputFenceFromEnv(cfg.Env); ok {
		return append([]string{";"}, runtime.TmuxPublishInputFenceArgs(execTmuxSession, f)...)
	}
	return append([]string{";"}, runtime.TmuxClearInputFenceArgs(execTmuxSession)...)
}
