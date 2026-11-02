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

// ErrBoundedCallOutstanding reports that CallBounded declined to start a new
// call because an earlier call under the same key has not returned yet. It
// always travels with ErrRuntimeUnavailable: the caller got no answer, and
// that is all it may conclude.
var ErrBoundedCallOutstanding = errors.New("runtime: an earlier bounded call of this key has not returned")

// errCallEnded is the answer of a call whose goroutine ended without
// returning (a runtime.Goexit), so there is no value to hand back.
var errCallEnded = fmt.Errorf("runtime: bounded call ended without an answer: %w", ErrRuntimeUnavailable)

// boundedCalls holds the keys with a call goroutine that has not returned. It
// is what keeps a wedged call at one leaked goroutine instead of one per
// reconcile tick. Keys are removed when the goroutine returns, so the map is
// as large as the number of keys currently wedged. The table is shared by
// every caller of CallBounded and CallShared in the process, including
// internal/worker's ObserveBounded and ObserveShared forwards: key each call
// within a caller namespace so one wedged session holds one slot per
// namespace, not one per call.
var boundedCalls sync.Map // key string -> struct{}

// boundedResult is what a call goroutine hands back: its answer, or the
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

// CallBounded runs call and waits for its answer no longer than ctx allows.
// A provider call that is stuck (a hung tmux subprocess, a wedged socket)
// cannot be canceled, so on expiry the call is abandoned and the caller gets
// an error wrapping both ErrRuntimeUnavailable and the context error. That is
// "no answer", never confirmed absence. An answer that arrives by the
// deadline is honored, including one that lands as it expires.
//
// At most one call per key is in flight at a time. While one is, a new call
// under that key returns ErrBoundedCallOutstanding (also wrapping
// ErrRuntimeUnavailable) without starting another, so a wedged key leaks one
// goroutine, not one per reconcile tick. The key is released as soon as the
// call returns, before its answer is delivered, so a caller that calls again
// the moment it has an answer is never told its own finished call is
// outstanding. Pick the key to name what is being called (one session in one
// city, within a caller namespace); a blank key is rejected with an error
// that does not wrap ErrRuntimeUnavailable, because it is a caller bug.
//
// An already-expired ctx starts nothing. A panic in call is re-raised on the
// caller's goroutine, so the caller's own recover sees it as it would a panic
// in an unbounded call. Whether or not anyone is still waiting, the panic is
// logged with the stack it was raised on, and one that comes after the caller
// has moved on is dropped rather than crashing the process.
func CallBounded[T any](ctx context.Context, key string, call func(context.Context) (T, error)) (T, error) {
	var zero T
	if strings.TrimSpace(key) == "" {
		return zero, errors.New("runtime: bounded call needs a non-blank key")
	}
	if err := ctx.Err(); err != nil {
		return zero, callUnanswered(err)
	}
	if _, outstanding := boundedCalls.LoadOrStore(key, struct{}{}); outstanding {
		return zero, fmt.Errorf("%w: %w", ErrBoundedCallOutstanding, ErrRuntimeUnavailable)
	}

	// Buffered, so a goroutine that finishes after its caller gave up delivers
	// into the buffer and exits instead of blocking forever.
	results := make(chan boundedResult[T], 1)
	go func() {
		res := boundedResult[T]{err: errCallEnded}
		defer func() {
			if r := recover(); r != nil {
				res = boundedResult[T]{panicked: true, panicVal: r}
				slog.Error("runtime: bounded call panicked",
					slog.String("key", key), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
			}
			boundedCalls.Delete(key)
			results <- res
		}()
		res.value, res.err = call(ctx)
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
		return zero, callUnanswered(ctx.Err())
	}
}

// callUnanswered is the error of a call that gave no answer in time:
// unavailable, with the reason the wait ended.
func callUnanswered(cause error) error {
	return fmt.Errorf("runtime: call unanswered: %w: %w", ErrRuntimeUnavailable, cause)
}

// sharedCalls holds the keys with a CallShared call that has not returned,
// each with the flight its joiners wait on. It is a map apart from
// boundedCalls: a key is called through one of the two, never both.
var sharedCalls sync.Map // key string -> *sharedFlight

// sharedFlight is one CallShared call: closed done publishes res.
type sharedFlight struct {
	done chan struct{}
	res  any // a boundedResult of the call's type
	// ctx is the context of the call that started the flight, which the call
	// runs under.
	ctx context.Context
}

// CallShared is CallBounded for a call several callers make of one key at
// once, such as reads of one session's turns. A call made while a call of key
// is outstanding waits for that call's answer, within its own ctx, instead of
// failing with ErrBoundedCallOutstanding; so concurrent healthy calls all get
// the answer, and a wedged key still leaks one goroutine, not one per call.
// The call runs under the context of the call that started it. A joiner whose
// answer is an error that call's context ending caused, while its own ctx
// still allows, starts a fresh call rather than take someone else's deadline
// as its own.
//
// As with CallBounded, giving up is an error wrapping both
// ErrRuntimeUnavailable and the context error, an answer that lands as ctx
// ends is honored, and a blank key is a caller bug. A panic in call is
// re-raised on the call that started it; joiners get an error wrapping
// ErrRuntimeUnavailable. A key is called with one type: a joiner of a call of
// another type, a caller bug, gets an error wrapping ErrRuntimeUnavailable
// rather than its answer.
func CallShared[T any](ctx context.Context, key string, call func(context.Context) (T, error)) (T, error) {
	var zero T
	if strings.TrimSpace(key) == "" {
		return zero, errors.New("runtime: shared call needs a non-blank key")
	}
	for {
		if err := ctx.Err(); err != nil {
			return zero, callUnanswered(err)
		}
		mine := &sharedFlight{done: make(chan struct{}), ctx: ctx}
		got, joined := sharedCalls.LoadOrStore(key, mine)
		flight := got.(*sharedFlight)
		if !joined {
			go runSharedCall(key, flight, call)
		}
		select {
		case <-flight.done:
		case <-ctx.Done():
			select {
			case <-flight.done:
			default:
				return zero, callUnanswered(ctx.Err())
			}
		}
		res, ok := flight.res.(boundedResult[T])
		if !ok {
			return zero, fmt.Errorf("runtime: shared call %q answered %T, not %T: %w", key, flight.res, res, ErrRuntimeUnavailable)
		}
		if !joined {
			return res.take()
		}
		if res.panicked {
			return zero, fmt.Errorf("runtime: shared call panicked: %w", ErrRuntimeUnavailable)
		}
		if res.err != nil && flight.ctx.Err() != nil && ctx.Err() == nil {
			continue // the starter's context ended the call, not ours
		}
		return res.value, res.err
	}
}

// runSharedCall runs one CallShared call and publishes its answer to everyone
// waiting on flight. The key is released before the answer is published, so a
// caller that calls again the moment it has an answer starts a fresh call.
func runSharedCall[T any](key string, flight *sharedFlight, call func(context.Context) (T, error)) {
	res := boundedResult[T]{err: errCallEnded}
	defer func() {
		if r := recover(); r != nil {
			res = boundedResult[T]{panicked: true, panicVal: r}
			slog.Error("runtime: shared call panicked",
				slog.String("key", key), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		}
		sharedCalls.CompareAndDelete(key, flight)
		flight.res = res
		close(flight.done)
	}()
	res.value, res.err = call(flight.ctx)
}
