package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// fanOutProbe counts how many boundedFanOut calls are running at once. Every
// call parks on release until the test opens it, so inside a synctest bubble
// synctest.Wait returns only once every goroutine is either parked here or
// waiting for a slot: the running count at that moment is exactly the fan-out
// the helper allowed, with no timing involved.
type fanOutProbe struct {
	release chan struct{}
	once    sync.Once
	running atomic.Int64
	peak    atomic.Int64
	mu      sync.Mutex
	visited []int
}

func newFanOutProbe() *fanOutProbe {
	return &fanOutProbe{release: make(chan struct{})}
}

func (p *fanOutProbe) call(i int) {
	n := p.running.Add(1)
	defer p.running.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	<-p.release
	p.mu.Lock()
	p.visited = append(p.visited, i)
	p.mu.Unlock()
}

// open lets every parked call return. It is idempotent so a test can both
// call it and defer it: the deferred call frees parked goroutines when a
// failed assertion ends the test early, which would otherwise deadlock the
// bubble.
func (p *fanOutProbe) open() { p.once.Do(func() { close(p.release) }) }

func (p *fanOutProbe) sortedVisits() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.visited)
	slices.Sort(out)
	return out
}

func indexRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestBoundedFanOutHoldsExactlyTheLimitInFlight is the bound itself: with more
// calls than the limit, exactly limit run at once and the rest wait for a slot,
// and every call still runs exactly once.
func TestBoundedFanOutHoldsExactlyTheLimitInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, limit = 7, 3
		probe := newFanOutProbe()
		defer probe.open()

		done := make(chan struct{})
		go func() {
			defer close(done)
			boundedFanOut(n, limit, probe.call)
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != limit {
			t.Fatalf("%d calls in flight with %d queued, want exactly the limit %d", got, n, limit)
		}

		probe.open()
		<-done
		if got := probe.peak.Load(); got != limit {
			t.Fatalf("peak concurrency = %d, want the limit %d", got, limit)
		}
		if got := probe.sortedVisits(); !slices.Equal(got, indexRange(n)) {
			t.Fatalf("visited %v, want every index in [0,%d) exactly once", got, n)
		}
	})
}

// TestBoundedFanOutRunsEveryCallAtOnceUnderTheLimit pins the other side of the
// bound: fewer calls than the limit all run together, so a caller pays for the
// slowest call rather than the sum of them.
func TestBoundedFanOutRunsEveryCallAtOnceUnderTheLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, limit = 4, 8
		probe := newFanOutProbe()
		defer probe.open()

		done := make(chan struct{})
		go func() {
			defer close(done)
			boundedFanOut(n, limit, probe.call)
		}()
		synctest.Wait()
		if got := probe.running.Load(); got != n {
			t.Fatalf("%d of %d calls in flight under a limit of %d, want all of them", got, n, limit)
		}
		probe.open()
		<-done
	})
}

// TestBoundedFanOutWithALimitOfOneRunsOnTheCallersGoroutine pins that a
// sequential fan-out is the plain loop it replaces: calls run in index order,
// one at a time, on the caller's goroutine, so a panic reaches the caller's own
// recover with its original value — the contract the controller's
// sequential rig-store opens keep under CityRuntime.safeTick.
func TestBoundedFanOutWithALimitOfOneRunsOnTheCallersGoroutine(t *testing.T) {
	var order []int
	boundedFanOut(4, 1, func(i int) { order = append(order, i) })
	if !slices.Equal(order, indexRange(4)) {
		t.Fatalf("sequential fan-out ran %v, want index order", order)
	}

	type sentinel struct{ msg string }
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		boundedFanOut(3, 1, func(i int) {
			if i == 1 {
				panic(sentinel{msg: "inline"})
			}
		})
	}()
	if got, ok := recovered.(sentinel); !ok || got.msg != "inline" {
		t.Fatalf("recovered %#v, want the call's own panic value unwrapped: a limit of 1 must not move calls onto another goroutine", recovered)
	}
}

// TestBoundedFanOutReraisesAWorkerPanicOnTheCaller pins that a panic on a
// worker goroutine is not a process crash: every other call still finishes,
// and the panic is raised again on the caller's goroutine, carrying the
// original value and the worker's stack.
func TestBoundedFanOutReraisesAWorkerPanicOnTheCaller(t *testing.T) {
	var ran atomic.Int64
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		boundedFanOut(6, 3, func(i int) {
			ran.Add(1)
			if i == 2 {
				panic("leg exploded")
			}
		})
	}()
	if recovered == nil {
		t.Fatal("a worker panic was swallowed; it must reach the caller")
	}
	if text := fmt.Sprint(recovered); !strings.Contains(text, "leg exploded") || !strings.Contains(text, "goroutine") {
		t.Fatalf("recovered %q, want the worker's panic value and its stack", text)
	}
	if got := ran.Load(); got != 6 {
		t.Fatalf("%d of 6 calls ran before the panic was re-raised, want all of them", got)
	}
}

// TestBoundedFanOutWithNothingToDoReturns pins the empty edge.
func TestBoundedFanOutWithNothingToDoReturns(t *testing.T) {
	boundedFanOut(0, 4, func(int) { t.Fatal("called with n == 0") })
}
