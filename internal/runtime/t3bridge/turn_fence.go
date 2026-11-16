package t3bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The WaitTurnEnded poll starts at turnPollInitial and doubles to
// turnPollMax. Tests shorten both.
var (
	turnPollInitial = 250 * time.Millisecond
	turnPollMax     = time.Second
)

var _ runtime.TurnFenceProvider = (*Provider)(nil)

// freshSessionThread reads the thread bound to name from an uncached
// snapshot, bounded by ctx. It reports [runtime.ErrRuntimeUnavailable] when
// the snapshot cannot be read, for any reason (wrapping ctx's error when ctx
// ended), or when no thread is visible yet inside the start grace, and
// [runtime.ErrSessionNotFound] when no thread is bound to name.
func (p *Provider) freshSessionThread(ctx context.Context, name string) (map[string]interface{}, error) {
	snapshot, err := p.rpcSnapshotFresh(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A failure over several candidates arrives flattened to text, so
			// ctx's error is wrapped explicitly.
			return nil, fmt.Errorf("%w: t3bridge snapshot unavailable for %q: %w: %w", runtime.ErrRuntimeUnavailable, name, ctxErr, err)
		}
		return nil, fmt.Errorf("%w: t3bridge snapshot unavailable for %q: %w", runtime.ErrRuntimeUnavailable, name, err)
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	if snapshotThreadBinding(thread) == nil {
		if p.withinRecentStart(name) {
			return nil, fmt.Errorf("%w: t3 thread for %q not visible yet", runtime.ErrRuntimeUnavailable, name)
		}
		return nil, fmt.Errorf("%w: no t3 thread is bound to %q", runtime.ErrSessionNotFound, name)
	}
	return thread, nil
}

// threadActiveTurn reads the T3 turn in progress on thread: its thread id and
// T3's raw activeTurnId, empty when no turn is in progress. A thread with no
// provider session, a session of an absent status (even with a stale id), and
// a live session with a null id have no turn. A running session with a null
// id, an unknown status and a malformed id are indeterminate:
// [runtime.ErrRuntimeUnavailable]. Unlike IsAlive and ListRunning, no start
// grace applies: a legacy pre-session status ("none", "gone") inside the grace
// is a session still starting, which has no turn in progress yet.
func threadActiveTurn(thread map[string]interface{}) (threadID, turnID string, err error) {
	threadID, _ = thread["id"].(string)
	session, _ := thread["session"].(map[string]interface{})
	if session == nil {
		return threadID, "", nil
	}
	status := snapshotThreadSessionStatus(thread)
	switch classifyT3SessionStatus(status) {
	case t3SessionStatusAbsent:
		return threadID, "", nil
	case t3SessionStatusLive:
	default:
		return threadID, "", fmt.Errorf("%w: t3 thread %s has unknown session status %q", runtime.ErrRuntimeUnavailable, threadID, status)
	}
	switch raw := session["activeTurnId"].(type) {
	case nil:
	case string:
		turnID = strings.TrimSpace(raw)
	default:
		return threadID, "", fmt.Errorf("%w: t3 thread %s has a malformed activeTurnId %v", runtime.ErrRuntimeUnavailable, threadID, raw)
	}
	if turnID == "" && status == "running" {
		return threadID, "", fmt.Errorf("%w: t3 thread %s is running with no active turn id", runtime.ErrRuntimeUnavailable, threadID)
	}
	return threadID, turnID, nil
}

// threadTurn is thread's turn as a [runtime.TurnState], with the thread id
// and T3's raw turn id it is built from. The state's id is
// "<threadId>/<activeTurnId>", so a thread recreated for the same session
// never repeats an earlier thread's turn id. Turn events, if this provider
// reports any, must use the same id, as [runtime.TurnState] requires.
func threadTurn(thread map[string]interface{}) (threadID, rawTurnID string, st runtime.TurnState, err error) {
	threadID, rawTurnID, err = threadActiveTurn(thread)
	if err != nil || rawTurnID == "" {
		return threadID, "", runtime.TurnState{}, err
	}
	return threadID, rawTurnID, runtime.TurnState{TurnID: threadID + "/" + rawTurnID}, nil
}

// threadTurnState is threadTurn's state alone.
func threadTurnState(thread map[string]interface{}) (runtime.TurnState, error) {
	_, _, st, err := threadTurn(thread)
	return st, err
}

// ObserveTurn implements [runtime.TurnFenceProvider] from an uncached
// snapshot.
func (p *Provider) ObserveTurn(ctx context.Context, name string) (runtime.TurnState, error) {
	thread, err := p.freshSessionThread(ctx, name)
	if err != nil {
		return runtime.TurnState{}, err
	}
	return threadTurnState(thread)
}

