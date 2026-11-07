package worker

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrObservationOutstanding reports that ObserveBounded declined to start a
// new observation because an earlier observation under the same key has not
// returned yet. It always travels with runtime.ErrRuntimeUnavailable: the
// caller got no answer, and that is all it may conclude. It is
// runtime.ErrBoundedCallOutstanding, which a bounded call made below this
// package reports.
var ErrObservationOutstanding = runtime.ErrBoundedCallOutstanding

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
//
// It is runtime.CallBounded, so its keys share one table with bounded calls
// made in layers below this package.
func ObserveBounded[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallBounded(ctx, key, observe)
}

// ObserveShared is ObserveBounded for an observation several callers make of
// one key at once, such as reads of one session's turns. A call made while an
// observation of key is outstanding waits for that observation's answer,
// within its own ctx, instead of failing with ErrObservationOutstanding; so
// concurrent healthy reads all get the answer, and a wedged session still
// leaks one goroutine, not one per call. The observation runs under the
// context of the call that started it. A joiner whose answer is an error
// that call's context ending caused, while its own ctx still allows, starts a
// fresh observation rather than take someone else's deadline as its own.
//
// As with ObserveBounded, giving up is an error wrapping both
// runtime.ErrRuntimeUnavailable and the context error, an answer that lands
// as ctx ends is honored, and a blank key is a caller bug. A panic in observe
// is re-raised on the call that started it; joiners get an error wrapping
// runtime.ErrRuntimeUnavailable. A key is observed with one type: a joiner of
// an observation of another type, a caller bug, gets an error wrapping
// runtime.ErrRuntimeUnavailable rather than its answer.
//
// It is runtime.CallShared.
func ObserveShared[T any](ctx context.Context, key string, observe func(context.Context) (T, error)) (T, error) {
	return runtime.CallShared(ctx, key, observe)
}
