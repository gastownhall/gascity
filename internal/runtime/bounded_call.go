package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
)

// The bounded-call implementation lives here, below every layer that waits on
// a call that may never return, so that internal/session (which internal/worker
// imports) and internal/worker share one implementation and one table of
// outstanding keys. internal/worker exposes it as ObserveBounded and
// ObserveShared; keys from every caller share the tables, so each caller
// namespaces its keys.

// ErrBoundedCallOutstanding reports that CallBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It always travels with ErrRuntimeUnavailable: the caller got
// no answer, and that is all it may conclude. worker.ErrObservationOutstanding
// is the same error.
var ErrBoundedCallOutstanding = errors.New("worker: an earlier bounded observation of this key has not returned")

// errObservationEnded is the answer of an observation whose goroutine ended
// without returning (runtime.Goexit), so there is no value to hand back.
var errObservationEnded = fmt.Errorf("worker: bounded observation ended without an answer: %w", ErrRuntimeUnavailable)

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

// CallBounded runs observe and waits for its answer no longer than ctx
// allows. A provider call that is stuck (a hung tmux subprocess, a wedged
// socket) cannot be canceled, so on expiry the call is abandoned and the
// caller gets an error wrapping both ErrRuntimeUnavailable and the
// context error. That is "no answer", never confirmed absence. An answer that
// arrives by the deadline is honored, including one that lands as it expires.
//
// At most one observation per key is in flight at a time. While one is, a new
// call under that key returns ErrBoundedCallOutstanding (also wrapping
// ErrRuntimeUnavailable) without starting another, so a wedged session
// leaks one goroutine, not one per reconcile tick. The key is released as soon
// as the observation returns, before its answer is delivered, so a caller that
// observes again the moment it has an answer is never told its own finished
// call is outstanding. Pick the key to name what is being observed (one
// session in one city); a blank key is rejected with an error that does not
// wrap ErrRuntimeUnavailable, because it is a caller bug.
//
// An already-expired ctx starts nothing. A panic in observe is re-raised on the
// caller's goroutine, so the caller's own recover sees it as it would a panic
// in an unbounded call. Whether or not anyone is still waiting, the panic is
// logged with the stack it was raised on, and one that comes after the caller
// has moved on is dropped rather than crashing the process.
func CallBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	var zero T
	if strings.TrimSpace(key) == "" {
		return zero, errors.New("worker: bounded observation needs a non-blank key")
	}
	if err := ctx.Err(); err != nil {
		return zero, observationUnanswered(err)
	}
	if _, outstanding := boundedObservations.LoadOrStore(key, struct{}{}); outstanding {
		return zero, fmt.Errorf("%w: %w", ErrBoundedCallOutstanding, ErrRuntimeUnavailable)
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
	return fmt.Errorf("worker: observation unanswered: %w: %w", ErrRuntimeUnavailable, cause)
}

// sharedObservations holds the keys with a CallShared observation that
// has not returned, each with the flight its joiners wait on. It is a map
// apart from boundedObservations: a key is observed through one of the two,
// never both.
var sharedObservations sync.Map // key string -> *sharedFlight

// sharedFlight is one CallShared observation: closed done publishes res.
type sharedFlight struct {
	done chan struct{}
	res  any // a boundedResult of the observation's type
	// ctx is the context of the call that started the observation, which
	// the observation runs under.
	ctx context.Context
}

// CallShared is CallBounded for an observation several callers make of
// one key at once, such as reads of one session's turns. A call made while an
// observation of key is outstanding waits for that observation's answer,
// within its own ctx, instead of failing with ErrBoundedCallOutstanding; so
// concurrent healthy reads all get the answer, and a wedged session still
// leaks one goroutine, not one per call. The observation runs under the
// context of the call that started it. A joiner whose answer is an error
// that call's context ending caused, while its own ctx still allows, starts a
// fresh observation rather than take someone else's deadline as its own.
//
// As with CallBounded, giving up is an error wrapping both
// ErrRuntimeUnavailable and the context error, an answer that lands
// as ctx ends is honored, and a blank key is a caller bug. A panic in observe
// is re-raised on the call that started it; joiners get an error wrapping
// ErrRuntimeUnavailable. A key is observed with one type: a joiner of
// an observation of another type, a caller bug, gets an error wrapping
// ErrRuntimeUnavailable rather than its answer.
func CallShared[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	var zero T
	if strings.TrimSpace(key) == "" {
		return zero, errors.New("worker: shared observation needs a non-blank key")
	}
	for {
		if err := ctx.Err(); err != nil {
			return zero, observationUnanswered(err)
		}
		mine := &sharedFlight{done: make(chan struct{}), ctx: ctx}
		got, joined := sharedObservations.LoadOrStore(key, mine)
		flight := got.(*sharedFlight)
		if !joined {
			go runSharedObservation(key, flight, observe)
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
			select {
			case <-flight.done:
			default:
				return zero, observationUnanswered(ctx.Err())
			}
		}
		res, ok := flight.res.(boundedResult[T])
		if !ok {
			return zero, fmt.Errorf("worker: shared observation %q answered %T, not %T: %w", key, flight.res, res, ErrRuntimeUnavailable)
		}
		if !joined {
			return res.take()
		}
		if res.panicked {
			return zero, fmt.Errorf("worker: shared observation panicked: %w", ErrRuntimeUnavailable)
		}
		if res.err != nil && flight.ctx.Err() != nil && ctx.Err() == nil {
			continue // the starter's context ended the read, not ours
		}
		return res.value, res.err
	}
}

// runSharedObservation runs one CallShared observation and publishes its
// answer to everyone waiting on flight. The key is released before the answer
// is published, so a caller that observes again the moment it has an answer
// starts a fresh observation.
func runSharedObservation[T any](key string, flight *sharedFlight, observe func(context.Context) (T, error)) {
	res := boundedResult[T]{err: errObservationEnded}
	defer func() {
		if r := recover(); r != nil {
			res = boundedResult[T]{panicked: true, panicVal: r}
			slog.Error("worker: shared observation panicked",
				slog.String("key", key), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		}
		sharedObservations.CompareAndDelete(key, flight)
		flight.res = res
		close(flight.done)
	}()
	res.value, res.err = observe(flight.ctx)
}
