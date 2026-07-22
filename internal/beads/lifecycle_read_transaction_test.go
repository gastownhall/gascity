package beads

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestMemLifecycleReadTransactionListsWithoutReenteringStoreMutex(t *testing.T) {
	store := NewMemStore()
	root, err := store.Create(Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := store.Create(Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- WithLifecycleMetadataTransaction(store, root.ID, func(tx LifecycleMetadataTransaction) error {
			reader, ok := tx.(LifecycleReadTransaction)
			if !ok {
				return ErrLifecycleReadUnsupported
			}
			got, err := reader.GetByID(child.ID)
			if err != nil {
				return err
			}
			if got.ID != child.ID {
				t.Errorf("GetByID = %#v, want child %q", got, child.ID)
			}
			listed, err := reader.List(ListQuery{
				ParentID:      root.ID,
				IncludeClosed: true,
				TierMode:      TierBoth,
			})
			if err != nil {
				return err
			}
			if len(listed) != 1 || listed[0].ID != child.ID {
				t.Errorf("List = %#v, want child %q", listed, child.ID)
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("lifecycle reads deadlocked while the MemStore mutex was held")
	}
}

func TestWithLifecycleReadTransactionsDeduplicatesSameDomain(t *testing.T) {
	store := NewMemStore()
	source, err := store.Create(Bead{Title: "source"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := store.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	root, err := store.Create(Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- WithLifecycleReadTransactions(store, source.ID, store, root.ID, func(sourceTx, rootTx LifecycleReadTransaction) error {
			got, err := sourceTx.GetByID(source.ID)
			if err != nil {
				return err
			}
			if got.Status != "closed" {
				t.Errorf("source status = %q, want closed", got.Status)
			}
			result, err := CloseWithinLifecycleMetadataTransaction(rootTx, "same-domain close")
			if err != nil {
				return err
			}
			if !result.AuthoritativeClosed(root.ID) {
				t.Errorf("close result = %#v, want authoritative root close", result)
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithLifecycleReadTransactions: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("same-domain lifecycle transactions were nested instead of deduplicated")
	}
}

func TestWithLifecycleReadTransactionsForwardsCachingStoreReads(t *testing.T) {
	backing := NewMemStore()
	source, err := backing.Create(Bead{Title: "source"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	root, err := backing.Create(Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := backing.Create(Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)

	err = WithLifecycleReadTransactions(backing, source.ID, cache, root.ID, func(sourceTx, rootTx LifecycleReadTransaction) error {
		got, err := sourceTx.GetByID(source.ID)
		if err != nil {
			return err
		}
		if got.ID != source.ID {
			t.Errorf("source GetByID = %#v, want %q", got, source.ID)
		}
		listed, err := rootTx.List(ListQuery{
			ParentID:      root.ID,
			IncludeClosed: true,
			TierMode:      TierBoth,
		})
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0].ID != child.ID {
			t.Errorf("root List = %#v, want child %q", listed, child.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLifecycleReadTransactions: %v", err)
	}
}

func TestWithLifecycleReadTransactionsFencesDistinctMemStores(t *testing.T) {
	sourceStore := NewMemStore()
	rootStore := NewMemStore()
	source, err := sourceStore.Create(Bead{Title: "source"})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	if err := sourceStore.Close(source.ID); err != nil {
		t.Fatalf("Close source: %v", err)
	}
	root, err := rootStore.Create(Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	checked := make(chan struct{})
	release := make(chan struct{})
	fencedDone := make(chan error, 1)
	go func() {
		fencedDone <- WithLifecycleReadTransactions(sourceStore, source.ID, rootStore, root.ID, func(sourceTx, rootTx LifecycleReadTransaction) error {
			got, err := sourceTx.GetByID(source.ID)
			if err != nil {
				return err
			}
			if got.Status != "closed" {
				return errors.New("source was not terminal inside lifecycle fence")
			}
			close(checked)
			<-release
			_, err = CloseWithinLifecycleMetadataTransaction(rootTx, "distinct-domain close")
			return err
		})
	}()

	select {
	case <-checked:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("two-store lifecycle fence did not reach callback")
	}
	reopenDone := make(chan error, 1)
	go func() { reopenDone <- sourceStore.Reopen(source.ID) }()
	select {
	case err := <-reopenDone:
		if err != nil {
			t.Fatalf("Reopen before release: %v", err)
		}
		t.Fatal("source reopened between terminality read and root close")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	select {
	case err := <-fencedDone:
		if err != nil {
			t.Fatalf("WithLifecycleReadTransactions: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("two-store lifecycle fence did not finish")
	}
	select {
	case err := <-reopenDone:
		if err != nil {
			t.Fatalf("Reopen after release: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("source reopen did not finish after lifecycle fence released")
	}
}

type unscopedLifecycleReadStore struct{ Store }

func TestWithLifecycleReadTransactionsFailsClosedWithoutStableDomains(t *testing.T) {
	source := unscopedLifecycleReadStore{Store: NewMemStore()}
	root := NewMemStore()
	called := false
	err := WithLifecycleReadTransactions(source, "source", root, "root", func(LifecycleReadTransaction, LifecycleReadTransaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrLifecycleMultiStoreUnsupported) {
		t.Fatalf("error = %v, want ErrLifecycleMultiStoreUnsupported", err)
	}
	if called {
		t.Fatal("callback ran without stable lifecycle domains")
	}
}
