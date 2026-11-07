package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It always travels with runtime.ErrRuntimeUnavailable: the
// caller got no answer, and that is all it may conclude. The sentinel is the
// one internal/runtime owns (runtime.ErrBoundedCallOutstanding), so the
// process holds one latch per key across every surface that bounds a call.
var ErrObservationOutstanding = runtime.ErrBoundedCallOutstanding

// ObserveBounded runs observe and waits for its answer no longer than ctx
// allows; a provider call that is stuck is abandoned at the deadline, at most
// one observation per key is in flight, and the key frees as the observation
// returns. The implementation is internal/runtime's CallBounded
// (bounded_call.go), which owns the latch tables; this is the worker surface
// callers observe through.
func ObserveBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallBounded(ctx, key, observe)
}

// ObserveShared is ObserveBounded for an observation several callers make of
// one key at once, such as reads of one session's turns: a call made while an
// observation of key is outstanding joins it, within its own ctx, instead of
// failing. The implementation is internal/runtime's CallShared; this is the
// worker surface callers observe through.
func ObserveShared[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallShared(ctx, key, observe)
}
