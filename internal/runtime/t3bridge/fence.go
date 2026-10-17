package t3bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports that t3bridge fences input by check: a fenced
// operation reads the thread bound to the session from an uncached
// snapshot, compares the fence with the incarnation the thread's binding
// records (its gc.sessionEnv), then dispatches to that thread by id. A
// respawn that binds the session to a new thread never receives the input;
// one that rebinds the same thread between the read and the dispatch does,
// since T3 cannot condition a command on the binding, so the trait is
// checked.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	return runtime.InputFencingChecked
}

// threadFence is the incarnation thread's binding records, false when it
// records none.
func threadFence(thread map[string]interface{}) (runtime.InputFence, bool) {
	return runtime.InputFenceFromEnv(ParseSessionEnv(threadCustomMetadata(thread)["gc.sessionEnv"]))
}

// fencedThread reads the thread bound to name fresh and returns it with its
// binding when the binding records the incarnation f names, else what a
// fenced operation reports: no thread is a miss finding it absent, a thread
// recording no incarnation an unproven one, another incarnation one finding
// its digest; a snapshot that cannot be read is ErrRuntimeUnavailable.
func (p *Provider) fencedThread(ctx context.Context, name string, f runtime.InputFence) (map[string]interface{}, *threadBinding, error) {
	if err := f.Validate(); err != nil {
		return nil, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	thread, err := p.freshSessionThread(ctx, name)
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		return nil, nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundAbsent}
	case err != nil:
		return nil, nil, err
	}
	live, ok := threadFence(thread)
	switch {
	case !ok:
		return nil, nil, &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundUnproven}
	case live.Digest() != f.Digest():
		return nil, nil, &runtime.FenceMismatchError{Session: name, Found: live.Digest()}
	}
	if p.fenceChecked != nil {
		p.fenceChecked(name)
	}
	return thread, snapshotThreadBinding(thread), nil
}

// NudgeFenced delivers content as NudgeKeyed does (n.DeliveryKey derives the
// turn's T3 ids; an empty key mints fresh ones) to the thread whose binding
// records n.Fence's incarnation. As for Nudge, a bound thread whose T3
// session is stopped takes the turn. T3 starts turns without an idle wait,
// so n.Immediate changes nothing.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	thread, binding, err := p.fencedThread(ctx, name, n.Fence)
	if err != nil {
		return err
	}
	return p.nudgeThread(name, thread, binding, content, n.DeliveryKey)
}

// SendKeysFenced is SendKeys (a no-op: T3 has no terminal) after the fence
// check.
func (p *Provider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, _ ...string) error {
	_, _, err := p.fencedThread(ctx, name, fence)
	return err
}

// InterruptFenced interrupts as Interrupt does the thread whose binding
// records fence's incarnation. A thread whose session is not live is a miss
// finding it dead; one whose status cannot be classified is
// ErrRuntimeUnavailable.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	thread, binding, err := p.fencedThread(ctx, name, fence)
	if err != nil {
		return err
	}
	status := snapshotThreadSessionStatus(thread)
	startingLegacy := isLegacyPreSessionStatus(status) && p.withinRecentStart(name)
	if !startingLegacy {
		switch classifyT3SessionStatus(status) {
		case t3SessionStatusLive:
		case t3SessionStatusAbsent:
			return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
		default:
			return fmt.Errorf("%w: t3bridge snapshot has unknown session status %q for %q", runtime.ErrRuntimeUnavailable, status, name)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.dispatchTurnInterruptFor(ctx, binding.ThreadID, "", nil); err != nil {
		return err
	}
	p.clearSnapshotCache()
	return nil
}
