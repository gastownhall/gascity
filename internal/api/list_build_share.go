package api

import (
	"context"
	"sync"
)

// listBuildSharer shares the fresh builds of one list endpoint among
// concurrent identical readers, as /status shares its fresh builds
// (statusBuildSlot): per key, one build runs and at most one waits queued
// behind it. A reader that finds no build running starts one; a reader that
// finds one running joins the queued build, creating it, and that build
// starts when the running one ends. No reader is answered by a build that
// started before the reader arrived, and readers that pile up behind a slow
// build cost one more build, not one each. The zero value is ready to use.
type listBuildSharer[T any] struct {
	mu    sync.Mutex
	slots map[string]*listBuildSlot[T]
	// joined, when a test sets it, runs once a reader has taken its build;
	// started reports whether the reader started it.
	joined func(key string, started bool)
}

// listBuildSlot holds one key's builds, guarded by listBuildSharer.mu: the
// one running and the one queued to start when it ends. A key with no build
// running has no slot.
type listBuildSlot[T any] struct {
	running *listBuild[T]
	queued  *listBuild[T]
}

// listBuild is one detached build of a list. run builds it; done closes when
// the build ends, after out and err are set.
type listBuild[T any] struct {
	run  func(context.Context) (*ListOutput[T], error)
	done chan struct{}
	out  *ListOutput[T]
	err  error
}

// share answers a reader of the list key names with a build that starts no
// earlier than this call. build is this reader's build of the list; it runs
// detached (runBackground) when this reader starts a build or creates the
// queued one, so it must not use the request's context or input: the caller
// passes copies.
func (sh *listBuildSharer[T]) share(_ context.Context, s *Server, key string, build func(context.Context) (*ListOutput[T], error)) (*ListOutput[T], error) {
	b, started := sh.take(key, build)
	if started {
		sh.start(s, key, b)
	}
	if sh.joined != nil {
		sh.joined(key, started)
	}
	<-b.done
	return b.out, b.err
}

// take returns the build a reader of key arriving now waits on, and whether
// the reader must start it: a new running build when none runs, or else the
// queued build, created by the first reader to queue.
func (sh *listBuildSharer[T]) take(key string, build func(context.Context) (*ListOutput[T], error)) (*listBuild[T], bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.slots == nil {
		sh.slots = make(map[string]*listBuildSlot[T])
	}
	slot := sh.slots[key]
	if slot == nil {
		b := &listBuild[T]{run: build, done: make(chan struct{})}
		sh.slots[key] = &listBuildSlot[T]{running: b}
		return b, true
	}
	if slot.queued == nil {
		slot.queued = &listBuild[T]{run: build, done: make(chan struct{})}
	}
	return slot.queued, false
}

// start runs b, the build running in key's slot, detached, and ends it on
// every exit path.
func (sh *listBuildSharer[T]) start(s *Server, key string, b *listBuild[T]) {
	s.runBackground(func(ctx context.Context) {
		defer sh.end(s, key, b)
		b.out, b.err = b.run(ctx)
	})
}

// end ends b, the build running in key's slot: the build queued behind it
// becomes the running one and starts, or the idle slot is removed, and b's
// readers wake.
func (sh *listBuildSharer[T]) end(s *Server, key string, b *listBuild[T]) {
	sh.mu.Lock()
	slot := sh.slots[key]
	next := slot.queued
	slot.running, slot.queued = next, nil
	if next == nil {
		delete(sh.slots, key)
	}
	sh.mu.Unlock()
	close(b.done)
	if next != nil {
		sh.start(s, key, next)
	}
}
