package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It always travels with runtime.ErrRuntimeUnavailable: the
// caller got no answer, and that is all it may conclude.
var ErrObservationOutstanding = errors.New("worker: an earlier bounded observation of this key has not returned")

// errObservationEnded is the answer of an observation whose goroutine ended
// without returning (runtime.Goexit), so there is no value to hand back.
var errObservationEnded = fmt.Errorf("worker: bounded observation ended without an answer: %w", runtime.ErrRuntimeUnavailable)

// boundedObservations holds the keys with an observation goroutine that has
// not returned. It is what keeps a wedged session at one leaked goroutine
// instead of one per reconcile tick. Keys are removed when the goroutine
// returns, so the map is as large as the number of sessions currently wedged.
var boundedObservations sync.Map // key string -> struct{}

// boundedResult is what an observation goroutine hands back: its answer, or the
// panic it ended with.
type boundedResult[T any] struct {
	value    T
	err      error
	panicked bool
	panicVal any
}

// take returns the answer, re-raising a recorded panic on the calling goroutine.
func (r boundedResult[T]) take() (T, error) {
	if r.panicked {
		panic(r.panicVal)
	}
	return r.value, r.err
}

// ObserveBounded runs observe and waits for its answer no longer than ctx
// allows. A provider call that is stuck (a hung tmux subprocess, a wedged
// socket) cannot be canceled, so on expiry the call is abandoned and the
// caller gets an error wrapping both runtime.ErrRuntimeUnavailable and the
// context error. That is "no answer", never confirmed absence. An answer that
// arrives by the deadline is honored, including one that lands as it expires.
//
// At most one observation per key is in flight at a time. While one is, a new
// call under that key returns ErrObservationOutstanding (also wrapping
// runtime.ErrRuntimeUnavailable) without starting another, so a wedged session
// leaks one goroutine, not one per reconcile tick. The key is released as soon
// as the observation returns, before its answer is delivered, so a caller that
// observes again the moment it has an answer is never told its own finished
// call is outstanding. Pick the key to name what is being observed (one
// session in one city); a blank key is rejected with an error that does not
// wrap runtime.ErrRuntimeUnavailable, because it is a caller bug.
//
// An already-expired ctx starts nothing. A panic in observe is re-raised on the
// caller's goroutine, so the caller's own recover sees it as it would a panic
// in an unbounded call. Whether or not anyone is still waiting, the panic is
// logged with the stack it was raised on, and one that comes after the caller
// has moved on is dropped rather than crashing the process.
func ObserveBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	var zero T
	if strings.TrimSpace(key) == "" {
		return zero, errors.New("worker: bounded observation needs a non-blank key")
	}
	if err := ctx.Err(); err != nil {
		return zero, observationUnanswered(err)
	}
	if _, outstanding := boundedObservations.LoadOrStore(key, struct{}{}); outstanding {
		return zero, fmt.Errorf("%w: %w", ErrObservationOutstanding, runtime.ErrRuntimeUnavailable)
	}

	// Buffered, so a goroutine that finishes after its caller gave up delivers
	// into the buffer and exits instead of blocking forever.
	results := make(chan boundedResult[T], 1)
	go func() {
		res := boundedResult[T]{err: errObservationEnded}
		defer func() {
			if r := recover(); r != nil {
				res = boundedResult[T]{panicked: true, panicVal: r}
				slog.Error("worker: bounded observation panicked",
					slog.String("key", key), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			}
			boundedObservations.Delete(key)
			results <- res
		}()
		res.value, res.err = observe(ctx)
	}()

	select {
	case res := <-results:
		return res.take()
	case <-ctx.Done():
		select {
		case res := <-results:
			return res.take()
		default:
		}
		return zero, observationUnanswered(ctx.Err())
	}
}

// observationUnanswered is the error of an observation that gave no answer in
// time: unavailable, with the reason the wait ended.
func observationUnanswered(cause error) error {
	return fmt.Errorf("worker: observation unanswered: %w: %w", runtime.ErrRuntimeUnavailable, cause)
}
