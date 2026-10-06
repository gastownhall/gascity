package worker

import (
	"context"
	"errors"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier, abandoned observation under the same key
// has not returned yet. It always travels with runtime.ErrRuntimeUnavailable:
// the caller got no answer, and that is all it may conclude.
var ErrObservationOutstanding = errors.New("worker: an earlier bounded observation of this key has not returned")

// ObserveBounded runs observe and waits for its answer no longer than ctx
// allows. A provider call that is stuck (a hung tmux subprocess, a wedged
// socket) cannot be canceled, so on expiry the call is abandoned and the
// caller gets an error wrapping both runtime.ErrRuntimeUnavailable and the
// context error. That is "no answer", never confirmed absence.
//
// At most one abandoned observation per key is outstanding at a time. While
// one is, a new call under that key returns ErrObservationOutstanding (also
// wrapping runtime.ErrRuntimeUnavailable) without starting another, so a
// wedged session leaks one goroutine, not one per reconcile tick. An answer
// that arrives by the deadline is honored.
func ObserveBounded[T any](ctx context.Context, _ string, observe func(context.Context) (T, error)) (T, error) {
	return observe(ctx)
}
