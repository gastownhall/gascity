package beads

import (
	"context"
	"errors"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// A row can carry a prefix this store does not mint: a pinned id, or a relic a
// storage migration copied in. The namespace boundary the weak-ParentID
// contract draws is the STORE's, so a parent inside the namespace this store
// mints is one it can see the absence of, and it must be refused before
// anything is written — whatever prefix the child happens to carry.
//
// Keying the question on the child's prefix instead reads a parent in the
// store's own namespace as foreign and lets the reparent land dangling, which
// is what main refused and what beads.Bead.ParentID promises.
func TestNativeDoltStoreResolvesAParentInItsOwnNamespaceForAForeignPrefixedChild(t *testing.T) {
	storage := newNativeDoltMemStorage()
	storage.issuePrefix = "ga"
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "native-test", "ga")
	relic, err := store.Create(Bead{ID: "gc-1", Title: "a row carrying another ledger's prefix"})
	if err != nil {
		t.Fatalf("Create with a pinned foreign-prefixed id: %v", err)
	}
	if relic.ID != "gc-1" {
		t.Fatalf("Create returned id %q, want the pinned %q", relic.ID, "gc-1")
	}

	dangling := "ga-999999"
	err = store.Update(relic.ID, UpdateOpts{ParentID: &dangling})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(%q, parent %q) = %v, want ErrNotFound: the parent is inside the namespace this store mints, so its absence is a row this store can see", relic.ID, dangling, err)
	}
	got, err := store.Get(relic.ID)
	if err != nil {
		t.Fatalf("Get after the refused reparent: %v", err)
	}
	if got.ParentID != "" {
		t.Errorf("ParentID after the refused reparent is %q, want it unchanged; the refusal has to come before the write", got.ParentID)
	}

	if _, err := store.Create(Bead{ID: "gc-2", Title: "same shape, on the create arm", ParentID: dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Create(child %q, parent %q) = %v, want ErrNotFound; Create and Update have to agree", "gc-2", dangling, err)
	}
	if _, err := store.Get("gc-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the refused create Get(%q) = %v, want ErrNotFound", "gc-2", err)
	}

	// The control that keeps this from being "resolve every parent": an id in a
	// namespace this store does not serve stays weak on both arms, because the
	// row it names lives in another ledger and refusing it would break every
	// cross-store molecule.
	foreign := "gcg-70b1e5f2-a"
	if err := store.Update(relic.ID, UpdateOpts{ParentID: &foreign}); err != nil {
		t.Errorf("Update(%q, parent %q) = %v, want it carried verbatim", relic.ID, foreign, err)
	}
	if _, err := store.Create(Bead{ID: "gc-3", Title: "child of another ledger's molecule", ParentID: foreign}); err != nil {
		t.Errorf("Create(child %q, parent %q) = %v, want it carried verbatim", "gc-3", foreign, err)
	}
}

