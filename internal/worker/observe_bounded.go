package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It is runtime.ErrBoundedCallOutstanding and always travels
// with runtime.ErrRuntimeUnavailable: the caller got no answer, and that is
// all it may conclude.
var ErrObservationOutstanding = runtime.ErrBoundedCallOutstanding

// ObserveBounded runs observe and waits for its answer no longer than ctx
// allows; it is runtime.CallBounded, whose doc comment holds the contract.
// On expiry the call is abandoned and the caller gets an error wrapping both
// runtime.ErrRuntimeUnavailable and the context error: "no answer", never
// confirmed absence. At most one observation per key is in flight; while one
// is, a new call returns ErrObservationOutstanding without starting another.
func ObserveBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallBounded(ctx, key, observe)
}

// ObserveShared is ObserveBounded for an observation several callers make of
// one key at once, such as reads of one session's turns; it is
// runtime.CallShared, whose doc comment holds the contract. A call made while
// an observation of key is outstanding waits for that observation's answer,
// within its own ctx, instead of failing with ErrObservationOutstanding.
func ObserveShared[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallShared(ctx, key, observe)
}
