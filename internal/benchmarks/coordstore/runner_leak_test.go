package coordstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMemSamplerAbortOnCeiling verifies that when the injected RSS reader
// returns a value above MaxRSSBytes, sampleOnce sends on abortCh.
func TestMemSamplerAbortOnCeiling(t *testing.T) {
	const ceiling = 100 * 1024 * 1024 // 100 MiB

	callCount := 0
	fake := func() (uint64, bool) {
		callCount++
		if callCount >= 2 {
			return ceiling + 1, true // exceeds ceiling on second call
		}
		return ceiling / 2, true // under ceiling on first call
	}

	m := newMemSampler(time.Millisecond)
	m.readRSS = fake
	m.maxRSSBytes = ceiling
	m.abortCh = make(chan string, 1)
	m.startedAt = time.Now()

	// First sample (baseline) — under ceiling.
	m.sampleOnce()
	select {
	case msg := <-m.abortCh:
		t.Fatalf("unexpected abort on under-ceiling sample: %s", msg)
	default:
	}

	// Second sample — over ceiling.
	m.sampleOnce()
	select {
	case msg := <-m.abortCh:
		if !strings.Contains(msg, "RSS") {
			t.Fatalf("abort finding should mention RSS, got: %s", msg)
		}
		if !strings.Contains(msg, "ceiling") {
			t.Fatalf("abort finding should mention ceiling, got: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected abort signal but got none")
	}
}

// TestMemSamplerAbortOnGrowthRate verifies that sustained RSS growth above
// MaxRSSGrowthBytesPerSec triggers an abort after the warm-up period.
func TestMemSamplerAbortOnGrowthRate(t *testing.T) {
	const growthLimit = 10 * 1024 * 1024 // 10 MiB/s

	rssNow := uint64(50 * 1024 * 1024) // 50 MiB
	fake := func() (uint64, bool) {
		return rssNow, true
	}

	m := newMemSampler(time.Millisecond)
	m.readRSS = fake
	m.maxRSSGrowthBytesPerSec = growthLimit
	m.abortCh = make(chan string, 1)

	// Simulate post-warm-up state: warm-up was 2x the warm-up period ago.
	// Place warm-up 2s ago so growth rate = (50 MiB - 10 MiB) / 2s = 20 MiB/s > 10 MiB/s limit.
	m.startedAt = time.Now().Add(-(memGuardWarmUp + 2*time.Second))
	m.warmUpAt = time.Now().Add(-2 * time.Second)
	m.warmUpRSS = 10 * 1024 * 1024 // 10 MiB at warm-up

	m.sampleOnce()

	select {
	case msg := <-m.abortCh:
		if !strings.Contains(msg, "growth") {
			t.Fatalf("abort finding should mention growth, got: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected abort signal on growth-rate breach but got none")
	}
}

// TestRunnerAbortsOnMemoryCeiling verifies that Runner.Run returns early
// (before full Duration) and sets LeakAborted+LeakFinding in the Scorecard
// when the injected RSS reader exceeds MaxRSSBytes.
func TestRunnerAbortsOnMemoryCeiling(t *testing.T) {
	ctx := context.Background()

	const ceiling = 1 // 1 byte — always exceeded by any real RSS

	wl := SmokeWorkload
	wl.Duration = 10 * time.Second // long enough that early abort is noticeable
	wl.MaxRSSBytes = ceiling

	seed := SeedResult{MainOpenIDs: []string{"main-1"}}
	runner := NewRunner(&leakTestAdapter{}, wl, seed)

	// Inject a fake RSS reader that immediately exceeds the ceiling.
	runner.rssReader = func() (uint64, bool) {
		return ceiling + 1, true
	}

	start := time.Now()
	sc, err := runner.Run(ctx, noopWriter{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("Run did not abort early: elapsed=%s, want < 5s", elapsed)
	}
	if !sc.LeakAborted {
		t.Fatal("Scorecard.LeakAborted should be true")
	}
	if !strings.Contains(sc.LeakFinding, "RSS") {
		t.Fatalf("Scorecard.LeakFinding should mention RSS, got: %q", sc.LeakFinding)
	}
	if sc.Passed() {
		t.Fatal("Scorecard.Passed() should be false when LeakAborted")
	}
}

// leakTestAdapter is a minimal no-op StoreAdapter for control-flow tests.
type leakTestAdapter struct{}

func (a *leakTestAdapter) Open(_ context.Context, _ Config) error   { return nil }
func (a *leakTestAdapter) Close() error                             { return nil }
func (a *leakTestAdapter) Reset(_ context.Context) error            { return nil }
func (a *leakTestAdapter) PrimeScan(_ context.Context) (int, error) { return 0, nil }
func (a *leakTestAdapter) Create(_ context.Context, r Record) (Record, error) {
	return r, nil
}
func (a *leakTestAdapter) Get(_ context.Context, _ string) (Record, error)          { return Record{}, nil }
func (a *leakTestAdapter) BatchGet(_ context.Context, _ []string) ([]Record, error) { return nil, nil }
func (a *leakTestAdapter) Update(_ context.Context, _ string, _ Update) error       { return nil }
func (a *leakTestAdapter) Delete(_ context.Context, _ string) error                 { return nil }
func (a *leakTestAdapter) SetMetadataBatch(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (a *leakTestAdapter) FilterScan(_ context.Context, _ Query) ([]Record, error) { return nil, nil }
func (a *leakTestAdapter) Ready(_ context.Context, _ ReadyQuery) ([]Record, error) { return nil, nil }
func (a *leakTestAdapter) RecentScan(_ context.Context, _ int) ([]Record, error)   { return nil, nil }
func (a *leakTestAdapter) DepAdd(_ context.Context, _, _, _ string) error          { return nil }
func (a *leakTestAdapter) DepRemove(_ context.Context, _, _ string) error          { return nil }
func (a *leakTestAdapter) DepList(_ context.Context, _, _ string) ([]Dep, error)   { return nil, nil }
func (a *leakTestAdapter) PurgeExpired(_ context.Context) (int, error)             { return 0, nil }
func (a *leakTestAdapter) PurgeTerminal(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}
func (a *leakTestAdapter) Stats(_ context.Context) map[string]int64 { return nil }

// noopWriter discards all output.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
