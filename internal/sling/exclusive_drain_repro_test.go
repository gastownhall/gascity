package sling

// Reproduction of the dip-tlbizd incident (2026-07-16): a drain declaring
// gc.drain_member_access: exclusive expanded over member anchor dip-3812vr
// while the PL's post-exhaustion re-dispatch launched a second, independent
// workflow over the SAME anchor. Both lanes ran in one shared build tree,
// wrote duplicate ~450-line implementations, and interleaved commits so badly
// that git attribution across three SHAs became unreliable. "Exclusive" did
// not exclude.
//
// These tests drive the REAL drain machinery (internal/dispatch.ProcessControl)
// to produce the reservation, then the REAL sling entry point (DoSling) against
// the reserved member — the two dispatch entry points, end to end, in one
// store. They are the proof asked for by sc-471fl5: not formula-reading, an
// executed refusal.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/runtime"
)

// graph.v2 formula compilation defaults on (set in the formula package's init),
// and no test in this package turns it off, so these tests rely on that default
// rather than the legacy process-global formula_v2 test setters — the coupling
// internal/testenv/legacy_flag_freeze_test.go freezes.

// writeReproDrainItemFormula writes the minimal graph.v2 item formula a drain
// instantiates per member.
func writeReproDrainItemFormula(t *testing.T, dir string) {
	t.Helper()
	content := `formula = "drain-item"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "work"
title = "Work {{convoy_id}}"
`
	if err := os.WriteFile(filepath.Join(dir, "drain-item.formula.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write drain item formula: %v", err)
	}
}

