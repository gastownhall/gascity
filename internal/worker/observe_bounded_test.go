package worker

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// observeBound is the bound the start path hands ObserveBounded. These tests
	// run on synctest's fake clock, so it costs no wall time.
	observeBound = 10 * time.Second
	// observeWatchdog is far past observeBound. It exists only so that an
	// unbounded wait fails the test with a message instead of deadlocking the
	// synctest bubble.
	observeWatchdog = 5 * time.Minute
)

// wedgedAnswer is what a wedged observation finally returns once it is
// unblocked: the late answer an abandoned call delivers after its caller left.
const wedgedAnswer = 1

// wedgedObservation is an observe function that behaves like a hung provider
// subprocess: it ignores its context and does not return until unblock.
type wedgedObservation struct {
	release chan struct{}
	once    sync.Once
	// started counts calls begun, running the calls begun and not yet returned,
	// peak the most calls ever running at once.
	started atomic.Int64
	running atomic.Int64
	peak    atomic.Int64
}

func newWedgedObservation() *wedgedObservation {
	return &wedgedObservation{release: make(chan struct{})}
}

func (w *wedgedObservation) observe(context.Context) (int, error) {
	w.started.Add(1)
	n := w.running.Add(1)
	defer w.running.Add(-1)
	for {
		p := w.peak.Load()
		if n <= p || w.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-w.release
	return wedgedAnswer, nil
}

// unblock lets every call return. It is idempotent so tests can both call it
// and defer it: the deferred call frees a wedged goroutine when a failed
// assertion ends the test early, which would otherwise deadlock the bubble.
func (w *wedgedObservation) unblock() {
	w.once.Do(func() { close(w.release) })
}

// observeWithWatchdog runs ObserveBounded on its own goroutine and fails the
// test if it has not returned within observeWatchdog of fake time.
func observeWithWatchdog[T any](ctx context.Context, t *testing.T, key string, observe func(context.Context) (T, error)) (T, error) {
	t.Helper()
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := ObserveBounded(ctx, key, observe)
		done <- outcome{value: value, err: err}
	}()
	select {
	case got := <-done:
		return got.value, got.err
	case <-time.After(observeWatchdog):
		t.Fatalf("ObserveBounded(%q) had not returned %v after a %v bound: the wait is unbounded", key, observeWatchdog, observeBound)
		var zero T
		return zero, nil
	}
}

// observeWithinBound is observeWithWatchdog under a fresh observeBound context.
func observeWithinBound[T any](t *testing.T, key string, observe func(context.Context) (T, error)) (T, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), observeBound)
	defer cancel()
	return observeWithWatchdog(ctx, t, key, observe)
}

func TestObserveBoundedReturnsPromptAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		providerErr := errors.New("provider said no")

		got, err := observeWithinBound(t, t.Name(), func(context.Context) (int, error) { return 42, providerErr })

		if got != 42 || !errors.Is(err, providerErr) {
			t.Fatalf("ObserveBounded = (%d, %v), want (42, %v)", got, err, providerErr)
		}
		if errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("a provider error that arrived in time was relabeled as unavailable: %v", err)
		}
	})
}

func TestObserveBoundedWedgedObservationReturnsUnavailableAtTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		start := time.Now()

		_, err := observeWithinBound(t, t.Name(), wedge.observe)

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > observeBound {
			t.Fatalf("returned after %v, want within %v", elapsed, observeBound)
		}
	})
}

func TestObserveBoundedAnswerJustBeforeTheBoundIsUsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got, err := observeWithinBound(t, t.Name(), func(context.Context) (int, error) {
			<-time.After(observeBound - time.Millisecond)
			return 7, nil
		})

		if err != nil || got != 7 {
			t.Fatalf("ObserveBounded = (%d, %v), want the answer (7, nil) that beat the bound", got, err)
		}
	})
}