// The conformance fixture opens this store with no declared prefix. Create is
// the one arm that cannot read the child's namespace off the child — the
// upstream library assigns the id — so a store that answered "foreign" for
// every parent there admitted a dangling parent inside its own namespace that
// Update, which sees the child's real id, refuses. The two arms have to agree:
// that is the rule the create-path skip is written against.
func TestNativeDoltStoreWithoutADeclaredPrefixAgreesBetweenCreateAndUpdate(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	control, err := store.Create(Bead{Title: "control"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	namespace := beadIDPrefix(control.ID)
	if namespace == "" {
		t.Fatalf("this fixture minted %q, which carries no namespace segment; the rows below would compare nothing", control.ID)
	}
	dangling := namespace + "-999999"

	if _, err := store.Create(Bead{Title: "step", ParentID: dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Create(parent %q) = %v, want ErrNotFound: the parent is in the namespace this store mints under, and Update refuses the same value", dangling, err)
	}
	if err := store.Update(control.ID, UpdateOpts{ParentID: &dangling}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(parent %q) = %v, want ErrNotFound", dangling, err)
	}

	// Both arms stay weak for an id in a namespace this store does not mint.
	foreign := "gcg-70b1e5f2-a"
	child, err := store.Create(Bead{Title: "cross-store step", ParentID: foreign})
	if err != nil {
		t.Fatalf("Create(parent %q) = %v, want it carried verbatim", foreign, err)
	}
	if child.ParentID != foreign {
		t.Errorf("ParentID came back %q, want %q verbatim", child.ParentID, foreign)
	}
	if err := store.Update(control.ID, UpdateOpts{ParentID: &foreign}); err != nil {
		t.Errorf("Update(parent %q) = %v, want it carried verbatim", foreign, err)
	}
}

// A foreign parent cannot ride the facade's Update role, which resolves
// ParentID like any local write, so Update rewrites that edge itself. Moving a
// bead from one foreign parent to another is two edge writes — the old edge out
// and the new one in — and the bead must never be left between them: a failed
// write has to leave the old parent standing, not a bead with no parent and an
// error that never names that state. These pin both routes: one transaction
// where the backing has one, and add-before-remove where it does not.

const (
	oldForeignParentForTest = "gcg-70b1e5f2-a"
	newForeignParentForTest = "gcg-70b1e5f2-b"
)

// foreignReparentFixture creates a child of oldForeignParentForTest, then arms
// the double so writing an edge to newForeignParentForTest fails.
func foreignReparentFixture(t *testing.T, store *NativeDoltStore, storage *nativeDoltFailingDependencyStorage, injected error) Bead {
	t.Helper()
	child, err := store.Create(Bead{Title: "step of a molecule in another ledger", ParentID: oldForeignParentForTest})
	if err != nil {
		t.Fatalf("Create child of %q: %v", oldForeignParentForTest, err)
	}
	if child.ParentID != oldForeignParentForTest {
		t.Fatalf("fixture child ParentID = %q, want %q; the rows below would assert nothing", child.ParentID, oldForeignParentForTest)
	}
	storage.addDependency = func(_ context.Context, dep *beadslib.Dependency) error {
		if dep.Type == beadslib.DepParentChild && dep.DependsOnID == newForeignParentForTest {
			return injected
		}
		return storage.nativeDoltMemStorage.AddDependency(context.Background(), dep, "test")
	}
	return child
}

// parentEdgesForTest lists id's parent-child targets as stored, so a test can
// tell "one parent" from "two parents" — Get reports only one.
func parentEdgesForTest(t *testing.T, store *NativeDoltStore, id string) []string {
	t.Helper()
	deps, err := store.DepList(id, "down")
	if err != nil {
		t.Fatalf("DepList(%q, down): %v", id, err)
	}
	var parents []string
	for _, dep := range deps {
		if dep.Type == string(beadslib.DepParentChild) {
			parents = append(parents, dep.DependsOnID)
		}
	}
	return parents
}

func TestNativeDoltStoreFailedForeignReparentKeepsTheOldParent(t *testing.T) {
	injected := errors.New("writing the new parent edge failed")
	storage := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	child := foreignReparentFixture(t, store, storage, injected)

	newParent := newForeignParentForTest
	if err := store.Update(child.ID, UpdateOpts{ParentID: &newParent}); !errors.Is(err, injected) {
		t.Fatalf("Update(parent %q) = %v, want the injected %v", newParent, err, injected)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get after the failed reparent: %v", err)
	}
	if got.ParentID != oldForeignParentForTest {
		t.Errorf("ParentID after the failed reparent = %q, want the old %q: the old edge came out and the new one never went in", got.ParentID, oldForeignParentForTest)
	}
}

// nativeDoltTxlessFailingDependencyStorage is the failing-dependency double on
// a backing with no transaction to offer, the served wire's shape: the store
// has to take the role doors one write at a time.
type nativeDoltTxlessFailingDependencyStorage struct {
	*nativeDoltFailingDependencyStorage
}

func (s *nativeDoltTxlessFailingDependencyStorage) RunInTransaction(context.Context, string, func(beadslib.Transaction) error) error {
	return &beadslib.ErrUnsupported{Op: "RunInTransaction", Backend: "http"}
}

func TestNativeDoltStoreFailedForeignReparentWithoutATransactionKeepsTheOldParent(t *testing.T) {
	injected := errors.New("writing the new parent edge failed")
	inner := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(&nativeDoltTxlessFailingDependencyStorage{nativeDoltFailingDependencyStorage: inner})
	child := foreignReparentFixture(t, store, inner, injected)

	newParent := newForeignParentForTest
	if err := store.Update(child.ID, UpdateOpts{ParentID: &newParent}); !errors.Is(err, injected) {
		t.Fatalf("Update(parent %q) = %v, want the injected %v", newParent, err, injected)
	}
	if got := parentEdgesForTest(t, store, child.ID); len(got) != 1 || got[0] != oldForeignParentForTest {
		t.Errorf("parent edges after the failed reparent = %v, want only the old %q: with no transaction to roll back, the new edge has to go in before the old one comes out", got, oldForeignParentForTest)
	}
}

func TestNativeDoltStoreForeignReparentWithoutATransactionMovesTheEdge(t *testing.T) {
	inner := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(&nativeDoltTxlessFailingDependencyStorage{nativeDoltFailingDependencyStorage: inner})
	child, err := store.Create(Bead{Title: "step of a molecule in another ledger", ParentID: oldForeignParentForTest})
	if err != nil {
		t.Fatalf("Create child of %q: %v", oldForeignParentForTest, err)
	}
	sibling, err := store.Create(Bead{Title: "a blocker the reparent must not touch"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	if err := store.DepAdd(child.ID, sibling.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd blocks: %v", err)
	}

	newParent := newForeignParentForTest
	if err := store.Update(child.ID, UpdateOpts{ParentID: &newParent}); err != nil {
		t.Fatalf("Update(parent %q): %v", newParent, err)
	}
	if got := parentEdgesForTest(t, store, child.ID); len(got) != 1 || got[0] != newParent {
		t.Errorf("parent edges after the reparent = %v, want only the new %q", got, newParent)
	}

	// Re-asserting the parent the bead already has adds nothing, so the old-edge
	// sweep must not take the edge it just asserted.
	if err := store.Update(child.ID, UpdateOpts{ParentID: &newParent}); err != nil {
		t.Fatalf("Update re-asserting parent %q: %v", newParent, err)
	}
	if got := parentEdgesForTest(t, store, child.ID); len(got) != 1 || got[0] != newParent {
		t.Errorf("parent edges after re-asserting the same parent = %v, want it still %q", got, newParent)
	}

	cleared := ""
	if err := store.Update(child.ID, UpdateOpts{ParentID: &cleared}); err != nil {
		t.Fatalf("Update clearing the parent: %v", err)
	}
	if got := parentEdgesForTest(t, store, child.ID); len(got) != 0 {
		t.Errorf("parent edges after clearing = %v, want none", got)
	}
	deps, err := store.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("DepList after the reparents: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != sibling.ID || deps[0].Type != "blocks" {
		t.Errorf("edges after the reparents = %+v, want only the blocks edge to %q: a parent sweep removes pairs, so it must not take an edge of another type", deps, sibling.ID)
	}
}

// nativeDoltServedWireStorage is the served wire's shape on the in-memory
// double: no transaction to offer, and an Update role that refuses a patch
// carrying no field, as beads' HTTP server does (internal/httpapi/update.go),
// where the in-process role answers one with a row read and no write.
type nativeDoltServedWireStorage struct {
	*nativeDoltTxlessFailingDependencyStorage
}

func newNativeDoltServedWireStorage() *nativeDoltServedWireStorage {
	inner := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	return &nativeDoltServedWireStorage{
		nativeDoltTxlessFailingDependencyStorage: &nativeDoltTxlessFailingDependencyStorage{nativeDoltFailingDependencyStorage: inner},
	}
}

func (s *nativeDoltServedWireStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	lifecycle, err := s.nativeDoltTxlessFailingDependencyStorage.IssueLifecycle()
	if err != nil {
		return nil, err
	}
	return nativeDoltServedWireLifecycle{Lifecycle: lifecycle}, nil
}

type nativeDoltServedWireLifecycle struct{ issueops.Lifecycle }

func (l nativeDoltServedWireLifecycle) Update(ctx context.Context, request issueops.UpdateRequest) (issueops.UpdateResult, error) {
	if reflect.DeepEqual(request.Patch, issueops.IssuePatch{}) {
		return issueops.UpdateResult{}, nativeDoltLifecycleValidationError("`patch` must carry at least one field; an update that updates nothing is refused rather than answered")
	}
	return l.Lifecycle.Update(ctx, request)
}

// A pure foreign reparent is what convoy membership and molecule attach send,
// and once its ParentID comes out for the edge rewrite it leaves the facade a
// patch carrying no field — one the served wire refuses outright. So a patch
// with nothing left in it must not reach the Update role at all, while a
// no-field update still reports what MemStore reports: nil for a bead that
// exists and ErrNotFound for one that does not.
func TestNativeDoltStoreUpdateSendsTheServedWireNoEmptyPatch(t *testing.T) {
	t.Run("a pure foreign reparent moves the edge", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltServedWireStorage())
		child, err := store.Create(Bead{Title: "step of a molecule in another ledger", ParentID: oldForeignParentForTest})
		if err != nil {
			t.Fatalf("Create child of %q: %v", oldForeignParentForTest, err)
		}
		newParent := newForeignParentForTest
		if err := store.Update(child.ID, UpdateOpts{ParentID: &newParent}); err != nil {
			t.Fatalf("Update(parent %q) = %v, want the edge moved", newParent, err)
		}
		if got := parentEdgesForTest(t, store, child.ID); len(got) != 1 || got[0] != newParent {
			t.Errorf("parent edges after the reparent = %v, want only the new %q", got, newParent)
		}
		cleared := ""
		if err := store.Update(child.ID, UpdateOpts{ParentID: &cleared}); err != nil {
			t.Fatalf("Update clearing the parent = %v, want the edge cleared", err)
		}
		if got := parentEdgesForTest(t, store, child.ID); len(got) != 0 {
			t.Errorf("parent edges after clearing = %v, want none", got)
		}
	})

	t.Run("a foreign reparent of a missing bead writes no edge", func(t *testing.T) {
		storage := newNativeDoltServedWireStorage()
		store := newNativeDoltStoreForTest(storage)
		const missing = "gc-nosuchbead"
		newParent := newForeignParentForTest
		if err := store.Update(missing, UpdateOpts{ParentID: &newParent}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Update(%q, parent %q) = %v, want ErrNotFound", missing, newParent, err)
		}
		deps, err := storage.store.DepList(missing, "down")
		if err != nil {
			t.Fatalf("DepList(%q) on the backing: %v", missing, err)
		}
		if len(deps) != 0 {
			t.Errorf("edges stored for the missing bead = %+v, want none: a reparent of a bead that does not exist must leave no edge behind", deps)
		}
	})

	t.Run("a no-field update reports what MemStore reports", func(t *testing.T) {
		store := newNativeDoltStoreForTest(newNativeDoltServedWireStorage())
		existing, err := store.Create(Bead{Title: "present"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Update(existing.ID, UpdateOpts{}); err != nil {
			t.Errorf("Update(%q, no fields) = %v, want nil", existing.ID, err)
		}
		if err := store.Update("gc-nosuchbead", UpdateOpts{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Update(missing, no fields) = %v, want ErrNotFound", err)
		}
	})
}

// A foreign reparent that carries other fields is two writes, the facade's and
// the edge rewrite, and their order decides what a failure leaves. Fields go
// first: a field the facade refuses stops the update before any edge is
// touched, and a failed edge rewrite after it leaves the new fields beside the
// old parent, which replaying the update converges from. Either way the bead
// keeps a parent.
func TestNativeDoltStoreMixedForeignReparentFailureKeepsTheOldParent(t *testing.T) {
	t.Run("a refused field touches no edge", func(t *testing.T) {
		storage := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		child, err := store.Create(Bead{Title: "step of a molecule in another ledger", ParentID: oldForeignParentForTest})
		if err != nil {
			t.Fatalf("Create child of %q: %v", oldForeignParentForTest, err)
		}

		newParent := newForeignParentForTest
		err = store.Update(child.ID, UpdateOpts{ParentID: &newParent, Metadata: map[string]string{"not a key": "x"}})
		if err == nil {
			t.Fatal("Update with a metadata key the facade refuses = nil, want the refusal")
		}
		if got := parentEdgesForTest(t, store, child.ID); len(got) != 1 || got[0] != oldForeignParentForTest {
			t.Errorf("parent edges after the refused update = %v, want only the old %q: the edge rewrite ran before the facade refused the fields", got, oldForeignParentForTest)
		}
	})

	t.Run("a failed edge write keeps the new fields and the old parent", func(t *testing.T) {
		injected := errors.New("writing the new parent edge failed")
		storage := &nativeDoltFailingDependencyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
		store := newNativeDoltStoreForTest(storage)
		child := foreignReparentFixture(t, store, storage, injected)

		newTitle := "retitled step"
		newParent := newForeignParentForTest
		if err := store.Update(child.ID, UpdateOpts{Title: &newTitle, ParentID: &newParent}); !errors.Is(err, injected) {
			t.Fatalf("Update(title, parent %q) = %v, want the injected %v", newParent, err, injected)
		}
		got, err := store.Get(child.ID)
		if err != nil {
			t.Fatalf("Get after the failed update: %v", err)
		}
		if got.ParentID != oldForeignParentForTest {
			t.Errorf("ParentID after the failed edge write = %q, want the old %q", got.ParentID, oldForeignParentForTest)
		}
		if got.Title != newTitle {
			t.Errorf("Title after the failed edge write = %q, want %q: the fields commit before the edge rewrite, and the update's doc says so", got.Title, newTitle)
		}
	})
}
