package tmux

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedFetcher returns a per-call result from fn, so a test can decide
// what each refresh observes and when it lands relative to Invalidate and
// EvictSession.
type scriptedFetcher struct {
	calls atomic.Int64
	fn    func(ctx context.Context, call int64) (runtimeStateSnapshot, error)
}

func (f *scriptedFetcher) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	return f.fn(ctx, f.calls.Add(1))
}

func runningSnapshot(names ...string) runtimeStateSnapshot {
	sessions := make(map[string]sessionRuntimeState, len(names))
	for _, name := range names {
		sessions[name] = sessionRuntimeState{Running: true}
	}
	return runtimeStateSnapshot{Sessions: sessions}
}

// TestStateCache_SteadyInvalidationDoesNotStarveToFalseAbsent pins the
// false-absent this file exists for. On a busy city an invalidation (a session
// start, relaunch, or stop elsewhere) can land during every fetch. A refresh
// that was superseded mid-flight used to be discarded without advancing
// fetchedAt, so nothing new was ever published; once staleTTL passed,
// currentState returned an empty snapshot and IsRunning reported every session
// absent while every single fetch had seen it running.
func TestStateCache_SteadyInvalidationDoesNotStarveToFalseAbsent(t *testing.T) {
	var cache *StateCache
	var armed atomic.Bool
	f := &scriptedFetcher{fn: func(context.Context, int64) (runtimeStateSnapshot, error) {
		if armed.Load() {
			cache.Invalidate() // lands while this fetch is in flight
		}
		return runningSnapshot("agent-1"), nil
	}}
	cache = NewStateCache(f, time.Hour)
	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false after prime, want true")
	}
	armed.Store(true)
	cache.Invalidate()

	// Before each read, age the last publish past staleTTL: the wall-clock
	// time a loaded box spends between reads, without sleeping for it. Every
	// fetch reports agent-1 running, so every read must too.
	const reads = 20
	before := f.calls.Load()
	for read := 1; read <= reads; read++ {
		cache.mu.Lock()
		cache.fetchedAt = time.Now().Add(-2 * cache.staleTTL)
		cache.mu.Unlock()
		if !cache.IsRunning("agent-1") {
			t.Fatalf("read %d: IsRunning(agent-1) = false, but all %d fetches reported it running", read, f.calls.Load())
		}
	}
	if fetches := f.calls.Load() - before; fetches != reads {
		t.Fatalf("%d fetches for %d reads, want one each: a superseded refresh must not retry within a read", fetches, reads)
	}

	cache.mu.RLock()
	dirty := cache.dirty
	cache.mu.RUnlock()
	if !dirty {
		t.Fatal("cache clean after a superseded refresh, want dirty so the next read refreshes again")
	}
}

// TestStateCache_SupersededRefreshKeepsSessionEvictedMidFetch pins the
// eviction contract the superseded publish must keep. Stop kills a session and
// then evicts it; a fetch that started before the kill may still have seen it.
// Publishing that fetch must not resurrect the session, while everything else
// it observed is published and the cache stays dirty.
func TestStateCache_SupersededRefreshKeepsSessionEvictedMidFetch(t *testing.T) {
	stale := runningSnapshot("agent-1", "agent-2")
	entered := make(chan struct{})
	release := make(chan struct{})
	f := &scriptedFetcher{fn: func(ctx context.Context, call int64) (runtimeStateSnapshot, error) {
		switch call {
		case 1:
			return runningSnapshot("agent-1", "agent-2"), nil
		case 2:
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
			// Observed before the kill, so agent-1 is still listed.
			return stale, nil
		default:
			return runningSnapshot("agent-2"), nil
		}
	}}
	cache := NewStateCache(f, time.Hour)
	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false after prime, want true")
	}
	cache.mu.RLock()
	primedAt := cache.fetchedAt
	cache.mu.RUnlock()

	cache.Invalidate()
	done := make(chan bool, 1)
	go func() { done <- cache.IsRunning("agent-1") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh to start")
	}
	cache.EvictSession("agent-1")
	close(release)
	select {
	case got := <-done:
		if got {
			t.Fatal("IsRunning(agent-1) = true: a fetch that started before the eviction resurrected the session")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for IsRunning")
	}

	cache.mu.RLock()
	published := cache.state
	fetchedAt := cache.fetchedAt
	dirty := cache.dirty
	cache.mu.RUnlock()
	if _, ok := published.Sessions["agent-1"]; ok {
		t.Fatalf("published snapshot holds evicted agent-1: %v", published.Sessions)
	}
	if !published.Sessions["agent-2"].Running {
		t.Fatalf("published snapshot lost agent-2, which the superseded fetch observed: %v", published.Sessions)
	}
	if !fetchedAt.After(primedAt) {
		t.Fatal("fetchedAt not advanced by the superseded refresh, want it advanced (the server was observed)")
	}
	if !dirty {
		t.Fatal("cache clean after a superseded refresh, want dirty")
	}
	if _, ok := stale.Sessions["agent-1"]; !ok {
		t.Fatal("filtering the evicted session mutated the fetcher's map in place, want a copy")
	}

	// Still dirty, so the next read refreshes; that refresh is not superseded
	// and settles the cache.
	if !cache.IsRunning("agent-2") {
		t.Fatal("IsRunning(agent-2) = false after follow-up refresh, want true")
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("fetch calls = %d, want 3 (prime, superseded, follow-up)", got)
	}
	cache.mu.RLock()
	dirty = cache.dirty
	pending := len(cache.evictedAt)
	cache.mu.RUnlock()
	if dirty {
		t.Fatal("cache still dirty after an unsuperseded refresh")
	}
	if pending != 0 {
		t.Fatalf("evictedAt holds %d entries after a clean refresh, want them pruned", pending)
	}
}

