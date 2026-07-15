package beads

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests exercise the native read-path reconnect: a read against the
// initial (dead) handle fails with a transient connection error, the injected
// reopen hook hands back a fresh (healthy) handle, and the retry succeeds. The
// reopen hook stands in for the store factory's real hook, which re-resolves the
// current managed Dolt port and re-opens against the live server.

func healthySearchStorage(issues ...*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return issues, nil
		},
	}
}

func deadSearchStorage(err error) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, err
		},
	}
}

// storeWithReopen builds a test NativeDoltStore starting on dead and swapping to
// fresh via the reopen hook; reopens counts hook invocations.
func storeWithReopen(dead beadslib.Storage, fresh beadslib.Storage, reopens *int32) *NativeDoltStore {
	store := newNativeDoltStoreForTest(dead)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(reopens, 1)
		return fresh, nil
	}
	return store
}

func TestNativeDoltStoreGetReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("begin read tx: dial tcp 127.0.0.1:58216: i/o timeout")), healthy, &reopens)

	got, err := store.Get("gc-1")
	if err != nil {
		t.Fatalf("Get after transient conn error: %v", err)
	}
	if got.ID != "gc-1" {
		t.Fatalf("Get.ID = %q, want gc-1", got.ID)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected the reopen hook to fire; got %d", n)
	}
}

func TestNativeDoltStoreListReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-2", Title: "recovered list", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("[mysql] i/o timeout")), healthy, &reopens)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after transient conn error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("List = %#v, want [gc-2]", got)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected the reopen hook to fire; got %d", n)
	}
}

func TestNativeDoltStoreReadDoesNotRetryNonTransientError(t *testing.T) {
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("syntax error near 'FROM'")), healthySearchStorage(), &reopens)

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "syntax error") {
		t.Fatalf("Get error = %v, want the non-transient syntax error", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("non-transient error must not reconnect; got %d reopens", n)
	}
}

func TestNativeDoltStoreReadWithoutReopenHookDoesNotReconnect(t *testing.T) {
	// No reopen hook injected -> reconnect disabled, transient error returns as-is.
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "invalid connection") {
		t.Fatalf("Get error = %v, want the transient error returned as-is (fail fast)", err)
	}
}

func TestNativeDoltStoreReconnectReopenErrorIsTerminalWhenNonTransient(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		return nil, errors.New("permission denied resolving managed dolt port")
	}
	_, err := store.Get("gc-1")
	if err == nil || !errContains(err, "reconnect after transient read error") {
		t.Fatalf("Get error = %v, want a wrapped reconnect failure", err)
	}
	if !errContains(err, "permission denied") {
		t.Fatalf("Get error = %v, want the reopen cause preserved", err)
	}
}

func TestIsNativeDoltTransientReadError(t *testing.T) {
	transient := []string{
		"begin read tx: invalid connection",
		"[mysql] i/o timeout",
		"dial tcp 127.0.0.1:3307: connect: connection refused",
		"write: broken pipe",
		"unexpected EOF",
		"use of closed network connection",
		"bad connection",
		"read: connection reset by peer",
	}
	for _, msg := range transient {
		if !isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = false, want true", msg)
		}
	}
	permanent := []string{
		"issue gc-1 not found",
		"syntax error",
		"no rows in result set",
	}
	for _, msg := range permanent {
		if isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = true, want false", msg)
		}
	}
	if isNativeDoltTransientReadError(nil) {
		t.Errorf("isNativeDoltTransientReadError(nil) = true, want false")
	}
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

func nativeDoltStoreClosedForTest(s *NativeDoltStore) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func nativeDoltStoreStateForTest(s *NativeDoltStore) (beadslib.Storage, NativeReopenFunc) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storage, s.reopen
}

