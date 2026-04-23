package supervisor

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// newTestLoop constructs a StoreMaintenanceLoop with deterministic clock
// and rand so nextDelay / jitter math is reproducible. Enabled defaults
// to true so Run() will execute unless the test overrides cfg.
func newTestLoop(t *testing.T, cfg config.DoltMaintenance, now time.Time, jitterFrac float64, lastRunAt time.Time) *StoreMaintenanceLoop {
	t.Helper()
	return NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:       cfg,
		CityPath:  t.TempDir(),
		Clock:     func() time.Time { return now },
		Rand:      func() float64 { return jitterFrac },
		LastRunAt: lastRunAt,
	})
}

func TestNextDelay_LastRunAtZeroValue_ReturnsZero(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	got := loop.nextDelay(now)
	if got != 0 {
		t.Fatalf("nextDelay(zero lastRunAt) = %v; want 0 (fire immediately)", got)
	}
}

func TestNextDelay_LastRunAtInsideInterval_ReturnsPositiveDelay(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	// Ran 10 minutes ago; next scheduled ~50 min out (±10% jitter).
	last := now.Add(-10 * time.Minute)
	loop := newTestLoop(t, cfg, now, 0.5, last)

	got := loop.nextDelay(now)
	if got <= 0 {
		t.Fatalf("nextDelay(inside interval) = %v; want > 0", got)
	}
	// With jitter=0.5 → factor 1.0 → interval 1h → due = last+1h → delay 50m.
	want := 50 * time.Minute
	if got != want {
		t.Fatalf("nextDelay = %v; want exactly %v with jitterFrac=0.5", got, want)
	}
}

func TestNextDelay_LastRunAtOutsideInterval_ReturnsZero(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	// Ran 2 hours ago; > 1.5× interval → catch up immediately.
	last := now.Add(-2 * time.Hour)
	loop := newTestLoop(t, cfg, now, 0.5, last)

	got := loop.nextDelay(now)
	if got != 0 {
		t.Fatalf("nextDelay(stale lastRunAt) = %v; want 0 (catch-up)", got)
	}
}

func TestNextDelay_JitterStaysWithinBounds(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	last := now // due exactly one jittered interval from now.
	cases := []struct {
		name    string
		jitter  float64
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"min_jitter", 0.0, 54 * time.Minute, 54 * time.Minute}, // factor 0.9
		{"max_jitter", 1.0, 66 * time.Minute, 66 * time.Minute}, // factor 1.1
		{"mid_jitter", 0.5, 60 * time.Minute, 60 * time.Minute}, // factor 1.0
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loop := newTestLoop(t, cfg, now, tc.jitter, last)
			got := loop.nextDelay(now)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("nextDelay = %v; want between %v and %v", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestRunOnce_LeaseContention_ReturnsWithoutUpdatingState(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	// Simulate another writer holding the lease.
	loop.mu.Lock()
	defer loop.mu.Unlock()

	// runOnce should return immediately without touching lastRunAt or history.
	loop.runOnce(context.Background())
	if !loop.lastRunAt.IsZero() {
		t.Fatalf("runOnce updated lastRunAt = %v under contention; want zero", loop.lastRunAt)
	}
	if len(loop.history) != 0 {
		t.Fatalf("runOnce appended %d history entries under contention; want 0", len(loop.history))
	}
}

func TestRunOnce_PlaceholderUpdatesStateAndLogs(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:      cfg,
		CityPath: "/tmp/city",
		Clock:    func() time.Time { return now },
		Rand:     func() float64 { return 0.5 },
		Stderr:   &stderr,
	})

	loop.runOnce(context.Background())

	if got := loop.LastRunAt(); !got.Equal(now) {
		t.Fatalf("LastRunAt = %v; want %v", got, now)
	}
	if hist := loop.History(); len(hist) != 1 {
		t.Fatalf("History length = %d; want 1", len(hist))
	}
	if !strings.Contains(stderr.String(), "would run maintenance") {
		t.Fatalf("stderr = %q; want it to contain 'would run maintenance'", stderr.String())
	}
}

func TestRun_DisabledLoopReturnsImmediately(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: false, Interval: "1h"}
	loop := newTestLoop(t, cfg, time.Now(), 0.5, time.Time{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run with Enabled=false did not return immediately")
	}
}

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	// Seed lastRunAt so the loop sleeps instead of spinning on immediate fires.
	now := time.Now()
	loop := newTestLoop(t, cfg, now, 0.5, now)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		loop.Run(ctx)
	}()
	cancel()

	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not exit within 500ms of context cancellation (goroutine leak)")
	}
}

func TestHistory_RingBufferBoundedAt16(t *testing.T) {
	t.Parallel()
	cfg := config.DoltMaintenance{Enabled: true, Interval: "1h"}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop := newTestLoop(t, cfg, now, 0.5, time.Time{})

	for i := 0; i < 20; i++ {
		loop.runOnce(context.Background())
	}
	if got := len(loop.History()); got != maintenanceHistorySize {
		t.Fatalf("History length after 20 runs = %d; want %d (bounded)", got, maintenanceHistorySize)
	}
}

func TestSeedLastRunAt_NoEventsReturnsZero(t *testing.T) {
	t.Parallel()
	got := SeedLastRunAt(events.NewFake())
	if !got.IsZero() {
		t.Fatalf("SeedLastRunAt(empty provider) = %v; want zero", got)
	}
}

func TestSeedLastRunAt_NilProviderReturnsZero(t *testing.T) {
	t.Parallel()
	got := SeedLastRunAt(nil)
	if !got.IsZero() {
		t.Fatalf("SeedLastRunAt(nil) = %v; want zero", got)
	}
}

func TestSeedLastRunAt_ReturnsLatestDoneTimestamp(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	earlier := time.Date(2026, 4, 20, 3, 0, 0, 0, time.UTC)
	later := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	// Unordered on purpose: function must pick the latest, not the last appended.
	fake.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: later})
	fake.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: earlier})

	got := SeedLastRunAt(fake)
	if !got.Equal(later) {
		t.Fatalf("SeedLastRunAt = %v; want %v (latest event ts)", got, later)
	}
}

func TestSeedLastRunAt_IgnoresUnrelatedEventTypes(t *testing.T) {
	t.Parallel()
	fake := events.NewFake()
	fake.Record(events.Event{Type: events.StoreMaintenanceFailed, Ts: time.Now()})
	fake.Record(events.Event{Type: "controller.started", Ts: time.Now()})

	got := SeedLastRunAt(fake)
	if !got.IsZero() {
		t.Fatalf("SeedLastRunAt (only non-done events) = %v; want zero", got)
	}
}

func TestSeedLastRunAt_BrokenProviderReturnsZero(t *testing.T) {
	t.Parallel()
	got := SeedLastRunAt(events.NewFailFake())
	if !got.IsZero() {
		t.Fatalf("SeedLastRunAt(failing provider) = %v; want zero (errors swallowed)", got)
	}
}

func TestNextDelay_IntervalDefaultsToWeekly(t *testing.T) {
	t.Parallel()
	// No Interval set — IntervalOrDefault returns 168h.
	cfg := config.DoltMaintenance{Enabled: true}
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	last := now
	loop := newTestLoop(t, cfg, now, 0.5, last)

	got := loop.nextDelay(now)
	want := 168 * time.Hour
	if got != want {
		t.Fatalf("nextDelay with default interval = %v; want %v", got, want)
	}
}
