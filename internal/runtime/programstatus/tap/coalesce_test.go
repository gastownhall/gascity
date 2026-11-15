package tap

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// drive publishes every interval for d at changesPerSecond of demand,
// starting at *now with *changes recorded so far, and returns the interval.
func drive(c *coalescer, now *time.Time, changes *uint64, d time.Duration, changesPerSecond int) time.Duration {
	start, base := *now, *changes
	end := now.Add(d)
	for now.Before(end) {
		*now = now.Add(c.interval)
		*changes = base + uint64(float64(changesPerSecond)*now.Sub(start).Seconds())
		c.observe(*now, *changes)
	}
	return c.interval
}

func TestTapBacksOffUnderChurn(t *testing.T) {
	c := newCoalescer(defaultConfig())
	now, changes := t0, uint64(0)
	c.observe(now, changes)
	if c.interval != 25*time.Millisecond {
		t.Fatalf("initial interval %v, want 25ms", c.interval)
	}

	// Under 5 s of churn the interval holds.
	if got := drive(c, &now, &changes, 4*time.Second, 40); got != 25*time.Millisecond {
		t.Fatalf("after 4s of churn interval = %v, want 25ms", got)
	}
	// Sustained churn doubles it, capped at 1 s.
	if got := drive(c, &now, &changes, 3*time.Second, 40); got <= 25*time.Millisecond {
		t.Fatalf("after 7s of churn interval = %v, want it backing off", got)
	}
	if got := drive(c, &now, &changes, 20*time.Second, 40); got != time.Second {
		t.Fatalf("after 27s of churn interval = %v, want the 1s cap", got)
	}
	// Churn keeps it at the cap, even though throttled publishes are rare.
	if got := drive(c, &now, &changes, 10*time.Second, 40); got != time.Second {
		t.Fatalf("continued churn interval = %v, want 1s", got)
	}

	// 5 s of calm resets it.
	if got := drive(c, &now, &changes, 3*time.Second, 2); got != time.Second {
		t.Fatalf("after 3s of calm interval = %v, want still 1s", got)
	}
	if got := drive(c, &now, &changes, 3*time.Second, 2); got != 25*time.Millisecond {
		t.Fatalf("after 6s of calm interval = %v, want reset to 25ms", got)
	}
}

func TestTapCoalescerResetsAfterQuietGap(t *testing.T) {
	c := newCoalescer(defaultConfig())
	now, changes := t0, uint64(0)
	c.observe(now, changes)
	drive(c, &now, &changes, 30*time.Second, 40)
	if c.interval != time.Second {
		t.Fatalf("interval %v, want 1s", c.interval)
	}
	// The program goes quiet; its next change arrives a minute later.
	now = now.Add(time.Minute)
	changes++
	c.observe(now, changes)
	if c.interval != 25*time.Millisecond {
		t.Fatalf("after a quiet minute interval = %v, want 25ms", c.interval)
	}
}

func TestTapCoalescerIgnoresModerateRate(t *testing.T) {
	c := newCoalescer(defaultConfig())
	now, changes := t0, uint64(0)
	c.observe(now, changes)
	if got := drive(c, &now, &changes, 30*time.Second, 10); got != 25*time.Millisecond {
		t.Fatalf("10 changes/s for 30s gave interval %v; only a rate above 10/s is churn", got)
	}
}

func TestTapRetryBackoff(t *testing.T) {
	cfg := defaultConfig()
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second, time.Second}
	for i, w := range want {
		if got := cfg.retryDelay(i + 1); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
}
