package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// sharedOutcome is one ObserveShared answer.
type sharedOutcome struct {
	value int
	err   error
}

func observeSharedAsync(ctx context.Context, key string, observe func(context.Context) (int, error)) <-chan sharedOutcome {
	done := make(chan sharedOutcome, 1)
	go func() {
		v, err := ObserveShared(ctx, key, observe)
		done <- sharedOutcome{v, err}
	}()
	return done
}

// Kills a second concurrent reader failing as unavailable, or starting a
// second observation: both get the one answer.
func TestObserveSharedConcurrentCallersShareTheAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		ctx, cancel := context.WithTimeout(context.Background(), observeBound)
		defer cancel()
		first := observeSharedAsync(ctx, t.Name(), wedge.observe)
		synctest.Wait()
		second := observeSharedAsync(ctx, t.Name(), wedge.observe)
		synctest.Wait()
		wedge.unblock()
		for _, ch := range []<-chan sharedOutcome{first, second} {
			if got := <-ch; got.err != nil || got.value != wedgedAnswer {
				t.Fatalf("ObserveShared = %+v, want the shared answer", got)
			}
		}
		if n := wedge.started.Load(); n != 1 {
			t.Fatalf("observations started = %d, want 1", n)
		}
	})
}

// Kills a joiner waiting past its own bound, or a wedged key spawning a
// goroutine per caller.
func TestObserveSharedJoinerIsBoundedByItsOwnContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
		defer cancelLong()
		leader := observeSharedAsync(long, t.Name(), wedge.observe)
		synctest.Wait()
		short, cancelShort := context.WithTimeout(context.Background(), observeBound)
		defer cancelShort()
		start := time.Now()
		got := <-observeSharedAsync(short, t.Name(), wedge.observe)
		if !errors.Is(got.err, runtime.ErrRuntimeUnavailable) || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("joiner = %+v, want unavailable at its own deadline", got)
		}
		if elapsed := time.Since(start); elapsed > observeBound {
			t.Fatalf("joiner returned after %v, want within %v", elapsed, observeBound)
		}
		if n := wedge.started.Load(); n != 1 {
			t.Fatalf("observations started = %d, want 1 for a wedged key", n)
		}
		wedge.unblock()
		<-leader
	})
}

// Kills a joiner handed the starter's cancellation as its own answer: it
// observes afresh while its own context allows.
func TestObserveSharedJoinerRetriesWhenTheStarterGaveUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		observe := func(ctx context.Context) (int, error) {
			if calls.Add(1) == 1 {
				<-ctx.Done() // honors its context, as a socket read does
				return 0, ctx.Err()
			}
			return 7, nil
		}
		starterCtx, cancelStarter := context.WithCancel(context.Background())
		starter := observeSharedAsync(starterCtx, t.Name(), observe)
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), observeBound)
		defer cancel()
		joiner := observeSharedAsync(ctx, t.Name(), observe)
		synctest.Wait()
		cancelStarter()
		if got := <-starter; !errors.Is(got.err, context.Canceled) {
			t.Fatalf("starter = %+v, want its own cancellation", got)
		}
		if got := <-joiner; got.err != nil || got.value != 7 {
			t.Fatalf("joiner = %+v, want a fresh answer", got)
		}
	})
}

func TestObserveSharedSequentialCallsObserveAfresh(t *testing.T) {
	var calls atomic.Int64
	for i := range 3 {
		got, err := ObserveShared(context.Background(), t.Name(), func(context.Context) (int, error) { return int(calls.Add(1)), nil })
		if err != nil || got != i+1 {
			t.Fatalf("call %d = %d, %v; want a fresh answer", i, got, err)
		}
	}
}

func TestObserveSharedPanicReachesTheStarterOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		observe := func(context.Context) (int, error) {
			<-release
			panic("boom")
		}
		ctx, cancel := context.WithTimeout(context.Background(), observeBound)
		defer cancel()
		recovered := make(chan any, 1)
		go func() {
			defer func() { recovered <- recover() }()
			_, _ = ObserveShared(ctx, t.Name(), observe) //nolint:errcheck // panics
		}()
		synctest.Wait()
		joiner := observeSharedAsync(ctx, t.Name(), observe)
		synctest.Wait()
		close(release)
		if r := <-recovered; r != "boom" {
			t.Fatalf("starter recovered %v, want the panic", r)
		}
		if got := <-joiner; !errors.Is(got.err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("joiner = %+v, want unavailable", got)
		}
	})
}

// Kills a joiner of a key observed with another type panicking in its own
// frame: it gets an unavailable error, and the starter its answer.
func TestObserveSharedJoinerOfAnotherTypeIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wedge := newWedgedObservation()
		defer wedge.unblock()
		ctx, cancel := context.WithTimeout(context.Background(), observeBound)
		defer cancel()
		starter := observeSharedAsync(ctx, t.Name(), wedge.observe)
		synctest.Wait()
		joiner := make(chan error, 1)
		go func() {
			_, err := ObserveShared(ctx, t.Name(), func(context.Context) (string, error) { return "other", nil })
			joiner <- err
		}()
		synctest.Wait()
		wedge.unblock()
		if err := <-joiner; !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("joiner of another type = %v, want unavailable", err)
		}
		if got := <-starter; got.err != nil || got.value != wedgedAnswer {
			t.Fatalf("starter = %+v, want its answer", got)
		}
	})
}

func TestObserveSharedRejectsBlankKey(t *testing.T) {
	_, err := ObserveShared(context.Background(), " ", func(context.Context) (int, error) { return 1, nil })
	if err == nil || errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("blank key = %v, want a caller-bug error that is not unavailable", err)
	}
}
