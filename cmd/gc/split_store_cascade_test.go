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
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
			if got := cleanupDeadRuntimeSessionCorpses(cityPath, sessions, rigs, cfg, snapshot, nil, sp, nil, &stderr); got != 1 {
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
				useSeatWorkPath(t, path, cityPath, cfg, sessions, nil)

				if !closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, sessions, nil, sessionInfosFromBeads([]beads.Bead{seat})[0], "drained", seatWorkNow, io.Discard, true) {
					t.Fatal("the drain-finalize close refused over the seat's own drain step")
				}
				assertWork(t, work, step.ID, "open", "")
			})
		}
	}
}

// A close inside a tick runs its cascade off the tick's index: on either
// shape, every store is listed once per status by the index and never again by
// the cascade, and the work store is never listed per identity. hotfix/1.5.1: main
// drives this through B1's killed-seat pool-slot close (#7362, not on hotfix);
// here the close is the drain-finalize close with the tick's index installed,
// whose cascade releases the seat's own drain step. hotfix's ReleaseWorkBead
// fences an unrouted release with a live re-read that is itself one status
// list of the step's leg (liveWorkAssignmentAssigneeMatches), so that one list
// is expected on top of the index's.
func TestSplitStoreCascade_CloseInATickReadsOnlyTheTickIndex(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
			sessions := &killRaceStore{Store: beads.NewMemStore()}
			work := sessions
			if shape == "split" {
				work = &killRaceStore{Store: beads.NewMemStore()}
			}
			registerWorkShapeWith(t, cityPath, shape, sessions, work)
			seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow, 1)
			root, err := work.Create(beads.Bead{Title: "root", Type: "task", Ref: "mol-do-work"})
			if err != nil {
				t.Fatal(err)
			}
			step := createWork(t, work, "in_progress", seat.ID, map[string]string{beadmeta.StepRefMetadataKey: "mol-do-work.drain", beadmeta.RootBeadIDMetadataKey: root.ID})

			// The index reads its legs concurrently.
			var mu sync.Mutex
			type legStatus struct {
				store  *killRaceStore
				status string
			}
			legLists := map[legStatus]int{}
			var perIdentity []beads.ListQuery
			watch := func(s *killRaceStore) func(beads.ListQuery, []beads.Bead) {
				return func(q beads.ListQuery, _ []beads.Bead) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case q.Assignee != "" || len(q.Assignees) > 0:
						// The close-time re-read lists the local binding per
						// identity; the work store never is.
						if s == work {
							perIdentity = append(perIdentity, q)
						}
					case q.Live && q.TierMode == beads.TierBoth && q.Type == "" && q.Label == "" && len(q.IDs) == 0:
						legLists[legStatus{s, q.Status}]++
					}
				}
			}
			sessions.afterList, work.afterList = watch(sessions), watch(work)
			useSeatWorkPath(t, "index", cityPath, cfg, sessions, nil)

			if !closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, sessions, nil, sessionInfosFromBeads([]beads.Bead{seat})[0], "drained", seatWorkNow, io.Discard, true) {
				t.Fatal("the drain-finalize close refused over the seat's own drain step")
			}
			sessions.afterList, work.afterList = nil, nil

			assertWork(t, work, step.ID, "open", "")
			if len(perIdentity) != 0 {
				t.Fatalf("the close listed work per identity %d times (first %+v)", len(perIdentity), perIdentity[0])
			}
			if len(legLists) == 0 {
				t.Fatal("the index listed no leg; the close did not read through it")
			}
			legs := []*killRaceStore{work}
			if shape == "split" {
				legs = append(legs, sessions)
			}
			for _, leg := range legs {
				for _, status := range seatWorkStatuses {
					want := 1
					if leg == work && status == step.Status {
						want++ // the release's own fence read of the step
					}
					if got := legLists[legStatus{leg, status}]; got != want {
						t.Fatalf("leg %p was listed %d times for %s, want %d (the index's list, plus the step's release fence)", leg, got, status, want)
					}
				}
			}
		})
	}
}

