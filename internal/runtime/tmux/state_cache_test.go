package tmux

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockFetcher implements StateFetcher for testing.
type mockFetcher struct {
	mu       sync.Mutex
	calls    int
	sessions map[string]bool
	err      error
	delay    time.Duration
}

func (m *mockFetcher) FetchRunning(ctx context.Context) (map[string]bool, error) {
	m.mu.Lock()
	m.calls++
	sessions := m.sessions
	err := m.err
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return sessions, err
}

func (m *mockFetcher) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockFetcher) setResult(sessions map[string]bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = sessions
	m.err = err
}

func TestStateCache_FreshCacheReturnsCorrectState(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true, "agent-2": true},
	}
	cache := NewStateCache(f, 2*time.Second)

	if !cache.IsRunning("agent-1") {
		t.Error("expected agent-1 to be running")
	}
	if !cache.IsRunning("agent-2") {
		t.Error("expected agent-2 to be running")
	}
	if cache.IsRunning("agent-3") {
		t.Error("expected agent-3 to not be running")
	}

	// Only one fetch should have occurred (the first call populated the cache,
	// the subsequent calls should use the cached data).
	if got := f.getCalls(); got != 1 {
		t.Errorf("expected 1 fetch call, got %d", got)
	}
}

func TestStateCache_StaleCacheTriggersRefresh(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
	}
	ttl := 50 * time.Millisecond
	cache := NewStateCache(f, ttl)

	// Prime the cache.
	if !cache.IsRunning("agent-1") {
		t.Fatal("expected agent-1 to be running initially")
	}
	if got := f.getCalls(); got != 1 {
		t.Fatalf("expected 1 fetch call after prime, got %d", got)
	}

	// Update the fetcher result and wait for the cache to go stale.
	f.setResult(map[string]bool{"agent-1": true, "agent-2": true}, nil)
	time.Sleep(ttl + 10*time.Millisecond)

	// This call should trigger a refresh.
	if !cache.IsRunning("agent-2") {
		t.Error("expected agent-2 to be running after stale refresh")
	}
	if got := f.getCalls(); got != 2 {
		t.Errorf("expected 2 fetch calls after stale, got %d", got)
	}
}

func TestStateCache_ConcurrentCallersCoalesceIntoOneFetch(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
		delay:    100 * time.Millisecond,
	}
	cache := NewStateCache(f, 2*time.Second)

	var wg sync.WaitGroup
	results := make([]bool, 20)
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = cache.IsRunning("agent-1")
		}(i)
	}
	wg.Wait()

	// All should have gotten the correct result.
	for i, r := range results {
		if !r {
			t.Errorf("goroutine %d: expected true, got false", i)
		}
	}

	// singleflight should have coalesced all callers into exactly 1 fetch.
	if got := f.getCalls(); got != 1 {
		t.Errorf("expected 1 fetch call (singleflight), got %d", got)
	}
}

func TestStateCache_RefreshFailurePreservesLastKnownGood(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
	}
	ttl := 50 * time.Millisecond
	cache := NewStateCache(f, ttl)

	// Prime the cache.
	if !cache.IsRunning("agent-1") {
		t.Fatal("expected agent-1 running initially")
	}

	// Make the fetcher fail and wait for staleness.
	f.setResult(nil, errors.New("tmux subprocess failed"))
	time.Sleep(ttl + 10*time.Millisecond)

	// The cache should still report the last-known-good state.
	if !cache.IsRunning("agent-1") {
		t.Error("expected agent-1 still running after refresh failure (last-known-good)")
	}

	// Verify the error is recorded.
	cache.mu.RLock()
	lastErr := cache.lastError
	cache.mu.RUnlock()
	if lastErr == nil {
		t.Error("expected lastError to be set after refresh failure")
	}
}

func TestStateCache_InvalidateForcesNextReadToRefresh(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
	}
	cache := NewStateCache(f, 10*time.Second) // long TTL

	// Prime the cache.
	if !cache.IsRunning("agent-1") {
		t.Fatal("expected agent-1 running initially")
	}
	if got := f.getCalls(); got != 1 {
		t.Fatalf("expected 1 fetch call, got %d", got)
	}

	// Update fetcher result and invalidate.
	f.setResult(map[string]bool{"agent-2": true}, nil)
	cache.Invalidate()

	// The next read should trigger a fresh fetch.
	if cache.IsRunning("agent-1") {
		t.Error("expected agent-1 to not be running after invalidate + new fetch")
	}
	if !cache.IsRunning("agent-2") {
		t.Error("expected agent-2 to be running after invalidate + new fetch")
	}
	if got := f.getCalls(); got != 2 {
		t.Errorf("expected 2 fetch calls after invalidate, got %d", got)
	}
}

