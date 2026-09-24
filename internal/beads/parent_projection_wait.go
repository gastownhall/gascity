package beads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// parentProjectionReads are the synchronous backend reads used to confirm a
// reparent: a point read of the child, and a check that the old and new
// parents' child listings agree with it.
type parentProjectionReads struct {
	get     func(id string) (Bead, error)
	matches func(ctx context.Context, id, oldParentID, newParentID string, ephemeral bool) (bool, error)
}

// parentProjectionPoll is the outcome of one confirmation attempt. done ends
// the wait with err; cutShort means ctx ended before the attempt finished its
// reads, so it proves nothing; otherwise err is the transient failure to
// report if the wait later times out.
type parentProjectionPoll struct {
	done     bool
	cutShort bool
	err      error
}

// errParentProjectionCutShort marks a projection check that stopped between
// its reads because ctx ended. Backends wrap it together with ctx.Err(), so
// the poll can tell a check it cut short from a backend read that failed with
// a context error of its own.
var errParentProjectionCutShort = errors.New("parent projection check cut short")

// awaitParentProjection polls reads until id's parent is newParentID and both
// parents' child listings reflect it. It returns ErrParentProjectionSuperseded
// when another writer moved the child elsewhere, and ErrNotFound when the
// child was deleted.
//
// Backend reads take no context (BdStore shells out to bd under its own
// per-command timeout), so each poll runs off the caller's goroutine and the
// wait honors ctx even while a read is stalled. An abandoned poll checks ctx
// before each further backend call, so only the call already in flight runs
// on (it may itself run a few bd commands and retries), and its result is
// discarded.
//
// Polls back off from bdParentProjectionPollInterval to
// parentProjectionMaxPollInterval, since each BdStore poll runs several bd
// subprocesses and a wait that never converges would otherwise run them back
// to back until the deadline.
func awaitParentProjection(ctx context.Context, reads parentProjectionReads, id, oldParentID, newParentID string) error {
	var lastErr error
	interval := bdParentProjectionPollInterval
	for {
		if ctx.Err() != nil {
			return parentProjectionWaitErr(ctx, id, oldParentID, newParentID, lastErr)
		}
		result := make(chan parentProjectionPoll, 1)
		go func() { result <- pollParentProjection(ctx, reads, id, oldParentID, newParentID) }()
		var poll parentProjectionPoll
		select {
		case poll = <-result:
		case <-ctx.Done():
			// A result that landed together with the deadline still counts.
			select {
			case poll = <-result:
			default:
				return parentProjectionWaitErr(ctx, id, oldParentID, newParentID, lastErr)
			}
		}
		if poll.done {
			return poll.err
		}
		// A poll cut short by ctx proves nothing; keep the last real check
		// failure for the timeout message instead.
		if !poll.cutShort {
			lastErr = poll.err
		}
		select {
		case <-ctx.Done():
			return parentProjectionWaitErr(ctx, id, oldParentID, newParentID, lastErr)
		case <-time.After(interval):
		}
		interval = min(2*interval, parentProjectionMaxPollInterval)
	}
}

// parentProjectionMaxPollInterval caps the backoff between confirmation polls.
const parentProjectionMaxPollInterval = 500 * time.Millisecond

func pollParentProjection(ctx context.Context, reads parentProjectionReads, id, oldParentID, newParentID string) parentProjectionPoll {
	current, err := reads.get(id)
	switch {
	case errors.Is(err, ErrNotFound):
		return parentProjectionPoll{done: true, err: fmt.Errorf("updating bead %q: waiting for parent projection: %w", id, err)}
	case err != nil:
		return parentProjectionPoll{err: err}
	case current.ParentID == newParentID:
		if ctx.Err() != nil {
			return parentProjectionPoll{cutShort: true}
		}
		matches, matchErr := reads.matches(ctx, id, oldParentID, newParentID, current.Ephemeral)
		switch {
		case errors.Is(matchErr, errParentProjectionCutShort):
			return parentProjectionPoll{cutShort: true}
		case matchErr == nil && matches:
			return parentProjectionPoll{done: true}
		}
		return parentProjectionPoll{err: matchErr}
	case current.ParentID == oldParentID:
		return parentProjectionPoll{}
	default:
		return parentProjectionPoll{done: true, err: fmt.Errorf("updating bead %q: %w", id, ErrParentProjectionSuperseded)}
	}
}

func parentProjectionWaitErr(ctx context.Context, id, oldParentID, newParentID string, lastErr error) error {
	return &parentProjectionWaitError{id: id, oldParentID: oldParentID, newParentID: newParentID, cause: ctx.Err(), last: lastErr}
}

// parentProjectionWaitError reports a wait that ended because its context
// did. Only that context error is in the Unwrap chain, so errors.Is tells a
// caller why the wait stopped; the last failed check is diagnostic detail in
// the message only. A backend read that earlier failed with its own timeout
// must not make a canceled wait look like a deadline, or the reverse.
type parentProjectionWaitError struct {
	id, oldParentID, newParentID string
	cause                        error
	last                         error
}

func (e *parentProjectionWaitError) Error() string {
	msg := fmt.Sprintf("updating bead %q: waiting for parent projection from %q to %q: %v", e.id, e.oldParentID, e.newParentID, e.cause)
	if e.last != nil {
		msg += fmt.Sprintf(" (last check error: %v)", e.last)
	}
	return msg
}

func (e *parentProjectionWaitError) Unwrap() error { return e.cause }
