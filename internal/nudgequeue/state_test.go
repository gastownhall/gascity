package nudgequeue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestWithState_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// WithState itself -- not just the withStateBounded helper it wraps -- must
// give a caller that cannot acquire the queue lock within its default budget
// a clear, actionable error, never an unbounded hang. The bound was
// specified to live inside WithState, since every production caller goes
// through it directly, not sit beside it as dead code only a test exercises.
func TestWithState_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	done := make(chan error, 1)
	go func() {
		done <- WithState(cityPath, func(_ *State) error {
			t.Error("WithState invoked fn after failing to acquire the lock")
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WithState returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("WithState error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
	case <-time.After(defaultLockWaitTimeout + 2*time.Second):
		t.Fatal("WithState blocked far past its default wait timeout while the queue lock was held (unbounded wait regression)")
	}
}

// TestWithStateBounded_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// a caller that cannot acquire the queue lock within its budget must get a
// clear, actionable error, never an unbounded hang.
func TestWithStateBounded_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	const waitTimeout = 150 * time.Millisecond
	done := make(chan error, 1)
	var called bool
	var mu sync.Mutex
	go func() {
		done <- withStateBounded(cityPath, "test", waitTimeout, func(_ *State) error {
			mu.Lock()
			called = true
			mu.Unlock()
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("withStateBounded returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("withStateBounded error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
		mu.Lock()
		gotCalled := called
		mu.Unlock()
		if gotCalled {
			t.Fatal("withStateBounded invoked fn after failing to acquire the lock")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("withStateBounded blocked far past its wait timeout while the queue lock was held (unbounded wait regression)")
	}
}

// TestWithStateBounded_TimeoutIsErrQueueBusy guards ga-fsp5w9: a lock timeout
// is a typed, retryable refusal, ErrQueueBusy, with the legacy text unchanged.
func TestWithStateBounded_TimeoutIsErrQueueBusy(t *testing.T) {
	cityPath := t.TempDir()
	release := holdQueueLockForTest(t, cityPath)
	defer release()

	err := withStateBounded(cityPath, "test", 50*time.Millisecond, func(_ *State) error {
		t.Error("fn ran without the lock")
		return nil
	})
	if !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("err = %v, want ErrQueueBusy", err)
	}
	if got, want := err.Error(), "locking nudge queue: timed out waiting 50ms for lock"; got != want {
		t.Fatalf("err text = %q, want %q", got, want)
	}
	if errors.Is(fmt.Errorf("locking nudge queue: %w", syscall.EBADF), ErrQueueBusy) {
		t.Fatal("a non-timeout lock error must not be ErrQueueBusy")
	}
}

// holdQueueLockForTest takes the queue flock on its own descriptor and
// returns the release.
func holdQueueLockForTest(t *testing.T, cityPath string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(LockPath(cityPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	return func() {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
		lockFile.Close()                                   //nolint:errcheck
	}
}
