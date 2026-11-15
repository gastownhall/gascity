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
// records (its gc.sessionEnv), then dispatches to that thread by id; a
// transient retry goes out only after another fresh read re-proves the
// fence. A respawn that binds the session to a new thread never receives the
// input. Start records a reused thread's new incarnation before it stops or
// rebinds the thread, and fails when it cannot. One that rebinds the same
// thread between a read and the dispatch it gates (one round trip) does
// receive it, since T3 cannot condition a command on the binding, so the
// trait is checked.
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
	thread, binding, err := p.readFencedThread(ctx, name, f)
	if err != nil {
		return nil, nil, err
	}
	if p.fenceChecked != nil {
		p.fenceChecked(name)
	}
	return thread, binding, nil
}

// readFencedThread is fencedThread's read and comparison, for a valid f.
func (p *Provider) readFencedThread(ctx context.Context, name string, f runtime.InputFence) (map[string]interface{}, *threadBinding, error) {
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
	return thread, snapshotThreadBinding(thread), nil
}

// requireLiveSession passes when thread's T3 session is live, or shows a
// legacy pre-session status within name's start grace. A session that is not
// live is a miss finding it dead; one whose status cannot be classified is
// ErrRuntimeUnavailable.
func (p *Provider) requireLiveSession(name string, thread map[string]interface{}) error {
	status := snapshotThreadSessionStatus(thread)
	if isLegacyPreSessionStatus(status) && p.withinRecentStart(name) {
		return nil
	}
	switch classifyT3SessionStatus(status) {
	case t3SessionStatusLive:
		return nil
	case t3SessionStatusAbsent:
		return &runtime.FenceMismatchError{Session: name, Found: runtime.FenceFoundDead}
	default:
		return fmt.Errorf("%w: t3bridge snapshot has unknown session status %q for %q", runtime.ErrRuntimeUnavailable, status, name)
	}
}

// refence is the check a fenced dispatch to threadID passes before each
// transient retry: a fresh read still binds name to threadID under f with a
// live session. Its failure is ErrRuntimeUnavailable and never a miss, so
// the miss it found is reported as text only: the failed attempt may have
// reached T3.
func (p *Provider) refence(name string, f runtime.InputFence, threadID string) func(context.Context) error {
	return func(ctx context.Context) error {
		thread, binding, err := p.readFencedThread(ctx, name, f)
		if err == nil && binding.ThreadID != threadID {
			err = fmt.Errorf("session is bound to thread %q, not %q", binding.ThreadID, threadID)
		}
		if err == nil {
			err = p.requireLiveSession(name, thread)
		}
		if err != nil {
			return fmt.Errorf("%w: re-checking the fence of %q before a retry (an earlier attempt may have reached t3): %s", runtime.ErrRuntimeUnavailable, name, err.Error())
		}
		return nil
	}
}

// NudgeFenced delivers content as NudgeKeyed does (n.DeliveryKey derives the
// turn's T3 ids; an empty key mints fresh ones) to the thread whose binding
// records n.Fence's incarnation and whose T3 session is live; a stopped
// session is a miss finding it dead, since T3 would relaunch a provider for
// the turn. A transient retry goes out only after a fresh read re-proves the
// fence on the same thread. T3 starts turns without an idle wait, so
// n.Immediate changes nothing. n.Receipt, when set, gets NudgeKeyed's
// receipt once the turn start succeeded.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	if ctx == nil {
		ctx = context.Background()
	}
	thread, binding, err := p.fencedThread(ctx, name, n.Fence)
	if err != nil {
		return err
	}
	if err := p.requireLiveSession(name, thread); err != nil {
		return err
	}
	r, err := p.nudgeThreadFor(ctx, name, thread, binding, content, n.DeliveryKey, p.refence(name, n.Fence, binding.ThreadID))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return fmt.Errorf("nudging %q: %w: %w", name, ctxErr, err)
		}
		return err
	}
	if n.Receipt != nil {
		*n.Receipt = r
	}
	return nil
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
// ErrRuntimeUnavailable. A transient retry goes out only after a fresh read
// re-proves the fence on the same thread.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	if ctx == nil {
		ctx = context.Background()
	}
	thread, binding, err := p.fencedThread(ctx, name, fence)
	if err != nil {
		return err
	}
	if err := p.requireLiveSession(name, thread); err != nil {
		return err
	}
	if err := p.dispatchTurnInterruptFor(ctx, binding.ThreadID, "", p.refence(name, fence, binding.ThreadID)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return fmt.Errorf("interrupting %q: %w: %w", name, ctxErr, err)
		}
		return err
	}
	p.clearSnapshotCache()
	return nil
}