// endingContext ends the instant ObserveBounded first looks at its Done
// channel, after letting the observation finish. The answer and the end of the
// wait are then both ready when ObserveBounded chooses between them, which a
// real deadline can only arrange by luck.
type endingContext struct {
	context.Context
	finish func()
	ended  chan struct{}
	once   sync.Once
}

func (c *endingContext) Done() <-chan struct{} {
	c.once.Do(func() {
		c.finish()
		close(c.ended)
	})
	return c.ended
}

func (c *endingContext) Err() error {
	select {
	case <-c.ended:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// An answer that is already in hand when the wait ends must be used. Go picks
// at random between two ready select cases, so one call could pass by luck:
// many calls make a dropped answer all but certain to show.
func TestObserveBoundedAnswerReadyWhenTheContextEndsIsUsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for i := 0; i < 64; i++ {
			release := make(chan struct{})
			ctx := &endingContext{
				Context: context.Background(),
				ended:   make(chan struct{}),
				finish: func() {
					close(release)
					synctest.Wait() // the observation has answered and exited
				},
			}

			got, err := ObserveBounded(ctx, fmt.Sprintf("%s/%d", t.Name(), i), func(context.Context) (int, error) {
				<-release
				return i, nil
			})

			if err != nil || got != i {
				t.Fatalf("attempt %d: ObserveBounded = (%d, %v), want the answer (%d, nil) that was ready as the context ended", i, got, err, i)
			}
		}
	})
}

func TestObserveBoundedAlreadyExpiredContextStartsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var calls atomic.Int64

		_, err := observeWithWatchdog(ctx, t, t.Name(), func(context.Context) (int, error) {
			calls.Add(1)
			return 1, nil
		})
		// An observation that was started anyway runs on its own goroutine, so
		// let every goroutine run before counting.
		synctest.Wait()

		if got := calls.Load(); got != 0 {
			t.Fatalf("observe ran %d times under an already-canceled context, want 0", got)
		}
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable and context.Canceled", err)
		}
	})
}

// A wedged session is retried every reconcile tick. Without a cap each tick
// would abandon one more goroutine; the contract is at most one per key.
func TestObserveBoundedLeavesOneAbandonedObservationPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		key := t.Name()
		const attempts = 100
		start := time.Now()

		for i := 0; i < attempts; i++ {
			if _, err := observeWithinBound(t, key, wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("attempt %d: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", i, err)
			}
		}

		if got := wedge.started.Load(); got != 1 {
			t.Fatalf("%d observations started over %d attempts, want 1: later attempts must not spawn while one is outstanding", got, attempts)
		}
		if got := wedge.peak.Load(); got > 1 {
			t.Fatalf("%d abandoned observations ran at once, want at most 1", got)
		}
		if elapsed := time.Since(start); elapsed > observeBound {
			t.Fatalf("%d attempts took %v of fake time, want only the first to wait (at most %v)", attempts, elapsed, observeBound)
		}
	})
}

func TestObserveBoundedOutstandingKeyAnswersUnavailableWithoutSpawning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		key := t.Name()
		if _, err := observeWithinBound(t, key, wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first attempt: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}

		var spawned atomic.Int64
		before := time.Now()
		_, err := observeWithinBound(t, key, func(context.Context) (int, error) {
			spawned.Add(1)
			return 2, nil
		})

		if got := spawned.Load(); got != 0 {
			t.Fatalf("a second observation started %d times while the first was outstanding, want 0", got)
		}
		if !errors.Is(err, ErrObservationOutstanding) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want it to wrap ErrObservationOutstanding and runtime.ErrRuntimeUnavailable", err)
		}
		if elapsed := time.Since(before); elapsed != 0 {
			t.Fatalf("outstanding key took %v to answer, want an immediate answer", elapsed)
		}
	})
}

func TestObserveBoundedKeysAreIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		if _, err := observeWithinBound(t, t.Name()+"/wedged", wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("wedged key: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}

		got, err := observeWithinBound(t, t.Name()+"/other", func(context.Context) (int, error) { return 9, nil })

		if err != nil || got != 9 {
			t.Fatalf("other key = (%d, %v), want (9, nil): one wedged session must not block another", got, err)
		}
	})
}

