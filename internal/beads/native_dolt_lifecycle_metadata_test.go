package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreImplementsLifecycleMetadataTransactionStore(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	if _, ok := any(store).(LifecycleMetadataTransactionStore); !ok {
		t.Fatal("NativeDoltStore does not implement LifecycleMetadataTransactionStore")
	}
}

type nativeLifecycleMetadataHydrationStorage struct {
	*nativeDoltMemStorage
	transactions int
}

func (s *nativeLifecycleMetadataHydrationStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	s.transactions++
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeLifecycleMetadataHydrationStorage) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	issue, err := s.nativeDoltMemStorage.GetIssue(ctx, id)
	if issue != nil {
		issue.Labels = nil
		issue.Dependencies = nil
	}
	return issue, err
}

func TestNativeDoltStoreLifecycleMetadataTransactionUsesNativeOperationsAndHydratesSnapshot(t *testing.T) {
	storage := &nativeLifecycleMetadataHydrationStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	blocker, err := store.Create(Bead{Title: "native lifecycle blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	root, err := store.Create(Bead{
		Title:  "native lifecycle root",
		Labels: []string{"run-accounting"},
		Needs:  []string{"blocks:" + blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	transactional := requireNativeLifecycleMetadataTransactionStore(t, store)

	err = transactional.WithLifecycleMetadataTransaction(root.ID, func(tx LifecycleMetadataTransaction) error {
		got, err := tx.Get()
		if err != nil {
			return err
		}
		if len(got.Dependencies) != 1 || got.Dependencies[0].DependsOnID != blocker.ID || got.Dependencies[0].Type != "blocks" {
			t.Fatalf("lifecycle Get dependencies = %#v, want complete blocks dependency on %s", got.Dependencies, blocker.ID)
		}
		if len(got.Labels) != 1 || got.Labels[0] != "run-accounting" {
			t.Fatalf("lifecycle Get labels = %#v, want complete run-accounting label", got.Labels)
		}
		return tx.SetMetadata("gc.test.lifecycle_phase", "prepared")
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
	if storage.transactions != 2 {
		t.Fatalf("native transaction boundaries = %d, want one read and one committed metadata mutation", storage.transactions)
	}
}

func TestNativeDoltStoreLifecycleMetadataTransactionSerializesReplacementAcrossHandles(t *testing.T) {
	const (
		pendingKey = "gc.test.lifecycle_pending"
		intentKey  = "gc.test.lifecycle_intent"
		oldIntent  = "intent-a"
		newIntent  = "intent-b"
	)

	scopeRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("create native store scope: %v", err)
	}
	shared := NewMemStore()
	cleanupStore := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
	replacementStore := newNativeDoltStoreForTest(&nativeDoltMemStorage{store: shared})
	cleanupStore.scopeRoot = scopeRoot
	replacementStore.scopeRoot = scopeRoot

	created, err := cleanupStore.Create(Bead{
		Title: "native lifecycle ownership",
		Metadata: StringMap{
			pendingKey: "v1",
			intentKey:  oldIntent,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cleanupTx := requireNativeLifecycleMetadataTransactionStore(t, cleanupStore)
	replacementTx := requireNativeLifecycleMetadataTransactionStore(t, replacementStore)
	cleanupOwnershipRead := make(chan struct{})
	releaseCleanup := make(chan struct{})
	replacementAttempted := make(chan struct{})
	replacementEntered := make(chan struct{})
	cleanupDone := make(chan error, 1)
	replacementDone := make(chan error, 1)

	go func() {
		cleanupDone <- cleanupTx.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
			owned, err := tx.Get()
			if err != nil {
				return err
			}
			if owned.Metadata[pendingKey] != "v1" || owned.Metadata[intentKey] != oldIntent {
				return errors.New("cleanup did not read the original lifecycle ownership")
			}
			if err := tx.SetMetadata(pendingKey, ""); err != nil {
				return err
			}
			fresh, err := tx.Get()
			if err != nil {
				return err
			}
			close(cleanupOwnershipRead)
			<-releaseCleanup
			if fresh.Metadata[pendingKey] == "" && fresh.Metadata[intentKey] == oldIntent {
				return tx.SetMetadata(intentKey, "")
			}
			return nil
		})
	}()

	waitNativeLifecycleSignal(t, "cleanup ownership read", cleanupOwnershipRead)
	go func() {
		close(replacementAttempted)
		replacementDone <- replacementTx.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
			close(replacementEntered)
			current, err := tx.Get()
			if err != nil {
				return err
			}
			if current.Metadata[pendingKey] != "" {
				return errors.New("replacement entered before cleanup cleared the pending marker")
			}
			if err := tx.SetMetadata(intentKey, newIntent); err != nil {
				return err
			}
			return tx.SetMetadata(pendingKey, "v1")
		})
	}()
	waitNativeLifecycleSignal(t, "replacement attempt", replacementAttempted)
	waitForLifecycleMetadataRefs(t, scopeRoot, created.ID, 2)
	lockPath := filepath.Join(scopeRoot, ".beads", lifecycleMetadataLockFilename)
	if _, err := os.Stat(lockPath); err != nil {
		close(releaseCleanup)
		t.Fatalf("stable native lifecycle metadata lock %s: %v", lockPath, err)
	}

	replacementEnteredBeforeRelease := false
	select {
	case <-replacementEntered:
		replacementEnteredBeforeRelease = true
	default:
	}
	close(releaseCleanup)
	waitNativeLifecycleResult(t, "cleanup", cleanupDone)
	waitNativeLifecycleResult(t, "replacement", replacementDone)

	if replacementEnteredBeforeRelease {
		t.Error("replacement lifecycle callback entered while cleanup held the same scope and bead transaction")
	}
	retained, err := cleanupStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get retained lifecycle: %v", err)
	}
	if retained.Metadata[pendingKey] != "v1" || retained.Metadata[intentKey] != newIntent {
		t.Fatalf("retained lifecycle metadata = %#v, want replacement pending=v1 intent=%q", retained.Metadata, newIntent)
	}
}

func TestNativeDoltStoreLifecycleMetadataTransactionKeepsWritesAndReleasesLockAfterCallbackError(t *testing.T) {
	const metadataKey = "gc.test.lifecycle_phase"
	wantErr := errors.New("injected callback failure")
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{Title: "native lifecycle callback error"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	transactional := requireNativeLifecycleMetadataTransactionStore(t, store)

	err = transactional.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
		if err := tx.SetMetadata(metadataKey, "prepared"); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	afterError, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after callback error: %v", err)
	}
	if got := afterError.Metadata[metadataKey]; got != "prepared" {
		t.Fatalf("metadata after callback error = %q, want durable successful write", got)
	}

	done := make(chan error, 1)
	go func() {
		done <- transactional.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
			return tx.SetMetadata(metadataKey, "recovered")
		})
	}()
	waitNativeLifecycleResult(t, "transaction after callback error", done)

	afterRecovery, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after subsequent transaction: %v", err)
	}
	if got := afterRecovery.Metadata[metadataKey]; got != "recovered" {
		t.Fatalf("metadata after subsequent transaction = %q, want recovered", got)
	}
}

func requireNativeLifecycleMetadataTransactionStore(t *testing.T, store *NativeDoltStore) LifecycleMetadataTransactionStore {
	t.Helper()
	transactional, ok := any(store).(LifecycleMetadataTransactionStore)
	if !ok {
		t.Fatal("NativeDoltStore does not implement LifecycleMetadataTransactionStore")
	}
	return transactional
}

func waitNativeLifecycleSignal(t *testing.T, operation string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not occur", operation)
	}
}

func waitNativeLifecycleResult(t *testing.T, operation string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish", operation)
	}
}
