package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports how the pack's sessions take fenced input: atomic for
// a pack that declares the exec connection (proc.exec) and input fencing
// (input-fence), whose input the tmux carrier guards with the tmux provider's
// if-shell on the agent pane's fence; none for any other pack, whose box tmux
// need not hold a fence, or which is driven by the dedicated wire ops that
// cannot condition input on an incarnation. Like the pack's other traits it
// is read from the handshake, so it is static per provider.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	if p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec) &&
		p.handshakeCapability(runtime.ProtocolCapabilityInputFence) {
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

// fencedResult is what a carrier-guarded op on name reports for err. A
// command the box did not answer (runtime.CarrierFenceUnreadError: the exec
// op's own non-zero exit, as for a box that does not exist, or tmux in the
// box failing) cannot tell absence from an outage, so the pack's is-running
// decides: a definite "false" is a miss finding the box absent, partial when
// some of the operation's effect was applied first; anything else stays
// ErrRuntimeUnavailable.
func (p *Provider) fencedResult(name string, err error) error {
	var unread *runtime.CarrierFenceUnreadError
	if !errors.As(err, &unread) {
		return err
	}
	if occupied, definite := p.boxOccupancy(name); definite && !occupied {
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundAbsent, Partial: unread.Partial}
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

// paneFenceSaveFormat reads the agent pane's fence value, id and root
// process, so a fence cleared before a start op can be put back on the same
// process (restoreInputFence).
const paneFenceSaveFormat = "#{" + runtime.TmuxInputFenceOption + "}|#{pane_id}|#{pane_pid}"

// savedPaneFence is the own fence clearInputFence removed from a live agent
// pane, with the pane and root process that held it. Zero when the pane held
// none or the read did not answer.
type savedPaneFence struct {
	digest, pane, pid string
}

// parseSavedPaneFence parses paneFenceSaveFormat's expansion, keeping only a
// fence the pane held as its own.
func parseSavedPaneFence(out string) savedPaneFence {
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 3 || !runtime.TmuxPaneID(parts[1]) {
		return savedPaneFence{}
	}
	if _, err := strconv.ParseUint(parts[2], 10, 32); err != nil {
		return savedPaneFence{}
	}
	digest, ok := strings.CutSuffix(parts[0], "."+parts[1])
	if !ok || !runtime.TmuxKeyName(digest) {
		return savedPaneFence{}
	}
	return savedPaneFence{digest: digest, pane: parts[1], pid: parts[2]}
}

// clearInputFence removes the agent pane's fence before a welded start op,
// for a pack that can fence, so a pack whose start reuses a pane never leaves
// the previous incarnation's fence naming the new agent. One tmux command
// list reads the fence it removes, which it returns so that a start op
// refused by a live box can put it back (restoreInputFence). Best-effort: a
// box that does not exist yet has no pane to clear, and the publish after
// the start op is required.
func (p *Provider) clearInputFence(ctx context.Context, name string) savedPaneFence {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return savedPaneFence{}
	}
	argv := append([]string{"tmux", "display-message", "-p", "-t", execTmuxSession, paneFenceSaveFormat, ";"},
		runtime.TmuxClearInputFenceArgs(execTmuxSession)...)
	out, code, err := p.Exec(ctx, name, argv)
	if err != nil || code != 0 {
		return savedPaneFence{}
	}
	return parseSavedPaneFence(string(out))
}

// restoreInputFence puts saved back on the agent pane after a start op that
// left a live box in place (a collision with the session it was started
// for), so that session keeps taking fenced input. It sets the fence only
// while the pane is the one saved, with the same root process and no fence
// of its own: a pane the start op respawned, or one a concurrent start has
// fenced since, is left alone. A restore that fails leaves the pane
// unproven, which fenced input misses on, so it is reported, not returned.
func (p *Provider) restoreInputFence(ctx context.Context, name string, saved savedPaneFence) {
	if saved.digest == "" {
		return
	}
	set, err := runtime.TmuxCommandString(runtime.TmuxGuardFormatArg,
		"set-option", "-p", "-F", "-t", execTmuxSession, runtime.TmuxInputFenceOption, runtime.TmuxFenceValue(saved.digest))
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec provider: session %q: restoring its input fence: %v\n", name, err)
		return
	}
	cond := "#{&&:#{==:#{pane_id}," + saved.pane + "},#{&&:#{==:#{pane_pid}," + saved.pid + "},#{==:#{" + runtime.TmuxInputFenceOption + "},}}}"
	_, code, err := p.Exec(ctx, name, []string{"tmux", "if-shell", "-F", "-t", execTmuxSession, cond, set})
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "exec provider: session %q: restoring its input fence: %v\n", name, err)
	case code != 0:
		fmt.Fprintf(os.Stderr, "exec provider: session %q: restoring its input fence: tmux exit %d\n", name, code)
	}
}

// publishInputFence records the incarnation cfg.Env names on the agent pane
// a welded start op created, for a pack that can fence. A pane left without
// its fence would be unproven to every fenced op, so a failed publish fails
// the start (which puts back the fence of a box it keeps).
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
