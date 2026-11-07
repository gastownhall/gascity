package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestWispAutocloseClosesOpenMolecule(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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

// TestWispAutocloseLeavesAnOpenParentsAttachments is F6: a spurious
// bead.closed for a parent that is still open (a false scan close, a replay)
// must not close its attachments, finished or not.
func TestWispAutocloseLeavesAnOpenParentsAttachments(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item", Metadata: map[string]string{"molecule_id": "gc-3"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"})                      // gc-2
	_, _ = store.Create(beads.Bead{Title: "attached", Type: "molecule"})                                    // gc-3

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

	for _, id := range []string{"gc-2", "gc-3"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "open" {
			t.Errorf("attachment %s status = %q under an open parent, want open", id, b.Status)
		}
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// An open workflow root's generated spec sidecars are its live spec.
func TestWispAutocloseLeavesAnOpenWorkflowRootsSpecSidecars(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "workflow", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
	spec, _ := store.Create(beads.Bead{Title: "spec", Type: "spec", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}})

	var stdout bytes.Buffer
	doWispAutocloseWith(store, root.ID, &stdout, beads.GraphStore{Store: store})

	if got, _ := store.Get(spec.ID); got.Status != "open" {
		t.Fatalf("spec sidecar %s under an open workflow root, want open", got.Status)
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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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

	parked, err := attachedMoleculeIsParked(store, root)
	if !parked {
		t.Fatal("attachedMoleculeIsParked = false on subtree walk error, want true (fail-safe preserve)")
	}
	if err == nil {
		t.Fatal("attachedMoleculeIsParked hid the walk error; the autoclose run must see it to retry")
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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, root.ID, &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, child.ID, &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, root.ID, &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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
	doWispAutocloseWith(store, "gc-1", &stdout, beads.GraphStore{Store: store})

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

func TestWispAutocloseClosesRootOnlyWispViaInputConvoy(t *testing.T) {
	// Regression for the graph.v2 workflow-root finalize-gap (gastownhall/gascity
	// review-pool spawn-churn): a root-only graph.v2 workflow wisp -- e.g.
	// mol-focus-review routed to a pool -- runs every step in one worker session,
	// so it materializes no step beads and no workflow-finalize control bead to
	// close its root. sling clears gc.source_bead_id for graph workflows
	// (sling.go), so the wisp carries NO source-bead attachment for
	// collectAttachedBeads to follow. Its only durable link to the work issue is
	// gc.input_convoy_id -> the synthetic tracking convoy whose member is the
	// issue. When the formula's finalize step closes the issue, the on_close hook
	// must reap the orphaned root via that input-convoy link; otherwise the open
	// root re-routes to the pool and churns a fresh worker.
	store := beads.NewMemStore()
	issue, _ := store.Create(beads.Bead{Title: "work issue", Type: "task"}) // gc-1
	convoy, _ := store.Create(beads.Bead{
		Title:    "synthetic input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	}) // gc-2
	if err := store.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}
	root, _ := store.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
			beadmeta.RoutedToMetadataKey:        "/home/ds/gascity/polecat",
			"gc.var.issue":                      issue.ID,
		},
	}) // gc-3

	_ = store.Close(issue.ID)

	var stdout bytes.Buffer
	doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if rootAfter.Status != "closed" {
		t.Fatalf("root-only wisp status = %q, want closed (reaped via input convoy)", rootAfter.Status)
	}
	if !strings.Contains(stdout.String(), "Auto-closed workflow "+root.ID+" on "+issue.ID) {
		t.Fatalf("stdout = %q, want workflow auto-close message", stdout.String())
	}
}

// TestWispAutocloseClosesRootOnlyWispViaInputConvoyAcrossStores runs the
// root-only reap over both store topologies through the splitEnv fixture. On a
// split city the two halves of the input-convoy link live in DIFFERENT stores:
// the synthetic input convoy is WORK class (coordclass classifies EVERY convoy
// as work, and convoy.TrackItemIn keeps it co-resident with the issue it
// tracks), so the convoy + its tracks edge land in the work store — while the
// graph.v2 workflow root it launched is graph class and lives in the relocated
// binding. A reap that probes only the graph store for the tracking convoy
// finds no tracks edge, silently no-ops, and the open root outlives its closed
// issue — re-routing to the pool and churning a fresh worker (the review-pool
// spawn-churn finalize-gap, reopened for every split city).
func TestWispAutocloseClosesRootOnlyWispViaInputConvoyAcrossStores(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		issue, err := e.work.Create(beads.Bead{Title: "work issue", Type: "task"})
		if err != nil {
			t.Fatalf("create work issue: %v", err)
		}
		convoy, err := e.work.Create(beads.Bead{
			Title:    "synthetic input convoy",
			Type:     "convoy",
			Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
		})
		if err != nil {
			t.Fatalf("create synthetic input convoy in the work store: %v", err)
		}
		if err := e.work.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
			t.Fatalf("DepAdd(tracks) in the work store: %v", err)
		}
		root, err := e.graphStore().Create(beads.Bead{
			Title: "mol-focus-review",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
				beadmeta.RoutedToMetadataKey:        "review-pool/worker",
			},
		})
		if err != nil {
			t.Fatalf("create root-only workflow root through the graph front door: %v", err)
		}
		if !coordclass.Classify(root).IsInfrastructure() {
			t.Fatalf("root %s classifies as work, want infrastructure; the cross-store premise is not staged", root.ID)
		}

		if err := e.work.Close(issue.ID); err != nil {
			t.Fatalf("close issue: %v", err)
		}

		var stdout bytes.Buffer
		doWispAutocloseWith(e.work, issue.ID, &stdout, beads.GraphStore{Store: e.graphStore()})

		rootAfter, err := e.graphStore().Get(root.ID)
		if err != nil {
			t.Fatalf("Get(root): %v", err)
		}
		if rootAfter.Status != "closed" {
			t.Fatalf("root-only wisp status = %q, want closed (reaped via the work-store input convoy)", rootAfter.Status)
		}
		if !strings.Contains(stdout.String(), "Auto-closed workflow "+root.ID+" on "+issue.ID) {
			t.Fatalf("stdout = %q, want workflow auto-close message", stdout.String())
		}
	})
}

