package resilience

import (
	"sync"
	"testing"
	"time"
)

// identityJitter is the canonical test pin: it returns the full backoff cap
// instead of a uniform draw from (0, cap], so an open episode's deadline is
// exactly OpenBase << (trips-1) and a test can assert on it.
func identityJitter(capDur time.Duration) time.Duration { return capDur }

func TestRegistrySetJitterForTestPinsNewBreakers(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, OpenMax: time.Minute})
	reg.SetJitterForTest(identityJitter)

	var mu sync.Mutex
	var got []Transition
	reg.SetOnStateChange(func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tr)
	})

	// Created AFTER the pin: the registry must seed it.
	reg.Breaker("/city", OpClassBd).RecordFailure()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d transitions, want 1", len(got))
	}
	if got[0].Backoff != time.Second {
		t.Fatalf("open backoff = %v, want exactly %v — a pinned jitter must not draw randomly", got[0].Backoff, time.Second)
	}
}

func TestRegistrySetJitterForTestPinsExistingBreakers(t *testing.T) {
	reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: 2 * time.Second, OpenMax: time.Minute})
	// Created BEFORE the pin: SetJitterForTest must reach it too, the same way
	// SetOnStateChange does.
	b := reg.Breaker("/city", OpClassBd)
	reg.SetJitterForTest(identityJitter)

	var mu sync.Mutex
	var got []Transition
	reg.SetOnStateChange(func(tr Transition) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, tr)
	})
	b.RecordFailure()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d transitions, want 1", len(got))
	}
	if got[0].Backoff != 2*time.Second {
		t.Fatalf("open backoff = %v, want exactly %v", got[0].Backoff, 2*time.Second)
	}
}

// TestRegistryPinnedJitterMakesBackoffRepeatable is the reason the seam
// exists: without it, two registries with identical settings choose different
// open deadlines, so any test that asserts on a backoff deadline is
// time-dependent. It also pins the exponent, so a caller can assert on the
// whole backoff ladder rather than only its cap.
func TestRegistryPinnedJitterMakesBackoffRepeatable(t *testing.T) {
	backoffs := func() []time.Duration {
		reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, OpenMax: 8 * time.Second})
		reg.SetJitterForTest(identityJitter)
		var mu sync.Mutex
		var out []time.Duration
		reg.SetOnStateChange(func(tr Transition) {
			mu.Lock()
			defer mu.Unlock()
			if tr.To == StateOpen {
				out = append(out, tr.Backoff)
			}
		})
		b := reg.Breaker("/city", OpClassBd)
		for i := 0; i < 4; i++ {
			b.RecordFailure()
			// Force the next failure to re-open from half-open so trips climbs.
			b.mu.Lock()
			b.state = StateHalfOpen
			b.mu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), out...)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for run := 0; run < 2; run++ {
		got := backoffs()
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d open transitions %v, want %d", run, len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d: backoff ladder = %v, want %v", run, got, want)
			}
		}
	}
}

// TestRegistryUnpinnedJitterStillDrawsRandomly guards the seam from becoming
// the default: a registry that was never pinned must keep full jitter, or the
// thundering-herd protection the backoff exists for is silently gone.
func TestRegistryUnpinnedJitterStillDrawsRandomly(t *testing.T) {
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 64; i++ {
		reg := NewRegistry(Settings{Enabled: true, ConsecutiveFailures: 1, OpenBase: time.Second, OpenMax: time.Minute})
		var mu sync.Mutex
		var backoff time.Duration
		reg.SetOnStateChange(func(tr Transition) {
			mu.Lock()
			defer mu.Unlock()
			backoff = tr.Backoff
		})
		reg.Breaker("/city", OpClassBd).RecordFailure()
		mu.Lock()
		seen[backoff] = struct{}{}
		mu.Unlock()
	}
	if len(seen) < 2 {
		t.Fatalf("64 unpinned registries produced %d distinct backoffs %v, want a spread — full jitter is gone", len(seen), seen)
	}
}
