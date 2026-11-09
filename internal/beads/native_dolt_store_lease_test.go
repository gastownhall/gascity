package beads

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// These tests pin the storage-lease contract: NativeDoltStore.mu guards pointer
// and flag state only and is never held across I/O, so an operation parked
// mid-attempt on one handle never stalls a reconnect, a new read, ReadOnly or
// CloseStore, and the handle an operation leased is closed exactly once, by the
// last release.

const leaseTestWait = 5 * time.Second

// leaseTestHandle is a storage spy that counts its Close calls and records
// whether any lease holder was still using it when it closed.
type leaseTestHandle struct {
	*nativeDoltStorageSpy
	name      string
	closes    atomic.Int32
	inUse     atomic.Int32
	closedUse atomic.Int32 // closes that ran while a holder was still using it
	closeErr  error
}

func newLeaseTestHandle(name string) *leaseTestHandle {
	h := &leaseTestHandle{name: name}
	h.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		close: func() error {
			if h.inUse.Load() > 0 {
				h.closedUse.Add(1)
			}
			h.closes.Add(1)
			return h.closeErr
		},
	}
	return h
}

// leaseEventually polls cond until it holds or the wait elapses.
func leaseEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(leaseTestWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// leaseCompletesWithin runs fn and reports whether it returned within d.
func leaseCompletesWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// leaseGeneration reads the reconnect generation the way a failing read
// records it.
func leaseGeneration(s *NativeDoltStore) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// parkWriter leases the store's handle the way a write does and holds it until
// the returned func is called. It returns once the lease is held.
func parkWriter(t *testing.T, s *NativeDoltStore) (unpark func()) {
	t.Helper()
	storage, release, err := s.acquireStorage()
	if err != nil {
		t.Fatalf("acquireStorage for the parked writer: %v", err)
	}
	if storage == nil {
		t.Fatal("acquireStorage returned a nil handle")
	}
	var once sync.Once
	return func() { once.Do(release) }
}

// leaseStoreOn builds a test store whose reopen hook hands out the given fresh
// handles in order.
func leaseStoreOn(first beadslib.Storage, fresh ...beadslib.Storage) *NativeDoltStore {
	store := newNativeDoltStoreForTest(first)
	var next atomic.Int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		i := int(next.Add(1)) - 1
		if i >= len(fresh) {
			return nil, errors.New("lease test: no more fresh handles")
		}
		return fresh[i], nil
	}
	return store
}

