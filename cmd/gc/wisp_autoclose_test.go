package main

import (
	"bytes"
	"strings"
	"testing"

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

func TestWispAutocloseClosesAttachedMoleculeDescendants(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "work item",
		Metadata: map[string]string{"molecule_id": "gc-2"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "molecule root", Type: "molecule"})        // gc-2
	_, _ = store.Create(beads.Bead{Title: "step", Type: "task", ParentID: "gc-2"})   // gc-3
	_, _ = store.Create(beads.Bead{Title: "nested", Type: "task", ParentID: "gc-3"}) // gc-4
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doWispAutocloseWith(store, "gc-1", &stdout)

	if !strings.Contains(stdout.String(), "Auto-closed molecule gc-2 on gc-1") {
		t.Fatalf("stdout = %q, want metadata auto-close message", stdout.String())
	}
	for _, id := range []string{"gc-2", "gc-3", "gc-4"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
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
