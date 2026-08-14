package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// This file is the split-store conformance suite: one set of ownership
// invariants, each run over BOTH store topologies through the splitEnv fixture
// (split_topology_env_test.go).
//
// Every invariant guards a bug class that has fired at least once, and every one
// of those bugs had the same root cause: a call site answering "which store owns
// this class of bead?" differently from the canonical dispatch,
// resolveClassStore (class_store.go). Two of them reached production and are
// named on the invariants that pin them — order-tracking beads born in the work
// store on a split city (#5127), and readers left on the work ledger for
// relocated ids (#5125).
//
// Each invariant is a named subtest. Each routes through forEachTopology or
// forEachTopologyWithRig, so the split subtest catches a path that hard-codes
// one store and the single-store subtest catches a split-city fix that changed
// legacy behavior. scripts/check-split-topology-rows.sh enforces that shape
// statically: an invariant that minted its own single-topology env would cover
// one row and let a regression in the other sail through.
//
// # Invariants that state a gap instead of asserting one
//
// Some invariants name a capability main does not have yet. Those SKIP with the
// reason and the seam that is missing, rather than being quietly omitted or
// asserted against a seam that does not exist. The skip list is this program's
// verified remaining work, and it is currently EMPTY: I5's claim-mutation half
// was the last entry, and it closed when the shared by-id class resolver landed
// (storeref.ClassCandidates, ga-ia7li). A new gap gets a skip, not a deletion.
//
// Others pin behavior main HAS but should not keep. Those carry a KNOWN GAP
// paragraph naming the divergence, the assertions that move when it closes, and
// the slice that closes it — I1 and I2 (the HQ work store is in neither arm of
// the controller's cross-store scan on a split city), I5 (the RELEASE tier does
// not follow the now class-routed claim: crash recovery, on_death/on_boot and
// the retired-session sweep are all still work-only) and I10 (the wake filter
// has no coordination-class reachability arm). Leaving such a leg UNSEEDED is the
// failure mode this convention exists to prevent: the invariant then reads as
// coverage of a path it never touches.
//
// # Which authority an invariant is pinning
//
// Some assertions are about a production backend (bd/Dolt, SQLite), some are
// about the splittest kit's deliberate domain rules, and some are about cmd/gc's
// own wrappers. They do not agree, so an invariant states which one it pins —
// see I6, where the cross-prefix dep refusal is the KIT's co-residence rule and
// neither backend refuses the write, and I5, where the negative is stated
// against the class store's namespace rather than against a wrapper's
// capability opacity.

// TestSplitTopologyConformance drives every conformance invariant over both
// store topologies. Run one invariant with e.g.
//
//	go test ./cmd/gc/ -run 'TestSplitTopologyConformance/I3'
func TestSplitTopologyConformance(t *testing.T) {
	t.Run("I1-ready-federation", func(t *testing.T) { forEachTopologyWithRig(t, conformanceReadyFederation) })
	t.Run("I2-assigned-work-capture", func(t *testing.T) { forEachTopologyWithRig(t, conformanceAssignedWorkCapture) })
	t.Run("I3-by-id-write-residence", func(t *testing.T) { forEachTopology(t, conformanceByIDWriteResidence) })
	t.Run("I4-materialization-residence", func(t *testing.T) { forEachTopology(t, conformanceMaterializationResidence) })
	t.Run("I5-claim-routing", func(t *testing.T) { forEachTopology(t, conformanceClaimRouting) })
	t.Run("I6-strict-cross-store-deps", func(t *testing.T) { forEachTopology(t, conformanceStrictCrossStoreDeps) })
	t.Run("I7-by-id-read-federation", func(t *testing.T) { forEachTopology(t, conformanceByIDReadFederation) })
	t.Run("I8-residence-sweep", func(t *testing.T) { forEachTopology(t, conformanceResidenceSweep) })
	t.Run("I9-warm-tick-demand", func(t *testing.T) { forEachTopologyWithRig(t, conformanceWarmTickDemand) })
	t.Run("I10-wake-ownership-fast-path", func(t *testing.T) { forEachTopologyWithRig(t, conformanceWakeOwnershipFastPath) })
	t.Run("I11-read-path-consistency", func(t *testing.T) { forEachTopology(t, conformanceReadPathConsistency) })
	t.Run("I12-molecule-membership", func(t *testing.T) { forEachTopology(t, conformanceMoleculeMembership) })
	t.Run("I13-cli-ready-federation", func(t *testing.T) { forEachTopologyWithRig(t, conformanceCLIReadyFederation) })
	t.Run("I14-projection-coherence", func(t *testing.T) { forEachTopology(t, conformanceProjectionCoherence) })
	t.Run("I15-work-query-federation", func(t *testing.T) { forEachTopologyWithRig(t, conformanceWorkQueryFederation) })
	t.Run("I16-federated-read-tier", func(t *testing.T) { forEachTopology(t, conformanceFederatedReadTier) })
}

// conformanceReadyFederation (I1) guards the "no work" fail-open: a worker
// spawns, the demand read cannot see the work it was spawned for, and the
// session drains. The controller's cross-store demand scan
// (collectOpenUnassignedRoutedWork, the input to openControlDispatcherDemand and
// the pool spawn decision) is handed the SESSIONS-class store as its leading
// leg, exactly as CityRuntime.buildDesiredState wires it. On a split city that
// leg is the class store — which is also where routed graph-class work lives,
// because the whole split shares one binding — so an open routed bead in the
// durable control shape AND in the wisp shape must both surface there, with the
// rig store's own routed work alongside it. A leading store resolved to the WORK
// class instead would read zero and drain the fleet.
//
// KNOWN GAP, pinned rather than asserted as desirable — the HQ WORK store leg.
// The scan's arms are the leading store plus the RIG stores, and production
// builds the rig arm as rigBeadStores(), which deletes the city entry
// (city_runtime.go). On a split city the leading store is the class store, so
// the HQ work store is in NEITHER arm and a city-scope routed WORK bead is
// invisible to controller-tick demand — the same "no work" fail-open this
// invariant is named for, live today.
//
// THE TWO LEG SETS, named together so the delta stays one fact rather than two
// hand-maintained lists. The CLAIM read's legs are enumerated in
// ready_federation.go's contract header: city work store, then rigs by name
// ascending, then the relocated binding LAST. The DEMAND read's legs are the
// ones this row asserts: the leading sessions-class store (the binding on a
// split city) plus rigBeadStores(). Their only difference is this row's subject
// — the HQ work store, present in the claim set and absent from the demand set.
// Every other divergence class between the two readers is closed at the row
// level (demand_serve_predicate.go and its agreement conformance); this one is
// a LEG-SET difference and is the last one outstanding. cmd_start.go already names the dual role
// as a shared E2 two-store split. `gc session close` still reaches the HQ store
// through unclaimWorkAssignedToRetiredSessionBead, so the bead is not lost
// forever; the controller tick is what is blind. The hqWork row below asserts
// today's asymmetric answer on both topologies. When the coordination-class /
// HQ-work arm lands, that row's expectation flips to found-on-both and this
// paragraph goes with it.
func conformanceReadyFederation(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "routed ready control bead", e.qualified)
	wisp := e.mintWispWith(t, wispOpts{title: "routed ready wisp", routedTo: e.qualified})
	rigWork, err := e.rig.Create(beads.Bead{
		Title:    "routed rig work bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.qualified},
	})
	if err != nil {
		t.Fatalf("create routed rig work bead: %v", err)
	}
	// The HQ work-store leg: a city-scope routed WORK bead, the shape
	// order dispatch stamps gc.routed_to on. It is the one leg whose
	// reachability actually changes with the topology.
	hqWork, err := e.work.Create(beads.Bead{
		Title:    "routed HQ work bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.qualified},
	})
	if err != nil {
		t.Fatalf("create routed HQ work bead: %v", err)
	}
	if coordclass.Classify(hqWork).IsInfrastructure() {
		t.Fatalf("HQ bead %s classifies as infrastructure; this leg is about the WORK class specifically", hqWork.ID)
	}

	found, stores, refs, partial := collectOpenUnassignedRoutedWork(e.cfg, e.sessionsStore(), e.rigStores, nil, os.Stderr)
	if partial {
		t.Fatal("collectOpenUnassignedRoutedWork reported a partial scan; the demand read must be complete for this invariant to mean anything")
	}
	if len(found) != len(stores) || len(found) != len(refs) {
		t.Fatalf("demand scan returned unaligned slices: %d beads, %d stores, %d refs", len(found), len(stores), len(refs))
	}

	leadingOwner, ownerName := e.owner()
	leadingRef, rigRef := "city:"+e.cfg.Workspace.Name, "rig:"+e.rigName
	for _, tc := range []struct {
		name    string
		id      string
		store   beads.Store
		wantRef string
	}{
		{"durable routed control bead", durable.ID, leadingOwner, leadingRef},
		{"routed wisp", wisp.ID, leadingOwner, leadingRef},
		{"routed rig work bead", rigWork.ID, e.rig, rigRef},
	} {
		i := beadIndexOf(found, tc.id)
		if i < 0 {
			t.Errorf("%s %s is missing from the cross-store demand scan — this is the exact \"no work\" fail-open: a pool spawns for work its demand read cannot see, then drains", tc.name, tc.id)
			continue
		}
		if !sameStorePtr(stores[i], tc.store) {
			t.Errorf("%s %s was captured under the wrong owner store (%s leg); a release or stamp would mutate a store that does not hold it", tc.name, tc.id, ownerName)
		}
		if refs[i] != tc.wantRef {
			t.Errorf("%s %s captured under store-ref %q, want %q", tc.name, tc.id, refs[i], tc.wantRef)
		}
	}

	// The HQ work leg, stated per topology because the answer really differs.
	// See the KNOWN GAP paragraph above: this pins what main does, not what it
	// should do.
	hqIndex := beadIndexOf(found, hqWork.ID)
	if e.split {
		if hqIndex >= 0 {
			t.Errorf("routed HQ work bead %s IS in the split-city demand scan (store-ref %q). Two things produce this and they need opposite responses: the HQ-work arm landed (flip this row to expect it on both topologies, drop this invariant's KNOWN GAP paragraph, and update I2's hq leg with it) — or the LEADING store regressed to the work class, which is the #5127/#5125 bug and shows up as the durable/wisp rows going missing above", hqWork.ID, refs[hqIndex])
		}
		return
	}
	if hqIndex < 0 {
		t.Errorf("routed HQ work bead %s is missing from the single-store demand scan — on a legacy city the HQ work store IS the leading store, so this is the \"no work\" fail-open with nothing topological to blame", hqWork.ID)
		return
	}
	if !sameStorePtr(stores[hqIndex], e.work) {
		t.Errorf("routed HQ work bead %s was captured under a store that does not hold it", hqWork.ID)
	}
	if refs[hqIndex] != leadingRef {
		t.Errorf("routed HQ work bead %s captured under store-ref %q, want %q", hqWork.ID, refs[hqIndex], leadingRef)
	}
}

