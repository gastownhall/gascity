//go:build integration

package beads

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// openRealNativeDoltStoreForFacade opens a NativeDoltStore over REAL upstream
// native storage. The unit tier stands in for the beads issue-lifecycle facade
// with a double, which cannot answer what the facade actually refuses — its
// prefix check, its assignee fence and its close policy all live behind a live
// Dolt server. Every claim this file makes is a claim about the real facade.
func openRealNativeDoltStoreForFacade(t *testing.T, actor string) *NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
}

// TestNativeDoltStoreCreateKeepsFlatIDsThroughFacade is the regression that
// justifies routing the parent edge through CreateRequest.Dependencies rather
// than CreateRequest.ParentID: the latter switches the facade to hierarchical
// "<parent>.N" child IDs, which would rewrite every ID Gas City mints.
func TestNativeDoltStoreCreateKeepsFlatIDsThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	parent, err := store.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	blocker, err := store.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	child, err := store.Create(Bead{
		Title:        "child",
		ParentID:     parent.ID,
		Labels:       []string{"created", "hydrated"},
		Dependencies: []Dep{{DependsOnID: blocker.ID, Type: "blocks"}},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ID == parent.ID+".1" {
		t.Fatalf("child ID = %q, want a flat minted ID, not the hierarchical child scheme", child.ID)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("child ParentID = %q, want %q", child.ParentID, parent.ID)
	}
	if !slices.Equal(child.Labels, []string{"created", "hydrated"}) {
		t.Fatalf("child labels = %v, want the created labels hydrated back", child.Labels)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	assertNativeDependency(t, got.Dependencies, child.ID, parent.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, got.Dependencies, child.ID, blocker.ID, "blocks")
}

// TestNativeDoltStoreCreateAcceptsOffPrefixExplicitIDThroughFacade pins
// CreateRequest.ForceIDPrefix. The storage-layer create this replaced passed
// SkipPrefixValidation, so an explicit gcg-/gcs- ID in a gc-prefixed store has
// always worked; the facade validates the prefix unless told not to.
func TestNativeDoltStoreCreateAcceptsOffPrefixExplicitIDThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	created, err := store.Create(Bead{ID: "gcg-17", Title: "graph bead"})
	if err != nil {
		t.Fatalf("Create off-prefix explicit ID: %v", err)
	}
	if created.ID != "gcg-17" {
		t.Fatalf("created ID = %q, want gcg-17", created.ID)
	}
}

// TestNativeDoltStoreCreateToleratesUnresolvableDependencyTargets records that
// the facade resolves only same-prefix targets, which is the rule the store's
// own deleted prevalidation helper implemented. External and cross-store
// endpoints still create.
func TestNativeDoltStoreCreateToleratesUnresolvableDependencyTargets(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	for _, target := range []string{"external:github/1", "gcg-9999"} {
		if _, err := store.Create(Bead{Title: "dep " + target, Needs: []string{"blocks:" + target}}); err != nil {
			t.Fatalf("Create with %q dependency target: %v", target, err)
		}
	}
}

// TestNativeDoltStoreCreateRollsBackAMissingSamePrefixTarget shows the facade's
// single transaction replacing the hand-rolled compensation: the refusal leaves
// no partial bead behind without the store deleting anything.
func TestNativeDoltStoreCreateRollsBackAMissingSamePrefixTarget(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	_, err := store.Create(Bead{Title: "orphan", Needs: []string{"blocks:gc-nosuchbead"}})
	if err == nil {
		t.Fatal("Create error = nil, want a missing dependency refusal")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Create error = %v, want ErrNotFound", err)
	}
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after refused create: %v", err)
	}
	for _, bead := range all {
		if bead.Title == "orphan" {
			t.Fatalf("refused create left a durable bead: %+v", bead)
		}
	}
}

// TestNativeDoltStoreCreateRefusesASelfDependencyThroughFacade covers the
// "invalid dependency" case a review of the facade port raised, which the
// deleted validateCreatedDependencies prevalidation helper never checked at
// all: an edge naming the new bead as its own target. The facade's own request
// validation (ValidatePublicCreateRequest, independent of any backend) refuses
// it before a transaction ever opens, which is stricter than the storage-layer
// create this replaced.
func TestNativeDoltStoreCreateRefusesASelfDependencyThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	_, err := store.Create(Bead{ID: "gc-selfdep", Title: "self", Needs: []string{"blocks:gc-selfdep"}})
	if err == nil {
		t.Fatal("Create error = nil, want a self-dependency refusal")
	}
	if !errors.Is(err, issueops.ErrValidation) || !errors.Is(err, issueops.ErrSelfDependency) {
		t.Fatalf("Create error = %v, want issueops.ErrValidation wrapping issueops.ErrSelfDependency", err)
	}
}

