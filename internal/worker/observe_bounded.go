package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It always travels with runtime.ErrRuntimeUnavailable: the
// caller got no answer, and that is all it may conclude. It is the same
// sentinel as runtime.ErrBoundedCallOutstanding: the bounded call lives in
// internal/runtime (runtime.CallBounded), these forwards carry its contract
// under the observation names callers here use, and both share one in-flight
// table per key.
var ErrObservationOutstanding = runtime.ErrBoundedCallOutstanding

// ObserveBounded runs observe and waits for its answer no longer than ctx
// allows: runtime.CallBounded under the worker's observation names. The call
// under the wait can hang on a wedged tmux subprocess or a stalled store, and
// nothing cancels it, so the wait is raced against the caller's context and
// abandoned when the context ends. Take the bound from the caller's own
// context.
//
// An abandoned wait is an error wrapping both runtime.ErrRuntimeUnavailable
// and the context error; it is never a confirmed absence, and an answer that
// lands as the context ends is used, not dropped. At most one observation per
// key is in flight; while an earlier one has not returned, a new call under
// that key gets ErrObservationOutstanding (also wrapping
// runtime.ErrRuntimeUnavailable) and starts nothing, so a wedged session costs
// one goroutine per key, not one per call. See runtime.CallBounded for the
// full contract, and internal/worker/AGENTS.md for the key namespaces this
// package uses.
func ObserveBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallBounded(ctx, key, observe)
}

// ObserveShared is ObserveBounded for an observation several callers make of
// one key at once (turn reads: CurrentTurn, WatchTurns and its session
// reload, the idle overlay): runtime.CallShared under the worker's observation
// names. A call made while an observation of key is outstanding waits for that
// observation's answer, within its own ctx, instead of getting
// ErrObservationOutstanding, so concurrent healthy readers all get the answer
// and a wedged session still leaks one goroutine. See runtime.CallShared for
// the full contract.
func ObserveShared[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallShared(ctx, key, observe)
}