// conformanceAssignedWorkCapture (I2) guards the post-claim half of the
// spawn/drain treadmill and the orphan-release TOCTOU class. A claimed
// (in_progress) routed bead whose assignee is a DEAD session must be captured by
// collectAssignedWorkBeadsWithStores under the leading store's arm (owner store
// aligned, store-ref "") so orphan release can recover it; a claimed bead held
// by a LIVE open session must NOT be released. Both topologies expect the same
// outcome — release exactly the dead claim — which on a split city means the
// capture and the release both have to reach the CLASS store, because that is
// the leading store the reconciler is handed.
//
// KNOWN GAP, pinned rather than asserted as desirable — the HQ WORK store leg,
// the same one I1 names. The capture arms are the leading store plus the rig
// stores, and rigBeadStores() deletes the city entry, so on a split city a dead
// claim on a city-scope WORK bead is captured by nothing and released by
// nothing: the bead stays in_progress against a session that is gone until some
// other path (`gc session close`'s unclaimWorkAssignedToRetiredSessionBead)
// happens to reach it. The hqDead leg below asserts that asymmetry on both
// topologies. When the HQ-work arm lands, it flips to released-on-both, together
// with I1's hqWork row.
func conformanceAssignedWorkCapture(t *testing.T, e splitEnv) {
	sess, err := e.sessionsStore().Create(splitEnvPoolSessionBead(e.qualified, "executor-1"))
	if err != nil {
		t.Fatalf("create live pool session bead: %v", err)
	}
	live := e.mintWispWith(t, wispOpts{title: "live-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: sess.ID})
	dead := e.mintWispWith(t, wispOpts{title: "dead-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: splitEnvDeadAssignee})
	hqDead := splitEnvDeadClaimedWorkBead(t, e.work, e.qualified)

	got, stores, refs, _, partial := collectAssignedWorkBeadsWithStores(e.cfg, e.sessionsStore(), e.rigStores, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	for _, want := range []beads.Bead{live, dead} {
		i := beadIndexOf(got, want.ID)
		if i < 0 {
			t.Fatalf("claimed wisp %s not captured by collectAssignedWorkBeadsWithStores — post-claim work is invisible to the reconciler", want.ID)
		}
		if !sameStorePtr(stores[i], e.sessionsStore()) {
			t.Errorf("wisp %s captured with the wrong owner store — release would mutate a store that does not hold it", want.ID)
		}
		if refs[i] != "" {
			t.Errorf("wisp %s captured under store-ref %q, want \"\" (the leading-store arm)", want.ID, refs[i])
		}
	}

	// The HQ leg's capture half, per topology. See the KNOWN GAP paragraph.
	hqCaptured := beadIndexOf(got, hqDead.ID) >= 0
	if hqCaptured != !e.split {
		if e.split {
			t.Errorf("dead-claimed HQ work bead %s IS captured on a split city. Either the HQ-work arm landed (flip this leg and I1's hqWork row to expect capture and release on both topologies, and drop both KNOWN GAP paragraphs) or the LEADING store regressed to the work class, which is the #5127/#5125 bug and shows up as the wisp rows above failing too", hqDead.ID)
		} else {
			t.Errorf("dead-claimed HQ work bead %s is NOT captured on a legacy city, where the HQ work store IS the leading store — post-claim HQ work has gone invisible to the reconciler", hqDead.ID)
		}
	}

	released := releaseOrphanedPoolAssignments(
		e.sessionsStore(), e.cfg, e.cityPath,
		sessionInfosFromBeads([]beads.Bead{sess}),
		got, stores, refs,
		e.rigStores,
	)
	// On a legacy city the HQ work store IS the leading store, so the dead HQ
	// claim recovers alongside the dead wisp. On a split city it is in neither
	// arm, so only the wisp does.
	wantReleased := []string{dead.ID}
	if !e.split {
		wantReleased = append(wantReleased, hqDead.ID)
	}
	releasedIDs := make(map[string]bool, len(released))
	for _, b := range released {
		releasedIDs[b.ID] = true
	}
	if len(released) != len(wantReleased) {
		t.Errorf("released = %v, want exactly %v (the live holder's claim must survive; every reachable dead claim must recover)", released, wantReleased)
	}
	for _, want := range wantReleased {
		if !releasedIDs[want] {
			t.Errorf("dead claim %s was not released; it stays assigned to a session that is gone", want)
		}
	}
	if e.split {
		// The gap, asserted rather than assumed: the HQ dead claim is still
		// stranded after the release pass a split-city controller tick runs.
		stranded, err := e.work.Get(hqDead.ID)
		if err != nil {
			t.Fatalf("reload HQ dead-claimed work bead: %v", err)
		}
		if stranded.Status != "in_progress" || stranded.Assignee != splitEnvDeadAssignee {
			t.Errorf("HQ dead claim %s = status %q assignee %q, want it still stranded at in_progress/%s. If a controller-tick path now recovers it, the HQ-work arm has landed: update this leg, the capture check above and I1's hqWork row together", hqDead.ID, stranded.Status, stranded.Assignee, splitEnvDeadAssignee)
		}
	}

	reloaded, err := e.graphStore().Get(live.ID)
	if err != nil {
		t.Fatalf("reload live-held wisp: %v", err)
	}
	if reloaded.Status != "in_progress" || reloaded.Assignee != sess.ID {
		t.Errorf("live holder's wisp = status %q assignee %q, want in_progress/%s (claim wrongfully released — the orphan-release TOCTOU class)", reloaded.Status, reloaded.Assignee, sess.ID)
	}
}

// conformanceByIDWriteResidence (I3) guards the by-id WRITE-residence class,
// which is one of the two bugs this program already paid for in production:
// order-tracking beads created through the target store instead of the
// orders-class store landed in the work ledger on a split city, and the city's
// own convergence check read them as infrastructure beads stranded off their
// binding — which is fatal to boot (#5127). Update, SetMetadata and Close
// through the class accessor, on a durable graph bead AND on a wisp, must land
// in the owning store and must leave NO residue in the work store.
func conformanceByIDWriteResidence(t *testing.T, e splitEnv) {
	shapes := []struct {
		name string
		bead beads.Bead
	}{
		{"durable", mintDurableGraphBead(t, e, "by-id write durable graph bead", "")},
		{"wisp", e.mintWisp(t, "by-id write wisp")},
	}
	owner, ownerName := e.owner()
	for _, tt := range shapes {
		front := e.classStore(config.BeadClassGraph)
		if err := front.Update(tt.bead.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("%s: Update via the graph class accessor: %v", tt.name, err)
		}
		if err := front.SetMetadata(tt.bead.ID, "gc.conformance_probe", tt.name); err != nil {
			t.Fatalf("%s: SetMetadata via the graph class accessor: %v", tt.name, err)
		}
		if err := front.Close(tt.bead.ID); err != nil {
			t.Fatalf("%s: Close via the graph class accessor: %v", tt.name, err)
		}
		got, err := owner.Get(tt.bead.ID)
		if err != nil {
			t.Fatalf("%s: bead %s not resident in the %s store after by-id writes: %v", tt.name, tt.bead.ID, ownerName, err)
		}
		if got.Status != "closed" || got.Metadata["gc.conformance_probe"] != tt.name {
			t.Errorf("%s: %s-store bead = status %q probe %q, want closed/%q — a by-id write landed elsewhere", tt.name, ownerName, got.Status, got.Metadata["gc.conformance_probe"], tt.name)
		}
		if e.split {
			if _, err := e.work.Get(tt.bead.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("%s: bead %s resolves in the WORK store (err=%v) — a write minted a shadow row; on a split city that row is a stranded infrastructure bead and boot refuses on it", tt.name, tt.bead.ID, err)
			}
		}
	}
}

// conformanceMaterializationResidence (I4) is the create-side sibling of I3 and
// the invariant memoryOrderDispatcher.graphStoreFor exists to satisfy: a
// molecule materialized through the graph-class front door — the durable graph.v2
// workflow shape AND the root-only wisp shape — must land EVERY bead in the
// owning store, and zero in the work store on a split city. Creating them
// through the order's own target store is what put graph beads in the work
// ledger under the work prefix.
func conformanceMaterializationResidence(t *testing.T, e splitEnv) {
	ctx := context.Background()
	res, err := molecule.Instantiate(ctx, e.graphStore(), conformanceGraphRecipe(), molecule.Options{})
	if err != nil {
		t.Fatalf("materialize durable graph molecule: %v", err)
	}
	if res.Created != 2 {
		t.Fatalf("durable molecule created %d beads, want 2 (root + finalize step)", res.Created)
	}
	wres, err := molecule.Instantiate(ctx, e.graphStore(), conformanceWispRecipe(), molecule.Options{})
	if err != nil {
		t.Fatalf("materialize root-only wisp molecule: %v", err)
	}

	ids := []string{wres.RootID}
	for _, id := range res.IDMapping {
		ids = append(ids, id)
	}
	owner, ownerName := e.owner()
	for _, id := range ids {
		if _, err := owner.Get(id); err != nil {
			t.Errorf("materialized bead %s is not resident in the %s store: %v", id, ownerName, err)
		}
		if e.split {
			if _, err := e.work.Get(id); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("materialized bead %s resolves in the WORK store (err=%v) — the explosion leaked across the class boundary", id, err)
			}
		}
	}
	if n := countGraphClassBeads(t, e.work); e.split && n != 0 {
		t.Errorf("work store holds %d graph-class beads after materialization, want 0", n)
	} else if !e.split && n != 3 {
		t.Errorf("single store holds %d graph-class beads, want all 3 (single-store collapse)", n)
	}
}

// conformanceClaimRouting (I5) is the by-id ROUTING invariant: given only a bead
// id, the program must be able to name the store that owns it — and must name
// the same one for the ordinary class id shape and for the <prefix>-wisp-<suffix>
// shape a wisp carries.
//
// storeref.PrefixOwner is that routing primitive (internal/dispatch,
// internal/convoy and cmd/gc/cmd_wait already route on it), and it resolves on
// the store's own declared namespace: the prefix+"-" SEGMENT rule. Its sibling
// predicate storeref.IDInNamespace — the CONFIGURED-prefix rule the shared class
// resolver gates on — is deliberately not the same rule, because a configured
// rig/HQ prefix can be a whole id while a store that mints "gcg-1" never mints
// the bare "gcg". They agree on every id shape this invariant routes and diverge
// on the bare form, so nothing here may be read as one implying the other.
// This invariant pins that both shapes route to the class store on a split
// city, and pins the TRAP that makes the wisp shape special: the config-free
// sling.BeadPrefix heuristic answers "gcg-wisp" for a gcg-wisp-0042 id, which is
// not a reserved class prefix at all, so a by-id router built on that heuristic
// instead of the namespace rule would hand every wisp to the work store. That
// divergence is asserted as a negative here so it cannot change silently under a
// future resolver.
//
// The CLAIM-MUTATION half runs on the SHARED resolver, storeref.ClassCandidates
// (ga-ia7li), which is the seam this invariant was waiting on: a candidate list
// keyed on the resolveClassStore identity, probed in order, with the mutation
// written through the store that answered. splitEnv.claimByID is that shape, and
// the assertion is residence — the claim is visible in the owning store and in
// no other leg.
//
// GAP CLOSED in ga-601v2 — `gc hook --claim` is class-routed, and NOT by the
// shape the pin predicted. The prediction is kept here because the shape it got
// wrong is the interesting part.
//
// What was pinned: the hook resolves its stores as hookStore{dir, env} pairs
// (hook_cross_store.go) and execs bd in a work directory; the fan-out is city +
// rigs, all WORK scopes, so a claim it issued for a relocated class id ran
// against a ledger that could not see the bead. The rows below asserted that
// fan-out on both topologies and were expected to REDDEN when the closure added
// a coordination-class leg to it.
//
// They did not, and could not: a relocated binding is not a bd workspace and
// cannot be expressed as a hookStore{dir, env} at all — the same sentence the
// pin used to explain why the gap existed also rules out the closure it
// predicted. Both halves landed beside the fan-out instead of inside it:
//
//   - The READ half (ga-bvdha): no leg was added. The primary leg's COMMAND was
//     swapped to the federated `gc ready`, which covers the binding in process.
//   - The WRITE half (ga-601v2, claim_class_route.go): no leg was added. The
//     CLASS axis went on the hookClaimOps seam, beside the rig axis already in
//     claim_cross_store.go, so the claim-time writes escalate off the work store
//     to the binding for a bead the work store proves it does not hold.
//
// So the fan-out rows below are no longer a gap statement. They are the
// invariant that keeps the two halves apart: every leg the hook QUERIES stays a
// bd workspace naming a work scope, and a coordination-class leg appearing there
// now means someone has tried to express a binding as a workspace. The rows that
// state the closure are the routed-claim ones after them, and they are asserted
// against the production seams (hookClaimClassRouteForCity, the resolver, and
// classRoutedHookClaimOps, the ops wrapper) rather than against claimByID.
//
// PROBE, NOT PREFIX, AND WORK FIRST. The claim is a WRITE, so it finds its store
// by asking which one holds the bead — `gc storage migrate` preserves ids, so a
// prefix route would miss exactly the beads that moved. The probe ORDER is the
// part that differs from the by-id door, and it is deliberate: the by-id
// resolvers lead with the class store, while the federated `gc ready` that
// SERVED this claim its candidate leads with the work store and runs the graph
// leg last (#5148/#5158/#5161). A claim must resolve co-residence the way the
// reader that produced it does, or it claims the class copy while the reader
// keeps answering from the still-open work copy — the treadmill this closure
// exists to end. The rows below assert that: a class-resident id routes to the
// binding, a co-resident id keeps the WORK copy.
//
// `gc bd update --claim` is also routed (#5132, cmd_bd_by_id.go) and probes the
// same way but in the opposite ORDER, because its caller holds only an id and no
// reader to agree with. That divergence is real and is asserted below rather
// than left to be discovered.
func conformanceClaimRouting(t *testing.T, e splitEnv) {
	workBead, err := e.work.Create(beads.Bead{Title: "claim-routing work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if reservedClassNamespace(workBead.ID) {
		t.Fatalf("work bead %q sits in a reserved class namespace; by-id routing cannot be built on this id space", workBead.ID)
	}
	classPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("config.ReservedClassPrefix(graph) = ok:false; there is no reserved namespace for by-id class routing to run on")
	}

	// The QUERY fan-out, on both topologies: the store `gc hook --claim` runs its
	// work query against is a WORK scope, resolved from cfg with no class arm
	// anywhere in it. Relocating graph does not move it, and after ga-601v2 it
	// still must not: the class axis is on the ops, not on a leg.
	hookEnv, err := hookQueryEnv(e.cityPath, e.cfg, &config.Agent{Name: splitEnvPoolAgent})
	if err != nil {
		t.Fatalf("build the hook's work-query env: %v", err)
	}
	if got := hookEnv["GC_STORE_SCOPE"]; got != "city" {
		t.Errorf("`gc hook --claim` resolves store scope %q, want \"city\" — a coordination class named here would mean a relocated binding is being expressed as a bd workspace, which it is not; the class axis belongs on hookClaimOps (claim_class_route.go)", got)
	}
	if got := hookEnv["GC_STORE_ROOT"]; got != e.cityPath {
		t.Errorf("`gc hook --claim` resolves store root %q, want the city work root %q", got, e.cityPath)
	}

	// Every leg is a (dir, env) pair pointing a bd subprocess at a work
	// workspace, and there are exactly as many as the city has work scopes —
	// relocating graph adds none, in either direction.
	hookAgent := &config.Agent{Name: splitEnvPoolAgent}
	fanout := hookWorkQueryStores(e.cityPath, e.cfg, hookAgent,
		hookAgent.QualifiedName(),
		agentCommandDir(e.cityPath, hookAgent, e.cfg.Rigs),
		mergeRuntimeEnv(nil, hookEnv), hookEnv)
	if want := 1 + len(e.cfg.Rigs); len(fanout) != want {
		t.Errorf("`gc hook --claim` fans out over %d store(s), want %d (city + %d rig) — a leg appeared or vanished; the class binding is reached through hookClaimOps, never as a leg, because it is not a bd workspace", len(fanout), want, len(e.cfg.Rigs))
	}
	for i, leg := range fanout {
		scope := hookStoreEnvValue(leg, "GC_STORE_SCOPE")
		if scope != "city" && scope != "rig" {
			t.Errorf("`gc hook --claim` fan-out leg %d names store scope %q, want a WORK scope (\"city\" or \"rig\")", i, scope)
		}
		if root := hookStoreEnvValue(leg, "GC_STORE_ROOT"); root == "" {
			t.Errorf("`gc hook --claim` fan-out leg %d names no GC_STORE_ROOT; every leg here is a bd workspace, and a relocated binding cannot be expressed as one", i)
		}
	}

	// The CLOSURE, asserted on the production resolver: the claim-time class
	// route exists exactly when the city relocates a class, decided by store
	// identity through graphClassBinding and read off the city on disk — the
	// same authority cityQueryTopology asks for the read half.
	cityRoute, err := hookClaimClassRouteForCity(e.cityPath)
	if err != nil {
		t.Fatalf("resolving the claim-time class route for this city: %v", err)
	}
	if (cityRoute != nil) != e.split {
		t.Fatalf("hookClaimClassRouteForCity returned route!=nil = %v on a split=%v city; a claim routed on a city that relocates nothing writes through a binding it has no business opening, and one NOT routed on a split city writes ownership into a ledger that does not hold the bead", cityRoute != nil, e.split)
	}

	if !e.split {
		wisp := e.mintWisp(t, "claim-routing wisp")
		// A legacy city mints no reserved-class ids at all, so there is nothing
		// for a by-id router to route ON. That is the whole single-store
		// statement, and reservedClassNamespace is the rule that expresses it.
		//
		// Deliberately NOT asserted: that storeref.PrefixOwner returns nil over
		// the single leg. It does today — but only because e.work is
		// wrapStoreWithBeadPolicies(...) and beadPolicyStore embeds beads.Store
		// as an interface, so the optional IDPrefix() accessor is not promoted
		// and PrefixOwner skips the leg entirely. That is the wrapper's
		// capability opacity, not an id-space fact: the raw leaf declares "gc"
		// and production's CachingStore reports "gc" too. Forwarding IDPrefix
		// through the wrapper (which the shared by-id resolver, ga-ia7li, wants)
		// is a correct change, and an invariant that reddened on it would be
		// blaming the id space for a wrapper detail.
		if reservedClassNamespace(wisp.ID) {
			t.Fatalf("single-store wisp %q sits in a reserved class namespace; a legacy city mints work-store ids", wisp.ID)
		}
		// The single-store statement about the shared resolver: its identity
		// gate (class store IS the work store) means it claims NOTHING here, so
		// every by-id claim keeps running on the one store exactly as it does
		// today. A resolver that answered on a legacy city would be routing a
		// city with nowhere to route to.
		//
		// The rows are not interchangeable, and the class-shaped one is the only
		// one that pins the IDENTITY gate. A legacy city's own ids sit outside
		// the reserved namespace, so the NAMESPACE gate alone rejects them and
		// those rows would still pass with the identity gate deleted. A
		// class-shaped id clears the namespace gate, so identity is the only
		// thing left that can reject it.
		for _, tt := range []struct{ name, id string }{
			{"work bead — namespace gate", workBead.ID},
			{"wisp — namespace gate", wisp.ID},
			{"class-shaped id — identity gate", classPrefix + "-1"},
		} {
			if got := e.classCandidatesForID(tt.id); got != nil {
				t.Fatalf("%s: classCandidatesForID(%q) returned %d candidates on a single-store city; the resolver must be inert where the class store IS the work store", tt.name, tt.id, len(got))
			}
		}
		for _, id := range []string{workBead.ID, wisp.ID} {
			landed := e.claimByID(t, id, "single-store-claimant")
			if !sameStorePtr(landed, e.work) {
				t.Errorf("claim of %s landed in %p, want the single work store %p", id, landed, e.work)
			}
			e.assertClaimedIn(t, id, "single-store-claimant", e.work)
		}
		// BYTE-IDENTICAL claim-time writes, proved by function identity rather
		// than by behavior. classRoutedHookClaimOps must return the ops value it
		// was handed — not a wrapper that delegates — so a legacy city runs the
		// exact same function values it runs today and pays nothing for a seam it
		// cannot use. Mutating the wrapper to wrap unconditionally reddens this.
		assertHookClaimOpsUnwrapped(t, classRoutedHookClaimOps(defaultedHookClaimOps(), nil), defaultedHookClaimOps())
		return
	}

	durable := mintDurableGraphBead(t, e, "claim-routing durable control bead", "")
	wisp := e.mintWisp(t, "claim-routing wisp")
	legs := []beads.Store{e.work, e.class}
	for _, tt := range []struct{ name, id string }{
		{"durable graph bead", durable.ID},
		{"wisp (the -wisp- suffix shape)", wisp.ID},
		{"work bead", workBead.ID},
	} {
		wantClass := tt.id != workBead.ID
		owner := storeref.PrefixOwner(tt.id, legs)
		if wantClass && !sameStorePtr(owner, e.class) {
			t.Errorf("%s: storeref.PrefixOwner(%q) did not route to the class store — a by-id mutation would run against the store that does not hold it", tt.name, tt.id)
		}
		// The negative is that no CLASS store claims a work id. It is stated
		// against the class leg rather than as "no owner at all": whether the
		// policy-wrapped work leg answers depends on whether the wrapper
		// forwards IDPrefix(), which is a wrapper detail the by-id resolver
		// slice (ga-ia7li) is free to change. Routing a work id INTO the
		// coordination-class store is the thing that would corrupt.
		if !wantClass {
			if sameStorePtr(owner, e.class) {
				t.Errorf("%s: storeref.PrefixOwner(%q) routed a WORK id to the CLASS store — a by-id mutation would run against a store that cannot hold it, which is the residence violation the SQLite leaf accepts silently", tt.name, tt.id)
			}
			if classOwner := storeref.PrefixOwner(tt.id, []beads.Store{e.class}); classOwner != nil {
				t.Errorf("%s: the class store's declared namespace claims work id %q; the two id spaces must stay disjoint or every by-id router built on the namespace rule misroutes", tt.name, tt.id)
			}
		}
		if got := reservedClassNamespace(tt.id); got != wantClass {
			t.Errorf("%s: %q in a reserved class namespace = %v, want %v", tt.name, tt.id, got, wantClass)
		}
	}

	// The trap, pinned as a negative: the config-free heuristic disagrees with
	// the namespace rule on exactly the wisp shape. If this ever starts agreeing,
	// the comment above is stale and the skip below may be closable more cheaply
	// than it looks.
	if got := beadPrefix(e.cfg, wisp.ID); config.IsReservedClassPrefix(got) {
		t.Errorf("sling.BeadPrefixForCity(%q) = %q, which now IS a reserved class prefix; this invariant's documented trap has changed and the by-id resolver design note needs updating", wisp.ID, got)
	}
	if got := beadPrefix(e.cfg, durable.ID); !config.IsReservedClassPrefix(got) {
		t.Errorf("sling.BeadPrefixForCity(%q) = %q, want the reserved class prefix — the heuristic must at least agree with the namespace rule on the ORDINARY class id shape", durable.ID, got)
	}

	// The CLAIM MUTATION, through the shared resolver. Both class id shapes must
	// route to the class store — including the wisp shape, which is where the
	// heuristic pinned above disagrees — and the work id must keep the legacy
	// work-scope fan-out. The negative matters more than the positive: a claim
	// that ran against the store not holding the bead is the silent-no-op the
	// work-scope fan-out performs on a split city today.
	for _, tt := range []struct {
		name, id  string
		wantClass bool
	}{
		{"durable graph bead", durable.ID, true},
		{"wisp (the -wisp- suffix shape)", wisp.ID, true},
		{"work bead", workBead.ID, false},
	} {
		assignee := "claimant-" + tt.id
		want := e.work
		if tt.wantClass {
			want = e.class
		}
		landed := e.claimByID(t, tt.id, assignee)
		if !sameStorePtr(landed, want) {
			t.Errorf("%s: the by-id claim of %s landed in %p, want %p (class store %p, work store %p)", tt.name, tt.id, landed, want, e.class, e.work)
			continue
		}
		e.assertClaimedIn(t, tt.id, assignee, want)
	}

	conformanceHookClaimClassRouting(t, e, workBead.ID)
}

// conformanceHookClaimClassRouting is I5's closure row: the claim `gc hook
// --claim` ISSUES, through the production ops seam, for the three id shapes a
// split city can hand it.
//
// The route is opened over the fixture's class leg — a real beads.SQLiteStore,
// what graphClassBinding(e.routes) resolves to — so the routed claim acquires
// through the binding's own compare-and-swap and the write lands where the row
// minted the bead. The base Claim is bound to the fixture's real WORK store, so
// the not-found that opens the escalation is the store's own answer rather than
// a restatement of one.
//
// The CO-RESIDENT row is the one that states the tie-break, and it is the
// migrated-city steady state: `gc storage migrate` copies with ids preserved and
// never deletes back, so the same id is live in both stores. The claim must keep
// the WORK copy, because the federated `gc ready` that served it dedupes to the
// work row (#5148 leg order, ready_federation.go). A class-first claim here would
// leave the reader's work row open and re-serve the bead every tick — the exact
// treadmill ga-601v2 closes.
func conformanceHookClaimClassRouting(t *testing.T, e splitEnv, workBeadID string) {
	t.Helper()
	route, err := newHookClaimClassRoute(e.class)
	if err != nil {
		t.Fatalf("opening the claim-time class front door over the fixture's class store: %v", err)
	}
	classResident := e.mintWispWith(t, wispOpts{title: "hook-claim routed graph step"})

	// The migration copy: a WORK-shaped id live in both stores. The class leaf
	// models SQLite, which accepts a foreign-prefix pinned id and records the
	// residence violation instead of refusing, so the fixture claims the record.
	if _, err := e.class.Create(beads.Bead{ID: workBeadID, Title: "migrated copy of a work bead", Type: "task"}); err != nil {
		t.Fatalf("staging the co-resident migration copy of %s in the class store: %v", workBeadID, err)
	}
	if violations := splittest.TakeResidenceViolations(e.class); len(violations) == 0 {
		t.Fatal("class store recorded no residence violation for the co-resident work id; the SQLite-semantics leaf is not modeling the migrated steady state")
	}

	for _, tt := range []struct {
		name, id  string
		wantClass bool
	}{
		{"class-resident graph step — the bead the work query now serves", classResident.ID, true},
		{"work bead — the legacy path, unchanged", workBeadID, false},
		{"co-resident id — first-leg-wins, WORK copy", workBeadID, false},
	} {
		ops := classRoutedHookClaimOps(hookClaimOps{Claim: splitEnvStoreClaim(e.work)}, route)
		assignee := "hook-claimant-" + tt.name
		claimed, ok, err := ops.Claim(context.Background(), e.cityPath, nil, tt.id, assignee)
		if err != nil || !ok {
			t.Errorf("%s: `gc hook --claim` claim of %s = (ok=%v err=%v), want a successful claim; a class-resident bead the work query serves must be claimable by the command that served it", tt.name, tt.id, ok, err)
			continue
		}
		if claimed.ID != tt.id {
			t.Errorf("%s: claim returned bead %q, want %q", tt.name, claimed.ID, tt.id)
		}
		want, other := e.work, e.class
		if tt.wantClass {
			want, other = e.class, e.work
		}
		if got, err := want.Get(tt.id); err != nil || strings.TrimSpace(got.Assignee) != assignee {
			t.Errorf("%s: the store that owns %s holds assignee %q (err=%v), want %q — the claim ran against a store that does not own the bead", tt.name, tt.id, got.Assignee, err, assignee)
		}
		if got, err := other.Get(tt.id); err == nil && strings.TrimSpace(got.Assignee) == assignee {
			t.Errorf("%s: %s is claimed for %q in the store that does NOT own it; ownership was written into the wrong ledger", tt.name, tt.id, assignee)
		}
	}

	assertClassRoutedClaimIsReleasable(t, e)
}

// assertClassRoutedClaimIsReleasable is the other half of the routed claim: the
// release tier reaches what the claim can now write.
//
// The claim can leave an in_progress assignee in the BINDING, so a crashed
// worker would strand a graph step forever if nothing that takes a claim back
// could see it. Two scans have to be checked, and they lead with different
// stores:
//
//   - the reconciler's (stranded-repair, named retirement, the closeBead
//     cascade) leads with the SESSIONS-class store, which on a converged split
//     is the same engine the graph class is served from — openStorageRoutes keys
//     every assigned class to the one store it opened — so it already reads the
//     binding. That is a property of the topology rather than of any call site,
//     which is exactly why it is asserted rather than assumed.
//   - `gc session close` leads with the WORK store (openCityStore), so it cannot
//     see the binding on its own and is handed the graph binding as a class leg.
//
// What is NOT closed here, and is ga-zp3uj: the agent-side recovery tiers
// (`bd list --status in_progress`, on_death, on_boot) are raw bd commands in the
// agent's work directory, so they stay topology-blind — internal/config's
// TestQueryKindsWithoutAReadyReadAreTopologyBlind pins that from the query side.
// Those re-serve a strand rather than losing it, because the reconciler scan
// above is what releases it.
func assertClassRoutedClaimIsReleasable(t *testing.T, e splitEnv) {
	t.Helper()
	for _, tt := range []struct {
		name    string
		leading beads.Store
		class   []beads.Store
	}{
		{
			name:    "reconciler scan — leads with the sessions-class store, which IS the binding",
			leading: e.sessionsStore(),
		},
		{
			name:    "gc session close — leads with the work store and is handed the binding",
			leading: e.work,
			class:   []beads.Store{e.class},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !workAssignmentStoresReach(workAssignmentStores(tt.leading, e.rigStores, tt.class...), e.class) {
				t.Fatalf("no leg of the release scan is the class binding; a claim routed there is released by nothing")
			}
			sessionBead := beads.Bead{ID: "gcg-retired-session", Metadata: map[string]string{"session_name": "worker-1"}}
			step := e.mintWispWith(t, wispOpts{
				title:    "graph step claimed by a session that then died",
				status:   "in_progress",
				assignee: sessionBead.ID,
			})
			unclaimWorkAssignedToRetiredSessionBead(tt.leading, e.rigStores, sessionBead, "", io.Discard, tt.class...)
			released, err := e.class.Get(step.ID)
			if err != nil {
				t.Fatalf("reading the claimed graph step back from the binding: %v", err)
			}
			if !strings.EqualFold(strings.TrimSpace(released.Status), "open") || strings.TrimSpace(released.Assignee) != "" {
				t.Fatalf("the retired-session sweep left the binding-resident step %s as status=%q assignee=%q; a routed claim whose session died must be released, not stranded", step.ID, released.Status, released.Assignee)
			}
		})
	}
}

// workAssignmentStoresReach reports whether want is one of the scanned legs.
func workAssignmentStoresReach(stores []beads.Store, want beads.Store) bool {
	for _, store := range stores {
		if sameStorePtr(store, want) {
			return true
		}
	}
	return false
}

// defaultedHookClaimOps is the production claim-op set with every seam filled —
// the value classRoutedHookClaimOps is handed on a real invocation.
func defaultedHookClaimOps() hookClaimOps {
	ops := hookClaimOps{}
	ops.applyDefaults()
	return ops
}

// assertHookClaimOpsUnwrapped asserts every claim-time seam in got is the SAME
// function value as in want, which is how a single-store city's byte-identity is
// stated: not "it behaves the same" but "it is the same call". A wrapper that
// delegated unconditionally would behave identically and still be a different
// function, another allocation and another branch on the hottest control-plane
// operation.
func assertHookClaimOpsUnwrapped(t *testing.T, got, want hookClaimOps) {
	t.Helper()
	for _, seam := range []struct {
		name      string
		got, want any
	}{
		{"Claim", got.Claim, want.Claim},
		{"StampWorkMeta", got.StampWorkMeta, want.StampWorkMeta},
		{"ReadWorkMeta", got.ReadWorkMeta, want.ReadWorkMeta},
		{"ListContinuation", got.ListContinuation, want.ListContinuation},
		{"AssignContinuation", got.AssignContinuation, want.AssignContinuation},
		{"EmitExecutionStepStarted", got.EmitExecutionStepStarted, want.EmitExecutionStepStarted},
	} {
		if reflect.ValueOf(seam.got).Pointer() != reflect.ValueOf(seam.want).Pointer() {
			t.Errorf("classRoutedHookClaimOps replaced the %s seam on a city with no relocated class; a legacy city must run the identical function value", seam.name)
		}
	}
}

// hookStoreEnvValue reads one variable out of a hook fan-out leg's subprocess
// environment. Last assignment wins, matching exec's own resolution.
func hookStoreEnvValue(leg hookStore, key string) string {
	value := ""
	for _, entry := range leg.env {
		if name, v, ok := strings.Cut(entry, "="); ok && name == key {
			value = v
		}
	}
	return value
}

// conformanceStrictCrossStoreDeps (I6) guards the cross-store dependency class:
// a blocking edge whose endpoints live in different stores. Three different
// authorities answer these calls, and the invariant names which one it is
// pinning on every row, because they do not agree:
//
//   - CROSS-PREFIX target through the WORK front door: NEITHER production
//     backend rejects it. bd resolves the source, fails to resolve the target,
//     compares prefixes, and — because they differ — passes the target through
//     as a cross-prefix external ref and writes the row (cmd/bd/dep.go); SQLite
//     writes it too. The refusal asserted below is the splittest kit's DOMAIN
//     co-residence rule (convoy.TrackItemIn's ErrMemberNotCoResident), which a
//     BdSemantics leaf enforces on purpose and says so in its own wording. So
//     this row pins the domain invariant, NOT a backend behavior, and the fail-
//     open it names is LIVE in production: on a real split city the dangling
//     edge lands, the dependent silently drops out of Ready, and the parent goes
//     READY mid-DAG. Closing that is a later slice (ga-ia7li's by-id resolver is
//     what a co-residence preflight would be built on); until then the honest
//     statement is that the kit refuses what production performs.
//   - SAME-PREFIX unresolvable endpoint through the WORK front door: this one bd
//     genuinely hard-fails, in its own wording ("resolving issue ID %s: no issue
//     found matching %q"), because a same-prefix id gets no external-ref
//     pass-through. Asserted on BOTH topologies, so the suite exercises bd's
//     real rejection and not only the kit's domain rule.
//   - CROSS-PREFIX endpoint through the CLASS front door (SQLite): ACCEPTED and
//     recorded as a residence violation, because the deps table has no foreign
//     key and DepAdd is a plain INSERT. Not asserting a rejection here is the
//     same reasoning that makes the first bullet a domain pin rather than a
//     backend pin.
//
// The single-store subtest is the byte-identity half: one store resolves both
// endpoints, so every cross-store row succeeds and only the unresolvable-id row
// still fails.
func conformanceStrictCrossStoreDeps(t *testing.T, e splitEnv) {
	workBead, err := e.work.Create(beads.Bead{Title: "cross-dep work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	durable := mintDurableGraphBead(t, e, "cross-dep durable graph bead", "")
	wisp := e.mintWisp(t, "cross-dep wisp")
	// Same-prefix and guaranteed absent: bd's resolver gets no cross-prefix
	// pass-through for this shape, so it is the one endpoint failure bd itself
	// produces. The prefix is cfg-derived, and assertSplitEnvPins pins that the
	// work leaf mints under it.
	absentSamePrefix := config.EffectiveHQPrefix(e.cfg) + "-absent"

	for _, tt := range []struct {
		name     string
		front    beads.Store
		from, to string
		// sameStoreResolves is true when the single-store topology collapses
		// both endpoints into one store, which is what makes the row succeed
		// there. The unresolvable-id row is false: no topology can resolve it.
		sameStoreResolves bool
	}{
		{"work front door: work blocks-on wisp", e.work, workBead.ID, wisp.ID, true},
		{"work front door: work blocks-on durable", e.work, workBead.ID, durable.ID, true},
		{"graph front door: wisp blocks-on work", e.graphStore(), wisp.ID, workBead.ID, true},
		{"graph front door: durable blocks-on work", e.graphStore(), durable.ID, workBead.ID, true},
		{"work front door: work blocks-on an absent same-prefix id", e.work, workBead.ID, absentSamePrefix, false},
	} {
		err := tt.front.DepAdd(tt.from, tt.to, "blocks")
		if !tt.sameStoreResolves {
			// bd's own failure, on both topologies. The wording is asserted
			// because a test that accepted any error would also accept the kit's
			// domain rule, which is a different rejection for a different reason.
			if err == nil || !strings.Contains(err.Error(), "no issue found matching") {
				t.Errorf("%s: DepAdd(%s → %s) = %v, want bd's own resolution failure (`no issue found matching`) — a same-prefix target gets no external-ref pass-through, so this is the endpoint rejection bd really performs", tt.name, tt.from, tt.to, err)
			}
			continue
		}
		if !e.split {
			if err != nil {
				t.Errorf("%s: single-store DepAdd(%s → %s) = %v, want success (one store resolves both endpoints)", tt.name, tt.from, tt.to, err)
			}
			continue
		}
		if sameStorePtr(tt.front, e.work) {
			// The kit's DOMAIN co-residence rule, not bd's. bd would WRITE this
			// row as a cross-prefix external ref; the failure text below says so
			// so a later slice cannot read this as "production refuses it".
			if err == nil || !strings.Contains(err.Error(), "belongs to another store's id namespace") {
				t.Errorf("%s: DepAdd(%s → %s) = %v, want splittest's DOMAIN co-residence refusal (convoy.TrackItemIn's ErrMemberNotCoResident shape). NOTE what this does NOT say: bd does not reject a cross-prefix target — it writes the dangling external ref, and so does SQLite. This row pins the domain invariant a co-residence preflight will have to enforce; the fail-open (dependent drops out of Ready, parent goes READY mid-DAG) is live in production until that lands", tt.name, tt.from, tt.to, err)
			}
			if err != nil && strings.Contains(err.Error(), "no issue found matching") {
				t.Errorf("%s: the cross-prefix refusal now borrows bd's resolution wording (%v); bd never emits that message for a cross-prefix target, and a domain rule dressed in bd's clothes is what made this invariant misread once already", tt.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: DepAdd(%s → %s) = %v, want SQLite's silent acceptance — asserting a rejection here would test an error branch production never takes", tt.name, tt.from, tt.to, err)
		}
	}

	if !e.split {
		return
	}
	// Claiming the recorded violations is the assertion: the class store DID
	// accept the dangling edges, which is what production does, and the suite
	// says so out loud instead of letting the kit's cleanup fail the test.
	violations := splittest.TakeResidenceViolations(e.class)
	if len(violations) != 2 {
		t.Errorf("class store recorded %d residence violations, want 2 (one per cross-store dep taken through the class front door): %v", len(violations), violations)
	}
	for _, v := range violations {
		if v.Op != "dep-add" {
			t.Errorf("recorded violation %q is not a dep-add", v)
		}
	}
}

// conformanceByIDReadFederation (I7) guards the by-id READ half of the split —
// the other bug this program already paid for in production. `bd sql` kept
// answering from the work ledger for relocated graph ids and reported every live
// molecule root as missing (#5125). Two things must hold on a split city:
//
//   - storeref.Resolve, the federating point read every future by-id router is
//     built on, finds a class-resident bead across [work, class] legs — for the
//     durable shape AND for the -wisp- suffix shape, whose prefix heuristic
//     answer differs from an ordinary class id's.
//   - the shipped protection holds: a `bd sql` / `bd query` naming a relocated
//     id is REFUSED rather than answered from the work ledger, and the refusal
//     names the class-routed verb.
//
// The single-store subtest is the inertness half: the identical query passes
// through untouched when nothing is relocated.
func conformanceByIDReadFederation(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "federated durable graph bead", "")
	wisp := e.mintWisp(t, "federated read wisp")
	legs := []beads.Store{e.work, e.class} // class is nil on single-store; Resolve skips nil legs

	for _, tt := range []struct{ name, id string }{
		{"durable", durable.ID},
		{"wisp", wisp.ID},
	} {
		got, err := storeref.Resolve(tt.id, legs)
		if err != nil || got.ID != tt.id {
			t.Errorf("%s: storeref.Resolve(%q) = (%q, %v), want the bead — a federating by-id read that misses is the \"root does not exist\" report of #5125", tt.name, tt.id, got.ID, err)
		}
		msg, refused := bdSQLRelocatedClassRefusal(e.cfg, []string{"sql", "select id, status from issues where id = '" + tt.id + "'"})
		if refused != e.split {
			t.Errorf("%s: bdSQLRelocatedClassRefusal(sql naming %s) refused = %v, want %v (%s)", tt.name, tt.id, refused, e.split, msg)
		}
		if refused && !strings.Contains(msg, "gc beads show <id>") {
			t.Errorf("%s: refusal does not point at the class-routed verb: %s", tt.name, msg)
		}
		// `bd query` is the sibling verb an operator lands on when steered off
		// `bd sql`; it names the same namespace and answers [] with exit 0.
		if _, refused := bdSQLRelocatedClassRefusal(e.cfg, []string{"query", "id=" + tt.id}); refused != e.split {
			t.Errorf("%s: bd query naming %s refused = %v, want %v", tt.name, tt.id, refused, e.split)
		}
	}

	// A work-ledger query must never be refused on either topology: a guard that
	// swallows ordinary reads is a worse outage than the one it prevents.
	if msg, refused := bdSQLRelocatedClassRefusal(e.cfg, []string{"sql", "select id from issues where status <> 'closed'"}); refused {
		t.Errorf("a work-ledger query was refused: %s", msg)
	}
}

// conformanceResidenceSweep (I8) is the integrity backstop, and it generalizes
// the check that made the stranded-order-tracking incident fatal rather than
// silent: boot refuses when an infrastructure bead sits in the work store. After
// minting a representative population — work beads with a dep, one durable bead
// per coordination class (session, mail, order-tracking, nudge), a wisp, and a
// full molecule — every bead's coordclass classification must match its resident
// store, every dependency's endpoints must co-reside, and the reserved id-prefix
// boundary that by-id routing rides on must hold. On a legacy city the
// population collapses into the one store and no id is reserved-prefixed.
func conformanceResidenceSweep(t *testing.T, e splitEnv) {
	w1, err := e.work.Create(beads.Bead{Title: "sweep work bead one", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	w2, err := e.work.Create(beads.Bead{Title: "sweep work bead two", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := e.work.DepAdd(w2.ID, w1.ID, "blocks"); err != nil {
		t.Fatalf("co-resident work dep: %v", err)
	}
	if _, err := e.classStore(config.BeadClassSessions).Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-1"},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassMessaging).Create(beads.Bead{Title: "mail: sweep", Type: "message"}); err != nil {
		t.Fatalf("create mail bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassOrders).Create(beads.Bead{
		Title:  "order tracking: sweep",
		Type:   "task",
		Labels: []string{labelOrderTracking},
	}); err != nil {
		t.Fatalf("create order-tracking bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassNudges).Create(beads.Bead{
		Title:  "nudge: sweep",
		Type:   "task",
		Labels: []string{nudgeBeadLabel},
	}); err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	e.mintWisp(t, "sweep wisp")
	if _, err := molecule.Instantiate(context.Background(), e.graphStore(), conformanceGraphRecipe(), molecule.Options{}); err != nil {
		t.Fatalf("materialize sweep molecule: %v", err)
	}

	type sweepLeg struct {
		name      string
		store     beads.Store
		wantClass bool
	}
	legs := []sweepLeg{{"work", e.work, false}}
	if e.split {
		legs = append(legs, sweepLeg{"class", e.class, true})
	}
	for _, leg := range legs {
		list, err := leg.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			t.Fatalf("%s store list: %v", leg.name, err)
		}
		if len(list) == 0 {
			t.Fatalf("%s store is empty after the representative mint; the sweep is vacuous", leg.name)
		}
		for _, b := range list {
			if e.split {
				if gotInfra := coordclass.Classify(b).IsInfrastructure(); gotInfra != leg.wantClass {
					t.Errorf("%s store holds %s (type=%q labels=%v metadata=%v): coordclass infrastructure=%v — resident on the wrong side of the boundary, which is what boot reads as a stranded bead", leg.name, b.ID, b.Type, b.Labels, b.Metadata, gotInfra)
				}
			}
			wantReserved := e.split && leg.wantClass
			if got := reservedClassNamespace(b.ID); got != wantReserved {
				t.Errorf("%s store bead %q: in a reserved class namespace = %v, want %v — the id-prefix boundary by-id routing rides on is broken", leg.name, b.ID, got, wantReserved)
			}
			deps, err := leg.store.DepList(b.ID, "down")
			if err != nil {
				t.Errorf("%s store DepList(%s): %v", leg.name, b.ID, err)
				continue
			}
			for _, d := range deps {
				if _, err := leg.store.Get(d.DependsOnID); err != nil {
					t.Errorf("dep %s → %s: the endpoint does not co-reside in the %s store (%v) — a dangling edge silently drops %s out of Ready", b.ID, d.DependsOnID, leg.name, err, b.ID)
				}
			}
		}
	}
}

// conformanceWarmTickDemand (I9) guards the treadmill driver: a cross-store
// demand probe that goes blind on WARM ticks drains every just-spawned session
// before its agent can claim, and pool_desired cycles forever. Through the
// rig-legged fixture with the sessions store LEADING (exactly as
// CityRuntime.buildDesiredState wires it): a cold tick spawns sessions for
// routed leading-store work, and CONSECUTIVE warm ticks — work still
// open/unclaimed — must keep demand AND the spawned sessions desired, without
// minting replacements. On a split city the routed work is class-resident, so a
// warm path that reads the work store instead reads zero.
func conformanceWarmTickDemand(t *testing.T, e splitEnv) {
	e.mintWispWith(t, wispOpts{title: "routed treadmill wisp A", routedTo: e.qualified})
	e.mintWispWith(t, wispOpts{title: "routed treadmill wisp B", routedTo: e.qualified})

	cold := buildDesiredStateWithSessionBeads(
		"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
		e.sessionsStore(), e.rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)
	if len(cold.State) != 2 {
		t.Fatalf("cold tick desired sessions = %d, want 2", len(cold.State))
	}

	for tick := 1; tick <= 2; tick++ {
		snap, err := loadSessionBeadSnapshot(e.sessionsStore())
		if err != nil {
			t.Fatalf("load session snapshot before warm tick %d: %v", tick, err)
		}
		warm := buildDesiredStateWithSessionBeads(
			"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), e.rigStores, snap, nil, os.Stderr,
		)
		if got := warm.ScaleCheckCounts[e.qualified]; got != 2 {
			t.Errorf("warm tick %d demand = %d, want 2 (routed leading-store demand went blind while sessions ran — the treadmill)", tick, got)
		}
		if len(warm.State) != 2 {
			t.Errorf("warm tick %d desired sessions = %d, want 2 (just-spawned sessions fell out of desiredState)", tick, len(warm.State))
		}
	}

	after, err := session.ListAllSessionBeads(e.sessionsStore(), beads.ListQuery{})
	if err != nil {
		t.Fatalf("list session beads after warm ticks: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("session beads after warm ticks = %d, want 2 (warm ticks must reuse the spawned sessions, not mint replacements)", len(after))
	}
}

// conformanceWakeOwnershipFastPath (I10) pins the conformance property of the
// wake filter and the orphan-release ownership index: whatever each answers, it
// answers IDENTICALLY on both topologies. Neither reads which physical store a
// bead came out of; both resolve from cfg plus the holder's own identities, so a
// legacy city and a split city cannot diverge.
//
// The two mechanisms no longer answer the same question, and that is deliberate.
//
//   - The WAKE filter now keeps a claim on the LEADING arm when the assignee is
//     one of the holder's own exact identities, whatever the holder's rig scope.
//     That arm is the relocated class binding on a split city — where claim-time
//     class routing writes the assignee — and the city store on a legacy one,
//     which a rig-scoped agent's hook fan-out reaches anyway
//     (appendCityHookStore). Dropping the claim left a live holder with
//     AwakeDecision{Reason:""} and the no-wake-reason drain recycled it mid-step
//     (ga-whzrt). Both sub-topologies keep it, so the conformance property holds.
//   - The ownership INDEX is unchanged and stays rig-scoped. It is a fast path
//     for orphan release, and its last-resort live-session probe already spares
//     a class-resident claim (I2 proves that), so the gap it leaves is a
//     per-tick cost rather than a wrong answer. Widening it is a release-path
//     change with its own blast radius and belongs to its own slice.
//
// If you widen the ownership index, update its assertion here and re-check that
// both sub-topologies still agree.
func conformanceWakeOwnershipFastPath(t *testing.T, e splitEnv) {
	sess, err := e.sessionsStore().Create(splitEnvPoolSessionBead(e.qualified, "executor-1"))
	if err != nil {
		t.Fatalf("create rig-bound pool session bead: %v", err)
	}
	wisp := e.mintWispWith(t, wispOpts{title: "claimed wake wisp", routedTo: e.qualified, status: "in_progress", assignee: sess.ID})
	rigWork, err := e.rig.Create(beads.Bead{
		Title:    "claimed rig work bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.qualified},
	})
	if err != nil {
		t.Fatalf("create rig work bead: %v", err)
	}
	if err := e.rig.Update(rigWork.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: &sess.ID}); err != nil {
		t.Fatalf("claim rig work bead: %v", err)
	}
	rigWork, err = e.rig.Get(rigWork.ID)
	if err != nil {
		t.Fatalf("reload claimed rig work bead: %v", err)
	}

	infos := sessionInfosFromBeads([]beads.Bead{sess})
	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(
		e.cfg, e.cityPath, infos,
		[]beads.Bead{wisp, rigWork}, []string{"", e.rigName},
	)
	index := makeOpenSessionStoreRefIndex(e.cityPath, e.cfg, infos, true)

	if len(kept) != 2 || kept[0].ID != wisp.ID || kept[1].ID != rigWork.ID ||
		len(keptRefs) != 2 || keptRefs[0] != classBindingAssignedWorkStoreRef || keptRefs[1] != e.rigName {
		t.Fatalf("wake filter kept %d beads (ids %v refs %v), want the leading-arm claim %s under ref %q AND the rig-store claim %s under ref %q",
			len(kept), assignedWorkIDs(kept), keptRefs, wisp.ID, classBindingAssignedWorkStoreRef, rigWork.ID, e.rigName)
	}
	if !openSessionOwnsWork(nil, index, sess.ID, e.rigName, true) {
		t.Error("the ownership index does not own the rig-store leg for its own rig-bound holder — orphan release would fall to the per-bead live probe every tick")
	}
	if openSessionOwnsWork(nil, index, sess.ID, "", true) {
		t.Error("the ownership index owns the leading-store leg for a rig-bound holder; reachability is cfg-derived and must answer the same on both topologies. If you are landing the coordination-class reachability arm, update this assertion and the single-store one together.")
	}
}

// conformanceReadPathConsistency (I11) pins the operator-confusion class from the
// live treadmill debugging: a store holding work looks EMPTY through the default
// `bd list` view while the controller is serving demand from it, so operators
// conclude "no work exists" while the fleet claims. The read paths must answer
// exactly as production does on each topology, and the two topologies DIFFER
// here for a structural reason worth stating: a work store sits behind
// cmd/gc's bead-policy layer (create-time storage policy + read-tier expansion)
// while a relocated class store does not — openStorageRoutes keys the class map
// straight to the engine value the provider returned.
//
// So on a legacy city the wisp is ephemeral and only the tier-expanding front
// door sees it; on a split city the same create is a durable row that every read
// path sees. What must hold on BOTH is the property the incident was about: the
// reader the controller actually uses must never be blind to work the store
// holds.
func conformanceReadPathConsistency(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "read-path durable graph bead", "")
	wisp := e.mintWisp(t, "read-path wisp")
	front := e.graphStore()

	// The controller's own demand reader must see both beads on either topology.
	// This is the assertion the incident maps onto: a reader that goes blind here
	// reports zero demand while the store holds work.
	demand, err := listOpenForControllerDemandLive(front)
	if err != nil {
		t.Fatalf("controller demand read: %v", err)
	}
	if !beadListHasID(demand, durable.ID) || !beadListHasID(demand, wisp.ID) {
		t.Errorf("the controller demand read sees durable=%v wisp=%v, want both — a demand reader blind to work the store holds is the treadmill", beadListHasID(demand, durable.ID), beadListHasID(demand, wisp.ID))
	}

	leaf, _, wrapped := unwrapBeadPolicyStore(front)
	if !wrapped {
		// Relocated class store: no policy layer, so no tier EXPANSION and no
		// tier for these two beads to hide behind — mintWisp's create lands a
		// plain row here. That is a statement about what this front door
		// CREATES, not about what the store can hold: a create that names the
		// tier itself still lands an ephemeral row in this store (see
		// mintEphemeralGraphBead), and reading it back is exactly what the
		// federated readers were failing to do. I16 owns that half.
		//
		// Which branch runs is decided by the fixture's model of the class
		// store, so it cannot police that model. The pin that does is
		// TestSplitEnvClassStoreWrappingMatchesOpenStorageRoutes, which asks
		// openStorageRoutes directly: wrap the class map there and the fixture,
		// this branch, and the wisp tier all move together.
		list, err := front.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatalf("class-store default list: %v", err)
		}
		if !beadListHasID(list, durable.ID) || !beadListHasID(list, wisp.ID) {
			t.Errorf("relocated class store default List sees durable=%v wisp=%v, want both (no policy layer means no tier to hide behind)", beadListHasID(list, durable.ID), beadListHasID(list, wisp.ID))
		}
		return
	}

	leafList, err := leaf.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("leaf default list: %v", err)
	}
	if !beadListHasID(leafList, durable.ID) || beadListHasID(leafList, wisp.ID) {
		t.Errorf("the `bd list` view (leaf default List) sees durable=%v wisp=%v, want true/false — wisps are invisible to the operator's default list, which is the whole confusion", beadListHasID(leafList, durable.ID), beadListHasID(leafList, wisp.ID))
	}

	frontList, err := front.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("front-door default list: %v", err)
	}
	if !beadListHasID(frontList, durable.ID) || !beadListHasID(frontList, wisp.ID) {
		t.Errorf("the front-door default List sees durable=%v wisp=%v, want both — warm-tick readers on this path must not be wisp-blind", beadListHasID(frontList, durable.ID), beadListHasID(frontList, wisp.ID))
	}

	frontReady, err := front.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("front-door ready: %v", err)
	}
	if !beadListHasID(frontReady, wisp.ID) || !beadListHasID(frontReady, durable.ID) {
		t.Errorf("front-door Ready sees durable=%v wisp=%v, want both — the ready view must include open wisps", beadListHasID(frontReady, durable.ID), beadListHasID(frontReady, wisp.ID))
	}
	leafReady, err := leaf.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("leaf default ready: %v", err)
	}
	if beadListHasID(leafReady, wisp.ID) {
		t.Errorf("leaf default Ready surfaces wisp %s — the default ready is main-tier only; the tier expansion belongs to the policy front door", wisp.ID)
	}

	eph, err := front.List(beads.ListQuery{
		Metadata:  map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("ephemeral (wisp-GC shape) query: %v", err)
	}
	if len(eph) != 1 || eph[0].ID != wisp.ID {
		got := make([]string, len(eph))
		for i, b := range eph {
			got[i] = b.ID
		}
		t.Errorf("the ephemeral query returned %v, want exactly the wisp %s", got, wisp.ID)
	}
	if len(eph) == 1 && !eph[0].Ephemeral {
		t.Errorf("the ephemeral query returned %s without the ephemeral flag — the bead is not genuinely on the wisp tier", eph[0].ID)
	}
}

// conformanceGraphRecipe is the durable graph.v2 workflow shape a compiled v2
// formula actually produces: a root plus one finalize step, wired with the ONE
// root -> finalize `blocks` edge and no parent-child edge at all.
//
// The missing parent-child edge is the point. internal/formula/compile.go gates
// both of its parent-child emitters on `!graphWorkflow`, and addWorkflowRootDeps
// emits only the blocks edge, so a graph.v2 recipe carries zero parent-child
// deps — measured over the core pack's v2 formulas. This recipe used to add one
// by hand, which is the only reason materializing it on the real SQLite backend
// tripped sqlite_store_graph_apply.go's reverse-of-a-parent-child guard: that
// guard reads sqliteGraphApplyParentDepPairs, which is built from ParentID,
// which on a recipe plan is set only by a `parent-child` dep. A fixture that
// carries an edge the compiler cannot emit turns a production-shaped invariant
// into a statement about the fixture.
func conformanceGraphRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "conformance-graph",
		Steps: []formula.RecipeStep{
			{
				ID:     "conformance-graph",
				Title:  "Conformance workflow",
				Type:   "task",
				IsRoot: true,
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
				},
			},
			{
				ID:       "conformance-graph.workflow-finalize",
				Title:    "Finalize conformance workflow",
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "conformance-graph", DependsOnID: "conformance-graph.workflow-finalize", Type: "blocks"},
		},
	}
}

