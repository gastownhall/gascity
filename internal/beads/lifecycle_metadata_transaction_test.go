package beads

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type lifecycleMetadataEmptyHandlesStore struct {
	*MemStore
}

func (s *lifecycleMetadataEmptyHandlesStore) Handles() StoreHandles {
	return StoreHandles{}
}

func TestWithLifecycleMetadataTransactionFallsBackToStoreHandles(t *testing.T) {
	store := NewMemStore()
	created, err := store.Create(Bead{Title: "lifecycle metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = WithLifecycleMetadataTransaction(store, created.ID, func(tx LifecycleMetadataTransaction) error {
		before, err := tx.Get()
		if err != nil {
			return err
		}
		if before.ID != created.ID {
			t.Fatalf("Get ID = %q, want %q", before.ID, created.ID)
		}
		if err := tx.SetMetadata("phase", "intent"); err != nil {
			return err
		}
		return tx.SetMetadataBatch(map[string]string{
			"marker": "committed",
			"owner":  "controller",
		})
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}

	after, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after transaction: %v", err)
	}
	for key, want := range map[string]string{
		"phase":  "intent",
		"marker": "committed",
		"owner":  "controller",
	} {
		if got := after.Metadata[key]; got != want {
			t.Errorf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestWithLifecycleMetadataTransactionRejectsMissingFallbackHandles(t *testing.T) {
	store := &lifecycleMetadataEmptyHandlesStore{MemStore: NewMemStore()}
	called := false
	err := WithLifecycleMetadataTransaction(store, "gc-1", func(LifecycleMetadataTransaction) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("WithLifecycleMetadataTransaction error = nil, want missing-handle error")
	}
	if called {
		t.Fatal("callback ran without valid fallback handles")
	}
}

func TestWithLifecycleMetadataTransactionFallbackSerializesAndReleasesAfterCallbackError(t *testing.T) {
	store := NewMemStore()
	created, err := store.Create(Bead{Title: "serialized fallback"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	callbackErr := errors.New("first callback failed")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithLifecycleMetadataTransaction(store, created.ID, func(LifecycleMetadataTransaction) error {
			close(firstEntered)
			<-releaseFirst
			return callbackErr
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first fallback callback did not enter")
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithLifecycleMetadataTransaction(store, created.ID, func(LifecycleMetadataTransaction) error {
			close(secondEntered)
			return nil
		})
	}()
	waitForLifecycleMetadataRefs(t, memLifecycleMutationScope(store), created.ID, 2)
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second fallback callback entered before the first released")
	default:
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if !errors.Is(err, callbackErr) {
			t.Fatalf("first callback error = %v, want %v", err, callbackErr)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first fallback callback did not finish")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second fallback callback: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("fallback lock was not released after callback error")
	}
}

type lifecycleMetadataCapabilitySpy struct {
	*MemStore
	calls int
}

func (s *lifecycleMetadataCapabilitySpy) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	s.calls++
	return fn(lifecycleMetadataDirectTransaction{
		id:     id,
		reader: s.MemStore,
		writer: s.MemStore,
	})
}

func TestTypedClassStoresForwardLifecycleMetadataTransactions(t *testing.T) {
	backing := &lifecycleMetadataCapabilitySpy{MemStore: NewMemStore()}
	created, err := backing.Create(Bead{Title: "typed lifecycle metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stores := []Store{
		WorkStore{Store: backing},
		GraphStore{Store: backing},
		SessionStore{Store: backing},
		MailStore{Store: backing},
		OrdersStore{Store: backing},
		NudgesStore{Store: backing},
	}

	for i, store := range stores {
		if err := WithLifecycleMetadataTransaction(store, created.ID, func(LifecycleMetadataTransaction) error {
			return nil
		}); err != nil {
			t.Fatalf("wrapper %d: %v", i, err)
		}
	}
	if backing.calls != len(stores) {
		t.Fatalf("capability calls = %d, want %d", backing.calls, len(stores))
	}
}

func waitForLifecycleMetadataRefs(t *testing.T, scope, id string, want int) {
	t.Helper()
	_ = id
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	key := closeTransitionScopeKey(scope)
	for {
		lifecycleMutationMutexRegistry.Lock()
		entry := lifecycleMutationMutexRegistry.entries[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		lifecycleMutationMutexRegistry.Unlock()
		if refs >= want {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("lifecycle metadata scope %q bead %q refs = %d, want at least %d", scope, id, refs, want)
		default:
			runtime.Gosched()
		}
	}
}

func TestLifecycleMutationScopeKeyCanonicalizesScope(t *testing.T) {
	scope := t.TempDir()
	canonical := closeTransitionScopeKey(scope)
	equivalent := closeTransitionScopeKey(scope + string(filepath.Separator) + ".")
	if canonical != equivalent {
		t.Fatalf("equivalent scopes produced different keys: %#v != %#v", canonical, equivalent)
	}
	if closeTransitionScopeKey("") == closeTransitionScopeKey("   ") {
		t.Fatal("whitespace filesystem scope shared the truly empty direct-store key")
	}
}

func TestBdStoreLifecycleMetadataTransactionsSerializeAcrossHandles(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	first := NewBdStore(scope, nil)
	second := NewBdStore(scope, nil)
	const id = "bd-42"

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithLifecycleMetadataTransaction(first, id, func(LifecycleMetadataTransaction) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first lifecycle metadata callback did not enter")
	}

	lockPath := filepath.Join(beadsDir, lifecycleMetadataLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		close(releaseFirst)
		t.Fatalf("stable lifecycle metadata lock %s: %v", lockPath, err)
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithLifecycleMetadataTransaction(second, id, func(LifecycleMetadataTransaction) error {
			close(secondEntered)
			return nil
		})
	}()
	waitForLifecycleMetadataRefs(t, scope, id, 2)
	select {
	case <-secondEntered:
		close(releaseFirst)
		t.Fatal("second lifecycle metadata callback entered before the first released")
	default:
	}

	close(releaseFirst)
	for name, done := range map[string]<-chan error{
		"first":  firstDone,
		"second": secondDone,
	} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s lifecycle metadata transaction: %v", name, err)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatalf("%s lifecycle metadata transaction did not finish", name)
		}
	}
}
