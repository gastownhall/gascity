package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
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

// seedInputConvoyWorkflow seeds one open graph.v2 workflow root over a
// synthetic input convoy tracking one work issue, plus one open idle step
// under the root, in a fresh single-store MemStore. Rows mutate the step or
// the membership to the shape under test and close the issue themselves.
func seedInputConvoyWorkflow(t *testing.T) (store *beads.MemStore, issue, convoy, root, step beads.Bead) {
	t.Helper()
	store = beads.NewMemStore()
	var err error
	issue, err = store.Create(beads.Bead{Title: "convoy member", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	convoy, err = store.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd(tracks): %v", err)
	}
	root, err = store.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err = store.Create(beads.Bead{
		Title:    "idle step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, issue, convoy, root, step
}

// The idle-retirement arm of the input-convoy reap (#8729): an open graph.v2
// workflow whose input convoy's membership is fully terminal and whose
// non-terminal members are all idle (unassigned, open) and ungated has nothing
// left to run — every step would dispose against input that no longer exists,
// one burned seat poll each. The close hook retires it at close time instead:
// root and open steps close skipped under the subtree close reason the
// controller recognizes, and a terminal step keeps its own disposition. This
// row replaces the pre-fix preservation of the same shape, which kept the
// workflow for a workflow-finalize control bead that a purged-input launch
// never runs.
func TestWispAutocloseRetiresIdleWorkflowViaInputConvoyWhenInputTerminal(t *testing.T) {
	store, issue, _, root, step := seedInputConvoyWorkflow(t)
	done, err := store.Create(beads.Bead{
		Title: "finished step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	_ = store.Close(issue.ID)

	var stdout bytes.Buffer
	doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" ||
			got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped ||
			got.Metadata["close_reason"] != sourceworkflow.WorkflowSubtreeClosedReason {
			t.Fatalf("%s: status=%q outcome=%q close_reason=%q; want closed/%s/%s (retired at close time, no seat poll)",
				id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata["close_reason"],
				beadmeta.OutcomeSkipped, sourceworkflow.WorkflowSubtreeClosedReason)
		}
	}
	gotDone, err := store.Get(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDone.Status != "closed" || gotDone.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("finished step: status=%q outcome=%q; a terminal step keeps its own disposition",
			gotDone.Status, gotDone.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

// The idle retirement over both convoy residences: a post-split launch keeps
// the convoy and its tracks edges in the work store beside the issue, while a
// store migrated from the single-store era holds them on the graph side. The
// arm reads membership through whichever store owns the edges, and the
// retirement lands in the graph store the workflow lives in.
func TestWispAutocloseRetiresIdleWorkflowAcrossStores(t *testing.T) {
	seed := func(t *testing.T, convoyInWork bool) (work, graph *beads.MemStore, issue, root, step beads.Bead) {
		t.Helper()
		work = beads.NewMemStore()
		graph = beads.NewMemStore()
		// Distinct mint prefixes so the two stores cannot alias ids.
		graph.IDPrefix = "gx"
		var err error
		issue, err = work.Create(beads.Bead{Title: "convoy member", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		convoyHome := work
		if !convoyInWork {
			convoyHome = graph
		}
		convoy, err := convoyHome.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
		if err != nil {
			t.Fatal(err)
		}
		if err := convoyHome.DepAdd(convoy.ID, issue.ID, "tracks"); err != nil {
			t.Fatalf("DepAdd(tracks): %v", err)
		}
		root, err = graph.Create(beads.Bead{
			Title: "workflow root",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
				beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				beadmeta.InputConvoyIDMetadataKey:   convoy.ID,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		step, err = graph.Create(beads.Bead{
			Title:    "idle step",
			Type:     "task",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := work.Close(issue.ID); err != nil {
			t.Fatal(err)
		}
		return work, graph, issue, root, step
	}
	assertRetired := func(t *testing.T, graph *beads.MemStore, ids ...string) {
		t.Helper()
		for _, id := range ids {
			got, err := graph.Get(id)
			if err != nil {
				t.Fatalf("Get(%s): %v", id, err)
			}
			if got.Status != "closed" ||
				got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped ||
				got.Metadata["close_reason"] != sourceworkflow.WorkflowSubtreeClosedReason {
				t.Fatalf("%s: status=%q outcome=%q close_reason=%q; want closed/%s/%s",
					id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], got.Metadata["close_reason"],
					beadmeta.OutcomeSkipped, sourceworkflow.WorkflowSubtreeClosedReason)
			}
		}
	}
	t.Run("convoy in the work store", func(t *testing.T) {
		work, graph, issue, root, step := seed(t, true)
		var stdout bytes.Buffer
		doWispAutocloseWith(work, issue.ID, &stdout, beads.GraphStore{Store: graph})
		assertRetired(t, graph, root.ID, step.ID)
		if src, err := work.Get(issue.ID); err != nil || src.Status != "closed" {
			t.Errorf("issue %s: status=%q err=%v; the retirement must not touch the work store", issue.ID, src.Status, err)
		}
	})
	t.Run("convoy migrated to the graph store", func(t *testing.T) {
		work, graph, issue, root, step := seed(t, false)
		var stdout bytes.Buffer
		doWispAutocloseWith(work, issue.ID, &stdout, beads.GraphStore{Store: graph})
		assertRetired(t, graph, root.ID, step.ID)
	})
}

// A step held by a seat — assigned, claimed to in_progress, or otherwise off
// the open status — is work in flight: the input closing may be that seat's
// own result, so the close hook preserves the workflow and the holder's
// release, finalization, or a later claim progresses it.
func TestWispAutoclosePreservesHeldWorkflowStepsViaInputConvoy(t *testing.T) {
	t.Run("assigned", func(t *testing.T) {
		store, issue, _, root, step := seedInputConvoyWorkflow(t)
		assignee := "worker-1"
		if err := store.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		for _, id := range []string{root.ID, step.ID} {
			got, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "open" {
				t.Fatalf("%s: status=%q; a held workflow is preserved for its seat", id, got.Status)
			}
		}
	})
	t.Run("in-progress", func(t *testing.T) {
		store, issue, _, root, step := seedInputConvoyWorkflow(t)
		progress := "in_progress"
		if err := store.Update(step.ID, beads.UpdateOpts{Status: &progress}); err != nil {
			t.Fatal(err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		got, err := store.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" {
			t.Fatalf("root: status=%q; a status other than open is work in flight, preserved", got.Status)
		}
	})
}

// gc.gate_type names a declared gate: a gated subtree is a checkpoint that
// outlives its input and must not be steamrolled at close time. Type=gate
// alone is deferred routing, not a gate (internal/molecule/molecule.go), so a
// gate-typed step without gc.gate_type still retires with its workflow.
func TestWispAutoclosePreservesGatedWorkflowStepsViaInputConvoy(t *testing.T) {
	t.Run("declared gate", func(t *testing.T) {
		store, issue, _, root, step := seedInputConvoyWorkflow(t)
		if err := store.SetMetadata(step.ID, beadmeta.GateTypeMetadataKey, "human"); err != nil {
			t.Fatal(err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		for _, id := range []string{root.ID, step.ID} {
			got, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "open" {
				t.Fatalf("%s: status=%q; a declared gate is a checkpoint the close hook must preserve", id, got.Status)
			}
		}
	})
	t.Run("gate type without a declared gate", func(t *testing.T) {
		store, issue, _, root, _ := seedInputConvoyWorkflow(t)
		routing, err := store.Create(beads.Bead{
			Title:    "routing bead",
			Type:     "gate",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		for _, id := range []string{root.ID, routing.ID} {
			got, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Fatalf("%s: status=%q; Type=gate without gc.gate_type is deferred routing, not a checkpoint, and retires", id, got.Status)
			}
		}
	})
}

// The retirement arm reads the convoy's live membership: input that is not
// fully terminal — an open member, or a member that resolves to nothing (an
// unresolved placeholder over a dangling tracks edge) — keeps the workflow.
// Unresolved is a question for the claim-time purge probe, not the close hook.
func TestWispAutoclosePreservesWorkflowViaInputConvoyWhileInputNotTerminal(t *testing.T) {
	t.Run("open member", func(t *testing.T) {
		store, issue, convoy, root, _ := seedInputConvoyWorkflow(t)
		second, err := store.Create(beads.Bead{Title: "still-open member", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, second.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		got, err := store.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" {
			t.Fatalf("root: status=%q; an open input member keeps the workflow live", got.Status)
		}
	})
	t.Run("unresolved member", func(t *testing.T) {
		store, issue, convoy, root, _ := seedInputConvoyWorkflow(t)
		other, err := store.Create(beads.Bead{Title: "purged member", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, other.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(other.ID); err != nil {
			t.Fatalf("purging the other tracked member: %v", err)
		}
		_ = store.Close(issue.ID)

		var stdout bytes.Buffer
		doWispAutocloseWith(store, issue.ID, &stdout, beads.GraphStore{Store: store})

		got, err := store.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" {
			t.Fatalf("root: status=%q; unresolved membership is the claim-time purge probe's question, not a close-time retirement", got.Status)
		}
	})
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
