package runtime

import (
	"context"
	"errors"
	stdruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The bound these tests give a call, and a watchdog far past it so a wedged
// call fails the test with a message instead of deadlocking the bubble.
const (
	callBound    = 10 * time.Second
	callWatchdog = 5 * time.Minute
)

// wedgedCall blocks until released, as a hung provider call would.
type wedgedCall struct {
	release chan struct{}
}

func (w *wedgedCall) call(context.Context) (string, error) {
	<-w.release
	return "late", nil
}

// withCallWatchdog runs f on its own goroutine and fails the test if it has
// not returned within callWatchdog of fake time.
func withCallWatchdog(t *testing.T, what string, f func() (string, error)) (string, error) {
	t.Helper()
	done := make(chan struct {
		v   string
		err error
	}, 1)
	go func() {
		v, err := f()
		done <- struct {
			v   string
			err error
		}{v, err}
	}()
	select {
	case res := <-done:
		return res.v, res.err
	case <-time.After(callWatchdog):
		t.Fatalf("%s still blocked %v after its bound: the call is unbounded", what, callWatchdog)
		return "", nil
	}
}

// TestCallBoundedReturnsTheAnswerThatBeatsItsBound: an answer that arrives
// within the bound is honored.
func TestCallBoundedReturnsTheAnswerThatBeatsItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), callBound)
		defer cancel()
		v, err := withCallWatchdog(t, "CallBounded", func() (string, error) {
			return CallBounded(ctx, "k", func(context.Context) (string, error) {
				<-time.After(callBound - 2*time.Second)
				return "answer", nil
			})
		})
		if err != nil || v != "answer" {
			t.Fatalf("CallBounded = (%q, %v), want (\"answer\", nil)", v, err)
		}
	})
}

// TestCallBoundedAbandonsAtItsBoundAndFreesTheKey: a call that outlives its
// bound is abandoned with an error wrapping both ErrRuntimeUnavailable and the
// context error, and its key frees once the abandoned call returns, so the
// next call under that key starts fresh.
func TestCallBoundedAbandonsAtItsBoundAndFreesTheKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := &wedgedCall{release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(wedge.release) })
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), callBound)
		defer cancel()
		begin := time.Now()

		_, err := withCallWatchdog(t, "CallBounded", func() (string, error) {
			return CallBounded(ctx, "k", wedge.call)
		})

		if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and DeadlineExceeded", err)
		}
		if elapsed := time.Since(begin); elapsed < callBound || elapsed > callBound+5*time.Second {
			t.Fatalf("returned after %v, want at the %v bound", elapsed, callBound)
		}
		release()
		synctest.Wait() // the abandoned call delivers into its buffer and exits
		v, err := withCallWatchdog(t, "CallBounded after release", func() (string, error) {
			return CallBounded(context.Background(), "k", func(context.Context) (string, error) {
				return "fresh", nil
			})
		})
		if err != nil || v != "fresh" {
			t.Fatalf("CallBounded after release = (%q, %v), want a fresh call to run", v, err)
		}
	})
}

// TestCallBoundedRefusesAnOutstandingKeyAtOnce: while a call is outstanding,
// a new call under the same key is refused at once with
// ErrBoundedCallOutstanding (wrapping ErrRuntimeUnavailable) instead of
// starting a second one, and a different key still runs.
func TestCallBoundedRefusesAnOutstandingKeyAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := &wedgedCall{release: make(chan struct{})}
		defer close(wedge.release)
		var started atomic.Int64
		wedged := func(ctx context.Context) (string, error) {
			started.Add(1)
			return wedge.call(ctx)
		}
		firstCtx, cancelFirst := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFirst()
		first := make(chan error, 1)
		go func() {
			_, err := CallBounded(firstCtx, "k", wedged)
			first <- err
		}()
		<-time.After(time.Second) // the first call is under way

		begin := time.Now()
		_, err := CallBounded(context.Background(), "k", wedged)
		if !errors.Is(err, ErrBoundedCallOutstanding) || !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want ErrBoundedCallOutstanding wrapping ErrRuntimeUnavailable", err)
		}
		if elapsed := time.Since(begin); elapsed >= time.Second {
			t.Fatalf("refusal waited %v, want an immediate refusal", elapsed)
		}
		v, err := CallBounded(context.Background(), "other", func(context.Context) (string, error) {
			return "elsewhere", nil
		})
		if err != nil || v != "elsewhere" {
			t.Fatalf("CallBounded under another key = (%q, %v), want it to run", v, err)
		}
		if n := started.Load(); n != 1 {
			t.Fatalf("calls started = %d, want 1: an outstanding key starts no second call", n)
		}
		<-first // the wedged call's own caller still ends at its own bound
	})
}

// TestCallBoundedBlankKeyIsACallerBug: a blank key is rejected with an error
// that does not wrap ErrRuntimeUnavailable, since it names no observation.
func TestCallBoundedBlankKeyIsACallerBug(t *testing.T) {
	_, err := CallBounded(context.Background(), "  ", func(context.Context) (string, error) {
		t.Fatal("a blank key must start nothing")
		return "", nil
	})
	if err == nil {
		t.Fatal("CallBounded(blank key) = nil, want an error")
	}
	if errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want a caller bug, not an unavailable answer", err)
	}
}