func TestObserveBoundedReservationClearsWhenAbandonedObservationReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		key := t.Name()
		if _, err := observeWithinBound(t, key, wedge.observe); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("first attempt: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}

		// The abandoned observation returns late. Nobody is waiting for it any
		// more: it must neither panic nor deliver its answer anywhere.
		wedge.unblock()
		synctest.Wait()
		if got := wedge.running.Load(); got != 0 {
			t.Fatalf("%d observations still running after the late return", got)
		}

		got, err := observeWithinBound(t, key, func(context.Context) (int, error) { return 5, nil })
		if err != nil || got != 5 {
			t.Fatalf("after the late return, ObserveBounded = (%d, %v), want a fresh observation (5, nil)", got, err)
		}
	})
}

// The reservation must be released before the answer is delivered: a caller
// that observes again the instant it has an answer must not be told that its
// own finished observation is still outstanding.
func TestObserveBoundedSequentialAnswersNeverReportOutstanding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := t.Name()
		for i := 0; i < 200; i++ {
			got, err := observeWithinBound(t, key, func(context.Context) (int, error) { return i, nil })
			if err != nil || got != i {
				t.Fatalf("observation %d = (%d, %v), want (%d, nil)", i, got, err, i)
			}
		}
	})
}

// A panic in an observation must reach the caller: runPreparedStartCandidate
// turns a panic into TraceOutcomePanicRecovered with its own recover, which only
// sees a panic raised on its own goroutine.
func TestObserveBoundedForwardsObservationPanicToCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := t.Name()
		const boom = "provider blew up"

		recovered := func() (r any) {
			defer func() { r = recover() }()
			ctx, cancel := context.WithTimeout(context.Background(), observeBound)
			defer cancel()
			_, _ = ObserveBounded(ctx, key, func(context.Context) (int, error) { panic(boom) })
			return nil
		}()

		if recovered != boom {
			t.Fatalf("recovered %v, want the observation's panic value %v re-raised on the caller's goroutine", recovered, boom)
		}
		got, err := observeWithinBound(t, key, func(context.Context) (int, error) { return 3, nil })
		if err != nil || got != 3 {
			t.Fatalf("after the panic, ObserveBounded = (%d, %v), want the key released and a fresh observation (3, nil)", got, err)
		}
	})
}

// An observation that ends without returning (runtime.Goexit, which t.FailNow
// uses) has no answer to hand back. Reading that as a zero value and no error
// would invent an answer, so it must read as unavailable, and it must release
// its key like any other observation that ends.
func TestObserveBoundedObservationThatEndsWithoutReturningIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		key := t.Name()

		got, err := observeWithinBound(t, key, func(context.Context) (int, error) {
			goruntime.Goexit()
			return wedgedAnswer, nil
		})

		if got != 0 || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("ObserveBounded = (%d, %v), want (0, an error wrapping runtime.ErrRuntimeUnavailable)", got, err)
		}
		again, err := observeWithinBound(t, key, func(context.Context) (int, error) { return 4, nil })
		if err != nil || again != 4 {
			t.Fatalf("after the observation ended, ObserveBounded = (%d, %v), want the key released and a fresh observation (4, nil)", again, err)
		}
	})
}

func TestObserveBoundedRejectsBlankKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, key := range []string{"", " \t"} {
			var calls atomic.Int64

			_, err := observeWithinBound(t, key, func(context.Context) (int, error) {
				calls.Add(1)
				return 1, nil
			})

			if err == nil {
				t.Fatalf("key %q: err = nil, want a rejection: a blank key cannot be bounded per session", key)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("key %q: observe ran %d times, want 0", key, got)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("key %q: err = %v, a blank key is a caller bug, not an unavailable runtime", key, err)
			}
		}
	})
}