// TestWispAutocloseClosesRootOnlyWispViaGraphResidentInputConvoy guards against
// a "swap" fix instead of a union: a store migrated from the single-store era
// can hold the tracking convoy + its tracks edge on the GRAPH side (migrated
// beads keep their pre-split residence, and SQLite records the dangling edge
// rather than refusing it), while the issue stays in the work store. The reap
// must still find that convoy through the graph leg after it learns to probe
// the work leg.
func TestWispAutocloseClosesRootOnlyWispViaGraphResidentInputConvoy(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		issue, err := e.work.Create(beads.Bead{Title: "work issue", Type: "task"})
		if err != nil {
			t.Fatalf("create work issue: %v", err)
		}
		convoy, err := e.graphStore().Create(beads.Bead{
			Title:    "migrated input convoy",
			Type:     "convoy",
			Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
		})
		if err != nil {
			t.Fatalf("create migrated convoy through the graph front door: %v", err)
		}
		if err := e.graphStore().DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
			t.Fatalf("DepAdd(tracks) in the graph store: %v", err)
		}
		if e.split {
			// Claiming the recorded violation is the assertion: the class store
			// ACCEPTED the cross-store tracks edge, which is what SQLite does with
			// migrated-era data — this row is the production corruption the union
			// probe must still reap through.
			violations := splittest.TakeResidenceViolations(e.class)
			if len(violations) != 1 || violations[0].Op != "dep-add" {
				t.Fatalf("class store recorded violations %v, want exactly the one accepted cross-store tracks edge", violations)
			}
		}
		root, err := e.graphStore().Create(beads.Bead{
			Title: "mol-focus-review",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
				beadmeta.RoutedToMetadataKey:        "review-pool/worker",
			},
		})
		if err != nil {
			t.Fatalf("create root-only workflow root through the graph front door: %v", err)
		}

		if err := e.work.Close(issue.ID); err != nil {
			t.Fatalf("close issue: %v", err)
		}

		var stdout bytes.Buffer
		doWispAutocloseWith(e.work, issue.ID, &stdout, beads.GraphStore{Store: e.graphStore()})

		rootAfter, err := e.graphStore().Get(root.ID)
		if err != nil {
			t.Fatalf("Get(root): %v", err)
		}
		if rootAfter.Status != "closed" {
			t.Fatalf("migrated-convoy root-only wisp status = %q, want closed (reaped via the graph-store input convoy)", rootAfter.Status)
		}
	})
}