// seedExclusiveDrain builds the incident's graph shape: a parent convoy
// tracking one member anchor, a graph.v2 workflow root over that convoy, and a
// drain control declaring exclusive member access. It returns the store, the
// drain control, and the member anchor ID.
func seedExclusiveDrain(t *testing.T) (*beads.MemStore, beads.Bead, string) {
	t.Helper()
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "parent", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Title: "member anchor", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItem(store, parent.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drain, err := store.Create(beads.Bead{
		Title: "Drain continuation implementation convoy",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                "drain",
			"gc.root_bead_id":        root.ID,
			"gc.drain_context":       "separate",
			"gc.drain_formula":       "drain-item",
			"gc.drain_member_access": "exclusive",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, drain, member.ID
}

// expandExclusiveDrain runs the drain to expansion and returns it reloaded.
func expandExclusiveDrain(t *testing.T, store *beads.MemStore, drain beads.Bead) beads.Bead {
	t.Helper()
	dir := t.TempDir()
	writeReproDrainItemFormula(t, dir)
	if _, err := dispatch.ProcessControl(store, drain, dispatch.ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(drain expand): %v", err)
	}
	reloaded, err := store.Get(drain.ID)
	if err != nil {
		t.Fatalf("reload drain: %v", err)
	}
	return reloaded
}

func mustReservation(t *testing.T, store *beads.MemStore, memberID string) string {
	t.Helper()
	member, err := store.Get(memberID)
	if err != nil {
		t.Fatalf("reload member: %v", err)
	}
	return strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"])
}

func reproSlingDeps(t *testing.T, store beads.Store) (SlingDeps, config.Agent, *fakeRunner) {
	t.Helper()
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store
	return deps, config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, runner
}

// TestExclusiveDrainReservationRefusesFreshDispatch is the incident
// reproduction. Entry point 1 (drain-unit dispatch) takes the reservation;
// entry point 2 (fresh/root dispatch through DoSling) is refused by it. Before
// this guard the second sling succeeded and both lanes ran the anchor.
func TestExclusiveDrainReservationRefusesFreshDispatch(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)

	// Entry point 1: the drain reserves the member as it expands.
	expandExclusiveDrain(t, store, drain)
	if got := mustReservation(t, store, memberID); got != drain.ID {
		t.Fatalf("gc.exclusive_drain_reservation = %q, want %s (the drain never took the reservation)", got, drain.ID)
	}

	// Entry point 2: a fresh dispatch of the same anchor must be refused.
	deps, a, runner := reproSlingDeps(t, store)
	convoysBefore := trackingConvoyIDs(t, store, memberID)
	_, err := DoSling(testOpts(a, memberID), deps, nil)
	var resErr *ExclusiveDrainReservationError
	if !errors.As(err, &resErr) {
		t.Fatalf("DoSling(reserved member) error = %v, want *ExclusiveDrainReservationError", err)
	}
	if resErr.BeadID != memberID || resErr.ControlID != drain.ID {
		t.Fatalf("error = %+v, want BeadID %s / ControlID %s", resErr, memberID, drain.ID)
	}
	if resErr.ControlStatus == "" {
		t.Errorf("ControlStatus is empty; the refusal must name the live holder's state")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner was invoked %d times; a refused sling must dispatch nothing", len(runner.calls))
	}

	// The refusal must not mutate: no second dispatch authority is minted (the
	// only convoy tracking the anchor is still the drain's own unit), and the
	// drain keeps its reservation.
	if got := trackingConvoyIDs(t, store, memberID); !slices.Equal(got, convoysBefore) {
		t.Fatalf("tracking convoys = %v, want %v unchanged: the refused sling minted a dispatch anyway", got, convoysBefore)
	}
	if got := mustReservation(t, store, memberID); got != drain.ID {
		t.Fatalf("reservation after refusal = %q, want %s intact", got, drain.ID)
	}
}

// trackingConvoyIDs returns the sorted IDs of every convoy tracking itemID.
func trackingConvoyIDs(t *testing.T, store beads.Store, itemID string) []string {
	t.Helper()
	convoys, err := convoycore.TrackingConvoysForItem(store, itemID)
	if err != nil {
		t.Fatalf("TrackingConvoysForItem: %v", err)
	}
	ids := make([]string, 0, len(convoys))
	for _, c := range convoys {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestExclusiveDrainReservationForceOverride keeps the documented escape hatch
// working: --force is how an operator accepts a deliberate double dispatch.
func TestExclusiveDrainReservationForceOverride(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)
	expandExclusiveDrain(t, store, drain)

	deps, a, runner := reproSlingDeps(t, store)
	opts := testOpts(a, memberID)
	opts.Force = true
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling(--force) error = %v, want the override to route", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1: --force must still dispatch", len(runner.calls))
	}
}

// TestExclusiveDrainReservationDryRunReportsRefusal proves the preview tells
// the truth. A dry run that hid the veto would send the operator straight into
// the sling that is about to be rejected.
func TestExclusiveDrainReservationDryRunReportsRefusal(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)
	expandExclusiveDrain(t, store, drain)

	deps, a, _ := reproSlingDeps(t, store)
	opts := testOpts(a, memberID)
	opts.DryRun = true
	var resErr *ExclusiveDrainReservationError
	if _, err := DoSling(opts, deps, nil); !errors.As(err, &resErr) {
		t.Fatalf("DoSling(--dry-run) error = %v, want *ExclusiveDrainReservationError", err)
	}
}

// TestExclusiveDrainReservationReleasedAfterDrainCloses proves the guard is not
// a one-way door: once the drain terminates and releases, the member is
// routable again with no --force needed.
func TestExclusiveDrainReservationReleasedAfterDrainCloses(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)
	expandExclusiveDrain(t, store, drain)
	if got := mustReservation(t, store, memberID); got != drain.ID {
		t.Fatalf("reservation = %q, want %s", got, drain.ID)
	}

	if err := store.SetMetadata(memberID, "gc.exclusive_drain_reservation", ""); err != nil {
		t.Fatal(err)
	}
	deps, a, _ := reproSlingDeps(t, store)
	if _, err := DoSling(testOpts(a, memberID), deps, nil); err != nil {
		t.Fatalf("DoSling(released member) error = %v, want success", err)
	}
}

