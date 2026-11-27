package nudgequeue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// holdQueueLock takes the city's nudge-queue lock from an independent handle
// and holds it for the rest of the test, the way a separate controller process
// would. It takes the same kernel lock WithState takes, so the contention it
// creates is the contention production would meet.
func holdQueueLock(t *testing.T, cityPath string) {
	t.Helper()
	path := LockPath(cityPath)
	lockFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	lock, err := filelock.Acquire(lockFile, filelock.Options{
		Mode:        filelock.Exclusive,
		NonBlocking: true,
		Label:       path,
	})
	if err != nil {
		_ = lockFile.Close()
		t.Fatalf("hold queue lock: %v", err)
	}
	t.Cleanup(func() {
		_ = lock.Release()
		_ = lockFile.Close()
	})
}

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

	holdQueueLock(t, cityPath)

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

	holdQueueLock(t, cityPath)

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
	if errors.Is(fmt.Errorf("locking nudge queue: %w", os.ErrClosed), ErrQueueBusy) {
		t.Fatal("a non-timeout lock error must not be ErrQueueBusy")
	}
}

// holdQueueLockForTest takes the queue lock on its own descriptor and
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
	lock, err := filelock.Acquire(lockFile, filelock.Options{Mode: filelock.Exclusive, Label: LockPath(cityPath)})
	if err != nil {
		lockFile.Close() //nolint:errcheck
		t.Fatalf("hold queue lock: %v", err)
	}
	return func() {
		lock.Release()   //nolint:errcheck
		lockFile.Close() //nolint:errcheck
	}
}

// TestItemWakeIfNeededRoundTrips: an item's declared wake intent survives a
// save and a load of the queue, and a state.json written before the field
// existed decodes with no wake intent.
func TestItemWakeIfNeededRoundTrips(t *testing.T) {
	cityPath := t.TempDir()
	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending,
			Item{ID: "notice", Agent: "worker", Source: "mail", WakeIfNeeded: true},
			Item{ID: "plain", Agent: "worker", Source: "session"},
		)
		return nil
	}); err != nil {
		t.Fatalf("WithState: %v", err)
	}
	state, err := LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := map[string]bool{}
	for _, item := range state.Pending {
		got[item.ID] = item.WakeIfNeeded
	}
	if want := map[string]bool{"notice": true, "plain": false}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("WakeIfNeeded by id = %v, want %v", got, want)
	}

	legacyCity := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(StatePath(legacyCity)), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacy := `{"pending":[{"id":"old","agent":"worker","source":"mail","message":"You have mail from human","created_at":"2026-01-01T00:00:00Z","deliver_after":"2026-01-01T00:00:00Z","expires_at":"2026-01-02T00:00:00Z"}]}`
	if err := os.WriteFile(StatePath(legacyCity), []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err = LoadState(legacyCity)
	if err != nil {
		t.Fatalf("LoadState(legacy): %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].WakeIfNeeded {
		t.Fatalf("legacy pending = %+v, want one item with no wake intent", state.Pending)
	}
}
