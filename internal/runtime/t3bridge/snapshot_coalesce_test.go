package t3bridge

import (
	"context"
	"errors"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// waitForBudget bounds waitFor, so a regression fails instead of hanging.
const waitForBudget = 10 * time.Second

// waitFor spins until cond holds, failing t when it has not within
// waitForBudget.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitForBudget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", waitForBudget)
		}
		goruntime.Gosched()
	}
}

// answered is ch's answer, failing t when none comes within waitForBudget.
func answered[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitForBudget):
		t.Fatalf("no answer within %s", waitForBudget)
		var zero T
		return zero
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
	waitFor(t, func() bool { return g.started.Load() == 1 })
	second := f.goDo(t.Context(), g.read)
	third := f.goDo(t.Context(), g.read)
	waitFor(t, func() bool { return f.queued() == 2 })
	close(g.gates[0])
	if r := answered(t, first); r.err != nil || r.snap["read"] != 1 {
		t.Fatalf("first caller = %v, %v; want read 1", r.snap, r.err)
	}
	waitFor(t, func() bool { return g.started.Load() == 2 })
	close(g.gates[1])
	for i, ch := range []<-chan freshResult{second, third} {
		if r := answered(t, ch); r.err != nil || r.snap["read"] != 2 {
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
	waitFor(t, func() bool { return g.started.Load() == 1 })

	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	a := f.goDo(ctxA, g.read)
	b := f.goDo(ctxB, g.read)
	waitFor(t, func() bool { return f.queued() == 2 })
	impatient, cancelImpatient := context.WithCancel(t.Context())
	gone := f.goDo(impatient, g.read)
	waitFor(t, func() bool { return f.queued() == 3 })
	cancelImpatient()
	if r := answered(t, gone); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %v; want its context error", r.err)
	}
	waitFor(t, func() bool { return f.queued() == 2 })

	close(g.gates[0])
	if r := answered(t, first); r.err != nil {
		t.Fatalf("first caller: %v", r.err)
	}
	// Read 2 runs under the context of whichever waiter ran it; end that
	// context, and the other waiter reads again.
	waitFor(t, func() bool { return g.started.Load() == 2 })
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
	if r := answered(t, ran); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the waiter whose read its context ended = %v; want its cancellation", r.err)
	}
	waitFor(t, func() bool { return g.started.Load() == 3 })
	close(g.gates[2])
	if r := answered(t, other); r.err != nil || r.snap["read"] != 3 {
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
	waitFor(t, func() bool { return g.started.Load() == 1 })
	ctx, cancel := context.WithCancel(t.Context())
	gone := f.goDo(ctx, g.read)
	waitFor(t, func() bool { return f.queued() == 1 })
	cancel()
	if r := answered(t, gone); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("a waiter whose context ended = %v; want its context error", r.err)
	}
	close(g.gates[0])
	answered(t, first)
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
	waitFor(t, func() bool { return started.Load() == 1 })
	for range callers - 1 {
		go func() {
			_, err := p.TurnHistory(t.Context(), turnFenceSession)
			errs <- err
		}()
	}
	waitFor(t, func() bool { return p.fresh.queued() == callers-1 })
	close(release)
	for range callers {
		if err := answered(t, errs); err != nil {
			t.Fatalf("TurnHistory: %v", err)
		}
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d for %d concurrent turn reads, want 2", calls, callers)
	}
}

// TestFreshReadsAbandonedRunningRead pins the interleaving where the last
// waiter of the queued read gives up as the read before it ends: by the time
// it takes the lock its read is the one in flight, unclaimed, and a later
// caller already queued behind it. The abandoned read ends unread, so the
// later caller runs its own, and the coalescer serves callers after it.
func TestFreshReadsAbandonedRunningRead(t *testing.T) {
	var f freshReads
	g := newGatedReads(3)
	close(g.gates[1])
	close(g.gates[2])
	abandoning := make(chan struct{})
	resume := make(chan struct{})
	f.abandonHook = func() {
		close(abandoning)
		<-resume
	}
	first := f.goDo(t.Context(), g.read)
	waitFor(t, func() bool { return g.started.Load() == 1 })
	ctx, cancel := context.WithCancel(t.Context())
	gone := f.goDo(ctx, g.read)
	waitFor(t, func() bool { return f.queued() == 1 })
	cancel()
	answered(t, abandoning)
	f.abandonHook = nil

	// Read 1 ends: the abandoned read is now the one in flight, and a new
	// caller queues behind it.
	close(g.gates[0])
	if r := answered(t, first); r.err != nil || r.snap["read"] != 1 {
		t.Fatalf("first caller = %v, %v; want read 1", r.snap, r.err)
	}
	later := f.goDo(t.Context(), g.read)
	waitFor(t, func() bool { return f.queued() == 1 })
	close(resume)
	if r := answered(t, gone); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the waiter that gave up = %v; want its context error", r.err)
	}
	if r := answered(t, later); r.err != nil || r.snap["read"] != 2 {
		t.Fatalf("the caller queued behind the abandoned read = %v, %v; want read 2, run by itself", r.snap, r.err)
	}
	if r := answered(t, f.goDo(context.Background(), g.read)); r.err != nil || r.snap["read"] != 3 {
		t.Fatalf("a later caller = %v, %v; want read 3", r.snap, r.err)
	}
}

