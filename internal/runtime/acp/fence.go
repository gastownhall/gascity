package acp

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports that ACP fences input by check: a fenced operation
// compares the fence with the digest Start recorded for the session's live
// process, then sends on that process's own connection. A process started
// in between has another connection, so it never receives the input; the
// check and the send are still two steps, so the trait is checked.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	return runtime.InputFencingChecked
}

// fencedConn returns the connection of session name's live process when it
// is the incarnation f names, else what a fenced operation reports: a miss
// for a process this provider does not own (unproven while another owner's
// socket is live, absent otherwise), an exited one (dead), one started with
// no incarnation (unproven) or another incarnation (its digest); a socket
// probe that cannot tell is ErrRuntimeUnavailable.
func (p *Provider) fencedConn(name string, f runtime.InputFence) (*sessionConn, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		present, err := p.probeSessionSocket(name)
		switch {
		case err != nil:
			return nil, fmt.Errorf("fenced input to %q: %w", name, err)
		case present:
			return nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundUnproven}
		}
		return nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundAbsent}
	}
	switch {
	case !sc.alive():
		return nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	case sc.fence == "":
		return nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundUnproven}
	case sc.fence != f.Digest():
		return nil, &runtime.FenceMismatchError{Session: name, Found: sc.fence}
	}
	if p.fenceChecked != nil {
		p.fenceChecked(name)
	}
	return sc, nil
}

// NudgeFenced delivers content as Nudge does, on the connection of the
// process fence names. An agent that exits before the prompt is written is a
// miss finding it dead, not Nudge's best-effort success. ACP waits for the
// agent to be nudgeable and cannot deduplicate, so n.Immediate and
// n.DeliveryKey change nothing.
func (p *Provider) NudgeFenced(_ context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	sc, err := p.fencedConn(name, n.Fence)
	if err != nil {
		return err
	}
	if err := p.sendPrompt(name, sc, content); errors.Is(err, errAgentExited) {
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	} else if err != nil {
		return err
	}
	return nil
}

// SendKeysFenced is SendKeys (a no-op: ACP has no terminal) after the fence
// check.
func (p *Provider) SendKeysFenced(_ context.Context, name string, fence runtime.InputFence, _ ...string) error {
	_, err := p.fencedConn(name, fence)
	return err
}

// InterruptFenced interrupts as Interrupt does, on the connection of the
// process fence names. An interrupt that reached that process (session/cancel
// written or SIGINT delivered) succeeds even if the process then exits, as a
// delivered Ctrl-C does on tmux. One that sent nothing because the process
// had exited is a whole miss finding it dead. Telling the two apart waits
// for a session/cancel write still in progress, up to
// interruptCancelWriteWait: the agent has exited, so its stdin no longer
// holds that write.
func (p *Provider) InterruptFenced(_ context.Context, name string, fence runtime.InputFence) error {
	sc, err := p.fencedConn(name, fence)
	if err != nil {
		return err
	}
	if !sc.alive() {
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	}
	delivery, err := sc.interruptReporting(name, p.cfg.cancelTimeout())
	if err != nil {
		return err
	}
	if !sc.alive() && !delivery.reached(interruptCancelWriteWait) {
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	}
	return nil
}
