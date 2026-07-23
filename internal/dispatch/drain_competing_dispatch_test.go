package dispatch

// The mirror half of the dip-tlbizd fix. The sling-side veto
// (internal/sling.CheckExclusiveDrainReservation) covers drain-first ordering:
// the drain reserves, a later fresh dispatch is refused. These tests cover
// dispatch-first ordering — a fresh/root workflow is already live on the anchor
// when the drain arrives — which the reservation alone cannot see, because a
// fresh dispatch leaves no reservation to compare against. Without this the
// exclusivity guarantee would be decided by arrival order.

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// graph.v2 formula compilation defaults on (set in the formula package's init)
// and no test in this package turns it off, so these tests rely on that default
// rather than the legacy process-global formula_v2 test setters — the coupling
// internal/testenv/legacy_flag_freeze_test.go freezes.

// seedLiveFreshDispatch reproduces what the incident's second lane left on the
// member anchor: an OPEN synthetic input convoy tracking it (dip-l93ikp,
// "input convoy for dip-3812vr", gc.synthetic=true) driven by a workflow root.
func seedLiveFreshDispatch(t *testing.T, store *beads.MemStore, memberID string) beads.Bead {
	t.Helper()
	convoy, err := store.Create(beads.Bead{
		Title:    "input convoy for " + memberID,
		Type:     "convoy",
		Metadata: map[string]string{"gc.synthetic": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItem(store, convoy.ID, memberID); err != nil {
		t.Fatal(err)
	}
	return convoy
}

func TestExclusiveDrainRefusesMemberWithLiveFreshDispatch(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)

	members := drainMemberIDs(t, store, drain)
	convoy := seedLiveFreshDispatch(t, store, members[0])

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(competing dispatch): %v", err)
	}
	if result.Action != "drain-reservation-failed" {
		t.Fatalf("Action = %q, want drain-reservation-failed", result.Action)
	}
	drain = mustGetBead(t, store, drain.ID)
	if drain.Status != "closed" || drain.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("drain = status %q outcome %q, want closed/fail", drain.Status, drain.Metadata["gc.outcome"])
	}
	if got := drain.Metadata["gc.failure_reason"]; got != "exclusive_dispatch_conflict" {
		t.Fatalf("gc.failure_reason = %q, want exclusive_dispatch_conflict", got)
	}
	if got := drain.Metadata["gc.failure_subject"]; got != members[0] {
		t.Fatalf("gc.failure_subject = %q, want the contended member %s", got, members[0])
	}
	if got := drain.Metadata["gc.failure_owner"]; !strings.Contains(got, convoy.ID) {
		t.Fatalf("gc.failure_owner = %q, want it to name the live convoy %s", got, convoy.ID)
	}
	// The drain must not have stamped itself onto work it refused.
	member := mustGetBead(t, store, members[0])
	if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != "" {
		t.Fatalf("reservation = %q, want empty: the drain reserved a member it refused", got)
	}
}

// TestExclusiveDrainToleratesNonSyntheticTrackingConvoy pins the guard's blast
// radius on the signal that would otherwise fire constantly: every drain member
// is tracked by the drain's own (hand-made, non-synthetic) parent convoy.
func TestExclusiveDrainToleratesNonSyntheticTrackingConvoy(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	members := drainMemberIDs(t, store, drain)

	// A second ordinary convoy also tracking the member: not a dispatch.
	extra, err := store.Create(beads.Bead{Title: "planning convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := convoycore.TrackItem(store, extra.ID, members[0]); err != nil {
		t.Fatal(err)
	}

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(non-synthetic tracker): %v", err)
	}
	member := mustGetBead(t, store, members[0])
	if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != drain.ID {
		t.Fatalf("reservation = %q, want %s: an ordinary tracking convoy must not block the drain", got, drain.ID)
	}
}

