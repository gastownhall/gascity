package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// stubFlockAcquire replaces the flock seam with fn for the test and restores
// it on cleanup. Tests that use it do not run in parallel.
func stubFlockAcquire(t *testing.T, fn func(*os.File, filelock.Options) (*filelock.Lock, error)) {
	t.Helper()
	previous := flockAcquire
	flockAcquire = fn
	t.Cleanup(func() { flockAcquire = previous })
}

// flockTestFile returns an open file the stubbed seam never touches.
func flockTestFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func busyErr() error { return fmt.Errorf("%w: test", filelock.ErrBusy) }

// TestFlockExclusiveTriesFirstEvenWithADoneContext pins the ga-sbvuui
// semantics: the context bounds only the wait, so a free lock still acquires
// when the context is already done.
func TestFlockExclusiveTriesFirstEvenWithADoneContext(t *testing.T) {
	var attempts atomic.Int64
	stubFlockAcquire(t, func(_ *os.File, opts filelock.Options) (*filelock.Lock, error) {
		attempts.Add(1)
		if !opts.NonBlocking || opts.Mode != filelock.Exclusive {
			t.Errorf("attempt options = %+v, want a non-blocking exclusive try", opts)
		}
		return &filelock.Lock{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lock, err := flockExclusive(ctx, flockTestFile(t), "label")
	if err != nil || lock == nil {
		t.Fatalf("flockExclusive(done ctx, free lock) = (%v, %v), want the lock", lock, err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// TestFlockExclusiveReturnsContextErrorAfterOneTryWhenBusyAndDone pins that a
// contended lock with a done context makes exactly the try-first attempt and
// then reports the context's own error, not a sentinel and not ErrBusy.
func TestFlockExclusiveReturnsContextErrorAfterOneTryWhenBusyAndDone(t *testing.T) {
	var attempts atomic.Int64
	stubFlockAcquire(t, func(*os.File, filelock.Options) (*filelock.Lock, error) {
		attempts.Add(1)
		return nil, busyErr()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := flockExclusive(ctx, flockTestFile(t), "label")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrStoreTimeout) {
		t.Fatalf("err = %v wraps ErrStoreTimeout; the callers add the sentinel, not the helper", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want exactly the try-first attempt", got)
	}
}

// TestFlockExclusiveNonCancellableContextTakesTheBlockingCall pins that a
// context with no Done channel keeps today's blocking acquire.
func TestFlockExclusiveNonCancellableContextTakesTheBlockingCall(t *testing.T) {
	var blocking atomic.Int64
	stubFlockAcquire(t, func(_ *os.File, opts filelock.Options) (*filelock.Lock, error) {
		if !opts.NonBlocking {
			blocking.Add(1)
		}
		return &filelock.Lock{}, nil
	})
	if _, err := flockExclusive(context.Background(), flockTestFile(t), "label"); err != nil {
		t.Fatalf("flockExclusive(Background) = %v", err)
	}
	if got := blocking.Load(); got != 1 {
		t.Fatalf("blocking acquires = %d, want 1", got)
	}
}

// TestFlockExclusiveRetriesEINTRAtOnce pins that an interrupted call is
// retried straight away, in both the cancellable and the blocking form.
func TestFlockExclusiveRetriesEINTRAtOnce(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"cancellable":     t.Context(),
		"non-cancellable": context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			var attempts atomic.Int64
			stubFlockAcquire(t, func(*os.File, filelock.Options) (*filelock.Lock, error) {
				if attempts.Add(1) <= 2 {
					return nil, syscall.EINTR
				}
				return &filelock.Lock{}, nil
			})
			lock, err := flockExclusive(ctx, flockTestFile(t), "label")
			if err != nil || lock == nil {
				t.Fatalf("flockExclusive = (%v, %v), want the lock after EINTR retries", lock, err)
			}
			if got := attempts.Load(); got != 3 {
				t.Fatalf("attempts = %d, want 3 (two EINTR, then success)", got)
			}
		})
	}
}

// TestFlockExclusiveOtherErrnoReturnsAtOnce pins that only contention is
// waited out: any other errno comes back wrapped after a single attempt.
func TestFlockExclusiveOtherErrnoReturnsAtOnce(t *testing.T) {
	var attempts atomic.Int64
	stubFlockAcquire(t, func(*os.File, filelock.Options) (*filelock.Lock, error) {
		attempts.Add(1)
		return nil, syscall.EACCES
	})
	_, err := flockExclusive(t.Context(), flockTestFile(t), "label")
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want it to wrap EACCES", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// TestFlockExclusivePollsOnlyOnContention runs the wait under a fake clock at
// the real poll interval: contention is retried once per interval and the
// context ending the wait returns the context's error.
func TestFlockExclusivePollsOnlyOnContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const busyTries = 3
		var attempts atomic.Int64
		stubFlockAcquire(t, func(*os.File, filelock.Options) (*filelock.Lock, error) {
			if attempts.Add(1) <= busyTries {
				return nil, busyErr()
			}
			return &filelock.Lock{}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		lock, err := flockExclusive(ctx, flockTestFile(t), "label")
		if err != nil || lock == nil {
			t.Fatalf("flockExclusive = (%v, %v), want the lock once the holder let go", lock, err)
		}
		if got, want := time.Since(start), busyTries*flockPollInterval; got != want {
			t.Fatalf("waited %s, want %d polls of %s = %s", got, busyTries, flockPollInterval, want)
		}
		if got := attempts.Load(); got != busyTries+1 {
			t.Fatalf("attempts = %d, want %d", got, busyTries+1)
		}

		// A lock that never frees: the wait ends with the context.
		stubFlockAcquire(t, func(*os.File, filelock.Options) (*filelock.Lock, error) {
			attempts.Add(1)
			return nil, busyErr()
		})
		attempts.Store(0)
		short, cancelShort := context.WithTimeout(context.Background(), 55*time.Millisecond)
		defer cancelShort()
		_, err = flockExclusive(short, flockTestFile(t), "label")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if got := attempts.Load(); got < 5 || got > 7 {
			t.Fatalf("attempts = %d over 55ms at a %s poll, want about 6", got, flockPollInterval)
		}
	})
}

// TestTryFlockExclusiveMakesExactlyOneAttempt pins the one-attempt form the
// in-transaction mint uses: it never waits and reports contention as
// filelock.ErrBusy.
func TestTryFlockExclusiveMakesExactlyOneAttempt(t *testing.T) {
	var attempts atomic.Int64
	stubFlockAcquire(t, func(_ *os.File, opts filelock.Options) (*filelock.Lock, error) {
		if !opts.NonBlocking {
			t.Error("tryFlockExclusive issued a blocking acquire")
		}
		attempts.Add(1)
		return nil, busyErr()
	})
	_, err := tryFlockExclusive(flockTestFile(t), "label")
	if !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("err = %v, want filelock.ErrBusy", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// unchangedByMapping reports that mapStoreTimeout handed back err itself: the
// same chain and text, with no further wrap and no sentinel it did not carry.
func unchangedByMapping(got, err error) bool {
	return errors.Is(got, err) && got.Error() == err.Error() &&
		errors.Is(got, ErrStoreTimeout) == errors.Is(err, ErrStoreTimeout)
}

// TestMapStoreTimeoutContract pins the mapping every ctx-owning caller uses:
// it returns err unchanged unless the store's own wait ended while the caller
// was still live and err is not already the sentinel.
func TestMapStoreTimeoutContract(t *testing.T) {
	cause := errors.New("locking SQLite sequence floor: wait ended")
	expired := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	if got := mapStoreTimeout(expired(), context.Background(), nil); got != nil {
		t.Fatalf("nil err mapped to %v", got)
	}
	if got := mapStoreTimeout(context.Background(), context.Background(), cause); !unchangedByMapping(got, cause) {
		t.Fatalf("live wait ctx: got %v, want err unchanged", got)
	}

	parent, cancelParent := context.WithCancel(context.Background())
	child, cancelChild := context.WithTimeout(parent, time.Hour)
	defer cancelChild()
	cancelParent()
	if got := mapStoreTimeout(child, parent, cause); !unchangedByMapping(got, cause) {
		t.Fatalf("done parent (child ended with it): got %v, want err unchanged and no sentinel", got)
	}

	already := fmt.Errorf("mapped once: %w", ErrStoreTimeout)
	if got := mapStoreTimeout(expired(), context.Background(), already); !unchangedByMapping(got, already) {
		t.Fatalf("already-mapped err: got %v, want it passed through without a second wrap", got)
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelWait()
	<-waitCtx.Done()
	got := mapStoreTimeout(waitCtx, context.Background(), cause)
	for name, target := range map[string]error{
		"ErrStoreTimeout":           ErrStoreTimeout,
		"context.DeadlineExceeded":  context.DeadlineExceeded,
		"the underlying wait error": cause,
	} {
		if !errors.Is(got, target) {
			t.Errorf("mapped err %v does not match %s", got, name)
		}
	}
	if errors.Is(got, context.Canceled) {
		t.Errorf("mapped err %v matches context.Canceled", got)
	}
}
