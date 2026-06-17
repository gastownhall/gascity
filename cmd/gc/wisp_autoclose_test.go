package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestWispAutocloseClosesOpenMolecule(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Errorf("stdout = %q, want auto-close message", stdout.String())
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("wisp Status = %q, want %q", b.Status, "closed")
	}
}

func TestWispAutocloseClosesMetadataAttachedMolecule(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want metadata auto-close message", stdout.String())
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("metadata-attached molecule status = %q, want closed", b.Status)
	}
}

func TestWispAutoclosePreservesParkedMoleculeSubtree(t *testing.T) {
	// Regression for PR #3474: when a dispatch/loop bead closes, the
	// on_close wisp-autoclose hook must NOT force-close an attached molecule that
	// is parked at an open human-gate plus the finalize step it blocks. The
	// molecule root is still open (live, awaiting the maintainer), so the whole
	// subtree must survive — otherwise the gate-close -> finalize handoff is
	// destroyed before the human ever acts.
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "dispatch/loop bead",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"}) // gc-2 (open, parked)
	_, _ = store.Create(beads.Bead{
		Title:    "human-gate",
		Type:     "task",
		ParentID: "gc-2",
		Metadata: map[string]string{"gc.step_ref": "mol-adopt-pr.human-gate"},
	}) // gc-3 (open, parked gate)
	_, _ = store.Create(beads.Bead{
		Title:    "finalize",
		Type:     "task",
		ParentID: "gc-2",
		Metadata: map[string]string{"gc.step_ref": "mol-adopt-pr.finalize"},
	}) // gc-4 (open, blocked by the gate)
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Fatalf("parked molecule must not be auto-closed, got %q", stdout.String())
	}
	for _, id := range []string{"gc-2", "gc-3", "gc-4"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Fatalf("%s status = %q, want open (parked subtree preserved)", id, b.Status)
		}
	}
}

func TestWispAutocloseForceClosesTerminalMoleculeSubtree(t *testing.T) {
	// A molecule whose descendants are all terminal is genuinely complete — no
	// parked checkpoint remains — so the owner-close reap still force-closes the
	// open root. Preserves the original wisp-cleanup intent for finished work.
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "dispatch/loop bead",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})      // gc-2 (open)
	_, _ = store.Create(beads.Bead{Title: "step", Type: "task", ParentID: "gc-2"}) // gc-3 (terminal)
	_ = store.Close("gc-3")
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want auto-close message for terminal subtree", stdout.String())
	}
	root, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" {
		t.Fatalf("molecule root status = %q, want closed", root.Status)
	}
}

// walkFailingStore makes the subtree walk in subtreeTerminalExcludingRoot ->
// molecule.ListSubtree fail for one root, by erroring its logical-member
// ListByMetadata lookup. It models a transient store read failure while the
// root bead itself is still resolvable.
type walkFailingStore struct {
	beads.Store
	failID string
}