// TestWispAutocloseFailsClosedOnRefusedGraphBinding pins the refusal posture of
// the union probe. A city whose [storage] split this build must not serve gets
// its graph class delivered AS a store that refuses every operation
// (refusedClassStore, cli_storage_routes.go); the refusal itself is printed
// once to stderr when the verdict is taken, so the probe's whole job here is to
// fail CLOSED — reap nothing — rather than silently narrow to the work store.
// The decoy is the discriminator: a work-resident bead carrying the root shape
// (gc.kind=workflow, graph.v2, gc.input_convoy_id) that a narrowed implementation
// — one that falls back to the work store when the graph leg refuses — would
// find via the work-found convoy and force-close. It must stay open, and no
// looks-like-success auto-close line may be written.
func TestWispAutocloseFailsClosedOnRefusedGraphBinding(t *testing.T) {
	store := beads.NewMemStore()
	issue, _ := store.Create(beads.Bead{Title: "work issue", Type: "task"}) // gc-1
	convoy, _ := store.Create(beads.Bead{
		Title:    "synthetic input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	}) // gc-2
	if err := store.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}
	decoy, _ := store.Create(beads.Bead{
		Title: "work-resident decoy root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
		},
	}) // gc-3
	_ = store.Close(issue.ID)

	refused := refusedClassStore{err: standingStorageRefusal{
		err: errors.New("storage: this city's [storage] binding has not converged; run `gc storage migrate`"),
	}}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: refused})

	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want no auto-close output on a refused graph binding; a success line here is the looks-like-success answer the refusal exists to close", stdout.String())
	}
	decoyAfter, err := store.Get(decoy.ID)
	if err != nil {
		t.Fatalf("Get(decoy): %v", err)
	}
	if decoyAfter.Status != "open" {
		t.Fatalf("work-resident decoy root status = %q, want open; the reap silently narrowed the graph leg to the work store instead of failing closed on the refusal", decoyAfter.Status)
	}
}

// partialGraphViewStore refuses only the tracking-convoy probe for one bead,
// serving every other read from the embedded graph store. Unlike
// refusedClassStore (which refuses ListByMetadata too, so the root lookup
// blocks the decoy independently) and the package's blanket
// depListFailingStore, this is the shape that can tell a fail-closed
// implementation apart from one that narrows to the work leg.
type partialGraphViewStore struct {
	beads.Store
	failID string
}

func (s partialGraphViewStore) DepList(id, dir string) ([]beads.Dep, error) {
	if id == s.failID {
		return nil, fmt.Errorf("graph leg unavailable for %s", id)
	}
	return s.Store.DepList(id, dir)
}

// TestWispAutocloseFailsClosedOnPartialGraphView is the discriminator the
// refused-binding arm cannot be: only the graph leg's tracking-convoy probe
// fails, so every other graph read still serves. The work leg finds the
// tracking convoy and the graph store holds a reapable open graph.v2 root
// bound to it — so an implementation that narrowed a failed graph probe to the
// work leg would resolve that root through the still-serving ListByMetadata
// and force-close it. Failing closed instead leaves the root for a later
// close, which is the recoverable direction on a partial view.
func TestWispAutocloseFailsClosedOnPartialGraphView(t *testing.T) {
	workStore := beads.NewMemStore()
	graph := beads.NewMemStore()
	// Distinct mint prefixes so the two stores cannot alias ids: the graph
	// root must be reachable only through the convoy binding under test.
	graph.IDPrefix = "gx"

	issue, err := workStore.Create(beads.Bead{Title: "work issue", Type: "task"})
	if err != nil {
		t.Fatalf("create work issue: %v", err)
	}
	convoy, err := workStore.Create(beads.Bead{
		Title:    "synthetic input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create synthetic input convoy in the work store: %v", err)
	}
	if err := workStore.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks) in the work store: %v", err)
	}
	root, err := graph.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
			beadmeta.RoutedToMetadataKey:        "review-pool/worker",
		},
	})
	if err != nil {
		t.Fatalf("create graph-resident workflow root: %v", err)
	}
	if err := workStore.Close(issue.ID); err != nil {
		t.Fatalf("close issue: %v", err)
	}

	graphDouble := partialGraphViewStore{Store: graph, failID: issue.ID}

	var stdout bytes.Buffer
	doWispAutocloseWith(workStore, issue.ID, &stdout, beads.GraphStore{Store: graphDouble})

	rootAfter, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if rootAfter.Status != "open" {
		t.Fatalf("root status = %q, want open; the reap narrowed the failed graph probe to the work leg and force-closed a root it could not fully see", rootAfter.Status)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want no auto-close output on a partial graph view", stdout.String())
	}
}

// inputWorkflowFixture is a graph.v2 workflow dispatched over an input convoy:
// the tracked issues in the work store, the convoy and its tracks edges in
// convoyStore, and the root, an open step and a step that already finished
// in the graph store. The root is in_progress, as gc sling leaves every
// graph.v2 root it launches (sling.PromoteWorkflowLaunchBead).
type inputWorkflowFixture struct {
	work, convoyStore, graph beads.Store
	issues                   []beads.Bead
	convoyID                 string
	root, step, finished     beads.Bead
}