// TestNativeDoltStoreCreateRefusesADependencyCycleThroughFacade is the second
// review-item case: a create whose own dependency edges, combined with an edge
// that already exists, would close a cycle. gc's deleted validation never
// looked past a single missing-target check, so this refusal is new strength
// the facade brings, not a regression to guard. The graph after this create
// would otherwise be closer -> mid -> blocker -> closer; the facade's
// CheckDependencyCycleInTx refuses it and rolls back the whole create.
func TestNativeDoltStoreCreateRefusesADependencyCycleThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	blocker, err := store.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	mid, err := store.Create(Bead{Title: "mid", Dependencies: []Dep{{DependsOnID: blocker.ID, Type: "blocks"}}})
	if err != nil {
		t.Fatalf("Create mid: %v", err)
	}
	_, err = store.Create(Bead{
		ID:    "gc-closer",
		Title: "closer",
		Dependencies: []Dep{
			{DependsOnID: mid.ID, Type: "blocks"},
			{IssueID: blocker.ID, DependsOnID: "gc-closer", Type: "blocks"},
		},
	})
	if err == nil {
		t.Fatal("Create error = nil, want a dependency-cycle refusal")
	}
	if !errors.Is(err, issueops.ErrDependencyCycle) {
		t.Fatalf("Create error = %v, want issueops.ErrDependencyCycle", err)
	}
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after refused create: %v", err)
	}
	for _, bead := range all {
		if bead.ID == "gc-closer" {
			t.Fatalf("refused cyclic create left a durable bead: %+v", bead)
		}
	}
}

