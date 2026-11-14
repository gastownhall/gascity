package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// strictSessionPeek peeks session id with gc session peek's contract (the
// GET /session/{id} peek_strict option) through the session's worker handle,
// the same domain call the local CLI path makes: every open session that is
// not suspended is peeked whatever its runtime state, and lines reaches the
// runtime as given. The session domain owns the inactive rule, so a suspended
// or closed session is refused with 409 session-inactive carrying the
// domain's own message. The session resolved above, so a pane the runtime
// reports gone means the seat is not running (asleep, or between starts):
// that is a peek with no output, which gc session peek names with the
// session's state, not a failure. Any other peek error fails the request
// instead of being dropped.
func strictSessionPeek(ctx context.Context, handle worker.PeekHandle, id string, lines int) (string, error) {
	output, err := handle.Peek(ctx, lines)
	if err != nil {
		if errors.Is(err, session.ErrSessionInactive) {
			return "", apierr.SessionInactive.Msg(err.Error())
		}
		if runtime.IsSessionGone(err) {
			return "", nil
		}
		return "", humaSessionManagerError(fmt.Errorf("peeking session %s: %w", id, err))
	}
	return output, nil
}
