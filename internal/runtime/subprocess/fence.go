package subprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports that subprocess fences input by check: a fenced
// operation compares the fence with the digest Start recorded for the
// session's live child, then acts on that child. A child started in between
// is another process, so it is never signaled; the check and the signal are
// still two steps, so the trait is checked.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	return runtime.InputFencingChecked
}

// fencedConn returns session name's live child when it is the incarnation f
// names, else what a fenced operation reports: a miss for a child this
// provider does not own (unproven while another owner's socket is live,
// absent otherwise), an exited one (dead), one started with no incarnation
// (unproven) or another incarnation (its digest); a socket probe that
// cannot tell is ErrRuntimeUnavailable.
func (p *Provider) fencedConn(name string, f runtime.InputFence) (*sessionConn, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	sc, ok := p.procs[name]
	p.mu.Unlock()
	if !ok {
		present, err := p.probeSessionSocket(name, os.Geteuid())
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

// NudgeFenced is Nudge (a no-op: a subprocess has no input channel) after
// the fence check.
func (p *Provider) NudgeFenced(_ context.Context, name string, _ []runtime.ContentBlock, n runtime.FencedNudge) error {
	_, err := p.fencedConn(name, n.Fence)
	return err
}

// SendKeysFenced is SendKeys (a no-op) after the fence check.
func (p *Provider) SendKeysFenced(_ context.Context, name string, fence runtime.InputFence, _ ...string) error {
	_, err := p.fencedConn(name, fence)
	return err
}

// InterruptFenced sends SIGINT, as Interrupt does, to the process group of
// the child fence names, re-checking right before the signal that the child
// has not exited. A child that has exited, whether the provider has observed
// it or the signal finds it gone, is a miss finding it dead.
func (p *Provider) InterruptFenced(_ context.Context, name string, fence runtime.InputFence) error {
	sc, err := p.fencedConn(name, fence)
	if err != nil {
		return err
	}
	if !sc.alive() {
		// Reaped: its process group id may already name another process.
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	}
	err = runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	default:
		return fmt.Errorf("interrupting %q: %w", name, err)
	}
}