// TestNativeDoltStoreUpdateWithoutAnAssigneeEditThroughFacade is the direct
// regression for an unconditional ForceAssigneeTransfer: the facade rejects the
// force outright when the patch carries no assignee, so every title-, status-
// and metadata-only update would fail validation.
func TestNativeDoltStoreUpdateWithoutAnAssigneeEditThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	bead, err := store.Create(Bead{Title: "plain"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "retitled"
	if err := store.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("title-only Update: %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.step_ref", "build"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != title || got.Metadata["gc.step_ref"] != "build" {
		t.Fatalf("bead after updates = %+v, want the retitle and the metadata key", got)
	}
}

// TestNativeDoltStoreUpdateTransfersAForeignClaimThroughFacade pins the reason
// ForceAssigneeTransfer is armed on an assignee edit: unforced, the facade
// refuses with ErrAlreadyClaimed, and the storage-layer write it replaced had no
// such fence.
func TestNativeDoltStoreUpdateTransfersAForeignClaimThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	bead, err := store.Create(Bead{Title: "claimed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, inProgress := "worker-a", "in_progress"
	if err := store.Update(bead.ID, UpdateOpts{Assignee: &first, Status: &inProgress}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second := "worker-b"
	if err := store.Update(bead.ID, UpdateOpts{Assignee: &second}); err != nil {
		t.Fatalf("claim transfer: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Assignee != second {
		t.Fatalf("assignee = %q, want %q", got.Assignee, second)
	}
}

// TestNativeDoltStoreClosesOverOpenChildrenThroughFacade pins both close-policy
// overrides. Unforced, the facade refuses a close and a status update that cross
// into the done category while children are open; Gas City closes molecule roots
// over open children as a matter of course.
func TestNativeDoltStoreClosesOverOpenChildrenThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	parent, err := store.Create(Bead{Title: "root"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	if _, err := store.Create(Bead{Title: "step", ParentID: parent.ID}); err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if err := store.Close(parent.ID); err != nil {
		t.Fatalf("Close over open children: %v", err)
	}
	if err := store.Reopen(parent.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	closed := "closed"
	if err := store.Update(parent.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("status update over open children: %v", err)
	}
	got, err := store.Get(parent.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

// TestNativeDoltStoreSetMetadataBatchMergesInsideTheWriteThroughFacade pins
// that a batch is a merge, not a replacement: each batch names only its own
// keys, and the facade resolves them against the current row, so every key an
// earlier write stored survives. A competing writer in another session is
// TestNativeDoltStoreSetMetadataBatchKeepsAnotherSessionsUpdate.
func TestNativeDoltStoreSetMetadataBatchMergesInsideTheWriteThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	bead, err := store.Create(Bead{Title: "meta", Metadata: map[string]string{"seeded": "yes"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadataBatch(bead.ID, map[string]string{"first": "1"}); err != nil {
		t.Fatalf("SetMetadataBatch first: %v", err)
	}
	if err := store.SetMetadataBatch(bead.ID, map[string]string{"second": "2"}); err != nil {
		t.Fatalf("SetMetadataBatch second: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for key, want := range map[string]string{"seeded": "yes", "first": "1", "second": "2"} {
		if got.Metadata[key] != want {
			t.Fatalf("metadata[%q] = %q, want %q (full: %#v)", key, got.Metadata[key], want, got.Metadata)
		}
	}
}

// TestNativeDoltStoreMetadataKeyRuleSplitsByRouteThroughFacade is the live
// evidence behind beadmeta.ValidKey and behind internal/dispatch dropping
// non-conforming keys instead of propagating them: the real backend admits such
// a key on create and on the map-based Store.Tx write, and refuses it on every
// standalone write. A bead created with one can therefore never be updated
// through the facade route while the key is in the patch.
func TestNativeDoltStoreMetadataKeyRuleSplitsByRouteThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	created, err := store.Create(Bead{Title: "dashed", Metadata: map[string]string{"my-key": "planted"}})
	if err != nil {
		t.Fatalf("Create with a non-conforming metadata key: %v, want the create route to accept it", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["my-key"] != "planted" {
		t.Fatalf("metadata = %#v, want the create to have stored the key", got.Metadata)
	}

	err = store.SetMetadataBatch(created.ID, map[string]string{"my-key": "rewritten"})
	if err == nil {
		t.Fatal("standalone SetMetadataBatch accepted a non-conforming key")
	}
	if !strings.Contains(err.Error(), beadmeta.ValidKeyPattern) {
		t.Fatalf("SetMetadataBatch error = %v, want the backend to quote %s; beadmeta.ValidKey mirrors that pattern and must be updated with it", err, beadmeta.ValidKeyPattern)
	}
	if err := store.Update(created.ID, UpdateOpts{Metadata: map[string]string{"my-key": "rewritten"}}); err == nil {
		t.Fatal("standalone Update accepted a non-conforming key")
	}

	if err := store.Tx("gc: tx", func(tx Tx) error {
		return tx.SetMetadataBatch(created.ID, map[string]string{"my-key": "rewritten"})
	}); err != nil {
		t.Fatalf("Tx SetMetadataBatch: %v, want the map-based route to accept the same key", err)
	}
}

// TestNativeDoltStoreClosePolicyIsStricterInsideTxThroughFacade pins the
// inversion documented on Store.Update: for the close policy the Tx route is
// the guarded one. Store.Update arms ForceClosePolicy on a status edit and
// closes over open children; the map-based Tx write has no exported spelling for
// that override and refuses the same write.
func TestNativeDoltStoreClosePolicyIsStricterInsideTxThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	parent, err := store.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	if _, err := store.Create(Bead{Title: "child", ParentID: parent.ID}); err != nil {
		t.Fatalf("Create child: %v", err)
	}
	closed := "closed"

	txErr := store.Tx("gc: close in tx", func(tx Tx) error {
		return tx.Update(parent.ID, UpdateOpts{Status: &closed})
	})
	if txErr == nil {
		t.Fatal("Tx status=closed over an open child succeeded; the close policy no longer guards the Tx route and Store.Update's doc comment is stale")
	}
	if !strings.Contains(txErr.Error(), "open child") {
		t.Fatalf("Tx close error = %v, want the open-children refusal", txErr)
	}

	if err := store.Update(parent.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("standalone Update status=closed over an open child: %v, want ForceClosePolicy to waive it", err)
	}
	got, err := store.Get(parent.ID)
	if err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("parent status = %q, want closed", got.Status)
	}
}

// TestNativeDoltStoreUpdateValidatesScalarsOnlyOnTheFacadeRouteThroughFacade
// records two refusals the storage-layer write it replaced did not make, and
// which the Tx route still does not make. Callers that take a title from user
// input guard it themselves (session.Manager.UpdatePresentation).
func TestNativeDoltStoreUpdateValidatesScalarsOnlyOnTheFacadeRouteThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	bead, err := store.Create(Bead{Title: "scalars"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blank, outOfRange := "", 7

	if err := store.Update(bead.ID, UpdateOpts{Title: &blank}); err == nil {
		t.Fatal("standalone Update accepted a blank title")
	}
	if err := store.Update(bead.ID, UpdateOpts{Priority: &outOfRange}); err == nil {
		t.Fatal("standalone Update accepted an out-of-range priority")
	}
	if err := store.Tx("gc: scalars", func(tx Tx) error {
		return tx.Update(bead.ID, UpdateOpts{Title: &blank, Priority: &outOfRange})
	}); err != nil {
		t.Fatalf("Tx Update: %v, want the map-based route to accept both", err)
	}
}

// TestNativeDoltStoreEmptyUpdateReportsNotFoundThroughFacade records that a
// no-field update now reads the row before discovering the patch is empty. The
// storage-layer write it replaced produced no columns and skipped the write, so
// a no-op update against a retired bead used to return nil.
func TestNativeDoltStoreEmptyUpdateReportsNotFoundThroughFacade(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "facade-test")
	err := store.Update("gc-nosuchbead", UpdateOpts{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update with no fields against a missing bead = %v, want ErrNotFound", err)
	}
	// A pure foreign reparent leaves the same empty patch once its ParentID
	// comes out for the edge rewrite, so it has to answer the same way.
	foreign := "gcg-70b1e5f2-a"
	if err := store.Update("gc-nosuchbead", UpdateOpts{ParentID: &foreign}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update with only a foreign ParentID against a missing bead = %v, want ErrNotFound", err)
	}
	if txErr := store.Tx("gc: empty", func(tx Tx) error {
		return tx.Update("gc-nosuchbead", UpdateOpts{})
	}); txErr != nil {
		t.Fatalf("Tx no-field update against a missing bead = %v, want the map route to stay a no-op", txErr)
	}
}
