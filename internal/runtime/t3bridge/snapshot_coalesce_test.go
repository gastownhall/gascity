package t3bridge

import (
	"context"
	"errors"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// gatedReads is a snapshot read that blocks until released, numbering each
// read it starts.
type gatedReads struct {
	started atomic.Int32
	gates   []chan struct{}
	mu      sync.Mutex
	ctxs    []context.Context
}

func newGatedReads(n int) *gatedReads {
	g := &gatedReads{}
	for range n {
		g.gates = append(g.gates, make(chan struct{}))
	}
	return g
}

func (g *gatedReads) read(ctx context.Context) (map[string]interface{}, error) {
	k := int(g.started.Add(1))
	g.mu.Lock()
	g.ctxs = append(g.ctxs, ctx)
	g.mu.Unlock()
	select {
	case <-g.gates[k-1]:
		return map[string]interface{}{"read": k}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitFor spins until cond holds.
func waitFor(cond func() bool) {
	for !cond() {
		goruntime.Gosched()
	}
}

type freshResult struct {
	snap map[string]interface{}
	err  error
}

func (f *freshReads) goDo(ctx context.Context, read func(context.Context) (map[string]interface{}, error)) <-chan freshResult {
	out := make(chan freshResult, 1)
	go func() {
		snap, err := f.do(ctx, read)
		out <- freshResult{snap, err}
	}()
	return out
}

// TestFreshReadsCoalesceByGeneration pins DES-T3I D7: callers that arrive
// while read k is in flight share read k+1, never k, and a caller that finds
// no read in flight starts one.
func TestFreshReadsCoalesceByGeneration(t *testing.T) {
	var f freshReads
	g := newGatedReads(3)
	first := f.goDo(t.Context(), g.read)
	waitFor(func() bool { return g.started.Load() == 1 })
	second := f.goDo(t.Context(), g.read)
	third := f.goDo(t.Context(), g.read)
	waitFor(func() bool { return f.queued() == 2 })
	close(g.gates[0])
	if r := <-first; r.err != nil || r.snap["read"] != 1 {
		t.Fatalf("first caller = %v, %v; want read 1", r.snap, r.err)
	}
	waitFor(func() bool { return g.started.Load() == 2 })
	close(g.gates[1])
	for i, ch := range []<-chan freshResult{second, third} {
		if r := <-ch; r.err != nil || r.snap["read"] != 2 {
			t.Fatalf("caller %d arriving during read 1 = %v, %v; want read 2", i+2, r.snap, r.err)
		}
	}
	if n := g.started.Load(); n != 2 {
		t.Fatalf("reads = %d, want 2 for three callers", n)
	}
	close(g.gates[2])
	if snap, err := f.do(t.Context(), g.read); err != nil || snap["read"] != 3 {
		t.Fatalf("a caller with no read in flight = %v, %v; want a read of its own", snap, err)
	}
}

// TestFreshReadsWaiterContext proves a waiter whose context ends returns at
// once and leaves the shared read to the others, and that a waiter whose
// shared read was ended by the context of the caller that ran it reads
// again.
func TestFreshReadsWaiterContext(t *testing.T) {
	var f freshReads
	g := newGatedReads(3)
	first := f.goDo(t.Context(), g.read)
	waitFor(func() bool { return g.started.Load() == 1 })

	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	a := f.goDo(ctxA, g.read)
	b := f.goDo(ctxB, g.read)
	waitFor(func() bool { return f.queued() == 2 })
	impatient, cancelImpatient := context.WithCancel(t.Context())
	gone := f.goDo(impatient, g.read)
	waitFor(func() bool { return f.queued() == 3 })
	cancelImpatient()
	if r := <-gone; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %v; want its context error", r.err)
	}
	waitFor(func() bool { return f.queued() == 2 })

	close(g.gates[0])
	if r := <-first; r.err != nil {
		t.Fatalf("first caller: %v", r.err)
	}
	// Read 2 runs under the context of whichever waiter ran it; end that
	// context, and the other waiter reads again.
	waitFor(func() bool { return g.started.Load() == 2 })
	g.mu.Lock()
	runner := g.ctxs[1]
	g.mu.Unlock()
	ran, other := a, b
	if runner == ctxA {
		cancelA()
	} else {
		cancelB()
		ran, other = b, a
	}
	if r := <-ran; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the waiter whose read its context ended = %v; want its cancellation", r.err)
	}
	waitFor(func() bool { return g.started.Load() == 3 })
	close(g.gates[2])
	if r := <-other; r.err != nil || r.snap["read"] != 3 {
		t.Fatalf("a waiter whose shared read another's context ended = %v, %v; want a read of its own", r.snap, r.err)
	}
	cancelA()
	cancelB()
}

// TestFreshReadsAbandonedQueue proves a queued read every waiter left is
// dropped, so the next caller runs a read of its own.
func TestFreshReadsAbandonedQueue(t *testing.T) {
	var f freshReads
	g := newGatedReads(2)
	first := f.goDo(t.Context(), g.read)
	waitFor(func() bool { return g.started.Load() == 1 })
	ctx, cancel := context.WithCancel(t.Context())
	gone := f.goDo(ctx, g.read)
	waitFor(func() bool { return f.queued() == 1 })
	cancel()
	if r := <-gone; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %v; want its context error", r.err)
	}
	close(g.gates[0])
	<-first
	close(g.gates[1])
	if snap, err := f.do(t.Context(), g.read); err != nil || snap["read"] != 2 {
		t.Fatalf("the next caller = %v, %v; want read 2, run by itself", snap, err)
	}
}

// TestFreshReadsPanicIsUnavailable proves a read that panics answers every
// caller sharing it with ErrRuntimeUnavailable and leaves the coalescer
// usable.
func TestFreshReadsPanicIsUnavailable(t *testing.T) {
	var f freshReads
	_, err := f.do(t.Context(), func(context.Context) (map[string]interface{}, error) { panic("boom") })
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("a panicking read = %v; want ErrRuntimeUnavailable", err)
	}
	snap, err := f.do(t.Context(), func(context.Context) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
	})
	if err != nil || snap["ok"] != true {
		t.Fatalf("the next read = %v, %v; want it served", snap, err)
	}
}

// TestTurnHistoryReadsCoalesce proves concurrent fresh turn reads of the
// bridge's sessions share snapshots: callers arriving during one read share
// the next, so a burst costs at most two snapshot reads.
func TestTurnHistoryReadsCoalesce(t *testing.T) {
	server := newTurnFenceServer(t, turnIdle())
	p := newTurnFenceTestProvider(t, server)
	release := make(chan struct{})
	var started atomic.Int32
	p.fresh.readHook = func() {
		if started.Add(1) == 1 {
			<-release
		}
	}
	const callers = 8
	errs := make(chan error, callers)
	go func() {
		_, err := p.TurnHistory(t.Context(), turnFenceSession)
		errs <- err
	}()
	waitFor(func() bool { return started.Load() == 1 })
	for range callers - 1 {
		go func() {
			_, err := p.TurnHistory(t.Context(), turnFenceSession)
			errs <- err
		}()
	}
	waitFor(func() bool { return p.fresh.queued() == callers-1 })
	close(release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("TurnHistory: %v", err)
		}
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d for %d concurrent turn reads, want 2", calls, callers)
	}
}