func newInputWorkflowFixture(t *testing.T, work, convoyStore, graph beads.Store, members int) inputWorkflowFixture {
	t.Helper()
	f := inputWorkflowFixture{work: work, convoyStore: convoyStore, graph: graph}
	conv, err := convoyStore.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	f.convoyID = conv.ID
	for i := 0; i < members; i++ {
		issue, err := work.Create(beads.Bead{Title: fmt.Sprintf("input %d", i), Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := convoyStore.DepAdd(conv.ID, issue.ID, "tracks"); err != nil {
			t.Fatalf("DepAdd(tracks): %v", err)
		}
		f.issues = append(f.issues, issue)
	}
	f.root, err = graph.Create(beads.Bead{Title: "orchestrated workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   conv.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sling.PromoteWorkflowLaunchBead(graph, f.root.ID); err != nil {
		t.Fatalf("promoting the launched root: %v", err)
	}
	if f.root, err = graph.Get(f.root.ID); err != nil {
		t.Fatal(err)
	}
	f.step, err = graph.Create(beads.Bead{Title: "idle step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
		beadmeta.RoutedToMetadataKey:   "worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.finished, err = graph.Create(beads.Bead{Title: "finished step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: f.root.ID,
		beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Close(f.finished.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

// closeInput closes issue i and runs the close hook for it.
func (f inputWorkflowFixture) closeInput(t *testing.T, i int, graph beads.Store) (autocloseRun, string) {
	t.Helper()
	if err := f.work.Close(f.issues[i].ID); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	run := doWispAutocloseWith(f.work, f.issues[i].ID, &stdout, beads.GraphStore{Store: graph})
	return run, stdout.String()
}

func (f inputWorkflowFixture) assertRetiredAsSkipped(t *testing.T) {
	t.Helper()
	for _, id := range []string{f.root.ID, f.step.ID} {
		got, err := f.graph.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped ||
			got.Metadata["close_reason"] != sourceworkflow.WorkflowSubtreeClosedReason {
			t.Fatalf("%s = %s %v; want closed as skipped by the subtree close", id, got.Status, got.Metadata)
		}
	}
	finished, err := f.graph.Get(f.finished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("finished step %s outcome = %q; a step that closed before keeps its disposition", f.finished.ID, finished.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

func (f inputWorkflowFixture) assertPreserved(t *testing.T) {
	t.Helper()
	for _, id := range []string{f.root.ID, f.step.ID} {
		got, err := f.graph.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" {
			t.Fatalf("%s = %s %v; the workflow must be preserved", id, got.Status, got.Metadata)
		}
	}
}

// TestWispAutocloseRetiresIdleInputWorkflowWhenItsLastInputCloses: a graph.v2
// workflow dispatched over an input convoy, with an idle, ungated step left
// open, has nothing to run once every input is terminal. The close of the
// last input retires its root and open steps as skipped from the close hook —
// no seat has to claim a step to discover it — while the close of an earlier
// input leaves it running. The convoy may sit beside the work (a post-split
// launch) or on the graph side (a single-store-era migration). The root is
// in_progress from its launch, which is no claim; a root that was never
// promoted retires the same way.
func TestWispAutocloseRetiresIdleInputWorkflowWhenItsLastInputCloses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stores   func() (work, convoyStore, graph beads.Store)
		openRoot bool
	}{
		{name: "single store", stores: func() (beads.Store, beads.Store, beads.Store) {
			s := beads.NewMemStore()
			return s, s, s
		}},
		{name: "split work and graph stores", stores: func() (beads.Store, beads.Store, beads.Store) {
			work := beads.NewMemStore()
			return work, work, &beads.MemStore{IDPrefix: "gcg"}
		}},
		{name: "convoy migrated onto the graph store", stores: func() (beads.Store, beads.Store, beads.Store) {
			graph := &beads.MemStore{IDPrefix: "gcg"}
			return beads.NewMemStore(), graph, graph
		}},
		{name: "root never promoted", openRoot: true, stores: func() (beads.Store, beads.Store, beads.Store) {
			s := beads.NewMemStore()
			return s, s, s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, convoyStore, graph := tc.stores()
			f := newInputWorkflowFixture(t, work, convoyStore, graph, 2)
			if tc.openRoot {
				updateInputWorkflowBead(t, graph, f.root.ID, beads.UpdateOpts{Status: strPtr("open")})
			}

			run, _ := f.closeInput(t, 0, graph)
			if !run.finished() {
				t.Fatalf("run after the first input closed = %+v, want finished", run)
			}
			f.assertPreserved(t)

			run, stdout := f.closeInput(t, 1, graph)
			if !run.finished() {
				t.Fatalf("run after the last input closed = %+v, want finished", run)
			}
			f.assertRetiredAsSkipped(t)
			if !strings.Contains(stdout, "Auto-closed workflow "+f.root.ID) {
				t.Fatalf("stdout = %q, want the retirement reported", stdout)
			}
		})
	}
}

// inputMembershipHookStore runs onMembers before each read of the input
// convoy's membership (its legacy-children List), so a test can fail that
// read or change the workflow between the decision and its live recheck.
type inputMembershipHookStore struct {
	beads.Store
	convoyID  string
	calls     int
	onMembers func(call int) error
}

func (s *inputMembershipHookStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID == s.convoyID && s.onMembers != nil {
		s.calls++
		if err := s.onMembers(s.calls); err != nil {
			return nil, err
		}
	}
	return s.Store.List(query)
}

// TestWispAutocloseRechecksIdleInputWorkflowLiveBeforeRetiring: the idle
// decision is read again, live, right before the subtree close. A step
// claimed in between keeps the workflow, and the run still finishes cleanly.
func TestWispAutocloseRechecksIdleInputWorkflowLiveBeforeRetiring(t *testing.T) {
	mem := beads.NewMemStore()
	hooked := &inputMembershipHookStore{Store: mem}
	f := newInputWorkflowFixture(t, mem, hooked, hooked, 1)
	f.work, f.graph = hooked, hooked
	hooked.convoyID = f.convoyID
	hooked.onMembers = func(call int) error {
		if call == 2 {
			return mem.Update(f.step.ID, beads.UpdateOpts{Assignee: strPtr("worker-1")})
		}
		return nil
	}

	run, _ := f.closeInput(t, 0, hooked)

	if hooked.calls != 2 {
		t.Fatalf("membership read %d times, want the decision and its live recheck", hooked.calls)
	}
	if !run.finished() {
		t.Fatalf("run = %+v, want finished", run)
	}
	f.assertPreserved(t)
}

// TestWispAutocloseKeepsAnInputWorkflowWhoseStepWasClaimedBehindItsCache: the
// controller runs the close hook over caching stores, and a seat's claim,
// written out of process, can still be missing from the cache when the last
// input closes. The idle check reads the workflow live, so it sees the claim
// and keeps the workflow instead of closing the claimed step as skipped.
func TestWispAutocloseKeepsAnInputWorkflowWhoseStepWasClaimedBehindItsCache(t *testing.T) {
	backing := beads.NewMemStore()
	f := newInputWorkflowFixture(t, backing, backing, backing, 1)
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Update(f.step.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-1")}); err != nil {
		t.Fatal(err)
	}
	if err := cached.Close(f.issues[0].ID); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	run := doWispAutocloseWith(cached, f.issues[0].ID, &stdout, beads.GraphStore{Store: cached})

	if !run.finished() {
		t.Fatalf("run = %+v, want finished", run)
	}
	f.assertPreserved(t)
	if got, err := backing.Get(f.step.ID); err != nil || got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("claimed step = %s %q (err=%v); want it still in_progress with worker-1", got.Status, got.Assignee, err)
	}
}

// TestWispAutocloseKeepsAnInputWorkflowWhoseInputChangedBehindItsCache: an
// input tracked by the convoy, or reopened, out of process can still be
// missing from the controller's cache when the last input it knows of closes.
// The idle decision reads the input convoy's membership live, its tracks
// edges and every member's status, so it sees that input open and keeps the
// workflow. The same write through the cache is the control.
func TestWispAutocloseKeepsAnInputWorkflowWhoseInputChangedBehindItsCache(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members int
		// change writes, through writer, the input that keeps the workflow
		// open, and returns its id.
		change func(t *testing.T, f inputWorkflowFixture, cached *beads.CachingStore, writer beads.Store) string
	}{
		{name: "input tracked", members: 1, change: func(t *testing.T, f inputWorkflowFixture, cached *beads.CachingStore, writer beads.Store) string {
			if _, err := cached.DepList(f.convoyID, "down"); err != nil {
				t.Fatal(err)
			}
			added, err := f.work.Create(beads.Bead{Title: "input tracked later", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.DepAdd(f.convoyID, added.ID, "tracks"); err != nil {
				t.Fatal(err)
			}
			return added.ID
		}},
		{name: "input reopened", members: 2, change: func(t *testing.T, f inputWorkflowFixture, cached *beads.CachingStore, writer beads.Store) string {
			if err := cached.Close(f.issues[1].ID); err != nil {
				t.Fatal(err)
			}
			if err := writer.Reopen(f.issues[1].ID); err != nil {
				t.Fatal(err)
			}
			return f.issues[1].ID
		}},
	} {
		for _, behindCache := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/behind cache=%t", tc.name, behindCache), func(t *testing.T) {
				backing := beads.NewMemStore()
				f := newInputWorkflowFixture(t, backing, backing, backing, tc.members)
				cached := beads.NewCachingStoreForTest(backing, nil)
				if err := cached.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
				var writer beads.Store = cached
				if behindCache {
					writer = backing
				}
				openInput := tc.change(t, f, cached, writer)
				if err := cached.Close(f.issues[0].ID); err != nil {
					t.Fatal(err)
				}

				var stdout bytes.Buffer
				run := doWispAutocloseWith(cached, f.issues[0].ID, &stdout, beads.GraphStore{Store: cached})

				if !run.finished() {
					t.Fatalf("run = %+v, want finished", run)
				}
				if root, err := backing.Get(f.root.ID); err != nil || root.Status == "closed" {
					t.Fatalf("root = %s %v (err=%v); want it kept while input %s is open (stdout=%q)", root.Status, root.Metadata, err, openInput, stdout.String())
				}
				f.assertPreserved(t)
			})
		}
	}
}

// TestWispAutocloseRetiresIdleInputWorkflowButLeavesItsTeardownTailToRun: the
// close of the last input retires the root and every open step but the
// teardown tail — a retry control scoped gc.scope_role=teardown and the
// attempt minted from it without the scope role — which runs after the root
// settles by contract and releases the workflow's resources. The settled
// workflow's input is not judged again at the next claim of the attempt,
// which the stop gate serves.
func TestWispAutocloseRetiresIdleInputWorkflowButLeavesItsTeardownTailToRun(t *testing.T) {
	store := beads.NewMemStore()
	f := newInputWorkflowFixture(t, store, store, store, 1)
	control, attempt := seedTeardownTail(t, store, f.root.ID)

	run, _ := f.closeInput(t, 0, store)

	if !run.finished() {
		t.Fatalf("run = %+v, want finished", run)
	}
	f.assertRetiredAsSkipped(t)
	assertNotRetired(t, store, map[string]string{control.ID: "open", attempt.ID: "open"})
	requireTeardownServed(t, store, attempt, nil)
}

// TestWispAutoclosePreservesInputWorkflowThatIsHeldGatedOrUnresolved: the close
// of the last input retires an input-convoy workflow only when nothing in it
// is held or gated and its whole input resolves terminal. Anything else keeps
// it, and that clean preservation finishes the run. A step whose Type is gate
// but declares no gc.gate_type is deferred routing, not a gate, and an open
// spec sidecar is not executable: neither keeps the workflow.
func TestWispAutoclosePreservesInputWorkflowThatIsHeldGatedOrUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name        string
		members     int
		mutate      func(t *testing.T, f inputWorkflowFixture)
		wantRetired bool
	}{
		{name: "step assigned", mutate: func(t *testing.T, f inputWorkflowFixture) {
			updateInputWorkflowBead(t, f.graph, f.step.ID, beads.UpdateOpts{Assignee: strPtr("worker-1")})
		}},
		{name: "step in progress", mutate: func(t *testing.T, f inputWorkflowFixture) {
			updateInputWorkflowBead(t, f.graph, f.step.ID, beads.UpdateOpts{Status: strPtr("in_progress")})
		}},
		{name: "step declares a gate", mutate: func(t *testing.T, f inputWorkflowFixture) {
			updateInputWorkflowBead(t, f.graph, f.step.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.GateTypeMetadataKey: "human"}})
		}},
		{name: "root assigned", mutate: func(t *testing.T, f inputWorkflowFixture) {
			updateInputWorkflowBead(t, f.graph, f.root.ID, beads.UpdateOpts{Assignee: strPtr("worker-1")})
		}},
		{name: "input tracks an unresolved member", mutate: func(t *testing.T, f inputWorkflowFixture) {
			if err := f.convoyStore.DepAdd(f.convoyID, "gc-404", "tracks"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "another input still open", members: 2},
		{name: "routing-only gate step", wantRetired: true, mutate: func(t *testing.T, f inputWorkflowFixture) {
			updateInputWorkflowBead(t, f.graph, f.step.ID, beads.UpdateOpts{Type: strPtr("gate")})
		}},
		{name: "open spec sidecar", wantRetired: true, mutate: func(t *testing.T, f inputWorkflowFixture) {
			if _, err := f.graph.Create(beads.Bead{Title: "spec", Type: "task", Assignee: "worker-1", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: f.root.ID,
				beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			members := tc.members
			if members == 0 {
				members = 1
			}
			f := newInputWorkflowFixture(t, store, store, store, members)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}

			run, _ := f.closeInput(t, 0, store)

			if !run.finished() {
				t.Fatalf("run = %+v, want finished", run)
			}
			if tc.wantRetired {
				f.assertRetiredAsSkipped(t)
			} else {
				f.assertPreserved(t)
			}
		})
	}
}

// TestWispAutocloseKeepsAnInputWorkflowWithNoStepLeftToClaim: the close of the
// last input retires a workflow to spare a seat a claim that would only find
// the work done. A workflow with no open step a seat would claim is left to the
// dispatcher, which drives its controls open and unassigned until its
// finalizer settles the root. That is the state a workflow is in when its last
// step closed the input and then itself before the close hook ran.
func TestWispAutocloseKeepsAnInputWorkflowWithNoStepLeftToClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed adds the open members beside the finalizer and returns their
		// ids with the status each must keep.
		seed func(t *testing.T, store beads.Store, rootID string) map[string]string
	}{
		{name: "only the finalizer"},
		{name: "the finalizer and a scope latch", seed: func(t *testing.T, store beads.Store, rootID string) map[string]string {
			scope := createInputWorkflowMember(t, store, rootID, "body scope", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope})
			return map[string]string{scope.ID: "open"}
		}},
		{name: "the finalizer and the teardown tail", seed: func(t *testing.T, store beads.Store, rootID string) map[string]string {
			control, attempt := seedTeardownTail(t, store, rootID)
			return map[string]string{control.ID: "open", attempt.ID: "open"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			f := newInputWorkflowFixture(t, store, store, store, 1)
			updateInputWorkflowBead(t, store, f.step.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}})
			if err := store.Close(f.step.ID); err != nil {
				t.Fatal(err)
			}
			finalize := createInputWorkflowMember(t, store, f.root.ID, "workflow-finalize", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize})
			kept := map[string]string{f.root.ID: "in_progress", finalize.ID: "open"}
			if tc.seed != nil {
				for id, status := range tc.seed(t, store, f.root.ID) {
					kept[id] = status
				}
			}

			run, stdout := f.closeInput(t, 0, store)

			if !run.finished() {
				t.Fatalf("run = %+v, want finished", run)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing retired", stdout)
			}
			assertNotRetired(t, store, kept)
		})
	}
}

