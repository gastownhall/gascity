package sling

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// listErrStore wraps a Store but fails List, so the attachment probe in
// CollectAttachedBeads returns an error with no attachments discovered. Get
// still delegates to the embedded store, so a parent bead is found before the
// probe runs -- the fixture models a transient store failure hit only while
// enumerating a bead's molecule/workflow children.
type listErrStore struct {
	beads.Store
	err error
}

func (s listErrStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

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
	if need, err := onFormulaNeedsAttachment(SlingOpts{BeadOrFormula: routedRaw.ID}, store, deps); need || err != nil {
		t.Errorf("plain sling: onFormulaNeedsAttachment = (%v, %v), want (false, nil)", need, err)
	}
	// --on on a routed-raw (unclaimed, no-molecule) bead must attach (the footgun).
	if need, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: routedRaw.ID}, store, deps); !need || err != nil {
		t.Errorf("routed-raw --on: onFormulaNeedsAttachment = (%v, %v), want (true, nil) (no molecule => must attach)", need, err)
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
	if need, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: claimed.ID}, store, deps); need || err != nil {
		t.Errorf("claimed --on: onFormulaNeedsAttachment = (%v, %v), want (false, nil) (worker owns it, stay idempotent)", need, err)
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
	if need, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: bead.ID}, store, SlingDeps{Store: store}); !need || err != nil {
		t.Fatalf("--on override should fire for a routed-raw bead with no molecule: got (%v, %v)", need, err)
	}
}

// A routed-raw bead that ALREADY has a live molecule child must stay idempotent
// under `--on`: the override fires only when there is no molecule to attach, so
// re-slinging the same formula is a no-op rather than a re-attach. This pins the
// idempotent-retry-preservation branch that a prior over-broad approach
// regressed — asserted here directly rather than only in the doc comment.
func TestOnFormulaNeedsAttachmentMoleculePresentStaysIdempotent(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-1"},
	}, nil)
	deps := SlingDeps{Store: store}
	if need, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: "BL-1"}, store, deps); need || err != nil {
		t.Errorf("molecule-present --on: onFormulaNeedsAttachment = (%v, %v), want (false, nil) (has molecule => stay idempotent)", need, err)
	}
}

// If the molecule-attachment probe cannot complete, onFormulaNeedsAttachment must
// NOT report that the bead needs attachment. A swallowed probe error previously
// looked identical to "no molecule", which would flip a fail-closed idempotent
// routed bead into a mutating attach path and risk a duplicate formula/workflow
// attachment. On a probe error it must return (false, err) so the caller
// preserves the idempotent state.
func TestOnFormulaNeedsAttachmentProbeErrorStaysIdempotent(t *testing.T) {
	mem := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}, nil)
	probeErr := errors.New("store unavailable")
	store := listErrStore{Store: mem, err: probeErr}
	deps := SlingDeps{Store: store}

	need, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: "BL-1"}, store, deps)
	if need {
		t.Error("probe error: onFormulaNeedsAttachment = true, want false (cannot prove no molecule => fail closed)")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("probe error: onFormulaNeedsAttachment err = %v, want %v surfaced", err, probeErr)
	}
}