// TestFreshReadsSharedTakesTheReadInFlight proves a caller content with any
// read that ends after it arrived (a stale cache) takes the read in flight
// instead of queueing for the next, and runs its own when none is.
func TestFreshReadsSharedTakesTheReadInFlight(t *testing.T) {
	var f freshReads
	g := newGatedReads(2)
	first := f.goDo(t.Context(), g.read)
	waitFor(t, func() bool { return g.started.Load() == 1 })
	out := make(chan freshResult, 1)
	go func() {
		snap, err := f.shared(context.Background(), g.read)
		out <- freshResult{snap, err}
	}()
	waitFor(t, func() bool { return f.inFlight() == 2 })
	close(g.gates[0])
	answered(t, first)
	if r := answered(t, out); r.err != nil || r.snap["read"] != 1 {
		t.Fatalf("shared caller during read 1 = %v, %v; want read 1", r.snap, r.err)
	}
	close(g.gates[1])
	if snap, err := f.shared(t.Context(), g.read); err != nil || snap["read"] != 2 {
		t.Fatalf("shared caller with no read in flight = %v, %v; want a read of its own", snap, err)
	}
	if n := g.started.Load(); n != 2 {
		t.Fatalf("reads = %d, want 2", n)
	}
}

// TestFreshReadsSharedRereadsWhenTheRunnerGaveUp proves a shared caller whose
// read in flight ended because the context of the caller running it ended
// reads again under its own context, so a lane probe's deadline never
// becomes another caller's error.
func TestFreshReadsSharedRereadsWhenTheRunnerGaveUp(t *testing.T) {
	var f freshReads
	g := newGatedReads(2)
	close(g.gates[1])
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := f.goDo(ctx, g.read)
	waitFor(t, func() bool { return g.started.Load() == 1 })
	out := make(chan freshResult, 1)
	go func() {
		snap, err := f.shared(context.Background(), g.read)
		out <- freshResult{snap, err}
	}()
	waitFor(t, func() bool { return f.inFlight() == 2 })
	cancel()
	if r := answered(t, runner); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the caller whose context ended = %v; want its cancellation", r.err)
	}
	if r := answered(t, out); r.err != nil || r.snap["read"] != 2 {
		t.Fatalf("shared caller whose read the runner's context ended = %v, %v; want read 2, its own", r.snap, r.err)
	}
}

// snapshotSessionStatus is the turn-fence session's status in snap.
func snapshotSessionStatus(snap map[string]interface{}) string {
	return snapshotThreadSessionStatus(snapshotThreadBySessionName(snap, turnFenceSession))
}

// stoppedTurnSnapshot is the turn-fence session after a Stop.
func stoppedTurnSnapshot() map[string]interface{} {
	return turnSnapshot(map[string]interface{}{"status": "stopped", "activeTurnId": nil})
}

// holdFirstRead blocks server's answer to the first snapshot read, chosen
// before it blocks, until the returned release is called (or t ends),
// reporting how many reads server has answered or holds.
func holdFirstRead(t *testing.T, server *t3BridgeTestServer) (release func(), started *atomic.Int32) {
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	started = &atomic.Int32{}
	server.mu.Lock()
	defer server.mu.Unlock()
	server.snapshotHook = func() {
		if started.Add(1) == 1 {
			<-gate
		}
	}
	return release, started
}