func (s *walkFailingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[beadmeta.RootBeadIDMetadataKey] == s.failID {
		return nil, fmt.Errorf("subtree walk unavailable for %s", s.failID)
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func TestAttachedMoleculeIsParkedPreservesOnWalkError(t *testing.T) {
	// Fail-safe arm of the #3474 fix: when the subtree walk errors (a transient
	// store read failure), the attached molecule must be classified as parked
	// and preserved, not force-closed. subtreeTerminalExcludingRoot maps a walk
	// error to (false, 0); the guard must treat that as parked, mirroring the
	// sibling autocloseMoleculeIfComplete's `if !terminal { return }`. This is
	// the only behavior that distinguishes the guard from the dropped
	// `&& descendants > 0` clause — end-to-end the close path's own walk also
	// fails on a persistent error, so it must be pinned at the predicate.
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "molecule root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	store := &walkFailingStore{Store: base, failID: root.ID}

	if !attachedMoleculeIsParked(store, root) {
		t.Fatal("attachedMoleculeIsParked = false on subtree walk error, want true (fail-safe preserve)")
	}
}

// walkFailOnceStore fails the subtree-walk ListByMetadata lookup for one root
// on its FIRST call, then succeeds. It models a *transient* store read failure
// (the fail-safe's documented trigger), which a persistent walkFailingStore
// cannot: with a persistent failure the close path's own ListSubtree walk also
// errors, so the subtree is preserved whether or not the fail-safe fires — the
// regression hides. Failing exactly once makes the owner-close reap's parked-
// check walk error while a later close-path walk would succeed, so an
// end-to-end test can tell the fail-safe preserve apart from a no-op close.
type walkFailOnceStore struct {
	beads.Store
	failID string
	failed bool
}

func (s *walkFailOnceStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if !s.failed && filters[beadmeta.RootBeadIDMetadataKey] == s.failID {
		s.failed = true
		return nil, fmt.Errorf("subtree walk transiently unavailable for %s", s.failID)
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func TestWispAutoclosePreservesParkedMoleculeSubtreeOnWalkError(t *testing.T) {
	// End-to-end fail-safe arm of the #3474 fix, driven through the full
	// on_close hook (doWispAutocloseWith) rather than the predicate alone.
	// A molecule parked at an open human-gate plus the finalize step it blocks
	// is attached to a dispatch/loop bead; when that owner bead closes and the
	// parked-check subtree walk errors transiently, the whole parked subtree
	// must be PRESERVED, never force-closed.
	//
	// The walk fails exactly once (walkFailOnceStore): the parked-check walk
	// errors, but the reap's own close-path walk would succeed. So if the
	// fail-safe regressed to `!terminal && descendants > 0`, the parked-check
	// would misclassify the (false, 0) walk-error result as not-parked and the
	// now-succeeding close walk would force-close gc-2..gc-4 — destroying the
	// gate-close -> finalize handoff #3474 protects. With the fail-safe intact
	// the close path is never reached and the subtree survives. This is the
	// end-to-end coverage TestAttachedMoleculeIsParkedPreservesOnWalkError pins
	// only at the predicate.
	base := beads.NewMemStore()
	_, _ = base.Create(beads.Bead{
		Title:    "dispatch/loop bead",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = base.Create(beads.Bead{Title: "molecule root", Type: "molecule"}) // gc-2 (open, parked)
	_, _ = base.Create(beads.Bead{
		Title:    "human-gate",
		Type:     "task",
		ParentID: "gc-2",
		Metadata: map[string]string{"gc.step_ref": "mol-adopt-pr.human-gate"},
	}) // gc-3 (open, parked gate)
	_, _ = base.Create(beads.Bead{
		Title:    "finalize",
		Type:     "task",
		ParentID: "gc-2",
		Metadata: map[string]string{"gc.step_ref": "mol-adopt-pr.finalize"},
	}) // gc-4 (open, blocked by the gate)
	_ = base.Close("gc-1")

	store := &walkFailOnceStore{Store: base, failID: "gc-2"}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Fatalf("walk-error fail-safe must not auto-close parked molecule, got %q", stdout.String())
	}
	for _, id := range []string{"gc-2", "gc-3", "gc-4"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Fatalf("%s status = %q, want open (parked subtree preserved under walk error)", id, b.Status)
		}
	}
}

func TestWispAutocloseChecksDescendantsWhenAttachedRootAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})      // gc-2
	_, _ = store.Create(beads.Bead{Title: "step", Type: "task", ParentID: "gc-2"}) // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want auto-close message for descendant cleanup", stdout.String())
	}
	child, err := store.Get("gc-3")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != "closed" {
		t.Fatalf("descendant status = %q, want closed", child.Status)
	}
}

func TestWispAutocloseClosesGeneratedSpecsForClosedWorkflowRoot(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	spec, err := store.Create(beads.Bead{
		Title: "Step spec for review",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "review",
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title: "real workflow work",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	_ = store.Close(root.ID)

	var stdout bytes.Buffer
	doWispAutocloseWith(store, root.ID, &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed 1 generated spec bead(s) on "+root.ID) {
		t.Fatalf("stdout = %q, want generated spec cleanup message", stdout.String())
	}
	specAfter, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec): %v", err)
	}
	if specAfter.Status != "closed" {
		t.Fatalf("spec status = %q, want closed", specAfter.Status)
	}
	workAfter, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if workAfter.Status != "open" {
		t.Fatalf("non-spec workflow bead status = %q, want open", workAfter.Status)
	}
}

