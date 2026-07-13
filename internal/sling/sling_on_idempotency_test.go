package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// A bead routed raw (gc.routed_to set, no molecule) by an earlier plain sling
// reads as Idempotent, which would silently no-op a later `--on <formula>`.
// onFormulaNeedsAttachment overrides that ONLY when there is no molecule to
// attach — so the footgun bead attaches, while a bead that already has a
// molecule (or a non---on sling) stays idempotent (preserving the retry
// contract and avoiding molecule churn).
func TestOnFormulaNeedsAttachment(t *testing.T) {
	store := beads.NewMemStore()
	routedRaw, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	deps := SlingDeps{Store: store}

	// A non---on sling never overrides idempotency.
	if onFormulaNeedsAttachment(SlingOpts{BeadOrFormula: routedRaw.ID}, store, deps) {
		t.Error("plain sling: onFormulaNeedsAttachment = true, want false")
	}
	// --on on a routed-raw (unclaimed, no-molecule) bead must attach (the footgun).
	if !onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: routedRaw.ID}, store, deps) {
		t.Error("routed-raw --on: onFormulaNeedsAttachment = false, want true (no molecule => must attach)")
	}

	// A CLAIMED bead (assignee set) with no molecule stays idempotent — do not
	// re-attach onto a worker's in-progress bead.
	claimed, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Assignee: "worker",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create claimed: %v", err)
	}
	if onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: claimed.ID}, store, deps) {
		t.Error("claimed --on: onFormulaNeedsAttachment = true, want false (worker owns it, stay idempotent)")
	}
}

// The footgun the fix addresses: a bead routed raw to the target (gc.routed_to
// set + a convoy) reads as Idempotent, so a plain sling no-ops. --on must not
// be gated on that when the bead has no molecule.
func TestRoutedRawBeadReadsIdempotentWhichOnFormulaMustOverride(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:    "repair root",
		Type:     "task",
		Status:   "open",
		ParentID: convoy.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	// routed-raw + convoy => CheckBeadState reports Idempotent (the trap).
	res := CheckBeadState(store, bead.ID, config.Agent{Name: "worker"}, SlingDeps{})
	if !res.Idempotent {
		t.Fatalf("routed-raw bead: expected Idempotent=true (the footgun), got %+v", res)
	}
	// ...and the --on override fires because there is no molecule.
	if !onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: bead.ID}, store, SlingDeps{Store: store}) {
		t.Fatal("--on override should fire for a routed-raw bead with no molecule")
	}
}
