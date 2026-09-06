package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
)

// liveTrackingRoots returns the non-terminal convoys tracking itemID. Test
// helper mirroring the production live-root lookup so assertions read in the
// same terms as the invariant under test: one live root per tracked bead.
func liveTrackingRoots(t *testing.T, store beads.Store, itemID string) []beads.Bead {
	t.Helper()
	convoys, err := convoycore.TrackingConvoysForItem(store, itemID)
	if err != nil {
		t.Fatalf("TrackingConvoysForItem(%s): %v", itemID, err)
	}
	var live []beads.Bead
	for _, c := range convoys {
		if !convoycore.IsTerminalStatus(c.Status) {
			live = append(live, c)
		}
	}
	return live
}

// trackNewConvoyRoot mints an extra auto-convoy root tracking itemID the way a
// re-sling did before the mint-site reuse guard existed: a fresh convoy bead
// plus a tracks edge, with no reference to the roots already tracking the item.
//
// It exists to reproduce the ledger state this fix has to converge — one work
// bead carrying several live roots — which the reuse guard alone can no longer
// produce.
func trackNewConvoyRoot(t *testing.T, store beads.Store, itemID string, labels ...string) beads.Bead {
	t.Helper()
	c, err := store.Create(beads.Bead{Title: "sling-" + itemID, Type: "convoy", Labels: labels})
	if err != nil {
		t.Fatalf("creating convoy root tracking %s: %v", itemID, err)
	}
	if err := convoycore.TrackItem(store, c.ID, itemID); err != nil {
		t.Fatalf("tracking %s from convoy %s: %v", itemID, c.ID, err)
	}
	return c
}

// convoyStatus reads a convoy bead's current status.
func convoyStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("getting convoy %s: %v", id, err)
	}
	return b.Status
}

// clearRoutingState reproduces what a polecat's submit-and-exit does when it
// hands a bead to the refinery, and what the refinery does when it rejects the
// branch and returns the bead to the pool: the routing metadata and assignee
// are cleared while the auto-convoy root stays open.
//
// This is the state a re-slung bead is actually in, and it is precisely the
// state in which CheckBeadStateWithOptions no longer recognizes the bead as
// routed — so the duplicate-convoy guard in resolveConvoyRecovery never runs.
func clearRoutingState(t *testing.T, store beads.Store, beadID string) {
	t.Helper()
	empty := ""
	if err := store.Update(beadID, beads.UpdateOpts{Assignee: &empty}); err != nil {
		t.Fatalf("clearing assignee on %s: %v", beadID, err)
	}
	if err := store.SetMetadata(beadID, "gc.routed_to", ""); err != nil {
		t.Fatalf("clearing gc.routed_to on %s: %v", beadID, err)
	}
}

// A bead that bounces off the refinery and is re-slung must not accumulate a
// second convoy root. The roots are not orphans — they all close together when
// the tracked bead reaches a terminal state — but until then every extra root
// double-counts one piece of in-flight work on the ready board (ga-qar0).
//
// The existing duplicate-convoy guard (needsConvoyRecovery, gastownhall/
// gascity#2987) is keyed on routing state, so it is skipped entirely once
// submit-and-exit has cleared gc.routed_to and the assignee. The reuse must
// therefore live at the mint site, which every dispatch path goes through.
func TestReSlingReusesLiveConvoyRootInsteadOfMintingDuplicate(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if first.ConvoyID == "" {
		t.Fatal("first sling: expected an auto-convoy root")
	}

	clearRoutingState(t, deps.Store, b.ID)

	second, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}

	if second.ConvoyID != first.ConvoyID {
		t.Errorf("re-sling ConvoyID = %q, want %q (the live root must be reused, not replaced)",
			second.ConvoyID, first.ConvoyID)
	}
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 1 {
		ids := make([]string, 0, len(live))
		for _, c := range live {
			ids = append(ids, c.ID)
		}
		t.Errorf("bead %s has %d live convoy roots %v after re-sling, want exactly 1", b.ID, len(live), ids)
	}
}

// Reuse must be scoped to LIVE roots only. Once the tracked bead's prior root
// has closed (the drain path working as designed — it is correct and untouched
// here), a fresh sling of the same bead is a genuinely new dispatch and must
// mint a new root rather than resurrecting the closed one.
func TestSlingMintsFreshRootWhenPriorRootClosed(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if first.ConvoyID == "" {
		t.Fatal("first sling: expected an auto-convoy root")
	}

	// The drain closes the root when the tracked bead reaches terminal state.
	if err := deps.Store.Close(first.ConvoyID); err != nil {
		t.Fatalf("closing prior root: %v", err)
	}
	clearRoutingState(t, deps.Store, b.ID)

	second, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}
	if second.ConvoyID == "" {
		t.Fatal("re-sling after closed root: expected a fresh auto-convoy root")
	}
	if second.ConvoyID == first.ConvoyID {
		t.Errorf("re-sling reused closed root %s; a closed root must not be resurrected", first.ConvoyID)
	}
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 1 {
		t.Errorf("bead %s has %d live convoy roots after re-sling, want exactly 1", b.ID, len(live))
	}
}