// The startup step's sweeps close through one seat work index, as a tick's
// session phases do: on either shape, two dead-runtime corpses and a
// stuck-creating row close on one list of every leg per status, the demand
// build runs under that index, and the index is gone when the step ends.
// hotfix/1.5.1: main runs these sweeps as session phases under runTickPhases'
// install; here startupReconcile installs it, and the stale reap hands its
// cascade the city path so the cascade finds it.
func TestSplitStoreCascade_StartupClosesReadOnlyTheStartupIndex(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			cityPath := t.TempDir()
			old := time.Now().Add(-time.Hour)
			row := func(id, name string, state session.State) beads.Bead {
				return beads.Bead{
					ID: id, Title: "worker", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, CreatedAt: old,
					Metadata: map[string]string{"session_name": name, "template": "worker", "state": string(state)},
				}
			}
			sessions := &killRaceStore{Store: beads.NewMemStoreFrom(3, []beads.Bead{
				row("gc-1", "kill-town--worker-1", session.StateActive),
				row("gc-2", "kill-town--worker-2", session.StateActive),
				row("gc-3", "kill-town--worker-3", session.StateCreating),
			}, nil)}
			work := sessions
			var routes *storageRoutes
			if shape == "split" {
				work = &killRaceStore{Store: beads.NewMemStore()}
				routes = splitRoutes(sessions)
			}
			registerWorkShapeWith(t, cityPath, shape, sessions, work)
			sp := newDeadRuntimeArtifactProvider()
			for _, name := range []string{"kill-town--worker-1", "kill-town--worker-2"} {
				sp.visible[name], sp.dead[name] = true, true
			}
			cr := &CityRuntime{
				cityPath:            cityPath,
				cityName:            "kill-town",
				cfg:                 &config.City{},
				sp:                  sp,
				standaloneCityStore: work,
				storageRoutes:       routes,
				wg:                  fixedWispGC{},
				rec:                 events.Discard,
				logPrefix:           "test",
				stdout:              io.Discard,
				stderr:              io.Discard,
			}
			var built *seatWorkIndex
			cr.buildFn = func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
				built = seatWorkIndexFor(cityPath, sessions)
				return DesiredStateResult{State: map[string]TemplateParams{}}
			}

			// The index reads its legs concurrently.
			var mu sync.Mutex
			type legStatus struct {
				store  *killRaceStore
				status string
			}
			legLists := map[legStatus]int{}
			watch := func(s *killRaceStore) func(beads.ListQuery, []beads.Bead) {
				return func(q beads.ListQuery, _ []beads.Bead) {
					if q.Live && q.TierMode == beads.TierBoth && q.Type == "" && q.Label == "" && len(q.IDs) == 0 && q.Assignee == "" && len(q.Assignees) == 0 {
						mu.Lock()
						legLists[legStatus{s, q.Status}]++
						mu.Unlock()
					}
				}
			}
			sessions.afterList, work.afterList = watch(sessions), watch(work)

			if !cr.startupReconcile(context.Background()) {
				t.Fatal("the startup step did not complete")
			}
			sessions.afterList, work.afterList = nil, nil

			for _, id := range []string{"gc-1", "gc-2", "gc-3"} {
				if got, _ := sessions.Get(id); got.Status != "closed" {
					t.Fatalf("row %s status = %q, want closed by the startup sweeps", id, got.Status)
				}
			}
			legs := []*killRaceStore{work}
			if shape == "split" {
				legs = append(legs, sessions)
			}
			for _, leg := range legs {
				for _, status := range seatWorkStatuses {
					if got := legLists[legStatus{leg, status}]; got != 1 {
						t.Fatalf("leg %p was listed %d times for %s, want 1 (the startup index's list)", leg, got, status)
					}
				}
			}
			if built == nil {
				t.Fatal("the demand build ran without the startup index")
			}
			if seatWorkIndexFor(cityPath, sessions) != nil {
				t.Fatal("the startup index outlived the step")
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
	registerWorkShapeWith(t, cityPath, "split", env.store, work)
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