func TestStateCache_StaleTTLReturnsFalseForAllSessions(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
	}
	ttl := 50 * time.Millisecond
	cache := NewStateCache(f, ttl)
	cache.staleTTL = 100 * time.Millisecond // short staleTTL for testing

	// Prime the cache.
	if !cache.IsRunning("agent-1") {
		t.Fatal("expected agent-1 running initially")
	}

	// Make all subsequent fetches fail.
	f.setResult(nil, errors.New("tmux dead"))

	// Wait past staleTTL.
	time.Sleep(150 * time.Millisecond)

	// After staleTTL, the cache should return false for everything.
	if cache.IsRunning("agent-1") {
		t.Error("expected agent-1 to be reported as not running after staleTTL exceeded")
	}
}

func TestStateCache_EmptySessionsMap(t *testing.T) {
	f := &mockFetcher{
		sessions: map[string]bool{},
	}
	cache := NewStateCache(f, 2*time.Second)

	if cache.IsRunning("anything") {
		t.Error("expected false for any session when tmux has no sessions")
	}
}

func TestStateCache_NilSessionsMap(t *testing.T) {
	// FetchRunning returns nil map (e.g., no tmux server) — same as empty.
	f := &mockFetcher{
		sessions: nil,
	}
	cache := NewStateCache(f, 2*time.Second)

	if cache.IsRunning("anything") {
		t.Error("expected false for any session when fetch returns nil map")
	}
}

func TestStateCache_ConcurrentInvalidateAndRead(_ *testing.T) {
	var fetchCount atomic.Int64
	f := &mockFetcher{
		sessions: map[string]bool{"agent-1": true},
	}

	cache := NewStateCache(f, 50*time.Millisecond)

	// Prime.
	cache.IsRunning("agent-1")

	var wg sync.WaitGroup
	// Hammer with concurrent reads and invalidates.
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			cache.IsRunning("agent-1")
			_ = fetchCount.Load()
		}()
		go func() {
			defer wg.Done()
			cache.Invalidate()
		}()
	}
	wg.Wait()

	// No panics, no data races — that's the assertion (run with -race).
}

// TestStateCache_RefreshLogIsOptInViaEnvVar verifies that the successful
// refresh log line is silent by default and only emitted when
// GC_LOG_TMUX_CACHE=true. Regression test for #644.
func TestStateCache_RefreshLogIsOptInViaEnvVar(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	t.Run("silent by default", func(t *testing.T) {
		buf.Reset()
		t.Setenv("GC_LOG_TMUX_CACHE", "")

		f := &mockFetcher{sessions: map[string]bool{"a": true}}
		cache := NewStateCache(f, 50*time.Millisecond)
		cache.IsRunning("a")

		if got := buf.String(); got != "" {
			t.Errorf("expected no log output by default, got %q", got)
		}
	})

	t.Run("logs when opted in", func(t *testing.T) {
		buf.Reset()
		t.Setenv("GC_LOG_TMUX_CACHE", "true")

		f := &mockFetcher{sessions: map[string]bool{"a": true}}
		cache := NewStateCache(f, 50*time.Millisecond)
		cache.IsRunning("a")

		got := buf.String()
		if !strings.Contains(got, "tmux state cache: refreshed") {
			t.Errorf("expected refresh log with GC_LOG_TMUX_CACHE=true, got %q", got)
		}
		if strings.Contains(got, "refresh failed") {
			t.Errorf("unexpected failure log in success path, got %q", got)
		}
	})

	t.Run("failure log still emitted when opt-out", func(t *testing.T) {
		buf.Reset()
		t.Setenv("GC_LOG_TMUX_CACHE", "")

		f := &mockFetcher{err: errors.New("boom")}
		cache := NewStateCache(f, 50*time.Millisecond)
		cache.IsRunning("a")

		got := buf.String()
		if !strings.Contains(got, "tmux state cache: refresh failed") {
			t.Errorf("expected refresh-failed log regardless of GC_LOG_TMUX_CACHE, got %q", got)
		}
	})
}