// TestExclusiveDrainReservationClosedHolderStillVetoes pins the corrected
// staleness rule. A drain releases its reservations BEFORE it closes, so a
// reservation still present on a CLOSED control is abnormal — the drain
// terminated without releasing, and an item-root worker it spawned may still be
// executing the member. Double-dispatching onto that is exactly the incident,
// so the veto holds; the message flags the holder as stale, and --force is the
// escape for a genuinely dead reservation.
func TestExclusiveDrainReservationClosedHolderStillVetoes(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)
	expandExclusiveDrain(t, store, drain)

	closed := "closed"
	if err := store.Update(drain.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close drain control: %v", err)
	}
	if got := mustReservation(t, store, memberID); got != drain.ID {
		t.Fatalf("reservation = %q, want the stamp %s to still be present", got, drain.ID)
	}

	deps, a, runner := reproSlingDeps(t, store)
	var resErr *ExclusiveDrainReservationError
	if _, err := DoSling(testOpts(a, memberID), deps, nil); !errors.As(err, &resErr) {
		t.Fatalf("DoSling(closed holder + live reservation) error = %v, want *ExclusiveDrainReservationError", err)
	}
	if !strings.Contains(resErr.Error(), "stale reservation") {
		t.Errorf("message %q should flag the terminated holder as a stale reservation", resErr.Error())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner invoked %d times; the veto must dispatch nothing", len(runner.calls))
	}

	// --force is the escape for a genuinely dead reservation.
	opts := testOpts(a, memberID)
	opts.Force = true
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling(--force over stale reservation) error = %v, want the override to route", err)
	}
}

// TestExclusiveDrainReservationReleasedSlotProceeds is the clean-completion
// counterpart: a drain that finished normally cleared the slot, so there is no
// veto and no --force needed. (The empty slot is the normal terminal state; a
// present reservation is what TestExclusiveDrainReservationClosedHolderStillVetoes
// covers.)
func TestExclusiveDrainReservationReleasedSlotProceeds(t *testing.T) {
	store, drain, memberID := seedExclusiveDrain(t)
	expandExclusiveDrain(t, store, drain)
	// Normal release before close.
	if err := store.SetMetadata(memberID, "gc.exclusive_drain_reservation", ""); err != nil {
		t.Fatal(err)
	}
	closed := "closed"
	if err := store.Update(drain.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	deps, a, runner := reproSlingDeps(t, store)
	if _, err := DoSling(testOpts(a, memberID), deps, nil); err != nil {
		t.Fatalf("DoSling(released slot) error = %v, want success", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
}

// TestExclusiveDrainReservationUnreadableHolderStillRefuses proves the holder
// read is diagnostic only: the reservation stamp alone vetoes, so an unreadable
// control (a cross-store holder, a transient store error) cannot downgrade the
// veto — it only yields a less-detailed message. Inferring "safe to
// double-dispatch" from a failed read is the advisory behavior this check ends.
func TestExclusiveDrainReservationUnreadableHolderStillRefuses(t *testing.T) {
	store := beads.NewMemStore()
	member, err := store.Create(beads.Bead{Title: "member anchor", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(member.ID, "gc.exclusive_drain_reservation", "dip-7qtaev"); err != nil {
		t.Fatal(err)
	}

	deps, a, _ := reproSlingDeps(t, store)
	var resErr *ExclusiveDrainReservationError
	if _, err := DoSling(testOpts(a, member.ID), deps, nil); !errors.As(err, &resErr) {
		t.Fatalf("DoSling(unreadable holder) error = %v, want *ExclusiveDrainReservationError", err)
	}
	if resErr.ControlID != "dip-7qtaev" {
		t.Fatalf("ControlID = %q, want dip-7qtaev", resErr.ControlID)
	}
}

// TestUnreservedBeadSlingsNormally pins the guard's blast radius: a bead with
// no reservation routes exactly as before.
func TestUnreservedBeadSlingsNormally(t *testing.T) {
	store := beads.NewMemStore()
	member, err := store.Create(beads.Bead{Title: "ordinary work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	deps, a, runner := reproSlingDeps(t, store)
	if _, err := DoSling(testOpts(a, member.ID), deps, nil); err != nil {
		t.Fatalf("DoSling(unreserved) error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
}
