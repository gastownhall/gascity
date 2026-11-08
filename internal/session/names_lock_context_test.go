package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// holdCityIdentifierLock takes the city-scoped identifier lock in a goroutine
// and returns once it is held. The returned release func lets go of it and
// waits for the holder to finish, so a test never leaks a held flock.
func holdCityIdentifierLock(t *testing.T, cityPath, identifier string) (release func()) {
	t.Helper()
	held := make(chan struct{})
	releaseCh := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithCitySessionNameLock(cityPath, identifier, func() error {
			close(held)
			<-releaseCh
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("holder of %q never acquired its lock: %v", identifier, err)
	}
	var once bool
	release = func() {
		if once {
			return
		}
		once = true
		close(releaseCh)
		if err := <-done; err != nil {
			t.Errorf("holder of %q: %v", identifier, err)
		}
	}
	t.Cleanup(release)
	return release
}

func TestWithCitySessionIdentifierLocksContext_BusyLockReturnsTypedErrorAndSkipsFn(t *testing.T) {
	cityPath := t.TempDir()
	holdCityIdentifierLock(t, cityPath, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ran := false
	err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"worker-1"}, func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want errors.Is ErrSessionIdentifierBusy", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the ctx cause (context.DeadlineExceeded) wrapped too", err)
	}
	if !strings.Contains(err.Error(), "worker-1") {
		t.Fatalf("error = %q, want it to name the identifier", err)
	}
	if ran {
		t.Fatal("fn ran although the lock was never acquired")
	}
}

func TestWithCitySessionIdentifierLocksContext_CanceledContextOnBusyLockWrapsCanceled(t *testing.T) {
	cityPath := t.TempDir()
	holdCityIdentifierLock(t, cityPath, "worker-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"worker-1"}, func() error {
		t.Fatal("fn ran although the lock was held and ctx was canceled")
		return nil
	})
	if !errors.Is(err, ErrSessionIdentifierBusy) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want ErrSessionIdentifierBusy wrapping context.Canceled", err)
	}
}

func TestWithCitySessionIdentifierLocksContext_FreeLockRunsFnEvenWithShortBudget(t *testing.T) {
	cityPath := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ran := false
	if err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"worker-1"}, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("uncontended acquire: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run on an uncontended lock")
	}
}

func TestWithCitySessionIdentifierLocksContext_LockReleasedWhileWaitingIsAcquired(t *testing.T) {
	cityPath := t.TempDir()
	release := holdCityIdentifierLock(t, cityPath, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"worker-1"}, func() error { return nil })
	}()
	release()
	if err := <-result; err != nil {
		t.Fatalf("waiter after release: %v, want it to acquire the lock", err)
	}
}

func TestWithCitySessionIdentifierLocksContext_FnErrorPassesThroughUnwrappedAsNotBusy(t *testing.T) {
	cityPath := t.TempDir()
	sentinel := errors.New("fn failed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"a", "b"}, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want fn's error", err)
	}
	if errors.Is(err, ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v: an fn failure must not read as a busy lock", err)
	}
}

func TestWithCitySessionIdentifierLocksContext_NoIdentifiersRunsFnWithoutLocking(t *testing.T) {
	ran := false
	if err := WithCitySessionIdentifierLocksContext(context.Background(), t.TempDir(), []string{" ", ""}, func() error {
		ran = true
		return nil
	}); err != nil || !ran {
		t.Fatalf("blank identifiers: ran=%v err=%v, want fn run with no error", ran, err)
	}
}

func TestWithCitySessionIdentifierLocksContext_SecondIdentifierBusyReleasesTheFirst(t *testing.T) {
	cityPath := t.TempDir()
	// Identifiers are acquired in sorted order, so "a" is taken and "b" is the
	// one that is busy.
	holdCityIdentifierLock(t, cityPath, "b")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"b", "a"}, func() error {
		t.Fatal("fn ran with one identifier still held by another")
		return nil
	})
	if !errors.Is(err, ErrSessionIdentifierBusy) {
		t.Fatalf("error = %v, want ErrSessionIdentifierBusy", err)
	}
	if !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("error = %q, want it to name the busy identifier \"b\", not \"a\"", err)
	}

	// "a" was acquired before "b" timed out; a leaked "a" would make this busy.
	probe, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer probeCancel()
	if err := WithCitySessionIdentifierLocksContext(probe, cityPath, []string{"a"}, func() error { return nil }); err != nil {
		t.Fatalf("re-acquiring \"a\" after the failed multi-acquire: %v, want it released", err)
	}
}

func TestWithCitySessionIdentifierLocksContext_MultiIdentifierSharesOneBudget(t *testing.T) {
	cityPath := t.TempDir()
	holdCityIdentifierLock(t, cityPath, "c")

	// A per-identifier budget would give "c" a fresh wait after "a" and "b"
	// were taken; the shared ctx makes the whole acquire fail at its deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := WithCitySessionIdentifierLocksContext(ctx, cityPath, []string{"a", "b", "c"}, func() error { return nil })
	if !errors.Is(err, ErrSessionIdentifierBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want ErrSessionIdentifierBusy wrapping the shared deadline", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the acquire returned busy before the shared ctx expired")
	}
}

func TestWithCitySessionIdentifierLocksContext_BackgroundContextStillBlocksUntilRelease(t *testing.T) {
	cityPath := t.TempDir()
	release := holdCityIdentifierLock(t, cityPath, "worker-1")

	result := make(chan error, 1)
	go func() {
		result <- WithCitySessionIdentifierLocksContext(context.Background(), cityPath, []string{"worker-1"}, func() error { return nil })
	}()
	release()
	if err := <-result; err != nil {
		t.Fatalf("non-cancellable waiter after release: %v, want it to acquire", err)
	}
}

func TestWithCitySessionLockWrappersKeepBlockingSemantics(t *testing.T) {
	cityPath := t.TempDir()
	for name, with := range map[string]func(fn func() error) error{
		"name":  func(fn func() error) error { return WithCitySessionNameLock(cityPath, "worker-1", fn) },
		"alias": func(fn func() error) error { return WithCitySessionAliasLock(cityPath, "worker-1", fn) },
		"multi": func(fn func() error) error {
			return WithCitySessionIdentifierLocks(cityPath, []string{"worker-1", "worker-2"}, fn)
		},
	} {
		release := holdCityIdentifierLock(t, cityPath, "worker-1")
		result := make(chan error, 1)
		go func() { result <- with(func() error { return nil }) }()
		release()
		if err := <-result; err != nil {
			t.Errorf("%s wrapper after release: %v, want it to acquire", name, err)
		}
	}
}
