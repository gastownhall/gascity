package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// mc-3ixn3.16, the close cascade: it releases a closing seat's work on the
// legs and under the identities its close gates read (split_store_work_legs_
// test.go pins the gates), on both storage shapes.

// J29 A: a dead-runtime corpse closes outside a reconcile tick, so its cascade
// reads the legs itself: the city work store and the serving rigs are
// released; a suspended rig is never read; a claim under the seat's alias
// history (outside the gates' narrow set) is left to the orphan backstop.
func TestSplitStoreCascade_DeadRuntimeReleasesTheGatesLegs(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "kill-town"},
				Rigs: []config.Rig{
					{Name: "riga", Path: filepath.Join(cityPath, "riga")},
					{Name: "rigb", Path: filepath.Join(cityPath, "rigb"), SuspendedOnStart: true},
				},
				Agents: []config.Agent{{Name: "worker"}},
			}
			sessions, seat, snapshot := newDeadRuntimeCorpseRow(t, map[string]string{
				"session_name": "kill-town--worker-1", "template": "worker", "state": string(session.StateActive),
				"alias_history": "kill-town/old-alias",
			})
			work := registerWorkShape(t, cityPath, shape, sessions)
			rig := beads.NewMemStore()
			suspended := &killRaceStore{Store: beads.NewMemStore()}
			cityClaim := createWork(t, work, "in_progress", seat.ID, nil)
			rigClaim := createWork(t, rig, "open", "kill-town--worker-1", nil)
			dormant := createWork(t, suspended.Store, "open", seat.ID, nil)
			history := createWork(t, work, "open", "kill-town/old-alias", nil)
			suspended.afterList = func(beads.ListQuery, []beads.Bead) { t.Error("the cascade read a suspended rig") }
			sp := newDeadRuntimeArtifactProvider()
			sp.visible["kill-town--worker-1"] = true
			sp.dead["kill-town--worker-1"] = true

			var stderr bytes.Buffer
			rigs := map[string]beads.Store{"riga": rig, "rigb": suspended}
			if got := cleanupDeadRuntimeSessionCorpses(cityPath, sessions, testSeatWork(cityPath, cfg, sessions, rigs), snapshot, nil, sp, nil, nil, &stderr); got != 1 {
				t.Fatalf("cleanupDeadRuntimeSessionCorpses() = %d, want 1; stderr=%q", got, stderr.String())
			}

			if got, _ := sessions.Get(seat.ID); got.Status != "closed" {
				t.Fatalf("corpse row status = %q, want closed", got.Status)
			}
			assertWork(t, work, cityClaim.ID, "open", "")
			assertWork(t, rig, rigClaim.ID, "open", "")
			assertWork(t, suspended.Store, dormant.ID, "open", seat.ID)
			assertWork(t, work, history.ID, "open", "kill-town/old-alias")
		})
	}
}

// The drain-finalize close excludes the seat's own drain step from its gate,
// and its cascade then releases that step on whichever leg it lives: the city
// work store, on a split city.
func TestSplitStoreCascade_DrainFinalizeReleasesTheOwnDrainStep(t *testing.T) {
	for _, shape := range workShapes {
		for _, path := range seatWorkPaths {
			t.Run(shape+"/"+path, func(t *testing.T) {
				cityPath := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
				sessions := beads.NewMemStore()
				work := registerWorkShape(t, cityPath, shape, sessions)
				seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow, 1)
				root, err := work.Create(beads.Bead{Title: "root", Type: "task", Ref: "mol-do-work"})
				if err != nil {
					t.Fatal(err)
				}
				step := createWork(t, work, "in_progress", seat.ID, map[string]string{beadmeta.StepRefMetadataKey: "mol-do-work.drain", beadmeta.RootBeadIDMetadataKey: root.ID})
				info := sessionInfosFromBeads([]beads.Bead{seat})[0]
				if !closeSessionBeadIfReachableStoreUnassigned(sessions, seatWorkOn(path, cityPath, cfg, sessions, nil, info), info, "drained", seatWorkNow, io.Discard, true) {
					t.Fatal("the drain-finalize close refused over the seat's own drain step")
				}
				assertWork(t, work, step.ID, "open", "")
			})
		}
	}
}