// TestWispAutocloseKeepsAnInputWorkflowWhoseDrainIsInFlight: a workflow that
// drains its own input convoy closes that input itself. Each drained member's
// item workflow runs under a root of its own, outside the workflow's members,
// and its last step closes the member. From the drain's expansion until it
// records its items' outcomes, the close of the last input keeps the
// workflow, the step after the drain included, and the item workflow whose
// step still runs. A pending drain has minted no item workflow and would
// expand over no open input, so that workflow retires with its idle step.
func TestWispAutocloseKeepsAnInputWorkflowWhoseDrainIsInFlight(t *testing.T) {
	for _, tc := range []struct {
		state       string
		wantRetired bool
	}{
		{state: beadmeta.DrainStatePending, wantRetired: true},
		{state: beadmeta.DrainStateExpanding},
		{state: beadmeta.DrainStateExpanded},
		{state: beadmeta.DrainStateCompleting},
	} {
		t.Run(tc.state, func(t *testing.T) {
			store := beads.NewMemStore()
			// f.step is the step after the drain, open and unassigned while
			// the drain waits on its items.
			f := newInputWorkflowFixture(t, store, store, store, 1)
			drain := createInputWorkflowMember(t, store, f.root.ID, "drain", map[string]string{
				beadmeta.KindMetadataKey:       beadmeta.KindDrain,
				beadmeta.DrainStateMetadataKey: tc.state,
			})
			finalize := createInputWorkflowMember(t, store, f.root.ID, "workflow-finalize", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize})

			if tc.wantRetired {
				run, _ := f.closeInput(t, 0, store)
				if !run.finished() {
					t.Fatalf("run = %+v, want finished", run)
				}
				f.assertRetiredAsSkipped(t)
				for _, id := range []string{drain.ID, finalize.ID} {
					if got, err := store.Get(id); err != nil || got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
						t.Fatalf("%s = %s %v (err=%v); want closed as skipped with the workflow", id, got.Status, got.Metadata, err)
					}
				}
				return
			}
			itemRoot, itemStep := seedDrainItemWorkflow(t, store, drain.ID, f.issues[0].ID)

			run, stdout := f.closeInput(t, 0, store)

			if !run.finished() {
				t.Fatalf("run = %+v, want finished", run)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing retired while the drain drives its items", stdout)
			}
			assertNotRetired(t, store, map[string]string{
				f.root.ID:   "in_progress",
				f.step.ID:   "open",
				drain.ID:    "open",
				finalize.ID: "open",
				itemRoot.ID: "open",
				itemStep.ID: "in_progress",
			})
		})
	}
}