// conformanceWispRecipe is the root-only shape a wisp materializes from: the
// root bead IS the work (gc.kind=wisp), no child steps.
func conformanceWispRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name:     "conformance-vapor",
		RootOnly: true,
		Steps: []formula.RecipeStep{{
			ID:       "conformance-vapor",
			Title:    "conformance vapor wisp root",
			Type:     "task",
			IsRoot:   true,
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
		}},
	}
}

// countGraphClassBeads counts the beads in a store that coordclass routes to the
// graph class, across both tiers.
func countGraphClassBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing beads for the graph-class count: %v", err)
	}
	n := 0
	for _, b := range list {
		if coordclass.Classify(b) == coordclass.ClassGraph {
			n++
		}
	}
	return n
}

// splitEnvDeadAssignee is the identity of a session that is gone: it holds a
// claim, and no session bead names it, so orphan release must recover the claim
// wherever the release pass can reach it.
const splitEnvDeadAssignee = "s-dead99"

// splitEnvDeadClaimedWorkBead mints a routed WORK-class bead claimed by a dead
// session, in the store handed to it. It is the HQ-work leg of I1/I2: a plain
// work-class bead (no reserved prefix, coordclass ClassWork) carrying
// gc.routed_to, which is what order dispatch stamps on a city-scope target.
func splitEnvDeadClaimedWorkBead(t *testing.T, store beads.Store, qualified string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:    "dead-claimed HQ work bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: qualified},
	})
	if err != nil {
		t.Fatalf("create HQ work bead: %v", err)
	}
	if coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("HQ bead %s classifies as infrastructure; this leg is about the WORK class specifically", created.ID)
	}
	inProgress := "in_progress"
	assignee := splitEnvDeadAssignee
	if err := store.Update(created.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("stage the dead claim on HQ work bead %s: %v", created.ID, err)
	}
	staged, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("reload staged HQ work bead %s: %v", created.ID, err)
	}
	return staged
}

