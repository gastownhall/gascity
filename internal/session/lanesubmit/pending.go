package lanesubmit

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// PendingCheckTimeout bounds the look CheckPending takes for a session's
// pending interaction.
const PendingCheckTimeout = 5 * time.Second

// PendingLook looks once for a session's pending interaction: nil when the
// session waits on none, or the look's error.
type PendingLook func(ctx context.Context) (*runtime.PendingInteraction, error)

// PendingCheckError is a look for session Session's pending interaction that
// did not answer: it failed with Err, did not answer within
// PendingCheckTimeout (or the caller's deadline), or panicked. The request
// is refused, nothing written, and may be retried.
type PendingCheckError struct {
	Session string
	Err     error
}

// Error says the session could not be checked, why, and that a retry may
// succeed.
func (e *PendingCheckError) Error() string {
	return fmt.Sprintf("could not check session %s for a pending interaction (%v); nothing was written, retry", e.Session, e.Err)
}

// Unwrap returns why the look did not answer.
func (e *PendingCheckError) Unwrap() error { return e.Err }

// CheckPending is the front door's pending-interaction check every
// projection shares (Request.Pending): it runs look for session id, bounded
// at PendingCheckTimeout or ctx's earlier deadline. A session waiting on a
// pending interaction is refused (session.PendingInteractionError). A look
// that fails, does not answer in time, or panics is refused too, as a
// *PendingCheckError: the front door accepts a message only once the
// runtime has said nothing is pending.
func CheckPending(ctx context.Context, id string, look PendingLook) error {
	return checkPending(ctx, id, PendingCheckTimeout, look)
}

// pendingAnswer is one look's answer.
type pendingAnswer struct {
	pending *runtime.PendingInteraction
	err     error
}

// checkPending is CheckPending bounded at timeout.
func checkPending(ctx context.Context, id string, timeout time.Duration, look PendingLook) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Buffered, so a look that answers after the bound does not block.
	answered := make(chan pendingAnswer, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				answered <- pendingAnswer{err: fmt.Errorf("the look panicked: %v", r)}
			}
		}()
		p, err := look(ctx)
		answered <- pendingAnswer{pending: p, err: err}
	}()
	var got pendingAnswer
	select {
	case got = <-answered:
	case <-ctx.Done():
		got.err = fmt.Errorf("the look did not answer in time: %w", ctx.Err())
	}
	switch {
	case got.err != nil:
		return &PendingCheckError{Session: id, Err: got.err}
	case got.pending != nil:
		return session.PendingInteractionError(got.pending)
	}
	return nil
}
