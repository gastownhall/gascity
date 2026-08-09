package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
// verified remaining work.

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
func conformanceAssignedWorkCapture(t *testing.T, e splitEnv) {
	sess, err := e.sessionsStore().Create(splitEnvPoolSessionBead(e.qualified, "executor-1"))
	if err != nil {
		t.Fatalf("create live pool session bead: %v", err)
	}
	live := e.mintWispWith(t, wispOpts{title: "live-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: sess.ID})
	dead := e.mintWispWith(t, wispOpts{title: "dead-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: "s-dead99"})

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

	released := releaseOrphanedPoolAssignments(
		e.sessionsStore(), e.cfg, e.cityPath,
		sessionInfosFromBeads([]beads.Bead{sess}),
		got, stores, refs,
		e.rigStores,
	)
	if len(released) != 1 || released[0].ID != dead.ID {
		t.Errorf("released = %v, want exactly the dead-assignee wisp %s (the live holder's claim must survive; the dead claim must recover)", released, dead.ID)
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
// the store's own declared namespace — the prefix+"-" segment rule
// internal/api's beadIDHasConfiguredPrefix applies too. This invariant pins that
// both shapes route to the class store on a split city, and pins the TRAP that
// makes the wisp shape special: the config-free sling.BeadPrefix heuristic
// answers "gcg-wisp" for a gcg-wisp-0042 id, which is not a reserved class
// prefix at all, so a by-id router built on that heuristic instead of the
// namespace rule would hand every wisp to the work store. That divergence is
// asserted as a negative here so it cannot change silently under a future
// resolver.
//
// The CLAIM-MUTATION half is SKIPPED — the seam does not exist on main.
// `gc hook --claim` resolves its stores as hookStore{dir, env} pairs
// (hook_cross_store.go) and execs bd in a work directory; the fan-out is city +
// rigs, all of them WORK scopes, with no coordination-class arm anywhere on the
// path. The only by-id class resolver in the tree is Server.beadStoresForID
// (internal/api/handler_beads.go), method-bound to the API server and graph-only.
// Lifting it into a shared resolver is the next slice in this program (ga-ia7li,
// which this work blocks).
func conformanceClaimRouting(t *testing.T, e splitEnv) {
	workBead, err := e.work.Create(beads.Bead{Title: "claim-routing work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if reservedClassNamespace(workBead.ID) {
		t.Fatalf("work bead %q sits in a reserved class namespace; by-id routing cannot be built on this id space", workBead.ID)
	}
	if !e.split {
		wisp := e.mintWisp(t, "claim-routing wisp")
		if reservedClassNamespace(wisp.ID) {
			t.Fatalf("single-store wisp %q sits in a reserved class namespace; a legacy city mints work-store ids", wisp.ID)
		}
		if owner := storeref.PrefixOwner(wisp.ID, []beads.Store{e.work}); owner != nil {
			t.Errorf("storeref.PrefixOwner routed single-store wisp %q to a namespace owner; a legacy city has one store and no namespace to route on", wisp.ID)
		}
		t.Skip("single-store collapse: there is no second store to route a claim to, and no coordination-class claim arm to route it with (ga-ia7li)")
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
		if !wantClass && owner != nil {
			t.Errorf("%s: storeref.PrefixOwner(%q) claimed an owner; a work id is outside every declared class namespace and must fall through", tt.name, tt.id)
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

	t.Skip("gc hook --claim has no coordination-class arm: hookStore is a (dir, env) bd scope pair over city+rigs, all work scopes. The shared by-id class resolver is ga-ia7li; until it lands there is no cmd/gc seam that routes a claim MUTATION by class.")
}

// conformanceStrictCrossStoreDeps (I6) guards the cross-store dependency class:
// a blocking edge whose endpoints live in different stores. bd cannot express it
// and SQLite will not refuse it, so the two backends answer differently and the
// invariant is stated per backend rather than as one rule:
//
//   - through the WORK front door (bd/Dolt): a hard failure, in bd's own
//     wording, because bd resolves both endpoints before writing the row.
//   - through the CLASS front door (SQLite): ACCEPTED and recorded as a
//     residence violation, because the deps table has no foreign key and DepAdd
//     is a plain INSERT — production keeps the dangling edge and silently drops
//     the dependent out of Ready instead of erroring.
//
// The single-store subtest is the byte-identity half: one store resolves both
// endpoints and every one of these calls succeeds.
func conformanceStrictCrossStoreDeps(t *testing.T, e splitEnv) {
	workBead, err := e.work.Create(beads.Bead{Title: "cross-dep work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	durable := mintDurableGraphBead(t, e, "cross-dep durable graph bead", "")
	wisp := e.mintWisp(t, "cross-dep wisp")

	for _, tt := range []struct {
		name     string
		front    beads.Store
		from, to string
	}{
		{"work front door: work blocks-on wisp", e.work, workBead.ID, wisp.ID},
		{"work front door: work blocks-on durable", e.work, workBead.ID, durable.ID},
		{"graph front door: wisp blocks-on work", e.graphStore(), wisp.ID, workBead.ID},
		{"graph front door: durable blocks-on work", e.graphStore(), durable.ID, workBead.ID},
	} {
		err := tt.front.DepAdd(tt.from, tt.to, "blocks")
		if !e.split {
			if err != nil {
				t.Errorf("%s: single-store DepAdd(%s → %s) = %v, want success (one store resolves both endpoints)", tt.name, tt.from, tt.to, err)
			}
			continue
		}
		if sameStorePtr(tt.front, e.work) {
			if err == nil || !strings.Contains(err.Error(), "belongs to another store's id namespace") {
				t.Errorf("%s: DepAdd(%s → %s) = %v, want the bd/Dolt work store to reject a cross-store edge — a fail-open edge here is a parent that goes READY mid-DAG", tt.name, tt.from, tt.to, err)
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

// conformanceWakeOwnershipFastPath (I10) pins the reachability model the wake
// filter and the orphan-release ownership index share: it is resolved from CFG —
// the holder's configured rig — and never from which physical store the bead came
// out of. The consequence is the conformance property: the answer is IDENTICAL on
// both topologies. A rig-bound pool holder owns the rig-store leg and does not own
// the leading-store leg, on a legacy city and on a split city alike.
//
// KNOWN GAP, pinned rather than asserted as desirable: on a split city the
// leading store IS the class store, so a rig-bound holder's CLAIMED
// class-resident bead has no wake reason and is not ownership-matched — the
// reachability model has no coordination-class arm. Orphan release still spares
// it through the last-resort live-session probe (I2 proves that), but the wake
// filter drops it every tick. Closing that gap is a later slice; when it lands,
// the two assertions below are the ones to update, and they must be updated
// TOGETHER or the topologies stop agreeing.
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

	if len(kept) != 1 || kept[0].ID != rigWork.ID || len(keptRefs) != 1 || keptRefs[0] != e.rigName {
		t.Fatalf("wake filter kept %d beads (refs %v), want exactly the rig-store claim %s under ref %q", len(kept), keptRefs, rigWork.ID, e.rigName)
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
	if wrapped != e.policyWrapped(front) {
		t.Fatalf("policy-wrap detection disagrees with the fixture: unwrap=%v policyWrapped=%v", wrapped, e.policyWrapped(front))
	}
	if !wrapped {
		// Relocated class store: no policy layer, so no tier expansion and no
		// ephemeral tier to be blind to. Everything the store holds is on the main
		// tier and every read path sees it. Pinned as the negative, so a change
		// that starts wrapping the class store — which would change wisp storage
		// on a split city — fails here first.
		list, err := front.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			t.Fatalf("class-store default list: %v", err)
		}
		if !beadListHasID(list, durable.ID) || !beadListHasID(list, wisp.ID) {
			t.Errorf("relocated class store default List sees durable=%v wisp=%v, want both (no policy layer means no tier to hide behind)", beadListHasID(list, durable.ID), beadListHasID(list, wisp.ID))
		}
		if wisp.Ephemeral {
			t.Error("a create through the relocated class store landed on the ephemeral tier; that store carries no bead-policy layer, so this test's model of production is wrong")
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

// conformanceGraphRecipe is the durable graph.v2 workflow shape: a root plus one
// finalize step, wired parent-child and blocks the way a compiled v2 formula
// wires them.
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
			{StepID: "conformance-graph.workflow-finalize", DependsOnID: "conformance-graph", Type: "parent-child"},
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