// createInputWorkflowMember creates an open member of the workflow rootID in
// store, with metadata beside its gc.root_bead_id.
func createInputWorkflowMember(t *testing.T, store beads.Store, rootID, title string, metadata map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}
	for k, v := range metadata {
		meta[k] = v
	}
	member, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: meta})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return member
}

// seedDrainItemWorkflow adds, in store, the item workflow drainID minted for
// memberID as a drain mints it: a unit convoy tracking the member, and a
// graph.v2 root of its own reading that convoy and linked to the drain, with
// the step that closes the member claimed and in progress.
func seedDrainItemWorkflow(t *testing.T, store beads.Store, drainID, memberID string) (root, step beads.Bead) {
	t.Helper()
	unit, err := store.Create(beads.Bead{Title: "drain unit 0 for " + memberID, Type: "convoy", Metadata: map[string]string{
		beadmeta.SyntheticMetadataKey:      "true",
		beadmeta.DrainControlIDMetadataKey: drainID,
		beadmeta.DrainMemberIDMetadataKey:  memberID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(unit.ID, memberID, "tracks"); err != nil {
		t.Fatal(err)
	}
	root, err = store.Create(beads.Bead{Title: "item workflow root", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.InputConvoyIDMetadataKey:   unit.ID,
		beadmeta.DrainControlIDMetadataKey:  drainID,
		beadmeta.DrainMemberIDMetadataKey:   memberID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	step = createInputWorkflowMember(t, store, root.ID, "close-source-anchor", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
	updateInputWorkflowBead(t, store, step.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-2")})
	if step, err = store.Get(step.ID); err != nil {
		t.Fatal(err)
	}
	return root, step
}

func updateInputWorkflowBead(t *testing.T, store beads.Store, id string, opts beads.UpdateOpts) {
	t.Helper()
	if err := store.Update(id, opts); err != nil {
		t.Fatalf("Update(%s): %v", id, err)
	}
}

// inputDiscoveryFailingStore fails the lookup of workflow roots by input
// convoy, the discovery read of the input-convoy branch.
type inputDiscoveryFailingStore struct{ beads.Store }

func (s inputDiscoveryFailingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[beadmeta.InputConvoyIDMetadataKey] != "" {
		return nil, errors.New("input-convoy root lookup unavailable")
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// closeAllFailingStore fails every batch close, as the subtree close writes.
type closeAllFailingStore struct{ beads.Store }

func (closeAllFailingStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return 0, errors.New("batch close unavailable")
}

// TestWispAutocloseLeavesInputWorkflowUnfinishedOnAReadOrCloseError: a failed
// discovery, decision, or close read leaves an otherwise idle input-convoy
// workflow open and the run unfinished, so the controller retries it.
func TestWispAutocloseLeavesInputWorkflowUnfinishedOnAReadOrCloseError(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(mem beads.Store) beads.Store
	}{
		{name: "discovery read fails", wrap: func(mem beads.Store) beads.Store { return inputDiscoveryFailingStore{mem} }},
		{name: "membership read fails", wrap: func(mem beads.Store) beads.Store {
			return &inputMembershipHookStore{Store: mem, onMembers: func(int) error { return errors.New("membership unavailable") }}
		}},
		{name: "subtree close fails", wrap: func(mem beads.Store) beads.Store { return closeAllFailingStore{mem} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			store := tc.wrap(mem)
			f := newInputWorkflowFixture(t, store, store, store, 1)
			if hooked, ok := store.(*inputMembershipHookStore); ok {
				hooked.convoyID = f.convoyID
			}

			run, _ := f.closeInput(t, 0, store)

			if run.finished() {
				t.Fatalf("run = %+v, want unfinished so the close is retried", run)
			}
			f.assertPreserved(t)
		})
	}
}

func TestWispAutocloseLeavesLegacyWorkflowRootViaInputConvoy(t *testing.T) {
	// The input-convoy reap is graph.v2-only by contract: a legacy
	// gc.kind=workflow root keeps its gc.source_bead_id attachment and is reaped
	// via collectAttachedBeads, so it must not be force-closed through the
	// input-convoy path. A root carrying only the legacy label (no
	// gc.formula_contract=graph.v2) and an input-convoy link stays open here.
	store := beads.NewMemStore()
	issue, _ := store.Create(beads.Bead{Title: "work issue", Type: "task"})
	convoy, _ := store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := store.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}
	root, _ := store.Create(beads.Bead{
		Title: "legacy workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          "workflow",
			beadmeta.InputConvoyIDMetadataKey: convoy.ID,
		},
	})

	_ = store.Close(issue.ID)

	var stdout bytes.Buffer
	doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if rootAfter.Status != "open" {
		t.Fatalf("legacy workflow root status = %q, want open (not reaped via input convoy)", rootAfter.Status)
	}
}

func TestWispAutocloseBeadNotFound(t *testing.T) {
	store := beads.NewMemStore()

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "nonexistent", &stdout, beads.GraphStore{Store: store})

	if stdout.String() != "" {
		t.Errorf("missing bead should produce no output, got %q", stdout.String())
	}
}