// InterruptTurn implements [runtime.TurnFenceProvider]. It dispatches
// thread.turn.interrupt carrying T3's turn id only when an uncached read shows
// turnID in progress. T3's protocol V1 records the turn id but interrupts
// whatever turn runs, so the answer is never Fenced.
//
// A transient dispatch failure is retried only after another uncached read
// still shows turnID in progress, which keeps the gap between the read and
// the interrupt T3 handles at about one round trip. When the re-read fails or
// shows another turn, the call fails with [runtime.ErrRuntimeUnavailable],
// not a *TurnNotCurrentError: the failed attempt may have reached T3. An
// interrupt a T3 address took without answering goes to no other address in
// the same attempt, so only a retry that passed the re-read sends it again.
func (p *Provider) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	if turnID == "" {
		return runtime.TurnInterrupt{}, fmt.Errorf("%w: interrupting a turn of %q", runtime.ErrTurnUnnamed, name)
	}
	thread, err := p.freshSessionThread(ctx, name)
	if err != nil {
		return runtime.TurnInterrupt{}, err
	}
	threadID, rawTurnID, current, err := threadTurn(thread)
	if err != nil {
		return runtime.TurnInterrupt{}, err
	}
	if current.TurnID != turnID {
		return runtime.TurnInterrupt{}, &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: current}
	}
	refence := func(ctx context.Context) error {
		thread, err := p.freshSessionThread(ctx, name)
		if err != nil {
			return fmt.Errorf("%w: re-reading turn %q of %q: %w", runtime.ErrRuntimeUnavailable, turnID, name, err)
		}
		st, err := threadTurnState(thread)
		if err != nil {
			return fmt.Errorf("%w: re-reading turn %q of %q: %w", runtime.ErrRuntimeUnavailable, turnID, name, err)
		}
		if st.TurnID != turnID {
			return fmt.Errorf("%w: turn %q of %q is no longer current (read %q) and an earlier attempt may have reached t3", runtime.ErrRuntimeUnavailable, turnID, name, st.TurnID)
		}
		return nil
	}
	if err := p.dispatchTurnInterruptFor(ctx, threadID, rawTurnID, refence); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A failure over several candidates arrives flattened to text, so
			// ctx's error is wrapped explicitly.
			return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %q: %w: %w", turnID, name, ctxErr, err)
		}
		return runtime.TurnInterrupt{}, fmt.Errorf("interrupting turn %q of %q: %w", turnID, name, err)
	}
	p.clearSnapshotCache()
	return runtime.TurnInterrupt{Fenced: false}, nil
}

// WaitTurnEnded implements [runtime.TurnFenceProvider] by polling uncached
// snapshots, first after turnPollInitial and then at doubling intervals up to
// turnPollMax. A thread no longer bound to name has ended its turn. A failed
// read is remembered and the poll goes on. When the bound elapses before any
// read succeeded, the state is zero and the error carries the last read error,
// if a read was made at all.
func (p *Provider) WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	if turnID == "" {
		return runtime.TurnState{}, fmt.Errorf("%w: waiting on a turn of %q", runtime.ErrTurnUnnamed, name)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var (
		last    runtime.TurnState
		lastErr error
		read    bool
	)
	for delay := turnPollInitial; ; delay = min(delay*2, turnPollMax) {
		poll := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			poll.Stop()
			if err := ctx.Err(); err != nil {
				return last, err
			}
			if !read && lastErr != nil {
				return last, fmt.Errorf("%w: session %q turn %q after %s: last read: %w", runtime.ErrTurnNotEnded, name, turnID, timeout, lastErr)
			}
			return last, fmt.Errorf("%w: session %q turn %q after %s", runtime.ErrTurnNotEnded, name, turnID, timeout)
		case <-poll.C:
		}
		st, err := p.ObserveTurn(waitCtx, name)
		switch {
		case errors.Is(err, runtime.ErrSessionNotFound):
			return runtime.TurnState{}, nil
		case err != nil:
			lastErr = err
			if waitCtx.Err() != nil && ctx.Err() == nil {
				// The wait's own bound cut this read; its deadline is not
				// the caller's, so only the read's text is kept.
				detail := strings.TrimPrefix(err.Error(), runtime.ErrRuntimeUnavailable.Error()+": ")
				lastErr = fmt.Errorf("%w: %s", runtime.ErrRuntimeUnavailable, detail)
			}
		case st.TurnID != turnID:
			return st, nil
		default:
			last, read = st, true
		}
	}
}
