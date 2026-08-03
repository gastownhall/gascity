package sling

import (
	"errors"
	"strings"
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
	if dec, err := onFormulaNeedsAttachment(SlingOpts{BeadOrFormula: routedRaw.ID}, store, deps); dec.NeedsAttach || err != nil {
		t.Errorf("plain sling: onFormulaNeedsAttachment = (%+v, %v), want NeedsAttach=false, nil", dec, err)
	}
	// --on on a routed-raw (unclaimed, no-molecule) bead must attach (the footgun).
	if dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: routedRaw.ID}, store, deps); !dec.NeedsAttach || err != nil {
		t.Errorf("routed-raw --on: onFormulaNeedsAttachment = (%+v, %v), want NeedsAttach=true, nil (no molecule => must attach)", dec, err)
	}

	// A CLAIMED bead (assignee set) with no molecule stays idempotent — do not
	// re-attach onto a worker's in-progress bead. The decision still reports the
	// claim so the caller can warn instead of silently no-op'ing the requested
	// formula attach.
	claimed, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Assignee: "worker",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create claimed: %v", err)
	}
	dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: claimed.ID}, store, deps)
	if dec.NeedsAttach || err != nil {
		t.Errorf("claimed --on: onFormulaNeedsAttachment = (%+v, %v), want NeedsAttach=false, nil (worker owns it, stay idempotent)", dec, err)
	}
	if !dec.SkippedForClaim || dec.Assignee != "worker" {
		t.Errorf("claimed --on: onFormulaNeedsAttachment = %+v, want SkippedForClaim=true, Assignee=%q", dec, "worker")
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
	if dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: bead.ID}, store, SlingDeps{Store: store}); !dec.NeedsAttach || err != nil {
		t.Fatalf("--on override should fire for a routed-raw bead with no molecule: got (%+v, %v)", dec, err)
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
	dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: "BL-1"}, store, deps)
	if dec.NeedsAttach || err != nil {
		t.Errorf("molecule-present --on: onFormulaNeedsAttachment = (%+v, %v), want NeedsAttach=false, nil (has molecule => stay idempotent)", dec, err)
	}
	if dec.SkippedForClaim {
		t.Errorf("molecule-present --on: onFormulaNeedsAttachment = %+v, want SkippedForClaim=false (molecule already present, not a claim skip)", dec)
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

	dec, err := onFormulaNeedsAttachment(SlingOpts{OnFormula: "code-review", BeadOrFormula: "BL-1"}, store, deps)
	if dec.NeedsAttach {
		t.Error("probe error: onFormulaNeedsAttachment NeedsAttach = true, want false (cannot prove no molecule => fail closed)")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("probe error: onFormulaNeedsAttachment err = %v, want %v surfaced", err, probeErr)
	}
}

// When resolveIdempotentShortCircuit stays idempotent specifically because the
// bead is claimed with no molecule attached, it must say so in a bead warning
// -- distinct from the generic "already routed" message -- rather than
// silently returning exit 0 with no indication that the requested --on
// formula was never attached. This pins ga-juszt2: the prior behavior gave no
// signal that --force was required to actually attach the formula.
func TestResolveIdempotentShortCircuitWarnsWhenOnFormulaSkippedForClaim(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID:       "BL-1",
			Type:     "task",
			Status:   "open",
			Assignee: "worker",
			Metadata: map[string]string{"gc.routed_to": "worker"},
		},
	}, nil)
	deps := SlingDeps{Store: store}
	// NoConvoy: true bypasses the separate convoy-tracking recovery check (a
	// parentless routed bead would otherwise read as needing finalize to
	// recreate a missing auto-convoy) so this test isolates the claim-skip
	// warning path under test rather than that unrelated mechanism.
	opts := SlingOpts{OnFormula: "mol-tdd-build", BeadOrFormula: "BL-1", Target: config.Agent{Name: "worker"}, NoConvoy: true}

	var result SlingResult
	shortCircuited := resolveIdempotentShortCircuit(opts, opts.Target, deps, store, &result)

	if !shortCircuited || !result.Idempotent {
		t.Fatalf("claimed bead, no molecule, --on: expected idempotent short-circuit, got shortCircuited=%v result=%+v", shortCircuited, result)
	}
	if len(result.BeadWarnings) != 1 {
		t.Fatalf("claimed bead, no molecule, --on: want exactly 1 bead warning, got %d: %+v", len(result.BeadWarnings), result.BeadWarnings)
	}
	warning := result.BeadWarnings[0]
	for _, want := range []string{"BL-1", "worker", "mol-tdd-build", "--force"} {
		if !strings.Contains(warning, want) {
			t.Errorf("claimed bead, no molecule, --on: warning %q does not mention %q", warning, want)
		}
	}
}