func TestWispAutocloseSkipsGeneratedSpecsForClosedWorkflowChild(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title: "workflow child",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	spec, err := store.Create(beads.Bead{
		Title: "Step spec for review",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "review",
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	_ = store.Close(child.ID)

	var stdout bytes.Buffer
	doWispAutocloseWith(store, child.ID, &stdout)

	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want no generated spec cleanup message", stdout.String())
	}
	specAfter, err := store.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec): %v", err)
	}
	if specAfter.Status != "open" {
		t.Fatalf("spec status = %q, want open", specAfter.Status)
	}
}

func TestWispAutocloseReadsClosedWorkflowRootFromLiveHandle(t *testing.T) {
	mem := beads.NewMemStore()
	root, err := mem.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	spec, err := mem.Create(beads.Bead{
		Title: "Step spec for review",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "review",
		},
	})
	if err != nil {
		t.Fatalf("Create(spec): %v", err)
	}
	if err := mem.Close(root.ID); err != nil {
		t.Fatalf("Close(root): %v", err)
	}
	store := wrapStoreWithBeadPolicies(staleCachedWispStore{MemStore: mem}, &config.City{})

	var stdout bytes.Buffer
	doWispAutocloseWith(store, root.ID, &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed 1 generated spec bead(s) on "+root.ID) {
		t.Fatalf("stdout = %q, want generated spec cleanup message", stdout.String())
	}
	specAfter, err := mem.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec): %v", err)
	}
	if specAfter.Status != "closed" {
		t.Fatalf("spec status = %q, want closed", specAfter.Status)
	}
}

type staleCachedWispStore struct {
	*beads.MemStore
}

func (s staleCachedWispStore) Get(_ string) (beads.Bead, error) {
	return beads.Bead{}, beads.ErrCacheUnavailable
}

func (s staleCachedWispStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: s,
		Live:   s.MemStore,
		Writer: s.MemStore,
	}
}

func TestWispAutocloseTraversesChildrenViaLiveHandle(t *testing.T) {
	mem := beads.NewMemStore()
	_, _ = mem.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = mem.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = mem.Close("gc-1")
	store := tierNarrowListWispStore{MemStore: mem}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want auto-close message for live-listed child", stdout.String())
	}
	b, err := mem.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("wisp Status = %q, want closed", b.Status)
	}
}

// tierNarrowListWispStore returns no rows from raw List calls while its Live
// handle reads the full MemStore — the shape of a tier-narrow raw store that
// cannot see ephemeral-tier attachments. Autoclose child traversal must read
// through the Live handle to find them.
type tierNarrowListWispStore struct {
	*beads.MemStore
}

func (s tierNarrowListWispStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, nil
}

func (s tierNarrowListWispStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: s,
		Live:   s.MemStore,
		Writer: s.MemStore,
	}
}

func TestWispAutocloseSkipsAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("already-closed wisp should produce no output, got %q", stdout.String())
	}
}

func TestWispAutocloseSkipsNonMoleculeChildren(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "convoy", Type: "convoy"})               // gc-1
	_, _ = store.Create(beads.Bead{Title: "task", Type: "task", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("non-molecule children should produce no output, got %q", stdout.String())
	}

	b, _ := store.Get("gc-2")
	if b.Status != "open" {
		t.Errorf("non-molecule child Status = %q, want %q", b.Status, "open")
	}
}

func TestWispAutocloseNoChildren(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "lone bead"}) // gc-1
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if stdout.String() != "" {
		t.Errorf("no-children bead should produce no output, got %q", stdout.String())
	}
}

func TestWispAutocloseMultipleMolecules(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                  // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp A", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "wisp B", Type: "molecule", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	out := stdout.String()
	if !strings.Contains(out, "gc-2") || !strings.Contains(out, "gc-3") {
		t.Errorf("should close both wisps, got %q", out)
	}

	for _, id := range []string{"gc-2", "gc-3"} {
		b, _ := store.Get(id)
		if b.Status != "closed" {
			t.Errorf("wisp %s Status = %q, want %q", id, b.Status, "closed")
		}
	}
}

func TestWispAutocloseBeadNotFound(t *testing.T) {
	store := beads.NewMemStore()

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "nonexistent", &stdout)

	if stdout.String() != "" {
		t.Errorf("missing bead should produce no output, got %q", stdout.String())
	}
}
