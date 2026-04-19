package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestParseBlockingParams(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/city/test-city/agents?index=42&wait=10s", nil)
	bp := parseBlockingParams(req)

	if bp.Index != 42 {
		t.Errorf("Index = %d, want 42", bp.Index)
	}
	if bp.Wait != 10*time.Second {
		t.Errorf("Wait = %v, want 10s", bp.Wait)
	}
}

func TestParseBlockingParamsDefaults(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/city/test-city/agents", nil)
	bp := parseBlockingParams(req)

	if bp.Index != 0 {
		t.Errorf("Index = %d, want 0", bp.Index)
	}
	if bp.Wait != defaultWait {
		t.Errorf("Wait = %v, want %v", bp.Wait, defaultWait)
	}
	if bp.isBlocking() {
		t.Error("isBlocking() = true, want false")
	}
}

func TestParseBlockingParamsMaxWait(t *testing.T) {
	req := httptest.NewRequest("GET", "/v0/city/test-city/agents?index=1&wait=10m", nil)
	bp := parseBlockingParams(req)

	if bp.Wait != maxWait {
		t.Errorf("Wait = %v, want %v (capped)", bp.Wait, maxWait)
	}
}

func TestWaitForChangeImmediate(t *testing.T) {
	ep := events.NewFake()
	// Record an event so LatestSeq > 0.
	ep.Record(events.Event{Type: "test"})

	bp := BlockingParams{Index: 0, Wait: time.Second}
	got := waitForChange(context.Background(), ep, bp)
	if got != 1 {
		t.Errorf("waitForChange = %d, want 1", got)
	}
}

func TestWaitForChangeTimeout(t *testing.T) {
	ep := events.NewFake()
	bp := BlockingParams{Index: 99, Wait: 100 * time.Millisecond}

	start := time.Now()
	got := waitForChange(context.Background(), ep, bp)
	elapsed := time.Since(start)

	if got != 0 {
		t.Errorf("waitForChange = %d, want 0", got)
	}
	// Should take ~100ms (plus jitter).
	if elapsed < 50*time.Millisecond {
		t.Errorf("returned too fast: %v", elapsed)
	}
}

func TestWaitForChangeEvent(t *testing.T) {
	ep := events.NewFake()
	bp := BlockingParams{Index: 0, Wait: 5 * time.Second}

	// Record an event after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		ep.Record(events.Event{Type: "test"})
	}()

	start := time.Now()
	got := waitForChange(context.Background(), ep, bp)
	elapsed := time.Since(start)

	if got != 1 {
		t.Errorf("waitForChange = %d, want 1", got)
	}
	// Should return quickly, not wait the full 5s.
	if elapsed > 2*time.Second {
		t.Errorf("waited too long: %v", elapsed)
	}
}

func TestWaitForChangeTinyWait(t *testing.T) {
	// Regression: wait < 16ns caused rand.Int64N(0) panic.
	ep := events.NewFake()
	bp := BlockingParams{Index: 99, Wait: 1 * time.Nanosecond}

	// Must not panic.
	got := waitForChange(context.Background(), ep, bp)
	if got != 0 {
		t.Errorf("waitForChange = %d, want 0", got)
	}
}

func TestWaitForChangeNilProvider(t *testing.T) {
	bp := BlockingParams{Index: 1, Wait: time.Second}
	got := waitForChange(context.Background(), nil, bp)
	if got != 0 {
		t.Errorf("waitForChange(nil) = %d, want 0", got)
	}
}
