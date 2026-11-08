package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeReading is what a confirmation of a session's runtime answered.
type runtimeReading int

const (
	// runtimeRunning: the runtime is alive. The caller treats it as it treats
	// any live runtime.
	runtimeRunning runtimeReading = iota
	// runtimeStopped: a fresh read answered that the runtime is not running.
	// Only this reading lets a caller close, retire, unclaim or cancel.
	runtimeStopped
	// runtimeUnknown: neither the cached nor a fresh read could answer. The
	// caller defers (SESSION-RUNTIME-009).
	runtimeUnknown
)

// runtimeConfirmBound is the longest one confirmation waits for its fresh
// read. It is the tmux StateCache's own fetch timeout. A var so tests can
// shrink it. A bound that is too small costs only a deferral: an abandoned
// read answers unknown, never absent.
var runtimeConfirmBound = 3 * time.Second

// runtimeConfirm is the latch of one reconciler pass over session rows: the
// first unknown a pass meets makes every later confirmation of that pass answer
// unknown without a probe, so a runtime that cannot be read costs one bound per
// pass, not one per row (the #7170 stall). A pass makes its own; it is never
// shared across passes and holds no package-level state. A nil *runtimeConfirm
// confirms without the latch. Safe for concurrent use.
type runtimeConfirm struct {
	unknown atomic.Bool
}

// confirm answers whether the runtime of session name in cityPath may be
// treated as stopped, in two steps. Step 1 is the cached read: sp.IsRunning
// true means alive, with today's hot path. A cached false is not evidence of
// absence, since the tmux StateCache reads false for every session while its
// snapshot is unprimed or stale. Step 2, only then, is a fresh read held to
// runtimeConfirmBound (runtime.ObserveLivenessSince on the wall clock: the
// StateCache stamps snapshots on its own clock, so the reconciler's injected
// clock must not be used). The read counts as answered only when it returned no
// error; any error, a timeout and ErrObservationOutstanding included, is
// unknown. Only Liveness.Running is read, so a dead pane still takes the
// dead-pane paths. A provider without a fresh read answers from its cached
// tri-state or bool, so its behavior is unchanged.
//
// The wait is built from context.Background(): the legacy reconciler functions
// take no context, and one is not threaded through them for this.
func (c *runtimeConfirm) confirm(cityPath string, sp runtime.Provider, name string) runtimeReading {
	if sp.IsRunning(name) {
		return runtimeRunning
	}
	if c != nil && c.unknown.Load() {
		return runtimeUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeConfirmBound)
	defer cancel()
	live, err := observeSessionBounded(ctx, cityPath, name, func() (runtime.Liveness, error) {
		return runtime.ObserveLivenessSince(sp, name, nil, time.Now())
	})
	if err != nil {
		if c != nil {
			c.unknown.Store(true)
		}
		return runtimeUnknown
	}
	if live.Running {
		return runtimeRunning
	}
	return runtimeStopped
}