// splitEnvPoolSessionBead is the open pool-worker session bead a warm rig pool
// runs on: session type + label, the pool template it was spawned from, and an
// active state. The reconciler resolves its reachable store-ref from the template
// (openSessionReachableStoreRefInfo), so the template is what makes it rig-bound.
func splitEnvPoolSessionBead(qualified, sessionName string) beads.Bead {
	return beads.Bead{
		Title:  sessionName,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": sessionName,
			"state":        "active",
		},
	}
}

// conformanceMoleculeMembership (I12) pins WHICH beads a molecule's member set
// contains and WHICH store has to be asked for it.
//
// beads.DirectMembers is the declared fan-out membership
// (beads.MembershipDirectRootID): the root plus everything carrying
// gc.root_bead_id. The rule is complete by construction — materialization
// stamps the key on every step — where a dependency walk is not: a gc.kind=spec
// sidecar is built with no DependsOn, Needs or WaitsFor, so no dep walk reaches
// it, and on the measured live molecule gcg-arn a dep walk returned 48 of the
// 61 beads the root-id scan returned, dropping exactly the 13 specs.
// internal/beads/membership_test.go pins those numbers; this invariant pins
// that the answer does not depend on the topology, only on the front door.
//
// The graph class is the owner, so both topologies must return the same member
// set through e.graphStore(). A projection that resolved the store any other
// way gets a shorter, entirely plausible answer on a split city.
//
// KNOWN GAP, pinned rather than asserted as desirable — the SILENT EMPTY.
// Asked for the same root, the WORK store on a split city answers with an empty
// member set rather than an error: the root does not resolve there (a
// deliberate non-error, so a relocated root does not read as an empty molecule)
// and no member carries the id, so the two absences compose into a confident
// "this molecule has no members". That is Invariant 0 of ga-iaj7k — a
// projection that cannot see a class must ERROR, not return [] — and it is not
// closed. When it closes, the split arm below flips from "0 members" to "an
// error naming the class", and both arms move together.
//
// The gap here is the OBJECT-MODEL one and I14 did not close it. I14 closed the
// CLI half — `gc bd list --metadata-field gc.root_bead_id=<root>` refuses
// instead of answering [] — by classifying the argv before any store is opened.
// This half is a beads.DirectMembers call handed the wrong store, and nothing in
// that signature says which classes the store serves, so closing it means giving
// a store the ability to refuse a class it does not hold. That is a behavior
// change on every DirectMembers consumer, not a read-path guard.
func conformanceMoleculeMembership(t *testing.T, e splitEnv) {
	front := e.graphStore()
	root := mintDurableGraphBead(t, e, "membership molecule root", "")

	step, err := front.Create(beads.Bead{
		Title: "dependency-linked step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.StepRefMetadataKey:    "mol.work",
		},
	})
	if err != nil {
		t.Fatalf("create linked step: %v", err)
	}
	if err := front.DepAdd(root.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("wire root -> step: %v", err)
	}
	// The sidecar with no edges: the member only the root-id rule can reach.
	spec, err := front.Create(beads.Bead{
		Title: "Step spec for the linked step",
		Type:  "spec",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.KindMetadataKey:       beadmeta.KindSpec,
			beadmeta.SpecForRefMetadataKey: "mol.work",
		},
	})
	if err != nil {
		t.Fatalf("create spec sidecar: %v", err)
	}

	members, err := beads.DirectMembers(front, root.ID)
	if err != nil {
		t.Fatalf("beads.DirectMembers through the graph front door: %v", err)
	}
	got := make(map[string]bool, len(members))
	for _, m := range members {
		got[m.ID] = true
	}
	for _, want := range []beads.Bead{root, step, spec} {
		if !got[want.ID] {
			t.Errorf("the molecule member set is missing %s (%q); %s is complete by construction on both topologies, and a short answer here is the shape that reads as a finished molecule",
				want.ID, want.Title, beads.MembershipDirectRootID)
		}
	}
	if len(members) != 3 {
		t.Errorf("member set = %d beads, want 3 (root + step + spec); got %v", len(members), beadIDsOf(members))
	}

	// The store the projection asks decides the answer. On a split city the
	// work store is not merely wrong, it is quietly wrong.
	if e.split {
		fromWork, err := beads.DirectMembers(e.work, root.ID)
		if err != nil {
			t.Fatalf("beads.DirectMembers through the WORK store: %v", err)
		}
		if len(fromWork) != 0 {
			t.Fatalf("the WORK store answered with %d members on a split city (%v); it holds none of them, so anything but an empty set means a shadow row was minted",
				len(fromWork), beadIDsOf(fromWork))
		}
		// Restate the gap as an assertion so it moves when Invariant 0 lands.
		t.Logf("KNOWN GAP (ga-iaj7k Invariant 0): the WORK store returned an empty member set for %s instead of an error naming the graph class", root.ID)
	}
}

