package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
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
	state    runtimeStateSnapshot
	err      error
	delay    time.Duration
}

func (m *mockFetcher) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	m.mu.Lock()
	m.calls++
	state := m.state
	sessions := m.sessions
	err := m.err
	delay := m.delay
	m.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return runtimeStateSnapshot{}, ctx.Err()
		}
	}
	if state.Sessions == nil && sessions != nil {
		state.Sessions = make(map[string]sessionRuntimeState, len(sessions))
		for name, running := range sessions {
			state.Sessions[name] = sessionRuntimeState{Running: running}
		}
	}
	return state, err
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
	m.state = runtimeStateSnapshot{}
	m.err = err
}

type controlledRefreshFetcher struct {
	mu        sync.Mutex
	calls     int
	state     runtimeStateSnapshot
	blockCall int
	entered   chan struct{}
	release   chan struct{}
}

func (f *controlledRefreshFetcher) FetchState(ctx context.Context) (runtimeStateSnapshot, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	state := f.state
	f.mu.Unlock()

	if call == f.blockCall {
		close(f.entered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return runtimeStateSnapshot{}, ctx.Err()
		}
	}
	return state, nil
}

func (f *controlledRefreshFetcher) getCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

func TestStateCache_ProcessAliveUsesFreshSnapshot(t *testing.T) {
	f := &mockFetcher{
		state: runtimeStateSnapshot{
			Sessions: map[string]sessionRuntimeState{
				"agent-1": {
					Running: true,
					Panes: []paneRuntimeState{{
						Command: "claude",
						PID:     "101",
					}},
				},
			},
			Processes: newProcessSnapshot([]processRuntimeState{{
				PID:     "101",
				PPID:    "1",
				Command: "claude",
				Args:    "claude --dangerously-skip-permissions",
			}}),
		},
	}
	cache := NewStateCache(f, 2*time.Second)

	if !cache.ProcessAlive("agent-1", []string{"claude"}) {
		t.Fatal("ProcessAlive(agent-1, claude) = false, want true")
	}
	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false, want true from same snapshot")
	}
	if cache.ProcessAlive("agent-1", []string{"codex"}) {
		t.Fatal("ProcessAlive(agent-1, codex) = true, want false")
	}
	if got := f.getCalls(); got != 1 {
		t.Fatalf("fetch calls = %d, want 1 across ProcessAlive and IsRunning", got)
	}
}

func TestStateCache_ProcessAliveMatchesShellDescendantFromSnapshot(t *testing.T) {
	f := &mockFetcher{
		state: runtimeStateSnapshot{
			Sessions: map[string]sessionRuntimeState{
				"agent-1": {
					Running: true,
					Panes: []paneRuntimeState{{
						Command: "bash",
						PID:     "101",
					}},
				},
			},
			Processes: newProcessSnapshot([]processRuntimeState{
				{PID: "101", PPID: "1", Command: "bash", Args: "bash -lc codex"},
				{PID: "102", PPID: "101", Command: "node", Args: "node /usr/local/bin/codex"},
			}),
		},
	}
	cache := NewStateCache(f, 2*time.Second)

	if !cache.ProcessAlive("agent-1", []string{"codex"}) {
		t.Fatal("ProcessAlive(agent-1, codex) = false, want true from cached descendant snapshot")
	}
	if got := f.getCalls(); got != 1 {
		t.Fatalf("fetch calls = %d, want 1", got)
	}
}