// TestExclusiveDrainToleratesClosedFreshDispatch proves a finished lane does not
// wedge the drain forever: a terminal input convoy is not a live dispatch.
func TestExclusiveDrainToleratesClosedFreshDispatch(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	members := drainMemberIDs(t, store, drain)

	convoy := seedLiveFreshDispatch(t, store, members[0])
	closed := "closed"
	if err := store.Update(convoy.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close input convoy: %v", err)
	}

	if _, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}}); err != nil {
		t.Fatalf("ProcessControl(closed fresh dispatch): %v", err)
	}
	member := mustGetBead(t, store, members[0])
	if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != drain.ID {
		t.Fatalf("reservation = %q, want %s: a closed input convoy is not a live dispatch", got, drain.ID)
	}
}

// TestExclusiveDrainDoesNotSelfCollideOnSyntheticParentConvoy is the critical
// regression: the drain's OWN parent convoy tracks every member, so if that
// convoy is synthetic (e.g. a drain launched by a single-bead formula) a naive
// competing-dispatch scan would flag it and the drain would kill itself on
// every member. The own-convoy exclusion must prevent that.
func TestExclusiveDrainDoesNotSelfCollideOnSyntheticParentConvoy(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t)
	if err := store.SetMetadata(drain.ID, "gc.drain_member_access", "exclusive"); err != nil {
		t.Fatalf("SetMetadata(exclusive): %v", err)
	}
	// Make the drain's own parent convoy synthetic — the self-collision trap.
	parentConvoyID := drainParentConvoyID(t, store, mustGetBead(t, store, drain.ID))
	if err := store.SetMetadata(parentConvoyID, "gc.synthetic", "true"); err != nil {
		t.Fatalf("SetMetadata(synthetic parent): %v", err)
	}
	drain = mustGetBead(t, store, drain.ID)
	members := drainMemberIDs(t, store, drain)

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(synthetic parent): %v", err)
	}
	if result.Action == "drain-reservation-failed" {
		t.Fatalf("the drain self-vetoed on its own synthetic parent convoy %s", parentConvoyID)
	}
	member := mustGetBead(t, store, members[0])
	if got := strings.TrimSpace(member.Metadata["gc.exclusive_drain_reservation"]); got != drain.ID {
		t.Fatalf("reservation = %q, want %s: the drain must reserve its own members", got, drain.ID)
	}
}

// drainParentConvoyID returns the drain's input/parent convoy ID.
func drainParentConvoyID(t *testing.T, store *beads.MemStore, drain beads.Bead) string {
	t.Helper()
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	id := strings.TrimSpace(root.Metadata["gc.input_convoy_id"])
	if id == "" {
		t.Fatal("drain root has no gc.input_convoy_id")
	}
	return id
}

// TestReadDrainIgnoresLiveFreshDispatch confirms the guard is scoped to
// exclusive drains. A read-access drain shares its members by design.
func TestReadDrainIgnoresLiveFreshDispatch(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	store, drain := seedDrainWorkflow(t) // seeded with gc.drain_member_access: read
	members := drainMemberIDs(t, store, drain)
	seedLiveFreshDispatch(t, store, members[0])

	result, err := ProcessControl(store, drain, ProcessOptions{FormulaSearchPaths: []string{dir}})
	if err != nil {
		t.Fatalf("ProcessControl(read drain): %v", err)
	}
	if result.Action == "drain-reservation-failed" {
		t.Fatal("a read-access drain must not be refused for a competing dispatch")
	}
}

// drainMemberIDs returns the member anchors of drain's input convoy, in
// manifest order.
func drainMemberIDs(t *testing.T, store *beads.MemStore, drain beads.Bead) []string {
	t.Helper()
	root := mustGetBead(t, store, drain.Metadata["gc.root_bead_id"])
	convoyID := strings.TrimSpace(root.Metadata["gc.input_convoy_id"])
	items, err := convoycore.Members(store, convoyID, false)
	if err != nil {
		t.Fatalf("Members(%s): %v", convoyID, err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		t.Fatalf("convoy %s tracks no members", convoyID)
	}
	return ids
}