// TestStateCache_SupersededRefreshDoesNotOverwriteNewerPublish covers the
// overlap a dirty read allows: currentState forgets the in-flight singleflight
// when dirty, so a newer fetch can start and publish while an older one is
// still running. The older one, superseded and finishing last, must not
// overwrite the newer observation.
func TestStateCache_SupersededRefreshDoesNotOverwriteNewerPublish(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	f := &scriptedFetcher{fn: func(ctx context.Context, call int64) (runtimeStateSnapshot, error) {
		switch call {
		case 1:
			return runningSnapshot("agent-1", "agent-2"), nil
		case 2:
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return runtimeStateSnapshot{}, ctx.Err()
			}
			return runningSnapshot("agent-1", "agent-2"), nil
		default:
			// agent-1 exited on its own between the two observations.
			return runningSnapshot("agent-2"), nil
		}
	}}
	cache := NewStateCache(f, time.Hour)
	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false after prime, want true")
	}

	cache.Invalidate()
	var older sync.WaitGroup
	older.Add(1)
	go func() {
		defer older.Done()
		_ = cache.IsRunning("agent-1")
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the older refresh to start")
	}

	cache.Invalidate()
	if cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = true after the newer refresh, want false")
	}

	close(release)
	older.Wait()
	cache.mu.RLock()
	published := cache.state
	dirty := cache.dirty
	cache.mu.RUnlock()
	if _, ok := published.Sessions["agent-1"]; ok {
		t.Fatalf("published snapshot = %v: an older superseded refresh overwrote a newer one", published.Sessions)
	}
	if dirty {
		t.Fatal("cache dirty after the discarded older refresh, want the newer publish to stand")
	}
	if cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = true after the older refresh landed, want false")
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("fetch calls = %d, want 3 (prime, older, newer)", got)
	}
}

// TestStateCache_UnprimedNoServerStaysDirtyWhenSupersededMidFetch covers the
// other publish path. An unprimed cache that finds no tmux server primes an
// empty snapshot. If a Start brought the server up and invalidated while that
// fetch was in flight, the empty snapshot must not also clear dirty, or the
// new session reads absent until the TTL lapses.
func TestStateCache_UnprimedNoServerStaysDirtyWhenSupersededMidFetch(t *testing.T) {
	var cache *StateCache
	f := &scriptedFetcher{fn: func(_ context.Context, call int64) (runtimeStateSnapshot, error) {
		if call == 1 {
			cache.Invalidate() // the Start landing mid-fetch
			return runtimeStateSnapshot{}, ErrNoServer
		}
		return runningSnapshot("agent-1"), nil
	}}
	cache = NewStateCache(f, time.Hour)

	if cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = true against a server-less fetch, want false")
	}
	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false: the empty prime cleared an invalidation that landed mid-fetch")
	}
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("fetch calls = %d, want 2", got)
	}
}