// TestCallBoundedExpiredContextStartsNothing: an already-ended context starts
// no call at all.
func TestCallBoundedExpiredContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var started atomic.Int64
	_, err := CallBounded(ctx, "k", func(context.Context) (string, error) {
		started.Add(1)
		return "never", nil
	})
	if !errors.Is(err, ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap ErrRuntimeUnavailable and Canceled", err)
	}
	if n := started.Load(); n != 0 {
		t.Fatalf("calls started = %d, want 0", n)
	}
}

// TestCallBoundedReraisesAPanicOnItsCaller: a panic in the call is re-raised
// on the original caller's goroutine, logged, and the key still frees.
func TestCallBoundedReraisesAPanicOnItsCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		panicked, panicVal := false, any(nil)
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked, panicVal = true, r
				}
			}()
			_, _ = CallBounded(context.Background(), "k", func(context.Context) (string, error) {
				panic("boom")
			})
		}()
		if !panicked || panicVal != "boom" {
			t.Fatalf("panicked = %v (%v), want the call's own panic on the caller", panicked, panicVal)
		}
		synctest.Wait() // the panicking goroutine records and releases the key
		v, err := CallBounded(context.Background(), "k", func(context.Context) (string, error) {
			return "after", nil
		})
		if err != nil || v != "after" {
			t.Fatalf("CallBounded after a panic = (%q, %v), want the key freed", v, err)
		}
	})
}

// TestCallBoundedGoexitEndsWithoutAnAnswer: a call whose goroutine ends
// without returning (runtime.Goexit) is an unanswered call, and its key
// frees.
func TestCallBoundedGoexitEndsWithoutAnAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, err := withCallWatchdog(t, "CallBounded", func() (string, error) {
			return CallBounded(context.Background(), "k", func(context.Context) (string, error) {
				stdruntime.Goexit()
				return "unreachable", nil
			})
		})
		if !errors.Is(err, ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want an answer-less error wrapping ErrRuntimeUnavailable", err)
		}
		synctest.Wait() // the exiting goroutine releases the key
		v, err := CallBounded(context.Background(), "k", func(context.Context) (string, error) {
			return "after", nil
		})
		if err != nil || v != "after" {
			t.Fatalf("CallBounded after Goexit = (%q, %v), want the key freed", v, err)
		}
	})
}

// TestCallSharedJoinsOneCallPerKey: concurrent calls under one key all wait
// for the one observation and get its answer.
func TestCallSharedJoinsOneCallPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var started atomic.Int64
		slow := func(ctx context.Context) (string, error) {
			started.Add(1)
			<-time.After(5 * time.Second)
			return "shared", ctx.Err()
		}
		const joiners = 3
		out := make(chan string, joiners+1)
		for i := 0; i < joiners+1; i++ {
			go func() {
				v, err := CallShared(context.Background(), "k", slow)
				if err != nil {
					t.Errorf("CallShared = %v, want the shared answer", err)
				}
				out <- v
			}()
		}
		for i := 0; i < joiners+1; i++ {
			select {
			case v := <-out:
				if v != "shared" {
					t.Fatalf("answer = %q, want \"shared\"", v)
				}
			case <-time.After(callWatchdog):
				t.Fatal("CallShared joiners still waiting: the shared flight is unbounded")
			}
		}
		if n := started.Load(); n != 1 {
			t.Fatalf("observations started = %d, want 1", n)
		}
	})
}

// TestCallSharedRetriesWhenTheStartersDeadlineEndedTheFlight: a joiner whose
// flight failed only because the call that started it ran out of context
// starts a fresh observation under its own context instead of taking that
// deadline as its own.
func TestCallSharedRetriesWhenTheStartersDeadlineEndedTheFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var flights atomic.Int64
		observe := func(ctx context.Context) (string, error) {
			flights.Add(1)
			select {
			case <-time.After(5 * time.Second):
				return "value", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		starterCtx, cancelStarter := context.WithTimeout(context.Background(), time.Second)
		defer cancelStarter()
		starter := make(chan error, 1)
		go func() {
			_, err := CallShared(starterCtx, "k", observe)
			starter <- err
		}()
		<-time.After(2 * time.Second) // the starter's flight has ended on its deadline

		v, err := withCallWatchdog(t, "CallShared joiner", func() (string, error) {
			return CallShared(context.Background(), "k", observe)
		})
		if err != nil || v != "value" {
			t.Fatalf("CallShared joiner = (%q, %v), want a fresh flight's answer", v, err)
		}
		if ferr := <-starter; !errors.Is(ferr, context.Canceled) && !errors.Is(ferr, context.DeadlineExceeded) {
			t.Fatalf("starter err = %v, want the starter's own deadline error", ferr)
		}
		if n := flights.Load(); n != 2 {
			t.Fatalf("flights = %d, want 2: the joiner retried after the starter's deadline", n)
		}
	})
}