// beadIDsOf renders a bead slice as ids for a failure message.
func beadIDsOf(list []beads.Bead) []string {
	ids := make([]string, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	return ids
}

// conformanceCLIReadyFederation (I13) is the CLI half of the split-store ready
// federation whose API half PR #5148 landed, and it asserts the one thing that
// makes the CLI half checkable: CLI == API.
//
// The bug it guards is the one measured on a live split city — `gc bd ready`
// answered with 5 beads and ZERO `gcg-`, while GET /v0/beads/ready answered with
// 22, 14 of them `gcg-`, at the same moment. The whole execution DAG was
// invisible to the CLI behind an answer that looked authoritative, and a work
// query that returns a short array is indistinguishable from one that returns
// "no work".
//
// The oracle is NOT invented. internal/api's humaHandleBeadReady carries an
// executable FEDERATION CONTRACT written for this invariant: legs city → rigs
// ascending → graph last, per-leg order whatever that leg's reader emits, dedupe
// first-leg-wins, and — the load-bearing part — BOTH sides compared after
// normalizing with beads.SortBeadsReadyOrder, because per-leg order is
// deterministic but not canonical across leg kinds (a caching-wrapped work store
// emits (priority, created_at, id); the canonical relocated binding emits
// (created_at, id) with no priority term). So this runs the real API handler and
// the real CLI reader over the SAME three stores and compares the two answers.
//
// The single-store row is not a formality: it is the byte-identity claim. There
// the graph class is not relocated, both surfaces federate the same two work
// legs, and the answer must be exactly the one a legacy city already got.
//
// # What an equality oracle cannot see, and what covers it
//
// CLI == API is blind by construction to a defect BOTH surfaces have. ga-8lyxc
// was exactly that: the CLI defaulted its ready read to the zero-value tier and
// the API passed no ready query at all, so both dropped the relocated store's
// ephemeral rows and this row stayed green while both surfaces were short. I16
// is the complement — its oracle is the LEG, not the other surface — and the two
// rows have to be read together: this one pins that the surfaces agree, that one
// pins that what they agree on is everything the stores hold.
func conformanceCLIReadyFederation(t *testing.T, e splitEnv) {
	cityWork, err := e.work.Create(beads.Bead{Title: "city work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seed city work: %v", err)
	}
	rigWork, err := e.rig.Create(beads.Bead{Title: "rig work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seed rig work: %v", err)
	}
	// Orchestration work is wisps, not durable rows: an invariant seeded only
	// with durable beads has already missed a live incident.
	wisp := e.mintWispWith(t, wispOpts{title: "graph step wisp"})
	durable := mintDurableGraphBead(t, e, "graph control bead", "")

	cliIDs := cliReadyIDs(t, e)
	apiIDs := apiReadyIDs(t, e)

	if !reflect.DeepEqual(cliIDs, apiIDs) {
		t.Fatalf("gc ready = %v but GET /v0/beads/ready = %v over the same stores; the CLI and the API disagree about what is claimable, which is the split-store blindness this federation exists to close", cliIDs, apiIDs)
	}
	// Orchestration work — the wisp — has to be in there on BOTH topologies, and
	// it gets there by two different routes: the single-store front door is
	// policy-wrapped, so it lands the wisp on the ephemeral tier and expands a
	// default ready read to reach it; the relocated class store has no policy
	// layer, so the same create lands a durable row that a plain ready read
	// already sees (see the fixture header). Either way an orchestration bead
	// that is claimable must be claimable through this reader.
	for _, want := range []string{cityWork.ID, rigWork.ID, durable.ID, wisp.ID} {
		if !containsString(cliIDs, want) {
			t.Errorf("federated ready is missing %s; it is claimable on both topologies (graph front door policy-wrapped = %v)", want, e.policyWrapped(e.graphStore()))
		}
	}
	if !e.split {
		// Byte identity: no second store exists, so the federation must not have
		// invented a leg. Every id came from the two work legs.
		if graph := fixtureGraphLeg(e); graph != nil {
			t.Fatalf("a single-store city resolved a graph leg (%T); its answer is no longer the one it had before the federation existed", graph)
		}
	}
	conformanceCLIReadyDeadRigLeg(t, e)
}

// conformanceCLIReadyDeadRigLeg pins the one place CLI and API are allowed to
// answer DIFFERENTLY, so the divergence stays a decision instead of drift.
//
// The two surfaces cannot answer the same way here and both be honest. An HTTP
// body has a `partial_errors` field, so the API serves what it could read and
// says which rig it could not: Partial 200. A CLI work query has no such field —
// its whole output is the array — so serving what it could read means serving a
// short array indistinguishable from "no work", which is the fail-open the whole
// federation exists to close. Its equivalent of "Partial 200 naming the rig" is
// therefore "non-zero exit naming the rig", and the correspondence pinned here is
// exactly that: BOTH surfaces name the dead rig, and NEITHER passes the short
// answer off as complete.
//
// The dead leg is an unavailableStore, which is not a contrivance: it is the
// value controllerState.buildStores puts in the map when a rig store fails to
// OPEN (api_state.go), so this is the API's own representation of the field
// failure — a rig whose .gc is unmounted, half-deleted, or no longer readable.
func conformanceCLIReadyDeadRigLeg(t *testing.T, e splitEnv) {
	t.Helper()
	const deadRig = "rig-DEAD"
	const cause = "stat .gc/beads.json: not a directory"
	withDead := make(map[string]beads.Store, len(e.rigStores)+1)
	for name, store := range e.rigStores {
		withDead[name] = store
	}
	withDead[deadRig] = unavailableStore{err: errors.New(cause)}
	dead := e
	dead.rigStores = withDead

	body := apiReadyBody(t, dead)
	if !body.Partial {
		t.Fatalf("GET /beads/ready over a dead %q leg reported a COMPLETE read of %d beads; the API's contract for an unreadable rig is a Partial 200", deadRig, len(body.Items))
	}
	if !containsSubstring(body.PartialErrors, deadRig) {
		t.Fatalf("API partial_errors = %v, want the dead rig %q named", body.PartialErrors, deadRig)
	}

	legs := readyFederationLegs(loadedCityName(dead.cfg, dead.cityPath), dead.work, dead.rigStores, fixtureGraphLeg(dead))
	rows, err := readyBeadsForOpts(legs, readyOpts{})
	if err == nil {
		t.Fatalf("gc ready served %d beads over the same dead %q leg; the API said that read was PARTIAL, and a bare array has nowhere to say so — the CLI's equivalent of Partial 200 is a non-zero exit", len(rows), deadRig)
	}
	if !strings.Contains(err.Error(), deadRig) || !strings.Contains(err.Error(), cause) {
		t.Fatalf("gc ready error = %v, want it to name the dead rig %q and the cause, the way the API's partial_errors do", err, deadRig)
	}
}

// containsSubstring reports whether any element contains want.
func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// cliReadyIDs runs the `gc ready` reader over the fixture's stores and returns
// the ids it emits, in the order it emits them (already canonical — the CLI
// applies beads.SortBeadsReadyOrder so a bounded read cuts the true top-N).
//
// The legs are assembled through the production helpers, including the identity
// gate, so this exercises the leg-selection decision rather than restating it.
func cliReadyIDs(t *testing.T, e splitEnv) []string {
	t.Helper()
	legs := readyFederationLegs(
		loadedCityName(e.cfg, e.cityPath),
		e.work,
		e.rigStores,
		fixtureGraphLeg(e),
	)
	rows, err := readyBeadsForOpts(legs, readyOpts{})
	if err != nil {
		t.Fatalf("gc ready over the fixture stores: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// apiReadyIDs serves GET /v0/city/{city}/beads/ready through the REAL API
// handler over the same stores, and returns its ids normalized with
// beads.SortBeadsReadyOrder.
//
// The normalization is the contract's, not a convenience: the API's merged order
// is leg concatenation and deliberately NOT re-sorted, because a global sort
// would change the bytes a multi-rig single-store city already serves. Both
// sides are therefore compared in canonical order.
//
// The state is a controllerState — the production api.State — built from the
// fixture, so BeadStores(), CityBeadStore() and GraphBeadStore() resolve through
// exactly the dispatch the running controller uses.
func apiReadyIDs(t *testing.T, e splitEnv) []string {
	t.Helper()
	body := apiReadyBody(t, e)
	if body.Partial {
		t.Fatalf("API reported a partial ready read over healthy stores: %v", body.PartialErrors)
	}
	beads.SortBeadsReadyOrder(body.Items)
	ids := make([]string, 0, len(body.Items))
	for _, b := range body.Items {
		ids = append(ids, b.ID)
	}
	return ids
}

// apiReadyListBody is the decoded 200 body of GET /beads/ready, including the
// partial tier the CLI has no room for.
type apiReadyListBody struct {
	Items         []beads.Bead `json:"items"`
	Partial       bool         `json:"partial"`
	PartialErrors []string     `json:"partial_errors"`
}

// apiReadyBody serves the ready read through the real handler and returns the
// whole decoded body, so a caller can assert on the partial tier as well as the
// rows.
func apiReadyBody(t *testing.T, e splitEnv) apiReadyListBody {
	t.Helper()
	return apiGetBeadListBody(t, e, "/beads/ready")
}

// apiGetBeadListBody serves one of the city-scoped bead read endpoints through
// the REAL handler stack over the fixture's stores and decodes the list body.
//
// The state is a controllerState — the production api.State — so BeadStores(),
// CityBeadStore() and GraphBeadStore() resolve through exactly the dispatch the
// running controller uses. suffix is the city-scoped path with its query string,
// e.g. "/beads/ready" or "/beads?status=in_progress".
func apiGetBeadListBody(t *testing.T, e splitEnv, suffix string) apiReadyListBody {
	t.Helper()
	cityName := loadedCityName(e.cfg, e.cityPath)
	state := &controllerState{
		cfg:           e.cfg,
		cityName:      cityName,
		cityPath:      e.cityPath,
		cityBeadStore: e.work,
		beadStores:    e.rigStores,
		storageRoutes: e.routes,
	}
	mux := api.NewSupervisorMux(&singleCityStateResolver{state: state}, nil, false, "test", "", time.Now()).WithAnyHostAllowed()
	req := httptest.NewRequest(http.MethodGet, "/v0/city/"+cityName+suffix, nil)
	rec := httptest.NewRecorder()
	mux.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body=%q)", suffix, rec.Code, rec.Body.String())
	}
	var body apiReadyListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (body=%q)", suffix, err, rec.Body.String())
	}
	// The partial tier is deliberately NOT asserted here: the dead-rig row
	// (conformanceCLIReadyDeadRigLeg) reads this endpoint expecting Partial
	// exactly, and a helper that failed on it would make that row unwritable.
	// Callers over healthy stores assert it themselves.
	return body
}

// conformanceFederatedReadTier (I16) pins that a city-wide federated read asks
// EVERY leg the same question about storage tiers.
//
// # The defect, measured on a live split city
//
// The legs are not wrapped alike. A work store sits behind cmd/gc's bead-policy
// layer, whose expandPolicyReadTier / expandPolicyReadyQuery rewrite a
// TierIssues read to TierBoth before it reaches the backend. A relocated class
// store has no such layer — openStorageRoutes keys the class map straight to the
// engine value the provider returned. So a query that left TierMode at its zero
// value asked the work legs one question and the class leg a narrower one, and
// merged the two answers as if they were the same question.
//
// win-mc-forge measured it, and the arithmetic is exact: against the relocated
// graph database `bd ready --include-ephemeral --limit 0` returned 17 claimable
// beads, three of them wisps; the federated reader over the same store returned
// exactly the other 14, while still serving the work stores' own ephemeral rows.
// No leg errored. No flag was rejected. The wisps were simply not there — and
// ephemeral wisps are how orchestration steps run, so a molecule mid-execution
// reads as having no runnable frontier and is diagnosed as stalled when it is
// fine.
//
// # Why the CLI == API row (I13) could not catch it
//
// I13's oracle is the OTHER surface, and both surfaces had the same hole: the
// CLI defaulted to TierIssues and the API passed no ready query at all. Two
// surfaces agreeing about a short answer is what an equality oracle is blind to
// by construction. So this row's oracle is the LEG ITSELF — everything the store
// holds must reach the merged answer — which is the same arithmetic
// win-mc-forge ran by hand and cannot be satisfied by two wrong surfaces
// agreeing.
//
// # Both topologies, and the single-store row is the byte-identity claim
//
// The single-store row is not a formality. Its owning leg is the policy-wrapped
// work store, which has ALWAYS answered at TierBoth, so this row passes before
// and after the fix and fails the moment the fix narrows a legacy city's answer.
func conformanceFederatedReadTier(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "federated-tier durable graph bead", "")
	wisp := mintEphemeralGraphBead(t, e, "federated-tier graph wisp")

	owner, ownerName := e.owner()
	legIDs := legReadyIDsAcrossTiers(t, owner)
	if !containsString(legIDs, wisp.ID) || !containsString(legIDs, durable.ID) {
		t.Fatalf("the %s store's own ready read = %v, missing durable=%s or wisp=%s; the fixture is not staging the two tiers this row is about", ownerName, legIDs, durable.ID, wisp.ID)
	}

	assertFederationServesWholeLeg(t, "gc ready", ownerName, legIDs, cliReadyIDs(t, e))
	assertFederationServesWholeLeg(t, "GET /v0/beads/ready", ownerName, legIDs, apiReadyIDs(t, e))

	conformanceFederatedInFlightTier(t, e, ownerName)
}

