package fanout

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/fanout/fanouttest"
)

func indexRange(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestBoundedHoldsExactlyTheLimitInFlight is the bound itself: with more
// calls than the limit, exactly limit run at once and the rest wait for a slot,
// and every call still runs exactly once.
func TestBoundedHoldsExactlyTheLimitInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, limit = 7, 3
		probe := fanouttest.NewProbe()
		defer probe.Open()

		done := make(chan struct{})
		go func() {
			defer close(done)
			Bounded(n, limit, probe.Call)
		}()
		synctest.Wait()
		if got := probe.Running(); got != limit {
			t.Fatalf("%d calls in flight with %d queued, want exactly the limit %d", got, n, limit)
		}

		probe.Open()
		<-done
		if got := probe.Peak(); got != limit {
			t.Fatalf("peak concurrency = %d, want the limit %d", got, limit)
		}
		if got := probe.SortedVisits(); !slices.Equal(got, indexRange(n)) {
			t.Fatalf("visited %v, want every index in [0,%d) exactly once", got, n)
		}
	})
}

// TestBoundedRunsEveryCallAtOnceUnderTheLimit pins the other side of the
// bound: fewer calls than the limit all run together, so a caller pays for the
// slowest call rather than the sum of them.
func TestBoundedRunsEveryCallAtOnceUnderTheLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n, limit = 4, 8
		probe := fanouttest.NewProbe()
		defer probe.Open()

		done := make(chan struct{})
		go func() {
			defer close(done)
			Bounded(n, limit, probe.Call)
		}()
		synctest.Wait()
		if got := probe.Running(); got != n {
			t.Fatalf("%d of %d calls in flight under a limit of %d, want all of them", got, n, limit)
		}
		probe.Open()
		<-done
	})
}

// TestBoundedWithALimitOfOneRunsOnTheCallersGoroutine pins that a
// sequential fan-out is the plain loop it replaces: calls run in index order,
// one at a time, on the caller's goroutine, so a panic reaches the caller's own
// recover with its original value — the contract the controller's
// sequential rig-store opens keep under CityRuntime.safeTick.
func TestBoundedWithALimitOfOneRunsOnTheCallersGoroutine(t *testing.T) {
	var order []int
	Bounded(4, 1, func(i int) { order = append(order, i) })
	if !slices.Equal(order, indexRange(4)) {
		t.Fatalf("sequential fan-out ran %v, want index order", order)
	}

	type sentinel struct{ msg string }
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		Bounded(3, 1, func(i int) {
			if i == 1 {
				panic(sentinel{msg: "inline"})
			}
		})
	}()
	if got, ok := recovered.(sentinel); !ok || got.msg != "inline" {
		t.Fatalf("recovered %#v, want the call's own panic value unwrapped: a limit of 1 must not move calls onto another goroutine", recovered)
	}
}

// TestBoundedReraisesAWorkerPanicOnTheCaller pins that a panic on a
// worker goroutine is not a process crash: every other call still finishes,
// and the panic is raised again on the caller's goroutine, carrying the
// original value and the worker's stack.
func TestBoundedReraisesAWorkerPanicOnTheCaller(t *testing.T) {
	var ran atomic.Int64
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		Bounded(6, 3, func(i int) {
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

// TestBoundedWithNothingToDoReturns pins the empty edge.
func TestBoundedWithNothingToDoReturns(t *testing.T) {
	Bounded(0, 4, func(int) { t.Fatal("called with n == 0") })
}
