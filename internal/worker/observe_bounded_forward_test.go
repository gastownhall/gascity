package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ObserveBounded and ObserveShared forward to runtime.CallBounded and
// runtime.CallShared (internal/runtime/bounded_call.go); observe_bounded_test.go
// holds the behavioral suite through the forwards. These tests pin the
// forwarding itself: the worker names are aliases of the runtime sentinels,
// and both APIs share one in-flight table per key, so a caller in one
// namespace (the pre-start orphan scan) and a worker observation of the same
// key hold one slot between them.

func TestObserveBoundedForwardsToTheRuntimeBoundedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()

		if _, err := observeWithinBound(t, t.Name(), wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("ObserveBounded = %v, want it to wrap runtime.ErrRuntimeUnavailable at the bound", err)
		}
		if got := wedge.started.Load(); got != 1 {
			t.Fatalf("observations started = %d, want 1", got)
		}
	})
}

func TestObserveBoundedAndRuntimeCallBoundedShareOneLatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		key := t.Name()

		if _, err := observeWithinBound(t, key, wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first observation: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}

		var spawned atomic.Int64
		_, err := runtime.CallBounded(context.Background(), key, func(context.Context) (int, error) {
			spawned.Add(1)
			return 2, nil
		})

		if got := spawned.Load(); got != 0 {
			t.Fatalf("a runtime call under the key the wedged observation holds started %d calls, want 0: the in-flight latch is shared", got)
		}
		if !errors.Is(err, runtime.ErrBoundedCallOutstanding) || !errors.Is(err, ErrObservationOutstanding) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want the one shared outstanding sentinel of runtime.ErrBoundedCallOutstanding, ErrObservationOutstanding, and runtime.ErrRuntimeUnavailable", err)
		}
	})
}