func TestProviderObserveLivenessUsesCacheProcessSnapshot(t *testing.T) {
	f := &mockFetcher{
		state: runtimeStateSnapshot{
			Sessions: map[string]sessionRuntimeState{
				"agent-1": {
					Running: true,
					Panes: []paneRuntimeState{{
						Command: "bash",
						PID:     "101",
					}},
				},
			},
			Processes: newProcessSnapshot([]processRuntimeState{
				{PID: "101", PPID: "1", Command: "bash", Args: "bash -lc codex"},
				{PID: "102", PPID: "101", Command: "node", Args: "node /usr/local/bin/codex"},
			}),
		},
	}
	provider := &Provider{cache: NewStateCache(f, time.Hour)}

	got := provider.ObserveLiveness("agent-1", []string{"codex"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v, want running and alive from cache", got)
	}
	got = provider.ObserveLiveness("agent-1", []string{"codex"})
	if !got.Running || !got.Alive {
		t.Fatalf("second ObserveLiveness = %+v, want running and alive from cache", got)
	}
	if calls := f.getCalls(); calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 across repeated ObserveLiveness calls", calls)
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

func TestStateCache_DiscardRefreshAfterEvictSession(t *testing.T) {
	state := runtimeStateSnapshot{
		Sessions: map[string]sessionRuntimeState{
			"agent-1": {Running: true},
		},
	}
	f := &controlledRefreshFetcher{
		state:     state,
		blockCall: 2,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	cache := NewStateCache(f, time.Nanosecond)

	if !cache.IsRunning("agent-1") {
		t.Fatal("expected agent-1 running after prime")
	}
	time.Sleep(time.Millisecond)

	result := make(chan bool, 1)
	go func() {
		result <- cache.IsRunning("agent-1")
	}()

	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh to start")
	}
	cache.EvictSession("agent-1")
	close(f.release)

	select {
	case got := <-result:
		if got {
			t.Fatal("IsRunning(agent-1) = true after concurrent eviction, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for IsRunning result")
	}
	if calls := f.getCalls(); calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", calls)
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

func TestFetchProcessSnapshotCanceledContextReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchProcessSnapshot(ctx)
	if err == nil {
		t.Fatal("fetchProcessSnapshot canceled context returned nil error")
	}
}

func TestParseProcessSnapshotLineFixedColumns(t *testing.T) {
	line := fmt.Sprintf("%10s %10s %-64s %s", "123", "1", "claude code", "claude code --print")
	got, ok := parseProcessSnapshotLineFixedColumns(line)
	if !ok {
		t.Fatal("parseProcessSnapshotLineFixedColumns returned ok=false")
	}
	if got.PID != "123" || got.PPID != "1" || got.Command != "claude code" || got.Args != "claude code --print" {
		t.Fatalf("parseProcessSnapshotLineFixedColumns = %+v, want fixed-column fields preserved", got)
	}
}

func TestParseProcessSnapshotLineWhitespace(t *testing.T) {
	// macOS `ps -eo pid=,ppid=,comm=,args=` output: right-aligned numeric
	// columns, comm separated from args by whitespace.
	cases := []struct {
		name     string
		line     string
		wantPID  string
		wantPPID string
		wantCmd  string
		wantArgs string
	}{
		{
			name:     "typical 4-token line",
			line:     "  123     1 /sbin/launchd    /sbin/launchd --boot",
			wantPID:  "123",
			wantPPID: "1",
			wantCmd:  "/sbin/launchd",
			wantArgs: "/sbin/launchd --boot",
		},
		{
			name:     "args has multiple spaces preserved",
			line:     "456 1 claude claude code --print --json",
			wantPID:  "456",
			wantPPID: "1",
			wantCmd:  "claude",
			wantArgs: "claude code --print --json",
		},
		{
			name:     "no args (3-token line)",
			line:     "789 1 kernel_task",
			wantPID:  "789",
			wantPPID: "1",
			wantCmd:  "kernel_task",
			wantArgs: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcessSnapshotLineWhitespace(tc.line)
			if !ok {
				t.Fatalf("returned ok=false for %q", tc.line)
			}
			if got.PID != tc.wantPID {
				t.Errorf("PID = %q, want %q", got.PID, tc.wantPID)
			}
			if got.PPID != tc.wantPPID {
				t.Errorf("PPID = %q, want %q", got.PPID, tc.wantPPID)
			}
			if got.Command != tc.wantCmd {
				t.Errorf("Command = %q, want %q", got.Command, tc.wantCmd)
			}
			if got.Args != tc.wantArgs {
				t.Errorf("Args = %q, want %q", got.Args, tc.wantArgs)
			}
		})
	}
}

func TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin(t *testing.T) {
	// Regression: macOS ps rejects the BSD `:N=` column-width form. Confirm
	// we don't emit it on Darwin. Skip elsewhere — Linux ps accepts both
	// forms so verifying the wide form there is just a tautology.
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin-specific syntax guard")
	}
	args := processSnapshotPSArgs()
	for _, a := range args {
		if strings.Contains(a, ":") {
			t.Fatalf("processSnapshotPSArgs returned %v on darwin; contains Linux-only `:N=` width specifier", args)
		}
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

func TestIsNoServerErrorRecognizesSentinel(t *testing.T) {
	if !isNoServerError(ErrNoServer) {
		t.Fatal("isNoServerError(ErrNoServer) = false, want true")
	}
}
