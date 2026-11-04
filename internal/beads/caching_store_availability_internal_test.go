package beads

import (
	"context"
	"sync"
	"testing"
)

// fakeAvailabilityGate is a controllable AvailabilityGate for tests.
type fakeAvailabilityGate struct {
	mu        sync.Mutex
	available bool
	probeDue  bool
}

func (g *fakeAvailabilityGate) Available() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.available
}

func (g *fakeAvailabilityGate) ProbeDue() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.probeDue
}

func (g *fakeAvailabilityGate) set(available, probeDue bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.available = available
	g.probeDue = probeDue
}

func newPrimedGatedCache(t *testing.T, beadsIn ...Bead) (*CachingStore, *fakeAvailabilityGate) {
	t.Helper()
	backing := NewMemStore()
	for _, b := range beadsIn {
		if _, err := backing.Create(b); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	gate := &fakeAvailabilityGate{available: true}
	cache.SetAvailabilityGate(gate)
	return cache, gate
}

func TestCachingStoreDegradedFollowsTheAvailabilityGate(t *testing.T) {
	t.Parallel()
	cache, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})

	if cache.Degraded() {
		t.Fatal("Degraded() = true with an available gate and a healthy cache, want false")
	}
	gate.set(false, false)
	if !cache.Degraded() {
		t.Fatal("Degraded() = false while the gate reports the store unreachable, want true")
	}
	gate.set(true, false)
	if cache.Degraded() {
		t.Fatal("Degraded() = true after the gate recovered, want false")
	}
}

func TestCachingStoreDegradedWithoutAGate(t *testing.T) {
	t.Parallel()
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if cache.availabilityGateRef() != nil {
		t.Fatal("availabilityGateRef() non-nil on a store that was never gated")
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true without a gate and with a healthy cache, want false")
	}
}

// TestCachingStoreDegradedReportsCacheStateWithoutAGate pins the half of
// Degraded() that needs no gate at all: a cache the reconciler has pushed into
// cacheDegraded is degraded whether or not a transport gate is wired.
func TestCachingStoreDegradedReportsCacheStateWithoutAGate(t *testing.T) {
	t.Parallel()
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()
	if !cache.Degraded() {
		t.Fatal("Degraded() = false in cacheDegraded with no gate wired, want true")
	}
}

func TestCachingStoreSetAvailabilityGateNilClearsTheGate(t *testing.T) {
	t.Parallel()
	cache, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	if !cache.Degraded() {
		t.Fatal("Degraded() = false with an unavailable gate, want true")
	}
	cache.SetAvailabilityGate(nil)
	if cache.availabilityGateRef() != nil {
		t.Fatal("availabilityGateRef() non-nil after SetAvailabilityGate(nil)")
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true after the gate was cleared, want false — a cleared gate must not latch")
	}
}

// TestCachingStoreAvailabilityGateReplaceable proves the seam is a live
// pointer swap, not a one-shot: rewiring a scope's store to a new gate
// (controller reload) must take effect for the next read.
func TestCachingStoreAvailabilityGateReplaceable(t *testing.T) {
	t.Parallel()
	cache, first := newPrimedGatedCache(t, Bead{Title: "task-1"})
	first.set(false, false)
	second := &fakeAvailabilityGate{available: true}
	cache.SetAvailabilityGate(second)
	if cache.Degraded() {
		t.Fatal("Degraded() = true after rewiring to an available gate; the store is still consulting the old gate")
	}
	second.set(false, false)
	if !cache.Degraded() {
		t.Fatal("Degraded() = false after the replacement gate went unavailable")
	}
}

// TestCachingStoreDegradedIgnoresProbeDue pins that the degraded verdict is a
// function of Available() alone. ProbeDue() answers a different question — may
// a recovery attempt run now — and a store that reported itself healthy for
// the length of a probe window would hide the outage from every status
// surface.
func TestCachingStoreDegradedIgnoresProbeDue(t *testing.T) {
	t.Parallel()
	cache, gate := newPrimedGatedCache(t, Bead{Title: "task-1"})
	for _, probeDue := range []bool{false, true} {
		gate.set(false, probeDue)
		if !cache.Degraded() {
			t.Fatalf("Degraded() = false with an unavailable gate (probeDue=%v), want true", probeDue)
		}
		gate.set(true, probeDue)
		if cache.Degraded() {
			t.Fatalf("Degraded() = true with an available gate (probeDue=%v), want false", probeDue)
		}
	}
}

// TestAvailabilityGateShape keeps the interface from drifting away from the
// transport breaker it exists to accept. internal/beads must not import
// internal/resilience (layering), so the shape is restated here rather than
// asserted against the concrete type.
func TestAvailabilityGateShape(t *testing.T) {
	t.Parallel()
	var g AvailabilityGate = &fakeAvailabilityGate{available: true, probeDue: true}
	if !g.Available() || !g.ProbeDue() {
		t.Fatal("AvailabilityGate must expose both Available() and ProbeDue()")
	}
}