// conformanceFederatedInFlightTier is the in-flight arm of I16, and it is the
// symptom the incident was reported as: an adopt-pr step running as an ephemeral
// wisp is invisible in the mid-flight listing, so a molecule that is executing
// normally reads as having nothing in progress.
//
// It covers the other two federated readers — `gc ready --status in_progress`
// (federateListBeads) and GET /v0/beads?status=in_progress (the API's list
// fan-out) — which take a ListQuery rather than a ReadyQuery and had the same
// unstated tier.
func conformanceFederatedInFlightTier(t *testing.T, e splitEnv, ownerName string) {
	t.Helper()
	const holder = "executor-1"
	claimed := mintEphemeralGraphBead(t, e, "federated-tier in-flight graph wisp")
	inProgress := "in_progress"
	if err := e.graphStore().Update(claimed.ID, beads.UpdateOpts{Status: &inProgress, Assignee: stringPtr(holder)}); err != nil {
		t.Fatalf("claiming the in-flight graph wisp %s: %v", claimed.ID, err)
	}

	legs := readyFederationLegs(loadedCityName(e.cfg, e.cityPath), e.work, e.rigStores, fixtureGraphLeg(e))
	rows, err := readyBeadsForOpts(legs, readyOpts{status: readyStatusInProgress})
	if err != nil {
		t.Fatalf("gc ready --status in_progress over the fixture stores: %v", err)
	}
	cli := make([]string, 0, len(rows))
	for _, row := range rows {
		cli = append(cli, row.ID)
	}
	if !containsString(cli, claimed.ID) {
		t.Errorf("`gc ready --status in_progress` = %v, missing the claimed ephemeral %s-store wisp %s. This is the reported symptom verbatim: a step running as a wisp is invisible mid-flight, so the molecule reads as having no runnable frontier and is diagnosed as stalled while it is executing", cli, ownerName, claimed.ID)
	}

	body := apiGetBeadListBody(t, e, "/beads?status=in_progress")
	if body.Partial {
		t.Fatalf("GET /v0/beads?status=in_progress reported a partial read over healthy stores: %v", body.PartialErrors)
	}
	apiIDs := make([]string, 0, len(body.Items))
	for _, b := range body.Items {
		apiIDs = append(apiIDs, b.ID)
	}
	if !containsString(apiIDs, claimed.ID) {
		t.Errorf("GET /v0/beads?status=in_progress = %v, missing the claimed ephemeral %s-store wisp %s; the CLI and the API fan out over the same legs and must not disagree about which tiers those legs span", apiIDs, ownerName, claimed.ID)
	}
}

