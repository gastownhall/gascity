package api

import (
	"context"
	"log"
	"runtime/debug"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// listBuild is one detached build of a shared list view. done closes when the
// build ends; outcome, set before that, is its typed result or error. start
// runs the build, so the build queued behind a running one carries the
// reader's own captured inputs and reads the state when it starts.
type listBuild struct {
	done    chan struct{}
	start   func()
	outcome any
}

// listBuildSlot holds a shared list view's builds, guarded by responseCacheMu:
// the one running and the one queued to start when it ends. One view never
// runs two builds at once, so concurrent cache-miss readers cannot stack
// them, and at most one build queues behind a running one.
type listBuildSlot struct {
	running *listBuild
	queued  *listBuild
}

// listBuildOutcome carries a build's typed result to its waiters.
type listBuildOutcome[T any] struct {
	value T
	err   error
}

// listBuildSlotLocked returns key's build slot, adding an empty one. The
// caller holds responseCacheMu.
func (s *Server) listBuildSlotLocked(key string) *listBuildSlot {
	if s.listBuilds == nil {
		s.listBuilds = make(map[string]*listBuildSlot)
	}
	slot := s.listBuilds[key]
	if slot == nil {
		slot = &listBuildSlot{}
		s.listBuilds[key] = slot
	}
	return slot
}

// shareListBuild answers key's list build by sharing: when a build is already
// running for key, the caller joins the one queued behind it (creating that
// one), and otherwise its own build runs. Either way the build the caller
// waits for starts no earlier than this call, so no cache-miss reader
// receives a build that read the state before its admission. build runs
// detached with a bounded background context — not the caller's request
// context, which may end while later readers still want the build — and
// every waiter gets a deep copy of the result. Nothing runs under the slot
// mutex: build touches stores and the runtime only in the detached goroutine.
func shareListBuild[T any](ctx context.Context, s *Server, key string, build func(ctx context.Context) (T, error)) (T, error) {
	s.responseCacheMu.Lock()
	slot := s.listBuildSlotLocked(key)
	var b *listBuild
	started := false
	if slot.running != nil {
		if slot.queued == nil {
			queued := &listBuild{done: make(chan struct{})}
			queued.start = func() { runListBuild(s, key, queued, build) }
			slot.queued = queued
		}
		b = slot.queued
	} else {
		b = &listBuild{done: make(chan struct{})}
		b.start = func() { runListBuild(s, key, b, build) }
		slot.running = b
		started = true
	}
	s.responseCacheMu.Unlock()
	if started {
		b.start()
	}
	return awaitListBuild[T](ctx, key, b)
}

// runListBuild runs b, the build running in key's slot, detached: it builds
// through build, records the outcome for its waiters and ends b (endListBuild)
// on every exit path. A panic in build is a logged typed failure; the slot
// still recovers — the queued build, or the next reader, starts a fresh one.
func runListBuild[T any](s *Server, key string, b *listBuild, build func(ctx context.Context) (T, error)) {
	s.runBackground(func(ctx context.Context) {
		defer s.endListBuild(key, b)
		value, err := func() (v T, err error) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("api: panic in shared list build %s: %v\n%s", key, r, debug.Stack())
					var zero T
					v, err = zero, apierr.Internal.Msg("building the list panicked; the server's log has the stack")
				}
			}()
			return build(ctx)
		}()
		s.responseCacheMu.Lock()
		b.outcome = &listBuildOutcome[T]{value: value, err: err}
		s.responseCacheMu.Unlock()
	})
}

// awaitListBuild waits for b's outcome and hands the caller a deep copy, so a
// waiter that mutates its response cannot reach another waiter's. A waiter
// whose context ends first gets a typed 503 — its alone; the build, its
// outcome and the other waiters are unaffected — unless the build finished
// first, which is the better answer.
func awaitListBuild[T any](ctx context.Context, key string, b *listBuild) (T, error) {
	var zero T
	select {
	case <-b.done:
	case <-ctx.Done():
		select {
		case <-b.done:
		default:
			return zero, apierr.ServiceUnavailable.Msgf("the list build for %s did not finish before the request ended", key)
		}
	}
	out, ok := b.outcome.(*listBuildOutcome[T])
	if !ok {
		return zero, apierr.Internal.Msg("building the list panicked; the server's log has the stack")
	}
	if out.err != nil {
		return zero, out.err
	}
	value, ok := cloneCachedValue[T](out.value)
	if !ok {
		return zero, apierr.Internal.Msgf("copying the built list for %s", key)
	}
	return value, nil
}

// endListBuild ends b, the build running in key's slot: its waiters wake, and
// the build queued behind it, if any, starts with its own reader's inputs.
// A slot with nothing queued is removed.
func (s *Server) endListBuild(key string, b *listBuild) {
	s.responseCacheMu.Lock()
	slot := s.listBuilds[key]
	var next *listBuild
	if slot != nil && slot.running == b {
		next = slot.queued
		slot.running, slot.queued = next, nil
		if next == nil {
			delete(s.listBuilds, key)
		}
	}
	s.responseCacheMu.Unlock()
	close(b.done)
	if next != nil {
		next.start()
	}
}