// TestRPCSnapshotAfterAClearIsNotAnsweredByAReadBeforeIt pins
// RUNTIME-CONTRACT-014's read-after-write rule: a write the bridge made
// (Stop, Interrupt, Start) clears the snapshot cache, and the next cached
// read is answered by a read that started after the clear, never by a fresh
// read already in flight (a lane probe's turn read), and caches what it read.
func TestRPCSnapshotAfterAClearIsNotAnsweredByAReadBeforeIt(t *testing.T) {
	stopped := stoppedTurnSnapshot()
	server := newTurnFenceServer(t, turnRunning("t1"), stopped, stopped, stopped)
	p := newTurnFenceTestProvider(t, server)
	release, started := holdFirstRead(t, server)
	probe := make(chan error, 1)
	go func() {
		_, err := p.TurnHistory(t.Context(), turnFenceSession)
		probe <- err
	}()
	waitFor(t, func() bool { return started.Load() == 1 })

	p.clearSnapshotCache()
	out := make(chan freshResult, 1)
	go func() {
		snap, err := p.rpcSnapshot()
		out <- freshResult{snap, err}
	}()
	waitFor(t, func() bool { return p.fresh.queued() == 1 })
	release()
	if err := answered(t, probe); err != nil {
		t.Fatalf("TurnHistory: %v", err)
	}
	r := answered(t, out)
	if r.err != nil {
		t.Fatalf("rpcSnapshot: %v", r.err)
	}
	if got := snapshotSessionStatus(r.snap); got != "stopped" {
		t.Fatalf("rpcSnapshot after a write's cache clear = %q, from a read that started before it; want stopped", got)
	}
	cached, err := p.rpcSnapshot()
	if err != nil {
		t.Fatalf("cached rpcSnapshot: %v", err)
	}
	if got := snapshotSessionStatus(cached); got != "stopped" {
		t.Fatalf("the cache after that read holds %q; want stopped", got)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2: the probe's and the cleared caller's", calls)
	}
}

// TestSnapshotReadBeforeAClearIsNotCached proves a read that started before
// the bridge cleared its cache does not refill it: the next cached read
// reads again instead of serving the pre-write state for the cache's TTL.
func TestSnapshotReadBeforeAClearIsNotCached(t *testing.T) {
	stopped := stoppedTurnSnapshot()
	server := newTurnFenceServer(t, turnRunning("t1"), stopped, stopped)
	p := newTurnFenceTestProvider(t, server)
	release, started := holdFirstRead(t, server)
	probe := make(chan error, 1)
	go func() {
		_, err := p.TurnHistory(t.Context(), turnFenceSession)
		probe <- err
	}()
	waitFor(t, func() bool { return started.Load() == 1 })
	p.clearSnapshotCache()
	release()
	if err := answered(t, probe); err != nil {
		t.Fatalf("TurnHistory: %v", err)
	}
	if status := p.threadSessionStatus("thread-" + turnFenceSession); status != "stopped" {
		t.Fatalf("thread status after the clear = %q, from a read that started before it; want stopped", status)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2", calls)
	}
}

// TestRPCSnapshotStaleCacheSharesTheReadInFlight proves a cached read whose
// cache only aged out (no write cleared it) takes the fresh read in flight
// rather than queueing a read of its own.
func TestRPCSnapshotStaleCacheSharesTheReadInFlight(t *testing.T) {
	server := newTurnFenceServer(t, turnIdle(), turnIdle())
	p := newTurnFenceTestProvider(t, server)
	p.cacheSnapshot(turnIdle())
	p.mu.Lock()
	p.snapshotCacheAt = time.Now().Add(-2 * snapshotCacheTTL)
	p.mu.Unlock()
	release, started := holdFirstRead(t, server)
	probe := make(chan error, 1)
	go func() {
		_, err := p.TurnHistory(t.Context(), turnFenceSession)
		probe <- err
	}()
	waitFor(t, func() bool { return started.Load() == 1 })
	out := make(chan freshResult, 1)
	go func() {
		snap, err := p.rpcSnapshot()
		out <- freshResult{snap, err}
	}()
	waitFor(t, func() bool { return p.fresh.inFlight() == 2 })
	release()
	if err := answered(t, probe); err != nil {
		t.Fatalf("TurnHistory: %v", err)
	}
	if r := answered(t, out); r.err != nil {
		t.Fatalf("rpcSnapshot: %v", r.err)
	}
	if calls := server.snapshotCalls(); calls != 1 {
		t.Fatalf("snapshot reads = %d, want 1: the stale cache shares the probe's read", calls)
	}
}