// legReadyIDsAcrossTiers is the ORACLE for I16: everything one leg holds as
// claimable work across both storage tiers, read from that store directly.
//
// The tier is spelled beads.TierBoth literally rather than through
// beads.FederatedReadTier, which is the constant under test. Taking the oracle
// from the same constant would make this row pass by construction the day
// somebody narrows it.
func legReadyIDsAcrossTiers(t *testing.T, store beads.Store) []string {
	t.Helper()
	rows, err := beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("reading the leg's own ready set across both tiers: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	return ids
}

// assertFederationServesWholeLeg asserts that every id a leg holds reached the
// merged answer, and reports the miss in the arithmetic shape the live report
// used: leg total, merged∩leg total, and the difference by id.
func assertFederationServesWholeLeg(t *testing.T, surface, legName string, legIDs, merged []string) {
	t.Helper()
	var missing []string
	served := 0
	for _, id := range legIDs {
		if containsString(merged, id) {
			served++
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return
	}
	t.Errorf("%s dropped %d of the %s store's %d claimable beads: the leg holds %d, the merged answer carries %d of them, %d - %d = %d missing %v. Nothing failed and nothing was rejected — the rows are simply not there, which is indistinguishable from work that does not exist",
		surface, len(missing), legName, len(legIDs), len(legIDs), served, len(legIDs), len(missing), served, missing)
}

// conformanceProjectionCoherence (I14) pins that the two `gc bd` PROJECTIONS
// over a molecule agree about what happens when the class cannot be seen.
//
// The bug is win-mc-forge's measurement row #2, and it survived the by-id lane
// because it does not look like a by-id read. On a converged split city:
//
//	gc bd dep tree <gcg root>                          → exit 1, refused
//	gc bd list --metadata-field gc.root_bead_id=<root> → 0 rows, exit 0
//
// Two projections, the same molecule, the same command, opposite failure
// semantics. `dep tree` names the bead in an id POSITION so the by-id door
// decides ownership and refuses; --metadata-field is not id-valued, so that door
// correctly declines (a QUOTED id decides nothing about ownership — see
// cmd_bd_by_id.go) and the passthrough asks the one ledger that holds no gcg-
// row, which answers `[]` and exits 0. The value named an id; the VERB is a
// projection. Invariant 0 of ga-iaj7k: a projection that cannot see a class must
// fail LOUDLY, and `[]` is forbidden.
//
// The asymmetry is what makes it urgent rather than merely wrong. An operator
// who has learned that this CLI refuses what it cannot see reads the empty array
// as a fact about the molecule.
//
// # What this asserts, and why on the argv predicates
//
// The two fates are decided before any store is touched, by two pure functions
// of (config, argv): bdSQLRelocatedClassRefusal for `list` and
// bdArgsNameClassOwnedBead for every other verb. So the coherence claim is
// checkable exactly where it is decided, and the row runs on both topologies
// without opening a binding. The end-to-end proof through the real command —
// real doBd, real refusals, a bd stub that answers `[]` and exits 0 — is
// TestGcBdProjectionsAgreeOnAClassTheyCannotSee.
//
// # The single-store row is the byte-identity claim
//
// It is not a formality and it is not hardcoded: the fixture mints work-prefixed
// ids on that topology, so the SAME two argvs carry no reserved prefix, name no
// relocated class, and both projections pass through to bd exactly as a legacy
// city always ran them. A guard that started refusing there would fail here
// first.
//
// # A refusal has to be actionable, and honest about how far it goes
//
// Loud is necessary and not sufficient: refusing a question nothing can answer
// just moves the dead end. So the last legs assert the way OUT that the refusal
// names — the federated `gc ready` reader — returns the molecule's members on
// BOTH topologies, from the store that owns them.
//
// They also assert its LIMIT, on a molecule that is not all open, because the
// modal case is a stuck one: a molecule with no claimable step is exactly the
// molecule an operator lists, and the escape's default spelling returns []
// there. The refusal text states that (one status per invocation, no --all, no
// deferred) and this row is what keeps the statement true — a fixture where
// every step is open would let the steering silently become wrong again.
//
// # The verbs move together
//
// `ready` is asserted alongside `list` because they take the SAME selector and
// answer no-match the same way. Guarding one and not the other reproduces the
// asymmetry this invariant exists to forbid, one verb over.
func conformanceProjectionCoherence(t *testing.T, e splitEnv) {
	root := mintDurableGraphBead(t, e, "projection coherence molecule root", "")
	step, err := e.graphStore().Create(beads.Bead{
		Title:    "graph step carrying the root id",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create molecule step: %v", err)
	}
	// A mid-flight sibling: the molecule is not all-open, which is the state a
	// stuck one is actually in when someone reaches for `bd list`. Stores mint
	// every bead open, so the status is a second write.
	inFlight, err := e.graphStore().Create(beads.Bead{
		Title:    "graph step already claimed",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create in-flight molecule step: %v", err)
	}
	inProgress := "in_progress"
	if err := e.graphStore().Update(inFlight.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("put the molecule step in flight: %v", err)
	}
	// A work-store bead under a DIFFERENT root: without it a projection that
	// returned everything would pass the last leg.
	decoy, err := e.work.Create(beads.Bead{
		Title:    "work bead under another root",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-some-other-root"},
	})
	if err != nil {
		t.Fatalf("create decoy work bead: %v", err)
	}

	selector := beadmeta.RootBeadIDMetadataKey + "=" + root.ID
	listArgs := []string{"list", "--metadata-field", selector, "--json"}
	readyArgs := []string{"ready", "--metadata-field", selector, "--json"}
	depTreeArgs := []string{"dep", "tree", root.ID}

	// `dep tree` must stay UNSERVED in process, or the arm being compared here is
	// not the refusal arm and the coherence claim is about something else.
	if _, served := parseBdByIDOp(depTreeArgs); served {
		t.Fatalf("`gc bd dep tree` is now served in process; I14 compares the REFUSAL arms, so re-point this row at the served answer")
	}

	msg, listRefused := bdSQLRelocatedClassRefusal(e.cfg, listArgs)
	_, readyRefused := bdSQLRelocatedClassRefusal(e.cfg, readyArgs)
	_, depTreeRefused := bdArgsNameClassOwnedBead(depTreeArgs)

	if listRefused != depTreeRefused {
		t.Fatalf("`gc bd list --metadata-field %s` refused = %v but `gc bd dep tree %s` refused = %v on the same molecule; two projections over the same data must not disagree about what happens when a class cannot be seen",
			selector, listRefused, root.ID, depTreeRefused)
	}
	if readyRefused != listRefused {
		t.Fatalf("`gc bd ready --metadata-field %s` refused = %v but `gc bd list` with the same selector refused = %v; the two verbs take the same predicate and answer no-match the same way, so guarding one moves the silent empty rather than removing it",
			selector, readyRefused, listRefused)
	}
	if listRefused != e.split {
		t.Fatalf("the projections over %s refused = %v on a split=%v city; a relocated class must refuse and a legacy city must pass through byte-identically", root.ID, listRefused, e.split)
	}
	if e.split {
		// Loud is not enough: the refusal has to say which class, where it is
		// served, what to run instead, and how far that gets.
		for _, want := range []string{
			"graph-class beads", `"gcg-"`, splitEnvBinding,
			"gc ready --metadata-field",
			"with no --status it returns only claimable work",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the list refusal does not name %q: %s", want, msg)
			}
		}
	}

	// The way out the refusal names, on both topologies.
	legs := readyFederationLegs(loadedCityName(e.cfg, e.cityPath), e.work, e.rigStores, fixtureGraphLeg(e))
	readyIDs := func(status string) []string {
		t.Helper()
		rows, err := readyBeadsForOpts(legs, readyOpts{status: status, metadataFields: []string{selector}})
		if err != nil {
			t.Fatalf("the federated reader the refusal steers to failed on --status %q: %v", status, err)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		return ids
	}

	open := readyIDs("open")
	if !containsString(open, step.ID) {
		t.Errorf("`gc ready --metadata-field %s --status open` = %v, missing the molecule step %s; the refusal steers operators here, so an empty answer here is the original bug one command over",
			selector, open, step.ID)
	}
	if containsString(open, decoy.ID) {
		t.Errorf("the federated reader returned %s, which carries a different root id; the metadata filter is not filtering", decoy.ID)
	}

	// The limit the refusal now states, asserted rather than assumed: the
	// spelling an operator copies verbatim cannot see a step that is already in
	// flight, and reaching it takes a second invocation naming its status.
	if bare := readyIDs(""); containsString(bare, inFlight.ID) {
		t.Errorf("`gc ready --metadata-field %s` (no --status) returned the in-flight step %s; the refusal text says that spelling returns only claimable work, so either the text or this expectation is now wrong",
			selector, inFlight.ID)
	}
	if byStatus := readyIDs("in_progress"); !containsString(byStatus, inFlight.ID) {
		t.Errorf("`gc ready --metadata-field %s --status in_progress` = %v, missing %s; the refusal tells operators to enumerate a mid-flight molecule one status at a time, and this is that leg",
			selector, byStatus, inFlight.ID)
	}
}

// fixtureGraphLeg resolves the fixture's graph leg through the SAME identity
// gate production uses: the routes answer whether the class is relocated, and a
// binding that resolved back to the work store is not a second store. Restating
// the rule here instead would let the fixture model a topology production does
// not serve.
func fixtureGraphLeg(e splitEnv) beads.Store {
	binding, relocated := graphClassBinding(e.routes)
	return relocatedGraphLegFrom(binding, relocated, e.work)
}

// conformanceWorkQueryFederation (I15) guards the fail-open the work query and
// the pool-demand count-form share: both are SHELL commands, and on a converged
// split city the `bd ready` they shell reads the work ledger, where the
// execution DAG no longer lives. A worker asks for its routed work, gets a
// valid-looking empty array, and idle-exits; the reconciler asks for its demand,
// gets zero, and drains the pool. Neither produces an error, which is why this
// is the capability whose absence is invisible.
//
// It asserts three things, in the order they have to hold:
//
//  1. The seam answers. cityQueryTopology is the production resolver, and it
//     rides on graphClassBinding — the same question resolveClassStore asks —
//     so a city that relocates nothing federates nothing.
//  2. The command changes, and only in the reader. The single-store row is the
//     byte-identity claim (its exact bytes are pinned by
//     internal/config's TestWorkQueryGolden); here it is the command WORD that
//     matters, because a split city emitting `bd ready` is the blindness.
//  3. The reader the command names actually answers with the routed graph work,
//     read through the production leg assembly rather than a restatement of it.
//
// GAP CLOSED in ga-601v2 — SEE AND CLAIM. This invariant makes graph-class work
// VISIBLE to the worker's query. What it used to pin, one paragraph down, was
// that the same command could not then CLAIM it: `gc hook --claim` ran its claim
// against a bd store rooted in the agent's work directory
// (hookClaimBdStoreContext over hookQueryEnv, the WORK scope I5 pins), which
// cannot reach the relocated class at all.
//
// The claim-time writes are now class-routed (claim_class_route.go). The work
// store is still the store the claim RUNS AGAINST first — that is the tie-break,
// and it has to be, because this query's own reader dedupes a co-resident id to
// the work row — and the binding is reached only where that work-scope write
// returns the not-found that PROVES the bead is not there
// (hookClaimBeadIsElsewhere, unwidened). So the two halves of the split-city
// worker now agree: the bead this query surfaces is one the same command claims.
//
// The rows below assert both halves, because both still have to hold:
//
//  1. The WORK store still cannot resolve a relocated bead, and still fails with
//     the not-found the claim path classifies as "it lives elsewhere". That is
//     the fixture's residence fact AND the escalation signal — if it stopped
//     being ErrNotFound, the routing would never fire and the claim would fail
//     closed instead.
//  2. The claim escalates past it and lands in the binding.
//  3. The fail-soft behavior SURVIVES. An id NEITHER store holds is still
//     skipped, not fatal: claimsErrored carries it to the shared drain, the
//     worker reports action=drain reason=claims_errored (distinguishable from a
//     healthy no_work), and the federated loop still reaches claimable work in
//     another store. That property is what kept ga-bvdha shippable alone, and
//     closing the gap must not spend it — a routed claim removes the common
//     cause of the skip, not the skip.
//
// The RELEASE tier follows the claim, and is asserted on I5 rather than assumed
// (assertClassRoutedClaimIsReleasable): the reconciler's retired-session scan
// leads with the sessions-class store, which on a converged split is the engine
// the graph class is served from, and `gc session close` — which leads with the
// work store — is handed the binding as a class leg. What stays topology-blind
// is the AGENT-side recovery (`bd list --status in_progress`, on_death,
// on_boot), which is raw bd in the agent's work directory: that is ga-zp3uj, and
// it re-serves rather than loses, because the reconciler scan releases.
func conformanceWorkQueryFederation(t *testing.T, e splitEnv) {
	agentCfg := e.cfg.Agents[0]
	topo := cityQueryTopology(e.cityPath, e.cfg)
	if topo.FederatedReady != e.split {
		t.Fatalf("cityQueryTopology reports FederatedReady=%v on a split=%v city; the generated query is built from this answer, so a wrong one is silent blindness or a needless federation", topo.FederatedReady, e.split)
	}
	workQuery := agentCfg.EffectiveWorkQueryFor(topo)
	poolDemand := agentCfg.EffectivePoolDemandQueryFor(topo)

	if !e.split {
		for name, cmd := range map[string]string{"work_query": workQuery, "scale_check": poolDemand} {
			if strings.Contains(cmd, "gc ready") {
				t.Errorf("a single-store city's %s reads through the federated reader: %q — its command must be the one it already runs", name, cmd)
			}
			if !strings.Contains(cmd, "bd ready") {
				t.Errorf("a single-store city's %s no longer shells `bd ready`: %q", name, cmd)
			}
		}
		// Nothing is relocated, so no override can be blind to it.
		blind := (&config.Agent{Name: "custom", WorkQuery: "bd ready --json"}).FederationBlindOverrides(topo)
		if len(blind) != 0 {
			t.Errorf("a single-store city reports work_query overrides as federation-blind (%v); there is no second store for them to miss", blind)
		}
		return
	}

	for name, cmd := range map[string]string{"work_query": workQuery, "scale_check": poolDemand} {
		if strings.Contains(cmd, "bd ready") {
			t.Fatalf("a split city's %s still shells `bd ready`, which reads the work ledger the graph class was migrated OFF: %q", name, cmd)
		}
		if !strings.Contains(cmd, "gc ready") {
			t.Fatalf("a split city's %s does not name the federated reader: %q", name, cmd)
		}
		// The consumer half of the fail-loud rule. `gc ready` exits non-zero on a
		// dead leg (I13); a tier that captured that exit into an empty result and
		// fell through would print `[]` and exit 0, which is the same fail-open one
		// layer up. internal/config's TestFederatedWorkQueryPropagatesADeadLeg
		// EXECUTES this; the clause is pinned here so the split-topology row cannot
		// go green on a command that swallows it.
		if !strings.Contains(cmd, `|| exit $?`) {
			t.Errorf("a split city's %s runs the federated reader without propagating its exit status: %q", name, cmd)
		}
	}

	// The routed tier's own predicate, restated as the reader's flags. The
	// assertions above pin that the command carries them, so the two cannot
	// drift apart silently.
	for _, flag := range []string{
		`--metadata-field "` + beadmeta.RoutedToMetadataKey + `=$target"`,
		"--unassigned",
		"--exclude-type=epic",
	} {
		if !strings.Contains(workQuery, flag) {
			t.Fatalf("the split work_query's routed tier lost %q, so the reader flags asserted below are not the ones it runs: %q", flag, workQuery)
		}
	}

	routed := e.mintWispWith(t, wispOpts{title: "routed graph step", routedTo: e.qualified})
	legs := readyFederationLegs(loadedCityName(e.cfg, e.cityPath), e.work, e.rigStores, fixtureGraphLeg(e))
	rows, err := readyBeadsForOpts(legs, readyOpts{
		unassigned:     true,
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=" + e.qualified},
		excludeTypes:   []string{"epic"},
		sortOrder:      readySortOldest,
		limit:          20,
	})
	if err != nil {
		t.Fatalf("the reader the split work_query names failed over healthy stores: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == routed.ID {
			found = true
		}
	}
	if !found {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		t.Fatalf("the reader the split work_query names answered %v and not the routed graph step %s; the command was swapped but the answer is still work-ledger-only, which is the blindness with extra steps", ids, routed.ID)
	}

	conformanceWorkQueryClaimsWhatItSees(t, e)
}

// conformanceWorkQueryClaimsWhatItSees asserts the closed gap: the bead the
// federated work query serves is one `gc hook --claim` claims, even though the
// store it issues its claim against first cannot resolve it.
//
// The store the claim runs against FIRST is the fixture's WORK leg, because that
// is what hookQueryEnv's scope resolves to on both topologies (I5 pins that env
// directly). What changed is what happens next, and it is asserted, not
// described: the work store's not-found escalates the write to the binding.
//
// It covers BOTH federated tiers, because they are different claim paths and the
// reviewer's tautology on the previous version was that only the routed one was
// ever exercised. The routed tier reaches claimFirstEligibleHookCandidate; the
// assigned tier — the one a graph step assigned to this worker arrives on —
// reaches claimFirstReadyHookAssignment, whose unresolvable-id branch is
// separate code.
//
// BOTH halves — the reader assertion and the claim — run against the same class
// leg, so the chain stays unbroken: the leg that serves the bead is the leg the
// claim writes to.
func conformanceWorkQueryClaimsWhatItSees(t *testing.T, e splitEnv) {
	t.Helper()

	routed := e.mintWispWith(t, wispOpts{title: "routed graph step", routedTo: e.qualified})
	assertFederatedReaderServes(t, e, routed.ID, "routed", readyOpts{
		unassigned:     true,
		metadataFields: []string{beadmeta.RoutedToMetadataKey + "=" + e.qualified},
		excludeTypes:   []string{"epic"},
		sortOrder:      readySortOldest,
		limit:          20,
	})
	assertClaimEscalatesOffTheWorkStore(t, e, routed.ID, "routed")

	// The ASSIGNED shape, minted the way graphroute produces it: an OPEN graph
	// step carrying this worker's identity as its assignee
	// (graphroute.ApplyGraphRouteBinding sets step.Assignee for a non-pool,
	// non-direct binding, and molecule materialization writes it through).
	assigned := e.mintWispWith(t, wispOpts{title: "assigned graph step", assignee: e.qualified})
	assertFederatedReaderServes(t, e, assigned.ID, "assigned", readyOpts{
		assignee:  e.qualified,
		sortOrder: readySortOldest,
		limit:     20,
	})
	assertClaimEscalatesOffTheWorkStore(t, e, assigned.ID, "assigned")

	conformanceAssignedTierClaimsTheGraphStep(t, e, assigned.ID)
	conformanceUnresolvableCandidateStillDoesNotStrandWork(t, e)
}

// assertFederatedReaderServes asserts the reader the split work_query NAMES
// answers with graphBeadID, read through the production leg assembly rather than
// a restatement of it. It is the "sees" half of every see-and-claim row: a claim
// assertion on a bead the query never served would prove nothing about the
// command.
func assertFederatedReaderServes(t *testing.T, e splitEnv, graphBeadID, tier string, opts readyOpts) {
	t.Helper()
	legs := readyFederationLegs(loadedCityName(e.cfg, e.cityPath), e.work, e.rigStores, fixtureGraphLeg(e))
	rows, err := readyBeadsForOpts(legs, opts)
	if err != nil {
		t.Fatalf("the reader the split work_query's %s tier names failed over healthy stores: %v", tier, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
		if row.ID == graphBeadID {
			return
		}
	}
	t.Fatalf("the %s tier's reader answered %v and not the graph step %s; a see-and-claim row whose reader never sees the bead pins nothing", tier, ids, graphBeadID)
}

// assertClaimEscalatesOffTheWorkStore pins one federated tier's bead end to end:
// the store `gc hook --claim` roots its BdStore in cannot resolve it, it fails
// with the not-found the claim path classifies as "the bead lives in another
// store" (hookClaimBeadIsElsewhere), and the class-routed ops therefore escalate
// the write to the binding and take the bead.
//
// All three halves matter and the middle one is load-bearing in the new
// direction: ErrNotFound is the ONLY error that opens the escalation, so if the
// work store ever started failing differently the claim would fail CLOSED rather
// than route, and the first two rows are what would say so.
func assertClaimEscalatesOffTheWorkStore(t *testing.T, e splitEnv, graphBeadID, tier string) {
	t.Helper()
	_, err := e.work.Get(graphBeadID)
	if err == nil {
		t.Fatalf("the WORK store resolved the relocated %s-tier graph bead %s; the fixture stopped relocating the graph class, which would make this whole invariant vacuous", tier, graphBeadID)
	}
	if !hookClaimBeadIsElsewhere(err) {
		t.Fatalf("the WORK store failed on the relocated %s-tier graph bead %s with %v, which the claim path does not classify as \"this store does not hold it\"; the class route would never fire and the claim would fail closed instead", tier, graphBeadID, err)
	}

	route, err := newHookClaimClassRoute(e.class)
	if err != nil {
		t.Fatalf("opening the claim-time class front door: %v", err)
	}
	ops := classRoutedHookClaimOps(hookClaimOps{Claim: splitEnvStoreClaim(e.work)}, route)
	// The claim actor is the worker's own identity, which is what the assigned
	// tier requires: claimFirstReadyHookAssignment passes the bead's CURRENT
	// assignee as the actor, because the store's idempotent claim treats a
	// different actor on an already-assigned bead as a conflict, not a claim.
	assignee := e.qualified
	claimed, ok, claimErr := ops.Claim(context.Background(), e.cityPath, nil, graphBeadID, assignee)
	if claimErr != nil || !ok {
		t.Fatalf("`gc hook --claim` claim of the relocated %s-tier graph bead %s = (ok=%v err=%v), want a successful claim escalated to the binding; the query serves this bead, so a command that cannot claim it re-serves and re-skips it every tick", tier, graphBeadID, ok, claimErr)
	}
	if strings.TrimSpace(claimed.Assignee) != assignee {
		t.Fatalf("the routed claim of %s returned assignee %q, want %q", graphBeadID, claimed.Assignee, assignee)
	}
	if held, err := e.class.Get(graphBeadID); err != nil || strings.TrimSpace(held.Assignee) != assignee {
		t.Fatalf("the binding holds %s with assignee %q (err=%v), want %q — the claim reported success without landing in the store that owns the bead", graphBeadID, held.Assignee, err, assignee)
	}
}

// conformanceAssignedTierClaimsTheGraphStep drives the whole federated claim
// command over the shape ga-601v2 exists for: a relocated graph step ASSIGNED to
// this worker, ranked ahead of everything else by bestStoreWithWork, arriving on
// the tier claimFirstReadyHookAssignment serves.
//
// Before the routing this produced the treadmill — the step was re-served and
// re-skipped every tick while the worker claimed something else — so the
// assertion is that the command now returns THE GRAPH STEP, not the consolation
// work-ledger bead beside it.
//
// The base claim is bound to the fixture's real WORK store, so the not-found the
// escalation keys on is the store's own answer rather than a restatement of it.
func conformanceAssignedTierClaimsTheGraphStep(t *testing.T, e splitEnv, assignedGraphBeadID string) {
	t.Helper()
	route, err := newHookClaimClassRoute(e.class)
	if err != nil {
		t.Fatalf("opening the claim-time class front door: %v", err)
	}
	claimable, err := e.work.Create(beads.Bead{
		Title:    "claimable work-ledger bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.qualified},
	})
	if err != nil {
		t.Fatalf("minting claimable work-ledger bead: %v", err)
	}
	stores := []hookStore{
		{dir: e.cityPath, env: []string{"GC_HOOK_LEG=assigned"}},
		{dir: e.cityPath, env: []string{"GC_HOOK_LEG=routed"}},
	}
	run := func(_, _ string, env []string) (string, error) {
		if len(env) > 0 && env[0] == "GC_HOOK_LEG=assigned" {
			return `[{"id":"` + assignedGraphBeadID + `","status":"open","assignee":"` + e.qualified + `"}]`, nil
		}
		return `[{"id":"` + claimable.ID + `","status":"open","metadata":{"` + beadmeta.RoutedToMetadataKey + `":"` + e.qualified + `"}}]`, nil
	}
	ops := classRoutedHookClaimOps(hookClaimOps{
		Claim:             splitEnvStoreClaim(e.work),
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}, route)
	opts := hookClaimOptions{
		Assignee:           e.qualified,
		IdentityCandidates: []string{e.qualified},
		RouteTargets:       []string{e.qualified},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", e.cityPath, stores[0].env, stores, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc hook --claim exited %d with the relocated assigned step %s ranked first (stdout=%q stderr=%q)", code, assignedGraphBeadID, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("gc hook --claim stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.BeadID != assignedGraphBeadID {
		t.Fatalf("gc hook --claim result = %+v, want the relocated assigned graph step %s — the worker claimed something else, which is the re-serve/re-skip treadmill ga-601v2 closes", result, assignedGraphBeadID)
	}
	if held, err := e.class.Get(assignedGraphBeadID); err != nil || !strings.EqualFold(strings.TrimSpace(held.Status), "in_progress") {
		t.Fatalf("the binding holds %s with status %q (err=%v), want in_progress — the command reported a claim it did not write", assignedGraphBeadID, held.Status, err)
	}
}

// conformanceUnresolvableCandidateStillDoesNotStrandWork keeps the property that
// made ga-bvdha shippable on its own: a candidate NO store can resolve costs the
// worker that ONE bead, not its whole tick.
//
// Class routing removes the common cause of that skip, not the skip — a routed
// id whose bead was deleted, a stale row in a work query's captured output, an
// id in neither ledger — and the assigned tier is where it matters, because
// bestStoreWithWork ranks an assigned candidate ahead of a routed one and
// therefore selects the store holding the unclaimable one FIRST. If that tier
// answered with a terminal exit, the federated drop-and-retry loop would never
// run and the claimable bead below would go unclaimed every tick.
//
// The claim is driven through the fixture's real WORK store with the class route
// installed, so the not-found is a store's own answer at BOTH legs.
func conformanceUnresolvableCandidateStillDoesNotStrandWork(t *testing.T, e splitEnv) {
	t.Helper()
	classPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("config.ReservedClassPrefix(graph) = ok:false")
	}
	// A reserved-prefix id neither store holds: the class binding is the only
	// place it could live, and it does not live there either.
	assignedGraphBeadID := classPrefix + "-deleted-under-us"
	if _, err := e.class.Get(assignedGraphBeadID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the binding resolved %s (err=%v); this row needs an id NO store holds", assignedGraphBeadID, err)
	}
	route, err := newHookClaimClassRoute(e.class)
	if err != nil {
		t.Fatalf("opening the claim-time class front door: %v", err)
	}
	claimable, err := e.work.Create(beads.Bead{
		Title:    "claimable work-ledger bead behind an unresolvable candidate",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.qualified},
	})
	if err != nil {
		t.Fatalf("minting claimable work-ledger bead: %v", err)
	}
	stores := []hookStore{
		{dir: e.cityPath, env: []string{"GC_HOOK_LEG=assigned"}},
		{dir: e.cityPath, env: []string{"GC_HOOK_LEG=routed"}},
	}
	run := func(_, _ string, env []string) (string, error) {
		if len(env) > 0 && env[0] == "GC_HOOK_LEG=assigned" {
			return `[{"id":"` + assignedGraphBeadID + `","status":"open","assignee":"` + e.qualified + `"}]`, nil
		}
		return `[{"id":"` + claimable.ID + `","status":"open","metadata":{"` + beadmeta.RoutedToMetadataKey + `":"` + e.qualified + `"}}]`, nil
	}
	ops := classRoutedHookClaimOps(hookClaimOps{
		Claim:             splitEnvStoreClaim(e.work),
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}, route)
	opts := hookClaimOptions{
		Assignee:           e.qualified,
		IdentityCandidates: []string{e.qualified},
		RouteTargets:       []string{e.qualified},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("gc ready --json", e.cityPath, stores[0].env, stores, opts, ops, run, func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc hook --claim exited %d with an unresolvable assigned candidate ranked first; one candidate no store holds must not strand the worker's other claimable work (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("gc hook --claim stdout is not JSON: %v\nraw: %q", err, stdout.String())
	}
	if result.BeadID != claimable.ID {
		t.Fatalf("gc hook --claim result = %+v, want the claimable work-ledger bead %s reached past the unresolvable assigned candidate", result, claimable.ID)
	}
}

// splitEnvStoreClaim expresses hookClaimWithBdStore's contract over a
// beads.Store: resolve the bead in the store the claim is rooted in, take it,
// and report the STORE's own error when it cannot — which is what makes the
// relocated-id case a real not-found rather than a fixture assertion about one.
func splitEnvStoreClaim(store beads.Store) hookClaimFunc {
	return func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		if _, err := store.Get(beadID); err != nil {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, err)
		}
		inProgress := "in_progress"
		if err := store.Update(beadID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: %w", beadID, err)
		}
		claimed, err := store.Get(beadID)
		if err != nil {
			return beads.Bead{}, true, fmt.Errorf("reloading claimed bead %q: %w", beadID, err)
		}
		return claimed, true, nil
	}
}