// The mint-site reuse guard keeps a bead from growing a SECOND root, but a bead
// that already carries several — every re-sling that ran before the guard
// landed left one behind — stays stuck at N until the tracked bead closes.
// Reuse alone never converges that backlog, so a re-sling must also reap the
// predecessors it superseded (ga-5jnq).
//
// The kept root is the one reuse selects: TrackingConvoysForItem sorts oldest
// first, so the original dispatch root survives and the later duplicates are
// the superseded ones.
func TestReSlingReapsSupersededConvoyRoots(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if first.ConvoyID == "" {
		t.Fatal("first sling: expected an auto-convoy root")
	}

	// Two roots left behind by re-slings that predate the reuse guard.
	dupA := trackNewConvoyRoot(t, deps.Store, b.ID)
	dupB := trackNewConvoyRoot(t, deps.Store, b.ID)
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 3 {
		t.Fatalf("setup: bead %s has %d live roots, want 3", b.ID, len(live))
	}

	clearRoutingState(t, deps.Store, b.ID)

	second, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}
	if second.ConvoyID != first.ConvoyID {
		t.Errorf("re-sling ConvoyID = %q, want %q (the oldest live root is reused)",
			second.ConvoyID, first.ConvoyID)
	}

	live := liveTrackingRoots(t, deps.Store, b.ID)
	if len(live) != 1 {
		ids := make([]string, 0, len(live))
		for _, c := range live {
			ids = append(ids, c.ID)
		}
		t.Fatalf("bead %s has %d live convoy roots %v after re-sling, want exactly 1", b.ID, len(live), ids)
	}
	if live[0].ID != first.ConvoyID {
		t.Errorf("surviving root = %s, want the reused root %s", live[0].ID, first.ConvoyID)
	}
	for _, dup := range []beads.Bead{dupA, dupB} {
		if got := convoyStatus(t, deps.Store, dup.ID); got != "closed" {
			t.Errorf("superseded root %s status = %q, want closed", dup.ID, got)
		}
	}
}

// An "owned" root's lifecycle is managed by whoever asked for it — that label
// is exactly what suppresses convoy autoclose — so a re-sling reuses one but
// must never reap its siblings. Closing an owned root here would silently
// convert a caller-managed lifecycle into an auto-managed one.
func TestReSlingLeavesOwnedConvoyRootsIntact(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID, Owned: true}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	dup := trackNewConvoyRoot(t, deps.Store, b.ID, "owned")

	clearRoutingState(t, deps.Store, b.ID)

	if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID, Owned: true}, deps, deps.Store); err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}

	for _, id := range []string{first.ConvoyID, dup.ID} {
		if got := convoyStatus(t, deps.Store, id); got != "open" {
			t.Errorf("owned root %s status = %q, want open (owned lifecycles are never reaped)", id, got)
		}
	}
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 2 {
		t.Errorf("bead %s has %d live owned roots, want both left intact", b.ID, len(live))
	}
}

// Reaping is scoped to roots of the same ownership shape as the reused one. A
// live owned root alongside unowned duplicates belongs to a different
// lifecycle: the unowned re-sling reuses and reaps only among the unowned
// roots and leaves the owned one alone.
func TestReSlingReapScopedToMatchingOwnershipShape(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	ownedRoot := trackNewConvoyRoot(t, deps.Store, b.ID, "owned")
	unownedFirst := trackNewConvoyRoot(t, deps.Store, b.ID)
	unownedDup := trackNewConvoyRoot(t, deps.Store, b.ID)

	res, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if res.ConvoyID != unownedFirst.ID {
		t.Errorf("reused ConvoyID = %q, want the oldest unowned root %s", res.ConvoyID, unownedFirst.ID)
	}
	if got := convoyStatus(t, deps.Store, ownedRoot.ID); got != "open" {
		t.Errorf("owned root %s status = %q, want open", ownedRoot.ID, got)
	}
	if got := convoyStatus(t, deps.Store, unownedDup.ID); got != "closed" {
		t.Errorf("superseded unowned root %s status = %q, want closed", unownedDup.ID, got)
	}
}

// Infrastructure convoys (session and order-tracking bookkeeping) are not
// dispatch roots. The live-root lookup already skips them for reuse; the reap
// must skip them too, or a re-sling would close the session/order bookkeeping
// that happens to track the same bead.
func TestReSlingIgnoresInfrastructureTrackingConvoys(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	for _, label := range []string{"gc:session", "gc:order-tracking", "order-tracking"} {
		infra := trackNewConvoyRoot(t, deps.Store, b.ID, label)

		res, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling with %s convoy present: %v", label, err)
		}
		if res.ConvoyID == infra.ID {
			t.Errorf("reused infrastructure convoy %s (label %q) as a dispatch root", infra.ID, label)
		}
		if got := convoyStatus(t, deps.Store, infra.ID); got != "open" {
			t.Errorf("infrastructure convoy %s (label %q) status = %q, want open", infra.ID, label, got)
		}
		clearRoutingState(t, deps.Store, b.ID)
	}
}