// The pool-slot close's cascade runs inside the tick and reads the tick's
// index: on either shape, every store is listed once per status by the index
// and never again by the cascade, and the work store is never listed per
// identity.
func TestSplitStoreCascade_PoolSlotCloseReadsOnlyTheTickIndex(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			work, registered := e.store, beads.Store(e.store)
			if shape == "split" {
				work = &killRaceStore{Store: beads.NewMemStore()}
				registered = cachedWorkStore(t, work) // a controller's cached work store
			}
			registerWorkShapeWith(t, e.city, shape, e.store, registered)
			routed := createWork(t, registered, "open", e.seat.ID, routedClaim())

			// The index reads its legs concurrently.
			var mu sync.Mutex
			legLists := map[*killRaceStore]int{}
			var perIdentity []beads.ListQuery
			watch := func(s *killRaceStore) func(beads.ListQuery, []beads.Bead) {
				return func(q beads.ListQuery, _ []beads.Bead) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case q.Assignee != "" || len(q.Assignees) > 0:
						// The close-time re-read lists the local binding per
						// identity, and a controller's cached work store never.
						if s == work && shape == "split" {
							perIdentity = append(perIdentity, q)
						}
					case q.Live && q.TierMode == beads.TierBoth && q.Type == "" && q.Label == "" && len(q.IDs) == 0:
						legLists[s]++
					}
				}
			}
			tickAtCityWatching(t, e, claims(routed), func(reconcile func()) {
				e.store.afterList, work.afterList = watch(e.store), watch(work)
				defer func() { e.store.afterList, work.afterList = nil, nil }()
				reconcile()
			})

			assertWork(t, work, routed.ID, "open", "")
			assertClosedAsKilled(t, e.reload(t, e.seat.ID))
			if len(perIdentity) != 0 {
				t.Fatalf("the tick listed work per identity %d times (first %+v)", len(perIdentity), perIdentity[0])
			}
			for _, n := range legLists {
				if n != len(seatWorkStatuses) {
					t.Fatalf("a leg was listed %d times in the tick, want %d (the index's lists only)", n, len(seatWorkStatuses))
				}
			}
		})
	}
}

// Inside a tick the stranded repair releases the index's rows: on a split city
// the repair tick lists the work store only through the index, never per
// identity.
func TestSplitStoreCascade_StrandedRepairReleasesFromTheIndex(t *testing.T) {
	env, seat, _, _ := strandedRepairReconcileEnv(t)
	cityPath := t.TempDir()
	work := &killRaceStore{Store: beads.NewMemStore()}
	registerWorkShapeWith(t, cityPath, "split", env.store, cachedWorkStore(t, work)) // a controller's cached work store
	claim := createWork(t, work, "in_progress", seat.ID, nil)
	var mu sync.Mutex
	var perIdentity int
	work.afterList = func(q beads.ListQuery, _ []beads.Bead) {
		if q.Assignee != "" || len(q.Assignees) > 0 {
			mu.Lock()
			perIdentity++
			mu.Unlock()
		}
	}
	tick := func() {
		current, err := env.store.Get(seat.ID)
		if err != nil {
			t.Fatal(err)
		}
		reconcileSessionBeadsAtPath(context.Background(), cityPath, []beads.Bead{current}, env.desiredState,
			map[string]bool{"worker": true}, env.cfg, env.sp, env.store, newFakeDrainOps(), nil, nil, nil, env.dt,
			nil, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)
	}

	tick()
	env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	tick()

	assertWork(t, work, claim.ID, "open", "")
	if perIdentity != 0 {
		t.Fatalf("the stranded ticks listed the work store per identity %d times; want only the index's lists", perIdentity)
	}
}

// The live-claim veto reads the stamped bead through the ByID plan, binding
// first: a binding-resident claim, held or closed, costs no read of the work
// store (a 5.6s bd show on maintainer-city, every tick, for every idle seat
// whose stamp names a closed graph bead).
func TestSplitStoreCascade_LiveClaimVetoReadsABindingClaimWithoutTheWorkStore(t *testing.T) {
	for _, closed := range []bool{false, true} {
		cityPath := t.TempDir()
		cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
		sessions := beads.NewMemStore()
		work := &workCallCounter{Store: beads.NewMemStore()}
		registerWorkShapeWith(t, cityPath, "split", sessions, work)
		seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow, 1)
		claim := createWork(t, sessions, "in_progress", seat.ID, nil)
		if closed {
			closeWork(t, sessions, claim)
		}
		if err := sessions.SetMetadata(seat.ID, beadmeta.CurrentClaimBeadIDMetadataKey, claim.ID); err != nil {
			t.Fatal(err)
		}

		owns, _, err := sessionOwnsLiveClaim(cityPath, cfg, sessions, nil, sessionInfosFromBeads([]beads.Bead{seat})[0])
		if err != nil || owns == closed {
			t.Fatalf("closed=%v: sessionOwnsLiveClaim = %v, %v", closed, owns, err)
		}
		if got := work.n(); got != 0 {
			t.Fatalf("closed=%v: the veto read the work store %d times, want 0", closed, got)
		}
	}
}
