package t3bridge

import (
	"context"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// freshReads coalesces the bridge's fresh snapshot reads by generation
// (DES-T3I D7). A caller that finds no read in flight runs one. A caller that
// arrives while read k is in flight waits for read k+1, which every caller
// arriving during read k shares: read k may have been answered before the
// caller's own write, so it never answers a later caller. Read k+1 runs, as
// read k ends, in the goroutine and under the context of the first of its
// callers to see read k end, so no read outlives every caller waiting for it.
// However many sessions a conversation-lane host polls at once, the bridge
// then has at most one snapshot read in flight and one queued. A caller that
// any read ending after it arrived serves (shared) takes the read in flight
// instead. A queued read whose callers all gave up is dropped, or, if it
// became the read in flight meanwhile, ended unread. The zero value is ready
// to use.
type freshReads struct {
	mu sync.Mutex
	// running is the read in flight, or about to be run by one of its
	// callers; next is the read queued behind it.
	running *freshRead
	next    *freshRead
	// readHook, a test seam, runs at the start of each read; abandonHook,
	// another, runs as a waiter whose context ended gives up its read,
	// before it takes mu.
	readHook    func()
	abandonHook func()
}

// freshRead is one shared read. Its fields under "result" are written by the
// caller that runs it and read by the others after done closes.
type freshRead struct {
	done chan struct{}
	// claimed and waiters are guarded by freshReads.mu: whether a caller
	// runs the read, and how many callers wait for it.
	claimed bool
	waiters int
	// result
	ctx  context.Context
	snap map[string]interface{}
	err  error
}

// do answers ctx's caller with a read that started after it arrived. A
// caller whose context ends while it waits returns its context's error, as
// a read under that context would, and leaves the shared read to the others;
// a caller whose shared read ended because the context of the caller that
// ran it ended reads again.
func (f *freshReads) do(ctx context.Context, read func(context.Context) (map[string]interface{}, error)) (map[string]interface{}, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for a T3 snapshot: %w", err)
		}
		r, prev := f.join()
		if prev == nil {
			f.perform(ctx, r, read)
			return r.snap, r.err
		}
		if !f.await(ctx, r, prev.done) {
			return nil, fmt.Errorf("waiting for a T3 snapshot: %w", ctx.Err())
		}
		if f.claim(r) {
			f.perform(ctx, r, read)
			return r.snap, r.err
		}
		if !f.await(ctx, r, r.done) {
			return nil, fmt.Errorf("waiting for a T3 snapshot: %w", ctx.Err())
		}
		if r.err != nil && r.ctx.Err() != nil && ctx.Err() == nil {
			continue
		}
		return r.snap, r.err
	}
}

// shared answers ctx's caller with the read in flight, if one is running,
// else as do does. It is for a caller content with any read that ends after
// it arrived, such as one whose cached snapshot went stale: it need not wait
// for the read after the one in flight. A read that ended because the
// context of the caller that ran it ended, while ctx still allows, answers
// it as do would.
func (f *freshReads) shared(ctx context.Context, read func(context.Context) (map[string]interface{}, error)) (map[string]interface{}, error) {
	f.mu.Lock()
	r := f.running
	if r != nil && !r.claimed {
		// About to be run by one of its callers, which arrived after the
		// read before it started: as fresh as do's.
		r = nil
	}
	if r != nil {
		r.waiters++
	}
	f.mu.Unlock()
	if r == nil {
		return f.do(ctx, read)
	}
	defer func() {
		f.mu.Lock()
		r.waiters--
		f.mu.Unlock()
	}()
	select {
	case <-r.done:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a T3 snapshot: %w", ctx.Err())
	}
	if r.ctx == nil || r.err != nil && r.ctx.Err() != nil && ctx.Err() == nil {
		return f.do(ctx, read)
	}
	return r.snap, r.err
}

// join enters the caller in a read: a new one it must run (prev nil) when
// none is in flight, else the read queued behind prev, the one in flight.
func (f *freshReads) join() (r, prev *freshRead) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = &freshRead{done: make(chan struct{}), claimed: true, waiters: 1}
		return f.running, nil
	}
	if f.next == nil {
		f.next = &freshRead{done: make(chan struct{})}
	}
	f.next.waiters++
	return f.next, f.running
}

// await waits for done, reporting false when ctx ended first; the caller
// then no longer waits for r.
func (f *freshReads) await(ctx context.Context, r *freshRead, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
		}
	}
	if f.abandonHook != nil {
		f.abandonHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r.waiters--
	if r.waiters > 0 || r.claimed {
		return false
	}
	// No caller is left to run r.
	switch {
	case f.running == r:
		// The read before it ended since this caller looked, so r is the
		// read in flight, with callers queued behind it: end it unread, as
		// perform ends a read, so the first of them to see it end runs
		// theirs.
		r.claimed = true
		f.running, f.next = f.next, nil
		close(r.done)
	case f.next == r:
		f.next = nil
	}
	return false
}

// claim reports whether the caller runs r: the first of r's callers to see
// the read before it end does.
func (f *freshReads) claim(r *freshRead) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.claimed || f.running != r {
		return false
	}
	r.claimed = true
	return true
}

// perform runs r under ctx, then ends it: the read queued behind it becomes
// the one in flight, and r's callers are answered. A read that panics
// answers them with runtime.ErrRuntimeUnavailable.
func (f *freshReads) perform(ctx context.Context, r *freshRead, read func(context.Context) (map[string]interface{}, error)) {
	r.ctx = ctx
	func() {
		defer func() {
			if p := recover(); p != nil {
				r.snap, r.err = nil, fmt.Errorf("%w: T3 snapshot read panicked: %v", runtime.ErrRuntimeUnavailable, p)
			}
		}()
		if f.readHook != nil {
			f.readHook()
		}
		r.snap, r.err = read(ctx)
	}()
	f.mu.Lock()
	f.running, f.next = f.next, nil
	f.mu.Unlock()
	close(r.done)
}

// inFlight is how many callers wait for, or run, the read in flight.
func (f *freshReads) inFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		return 0
	}
	return f.running.waiters
}

// queued is how many callers wait for the read queued behind the one in
// flight.
func (f *freshReads) queued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next == nil {
		return 0
	}
	return f.next.waiters
}