func TestNativeDoltStoreCloseStoreWinsInFlightReconnect(t *testing.T) {
	var oldCloseCalls atomic.Int32
	var freshCloseCalls atomic.Int32
	var freshReadCalls atomic.Int32
	old := deadSearchStorage(errors.New("invalid connection"))
	old.close = func() error {
		oldCloseCalls.Add(1)
		return nil
	}
	fresh := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			freshReadCalls.Add(1)
			return nil, nil
		},
		close: func() error {
			freshCloseCalls.Add(1)
			return nil
		},
	}

	reopenStarted := make(chan struct{})
	releaseReopen := make(chan struct{})
	var once sync.Once
	store := newNativeDoltStoreForTest(old)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		once.Do(func() { close(reopenStarted) })
		<-releaseReopen
		return fresh, nil
	}

	getDone := make(chan error, 1)
	go func() { _, err := store.Get("gc-1"); getDone <- err }()

	select {
	case <-reopenStarted:
	case <-time.After(time.Second):
		t.Fatal("reopen hook did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- store.CloseStore() }()

	deadline := time.Now().Add(time.Second)
	for !nativeDoltStoreClosedForTest(store) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !nativeDoltStoreClosedForTest(store) {
		t.Fatal("CloseStore did not latch the store closed")
	}
	if storage, _, release, err := store.acquireStorageGen(); !errors.Is(err, ErrStoreClosed) {
		if release != nil {
			release()
		}
		t.Fatalf("acquireStorageGen after close latch = (%T, %v), want ErrStoreClosed", storage, err)
	}
	if storage, release, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
		if release != nil {
			release()
		}
		t.Fatalf("acquireStorage after close latch = (%T, %v), want ErrStoreClosed", storage, err)
	}

	close(releaseReopen)
	select {
	case err := <-getDone:
		if !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("Get racing CloseStore = %v, want ErrStoreClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not return after reopen was released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseStore did not return after reopen was released")
	}

	storage, reopen := nativeDoltStoreStateForTest(store)
	if storage != nil || reopen != nil {
		t.Fatalf("closed store state = (storage=%T, reopen=%v), want both nil", storage, reopen != nil)
	}
	deadline = time.Now().Add(time.Second)
	for freshCloseCalls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := oldCloseCalls.Load(); got != 1 {
		t.Fatalf("old storage close calls = %d, want 1", got)
	}
	if got := freshCloseCalls.Load(); got != 1 {
		t.Fatalf("fresh storage close calls = %d, want 1", got)
	}
	if got := freshReadCalls.Load(); got != 0 {
		t.Fatalf("fresh storage read calls = %d, want 0", got)
	}
	if _, err := store.Get("gc-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after CloseStore = %v, want ErrStoreClosed", err)
	}
}

func TestNativeDoltStoreReadRetrySharesOneWallClockBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	firstReadDeadline := make(chan time.Time, 1)
	reopenDeadline := make(chan time.Time, 1)
	dead := &nativeDoltStorageSpy{
		searchIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			deadline, _ := ctx.Deadline()
			firstReadDeadline <- deadline
			time.Sleep(10 * time.Millisecond)
			return nil, errors.New("invalid connection")
		},
	}
	stillDead := deadSearchStorage(errors.New("invalid connection"))
	store := newNativeDoltStoreForTest(dead)
	store.readRetryBudgetOverride = budget
	store.reopen = func(ctx context.Context) (beadslib.Storage, error) {
		deadline, _ := ctx.Deadline()
		reopenDeadline <- deadline
		time.Sleep(10 * time.Millisecond)
		return stillDead, nil
	}

	started := time.Now()
	_, err := store.Get("gc-1")
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want context deadline exceeded", err)
	}
	if !isNativeDoltTransientReadError(err) {
		t.Fatalf("Get error = %v, want the last transient cause preserved", err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("Get elapsed = %s, want one %s wall-clock budget", elapsed, budget)
	}
	read := <-firstReadDeadline
	var reopen time.Time
	select {
	case reopen = <-reopenDeadline:
	case <-time.After(time.Second):
		t.Fatal("reopen did not receive the shared retry context")
	}
	if !read.Equal(reopen) {
		t.Fatalf("read deadline = %s, reopen deadline = %s; want one shared deadline", read, reopen)
	}
}

func TestNativeDoltStoreReadRetryBudgetBoundsReconnectGateWait(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.readRetryBudgetOverride = 40 * time.Millisecond
	var reopens atomic.Int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		reopens.Add(1)
		return healthySearchStorage(), nil
	}

	gate, err := store.acquireReconnectGate(context.Background())
	if err != nil {
		t.Fatalf("acquire reconnect gate: %v", err)
	}
	defer store.releaseReconnectGate(gate)

	started := time.Now()
	_, err = store.Get("gc-1")
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get waiting for reconnect gate = %v, want context deadline exceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Get waiting for reconnect gate took %s, want <= 200ms", elapsed)
	}
	if got := reopens.Load(); got != 0 {
		t.Fatalf("reopen calls while reconnect gate held = %d, want 0", got)
	}
}

func TestNativeDoltStoreNilReadReturnsStoreClosed(t *testing.T) {
	var store *NativeDoltStore
	if _, err := store.Get("gc-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("nil store Get = %v, want ErrStoreClosed", err)
	}
}

// TestNativeDoltStoreConcurrentReadersReopenOnce pins single-flight: many
// readers racing a dead handle trigger exactly one reopen; the losers discard
// and retry against the installed handle.
func TestNativeDoltStoreConcurrentReadersReopenOnce(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return healthy, nil
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.Get("gc-1")
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("reader %d: %v", i, e)
		}
	}
	if got := atomic.LoadInt32(&reopens); got != 1 {
		t.Fatalf("reopen called %d times, want exactly 1 (single-flight)", got)
	}
}