func TestNativeDoltStoreReconnectDoesNotWaitForParkedWriter(t *testing.T) {
	h1, h2 := newLeaseTestHandle("h1"), newLeaseTestHandle("h2")
	store := leaseStoreOn(h1, h2)
	unpark := parkWriter(t, store)
	t.Cleanup(unpark)

	var reconnectErr error
	if !leaseCompletesWithin(time.Second, func() {
		reconnectErr = store.reconnect(context.Background(), leaseGeneration(store))
	}) {
		t.Fatal("reconnect waited for a parked writer; it must install the fresh handle without it")
	}
	if reconnectErr != nil {
		t.Fatalf("reconnect: %v", reconnectErr)
	}

	got, release, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("acquireStorage after reconnect: %v", err)
	}
	release()
	if got != beadslib.Storage(h2) {
		t.Fatalf("a new operation leased %v, want the fresh handle", got)
	}

	// The retired handle stays open while the writer still holds it.
	time.Sleep(50 * time.Millisecond)
	if n := h1.closes.Load(); n != 0 {
		t.Fatalf("retired handle closed %d time(s) while a writer still held it", n)
	}
	unpark()
	leaseEventually(t, "the retired handle to close on the last release", func() bool { return h1.closes.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := h1.closes.Load(); n != 1 {
		t.Fatalf("retired handle closed %d times, want exactly once", n)
	}
	if n := h2.closes.Load(); n != 0 {
		t.Fatalf("the serving handle closed %d time(s)", n)
	}
}

func TestNativeDoltStoreReadsReadOnlyAndCloseDoNotBlockBehindParkedWriter(t *testing.T) {
	h1, h2 := newLeaseTestHandle("h1"), newLeaseTestHandle("h2")
	store := leaseStoreOn(h1, h2)
	unpark := parkWriter(t, store)
	t.Cleanup(unpark)

	// A reconnect arriving behind the parked writer must not park a pending
	// Lock in front of every later reader.
	reconnected := make(chan error, 1)
	gen := leaseGeneration(store)
	go func() { reconnected <- store.reconnect(context.Background(), gen) }()
	time.Sleep(20 * time.Millisecond)

	if !leaseCompletesWithin(time.Second, func() {
		_, release, err := store.acquireStorage()
		if err != nil {
			t.Errorf("new read behind a parked writer: %v", err)
			return
		}
		release()
	}) {
		t.Fatal("a new read blocked behind a parked writer and a pending reconnect")
	}
	if !leaseCompletesWithin(time.Second, func() { _ = store.ReadOnly() }) {
		t.Fatal("ReadOnly blocked behind a parked writer and a pending reconnect")
	}
	select {
	case err := <-reconnected:
		if err != nil {
			t.Fatalf("reconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not complete while a writer was parked")
	}

	var closeErr error
	if !leaseCompletesWithin(time.Second, func() { closeErr = store.CloseStore() }) {
		t.Fatal("CloseStore blocked behind a parked writer")
	}
	if closeErr != nil {
		t.Fatalf("CloseStore with an operation in flight = %v, want nil", closeErr)
	}
}

func TestNativeDoltStoreCloseStoreReturnsNilWithOpInFlightAndClosesOnLastRelease(t *testing.T) {
	h1 := newLeaseTestHandle("h1")
	h1.closeErr = errors.New("close failed")
	store := leaseStoreOn(h1)
	unpark := parkWriter(t, store)
	t.Cleanup(unpark)

	var closeErr error
	if !leaseCompletesWithin(time.Second, func() { closeErr = store.CloseStore() }) {
		t.Fatal("CloseStore waited for the in-flight operation")
	}
	if closeErr != nil {
		t.Fatalf("CloseStore with an operation in flight = %v, want nil", closeErr)
	}
	if n := h1.closes.Load(); n != 0 {
		t.Fatalf("handle closed %d time(s) before the in-flight operation released it", n)
	}
	if _, _, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("acquireStorage after CloseStore = %v, want ErrStoreClosed", err)
	}
	unpark()
	leaseEventually(t, "the last release to close the handle", func() bool { return h1.closes.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := h1.closes.Load(); n != 1 {
		t.Fatalf("handle closed %d times, want exactly once", n)
	}
}

func TestNativeDoltStoreCloseStoreWithNoOpInFlightReturnsTheCloseError(t *testing.T) {
	want := errors.New("close failed")
	h1 := newLeaseTestHandle("h1")
	h1.closeErr = want
	store := leaseStoreOn(h1)

	if err := store.CloseStore(); !errors.Is(err, want) {
		t.Fatalf("CloseStore = %v, want the handle's close error %v", err, want)
	}
	if n := h1.closes.Load(); n != 1 {
		t.Fatalf("handle closed %d times, want 1", n)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("second CloseStore = %v, want nil", err)
	}
	if n := h1.closes.Load(); n != 1 {
		t.Fatalf("handle closed %d times after a second CloseStore, want 1", n)
	}
}

func TestNativeDoltStoreDoubleReleaseIsANoOp(t *testing.T) {
	h1 := newLeaseTestHandle("h1")
	store := leaseStoreOn(h1)

	_, release, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("acquireStorage: %v", err)
	}
	_, other, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("second acquireStorage: %v", err)
	}
	release()
	release() // must not drop the second holder's reference
	release()

	if n := h1.closes.Load(); n != 0 {
		t.Fatalf("handle closed %d time(s) while a second holder still had it", n)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h1.closes.Load(); n != 0 {
		t.Fatalf("handle closed %d time(s) while a holder still had it after CloseStore", n)
	}
	other()
	leaseEventually(t, "the handle to close once", func() bool { return h1.closes.Load() == 1 })
	other() // a spent release stays spent
	time.Sleep(50 * time.Millisecond)
	if n := h1.closes.Load(); n != 1 {
		t.Fatalf("handle closed %d times, want exactly once", n)
	}
}

func TestNativeDoltStoreLeaseStressNeverClosesAHeldHandleOrClosesTwice(t *testing.T) {
	const (
		workers    = 8
		reconnects = 200
	)
	var (
		handlesMu sync.Mutex
		handles   []*leaseTestHandle
	)
	mint := func() *leaseTestHandle {
		h := newLeaseTestHandle(fmt.Sprintf("h%d", len(handles)))
		handlesMu.Lock()
		handles = append(handles, h)
		handlesMu.Unlock()
		return h
	}
	first := mint()
	store := newNativeDoltStoreForTest(first)
	store.reopen = func(context.Context) (beadslib.Storage, error) { return mint(), nil }

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				storage, release, err := store.acquireStorage()
				if err != nil {
					return // the store closed
				}
				h := storage.(*leaseTestHandle)
				h.inUse.Add(1)
				time.Sleep(50 * time.Microsecond)
				h.inUse.Add(-1)
				release()
			}
		}()
	}
	for range reconnects {
		if err := store.reconnect(context.Background(), leaseGeneration(store)); err != nil {
			t.Fatalf("reconnect: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}

	handlesMu.Lock()
	all := append([]*leaseTestHandle(nil), handles...)
	handlesMu.Unlock()
	leaseEventually(t, "every handle to close", func() bool {
		for _, h := range all {
			if h.closes.Load() != 1 {
				return false
			}
		}
		return true
	})
	time.Sleep(50 * time.Millisecond)
	for _, h := range all {
		if n := h.closes.Load(); n != 1 {
			t.Errorf("handle %s closed %d times, want exactly once", h.name, n)
		}
		if n := h.closedUse.Load(); n != 0 {
			t.Errorf("handle %s closed %d time(s) while a lease holder was using it", h.name, n)
		}
	}
}

// leaseTaggedLifecycle is a lifecycle whose Update and Close record the handle
// they ran on and let the test intercept the first Update.
type leaseTaggedLifecycle struct {
	issueops.Lifecycle
	handle   *leaseTestHandle
	record   func(op string, h *leaseTestHandle)
	onUpdate func()
}

func (l *leaseTaggedLifecycle) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	l.record("update", l.handle)
	if l.onUpdate != nil {
		l.onUpdate()
	}
	return issueops.UpdateResult{Issue: &issueops.Issue{ID: req.IssueID}, Changed: true}, nil
}

func (l *leaseTaggedLifecycle) Close(_ context.Context, req issueops.CloseRequest) (issueops.CloseResult, error) {
	l.record("close", l.handle)
	return issueops.CloseResult{Issue: &issueops.Issue{ID: req.IssueID}, Changed: true}, nil
}

func TestNativeDoltStoreReconnectDuringStampAndCloseKeepsItsHandle(t *testing.T) {
	h1, h2 := newLeaseTestHandle("h1"), newLeaseTestHandle("h2")
	store := leaseStoreOn(h1, h2)

	var (
		mu  sync.Mutex
		ran []string
	)
	record := func(op string, h *leaseTestHandle) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, op+"@"+h.name)
	}
	var h1CloseesAtClose atomic.Int32
	wire := func(h *leaseTestHandle, onUpdate func()) {
		h.issueLifecycle = func() (issueops.Lifecycle, error) {
			return &leaseTaggedLifecycle{handle: h, record: func(op string, hh *leaseTestHandle) {
				if op == "close" {
					h1CloseesAtClose.Store(h1.closes.Load())
				}
				record(op, hh)
			}, onUpdate: onUpdate}, nil
		}
	}
	// A reconnect lands between the stamp's Update and the close.
	wire(h1, func() {
		if err := store.reconnect(context.Background(), leaseGeneration(store)); err != nil {
			t.Errorf("reconnect between the stamp and the close: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	})
	wire(h2, nil)

	if !leaseCompletesWithin(leaseTestWait, func() {
		if err := store.stampAndClose("gc-1", map[string]string{"k": "v"}); err != nil {
			t.Errorf("stampAndClose: %v", err)
		}
	}) {
		t.Fatal("stampAndClose did not complete across a reconnect")
	}

	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	if want := []string{"update@h1", "close@h1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stampAndClose ran %v, want both writes on the leased handle %v", got, want)
	}
	if n := h1CloseesAtClose.Load(); n != 0 {
		t.Fatalf("the leased handle was closed (%d) before the chain's second write", n)
	}
	leaseEventually(t, "the retired handle to close after the chain", func() bool { return h1.closes.Load() == 1 })
}

func TestNativeDoltStoreInstallAfterCloseStoreIsVoidAndClosesTheFreshHandle(t *testing.T) {
	h1, h2 := newLeaseTestHandle("h1"), newLeaseTestHandle("h2")
	store := newNativeDoltStoreForTest(h1)
	reopenEntered := make(chan struct{})
	releaseReopen := make(chan struct{})
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		close(reopenEntered)
		<-releaseReopen
		return h2, nil
	}

	gen := leaseGeneration(store)
	errc := make(chan error, 1)
	go func() { errc <- store.reconnect(context.Background(), gen) }()
	<-reopenEntered

	if !leaseCompletesWithin(time.Second, func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("CloseStore during a reconnect's reopen: %v", err)
		}
	}) {
		t.Fatal("CloseStore waited for an in-flight reconnect's reopen")
	}
	close(releaseReopen)

	if err := <-errc; !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("reconnect after CloseStore = %v, want ErrStoreClosed", err)
	}
	leaseEventually(t, "both handles to close", func() bool { return h1.closes.Load() == 1 && h2.closes.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if h1.closes.Load() != 1 || h2.closes.Load() != 1 {
		t.Fatalf("closes h1=%d h2=%d, want exactly one each", h1.closes.Load(), h2.closes.Load())
	}
	if _, _, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("acquireStorage after a voided install = %v, want ErrStoreClosed", err)
	}
}
