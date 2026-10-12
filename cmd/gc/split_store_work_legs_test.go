package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// mc-3ixn3.16: on a split-storage city the legacy reconciler hands every close
// path its SESSIONS store, and the close side read work from it. The census was
// fixed for this in #5280; these pin the gate and release side. Each path runs
// on both shapes a legacy controller serves:
//
//   - split (maintainer-city, stage-mc): sessions on the binding the reconciler
//     leads with, work in a distinct registered city work store;
//   - single (stage-dolt): one store is both, registered as the work store.
//
// Staging journeys J39 (C, E, F) and J29 (S, A) on stage-mc are the evidence.

// workShapes are the two topologies every test here runs on.
var workShapes = []string{"single", "split"}

// registerWorkShape registers cityPath as a controller serving shape would, and
// returns the city work store: sessions itself on a single-store city, a
// distinct store beside the binding on a split one.
func registerWorkShape(t *testing.T, cityPath, shape string, sessions beads.Store) beads.Store {
	t.Helper()
	work := sessions
	if shape == "split" {
		work = beads.NewMemStore()
	}
	registerWorkShapeWith(t, cityPath, shape, sessions, work)
	return work
}

// registerWorkShapeWith is registerWorkShape with the caller's work store (the
// sessions store itself on a single-store city).
func registerWorkShapeWith(t *testing.T, cityPath, shape string, sessions, work beads.Store) {
	t.Helper()
	var routes *storageRoutes
	if shape == "split" {
		routes = splitRoutes(sessions)
	}
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
}

var seatWorkNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// seatWorkPaths are the two ways a gate answers: by its live probes, or from a
// tick's seat work index.
var seatWorkPaths = []string{"live", "index"}

// useSeatWorkPath installs a tick's index for the rest of the test when path
// is "index".
func useSeatWorkPath(t *testing.T, path, cityPath string, cfg *config.City, leading beads.Store, rigs map[string]beads.Store) {
	t.Helper()
	if path == "index" {
		t.Cleanup(installSeatWorkIndex(cityPath, cfg, leading, rigs))
		if seatWorkIndexFor(cityPath, leading) == nil {
			t.Fatal("no seat work index installed")
		}
	}
}

// createWork creates a work bead in store assigned to assignee.
func createWork(t *testing.T, store beads.Store, status, assignee string, meta map[string]string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{Title: "work " + status, Type: "task", Status: "open", Assignee: assignee, Metadata: meta})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if status != "open" {
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set work status %s: %v", status, err)
		}
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return got
}

func assertWork(t *testing.T, store beads.Store, id, status, assignee string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Status != status || got.Assignee != assignee {
		t.Fatalf("work %s: status=%q assignee=%q, want %q/%q", id, got.Status, got.Assignee, status, assignee)
	}
}

// The pool-slot close's gate reads the city work store, and fails closed: a
// claim there refuses the close, an unreadable work leg refuses it too, and an
// empty readable one lets it through. Both answers agree: the live probes (one-
// shot callers) and the tick's seat work index.
func TestSplitStoreWork_PoolSlotCloseGateReadsTheWorkStore(t *testing.T) {
	for _, shape := range workShapes {
		for _, path := range seatWorkPaths {
			for _, tc := range []string{"claim", "dark leg", "empty"} {
				t.Run(shape+"/"+path+"/"+tc, func(t *testing.T) {
					cityPath := t.TempDir()
					cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
					sessions := &killRaceStore{Store: beads.NewMemStore()}
					work := sessions
					if shape == "split" {
						work = &killRaceStore{Store: beads.NewMemStore()}
					}
					registerWorkShapeWith(t, cityPath, shape, sessions, work)
					seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)
					switch tc {
					case "claim":
						createWork(t, work, "open", seat.ID, nil)
					case "dark leg":
						work.listErr = map[string]error{"open": errors.New("leg is dark"), "in_progress": errors.New("leg is dark")}
					}
					useSeatWorkPath(t, path, cityPath, cfg, sessions, nil)

					closed := closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, sessions, nil,
						sessionInfosFromBeads([]beads.Bead{seat})[0], "drained", seatWorkNow, io.Discard, false)

					if want := tc == "empty"; closed != want {
						t.Fatalf("close = %v, want %v", closed, want)
					}
				})
			}
		}
	}
}

// J29 S: a pool seat whose runtime vanished holding in_progress work on the
// city work store takes the stranded branch: marker, no repair inside the
// grace, then the repair releases the work and closes the seat. On a split
// city the branch never ran.
func TestSplitStoreWork_StrandedRepairFiresForWorkStoreWork(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			env, seat, _, rec := strandedRepairReconcileEnv(t)
			cityPath := t.TempDir()
			work := registerWorkShape(t, cityPath, shape, env.store)
			claim := createWork(t, work, "in_progress", seat.ID, nil)
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
			if got := len(rec.strandedEvents()); got != 1 {
				t.Fatalf("stranded events = %d, want 1 (the branch did not run)", got)
			}
			assertWork(t, work, claim.ID, "in_progress", seat.ID)

			env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
			tick()

			assertWork(t, work, claim.ID, "open", "")
			got, err := env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" || got.Metadata["close_reason"] != session.CanonicalCloseReason(strandedRepairCloseReason) {
				t.Fatalf("seat status=%q close_reason=%q, want closed by the stranded repair", got.Status, got.Metadata["close_reason"])
			}
		})
	}
}

// declaredRigsConfig declares names as rigs, so their stores are serving legs
// (servingRigStores drops a rig store city.toml does not declare).
func declaredRigsConfig(names ...string) *config.City {
	cfg := &config.City{}
	for _, name := range names {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: filepath.Join("/nonexistent-rigs", name)})
	}
	return cfg
}

// One leg set for every seat (mc-3ixn3.16 ruling): a rig-bound seat's gate and
// stranded read include the city work store, so its city-store claim holds it
// rather than being released at close; and a suspended rig is never read,
// though its store is dark.
func TestSplitStoreWork_RigBoundSeatHoldsItsCityStoreClaim(t *testing.T) {
	for _, shape := range workShapes {
		for _, path := range seatWorkPaths {
			t.Run(shape+"/"+path, func(t *testing.T) {
				cfg, cityPath, infos := rigScopedWakeFixture(t)
				cfg.Rigs = append(cfg.Rigs, config.Rig{Name: "rigb", Path: filepath.Join(cityPath, "rigb"), SuspendedOnStart: true})
				sessions := beads.NewMemStore()
				work := registerWorkShape(t, cityPath, shape, sessions)
				dark := &killRaceStore{Store: beads.NewMemStore(), listErr: map[string]error{"open": errors.New("suspended"), "in_progress": errors.New("suspended")}}
				rigs := map[string]beads.Store{"riga": beads.NewMemStore(), "rigb": dark}
				claim := createWork(t, work, "in_progress", infos[0].ID, nil)
				useSeatWorkPath(t, path, cityPath, cfg, sessions, rigs)

				if closeSessionBeadIfReachableStoreUnassigned(cityPath, cfg, sessions, rigs, infos[0], "drained", seatWorkNow, io.Discard, false) {
					t.Fatal("the rig-bound seat closed holding a city-store claim")
				}
				stranded, err := collectSessionAssignedWorkInfo(cityPath, cfg, sessions, rigs, infos[0])
				if err != nil || len(stranded) != 1 || stranded[0].bead.ID != claim.ID {
					t.Fatalf("stranded read = %+v, %v; want the city-store claim", stranded, err)
				}
			})
		}
	}
}

// The leg set itself: on a split city every seat's plan names the registered
// work store FIRST, the serving rigs, then the binding it was handed as its own
// leg, which is the census's set.
func TestSplitStoreWork_LegSetMatchesTheCensus(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	work := registerWorkShape(t, cityPath, "split", binding)
	rigs := map[string]beads.Store{"riga": beads.NewMemStore()}

	plan, err := assignedWorkPlanForSessionInfo(cityPath, cfg, binding, rigs, infos[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := planStores(t, plan); !sameStores(got, work, rigs["riga"], binding) {
		t.Fatalf("rig-bound seat legs = %v, want [work, rig, binding]", got)
	}
	census, err := censusStoreCandidates(cityPath, cfg, binding, rigs, nil, censusRefBare)
	if err != nil {
		t.Fatal(err)
	}
	var censusStores []beads.Store
	for _, c := range census {
		censusStores = append(censusStores, c.store)
	}
	if !sameStores(censusStores, work, rigs["riga"], binding) {
		t.Fatalf("census legs = %v, disagree with the gates'", censusStores)
	}
}

// The index answers every gate it serves exactly as that gate's live probes
// do. Each case seeds one kind of row for the seat on the city work store of a
// split city.
func TestSeatWorkIndex_AnswersAsTheLiveProbesDo(t *testing.T) {
	cases := map[string]func(t *testing.T, work beads.Store, seat string){
		"nothing":     func(*testing.T, beads.Store, string) {},
		"open":        func(t *testing.T, w beads.Store, seat string) { createWork(t, w, "open", seat, nil) },
		"in_progress": func(t *testing.T, w beads.Store, seat string) { createWork(t, w, "in_progress", seat, nil) },
		"closed": func(t *testing.T, w beads.Store, seat string) {
			closeWork(t, w, createWork(t, w, "in_progress", seat, nil))
		},
		"other seat": func(t *testing.T, w beads.Store, _ string) { createWork(t, w, "in_progress", "someone-else", nil) },
		"mail": func(t *testing.T, w beads.Store, seat string) {
			if _, err := w.Create(beads.Bead{Title: "note", Type: "message", Assignee: seat, Status: "open"}); err != nil {
				t.Fatal(err)
			}
		},
		"own drain step": func(t *testing.T, w beads.Store, seat string) {
			root, err := w.Create(beads.Bead{Title: "root", Type: "task", Ref: "mol-do-work"})
			if err != nil {
				t.Fatal(err)
			}
			createWork(t, w, "in_progress", seat, map[string]string{beadmeta.StepRefMetadataKey: "mol-do-work.drain", beadmeta.RootBeadIDMetadataKey: root.ID})
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
			answers := map[string]string{}
			for _, path := range seatWorkPaths {
				cityPath := t.TempDir()
				sessions := beads.NewMemStore()
				work := registerWorkShape(t, cityPath, "split", sessions)
				seatBead := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)
				info := sessionInfosFromBeads([]beads.Bead{seatBead})[0]
				seed(t, work, seatBead.ID)
				useSeatWorkPath(t, path, cityPath, cfg, sessions, nil)
				answers[path] = seatWorkAnswers(t, cityPath, cfg, sessions, info)
			}
			if answers["live"] != answers["index"] {
				t.Fatalf("index answered\n  %s\nlive answered\n  %s", answers["index"], answers["live"])
			}
		})
	}
}

// seatWorkAnswers renders every index-served gate's answer for info, with bead
// ids reduced to found/not found (the two paths run on separate stores).
func seatWorkAnswers(t *testing.T, cityPath string, cfg *config.City, store beads.Store, info session.Info) string {
	t.Helper()
	var out []string
	add := func(name string, v bool, err error) {
		out = append(out, fmt.Sprintf("%s=%v/%v", name, v, err != nil))
	}
	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, nil, info)
	add("open", has, err)
	has, err = sessionHasOpenAssignedWorkForReachableStoreForCloseGate(cityPath, cfg, store, nil, info)
	add("close-gate", has, err)
	has, err = sessionHasInProgressAssignedWorkForConfig(cityPath, cfg, store, nil, info)
	add("in-progress", has, err)
	has, err = sessionHasOpenAssignedWorkForConfigInfo(cityPath, cfg, store, nil, info)
	add("for-config", has, err)
	has, err = sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, store, nil, info)
	add("awake", has, err)
	_, has, err = firstInProgressAssignedWorkBeadForReachableStore(cityPath, cfg, store, nil, info)
	add("first-in-progress", has, err)
	_, has, err = firstOpenClaimableAssignedWorkBeadForReachableStore(cityPath, cfg, store, nil, info, seatWorkNow)
	add("first-claimable", has, err)
	stranded, err := collectSessionAssignedWorkInfo(cityPath, cfg, store, nil, info)
	add("stranded", len(stranded) > 0, err)
	return strings.Join(out, " ")
}

func closeWork(t *testing.T, store beads.Store, b beads.Bead) {
	t.Helper()
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
}

// workCallCounter counts the calls a store answers: the cost model for a work
// store that is bd over a remote database (about 4s a call).
type workCallCounter struct {
	beads.Store
	mu    sync.Mutex
	reads int
}

func (s *workCallCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.count()
	return s.Store.List(q)
}

func (s *workCallCounter) Get(id string) (beads.Bead, error) {
	s.count()
	return s.Store.Get(id)
}

// UpdateIfAssignment models bd's guarded update (`bd update --if-status
// --if-assignee`): the fence and the write are one call.
func (s *workCallCounter) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	s.count()
	current, err := s.Store.Get(id)
	if err != nil {
		return false, err
	}
	if current.Status != expectedStatus || strings.TrimSpace(current.Assignee) != expectedAssignee {
		return false, nil
	}
	return true, s.Update(id, opts)
}

func (s *workCallCounter) count() {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
}

func (s *workCallCounter) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// mcWorkStoreCall is maintainer-city's measured cost of one bd call against its
// remote work store (3-5s).
const mcWorkStoreCall = 4 * time.Second

// Cost: inside a tick, a gate on a seat that holds nothing makes no work-store
// read beyond the index's (one list per status, once per tick).
func TestSeatWorkIndex_GatesCostAtMcLatency(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
	sessions := beads.NewMemStore()
	work := &workCallCounter{Store: beads.NewMemStore()}
	registerWorkShapeWith(t, cityPath, "split", sessions, work)
	held := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)
	for i := 0; i < 3; i++ {
		createWork(t, work.Store, "open", held.ID, nil)
	}
	var idle []session.Info
	for slot := 2; slot <= 6; slot++ {
		idle = append(idle, sessionInfosFromBeads([]beads.Bead{createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), slot)})[0])
	}

	// The live probes, for scale: each idle gate lists the work store per
	// identity and status.
	for _, info := range idle {
		if has, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate(cityPath, cfg, sessions, nil, info); has || err != nil {
			t.Fatalf("live idle seat gate = %v, %v", has, err)
		}
	}
	if live := time.Duration(work.n()) * mcWorkStoreCall; live <= time.Duration(len(seatWorkStatuses))*mcWorkStoreCall {
		t.Fatalf("live idle gates cost %v; the fixture no longer shows the per-gate cost", live)
	}

	before := work.n()
	useSeatWorkPath(t, "index", cityPath, cfg, sessions, nil)
	for _, info := range idle {
		if has, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate(cityPath, cfg, sessions, nil, info); has || err != nil {
			t.Fatalf("idle seat gate = %v, %v", has, err)
		}
	}
	if got := work.n() - before; got != len(seatWorkStatuses) {
		t.Fatalf("the index and %d idle gates read the work store %d times, want %d (one list per status)", len(idle), got, len(seatWorkStatuses))
	}
}

// A hit is re-read live before it decides anything: a claim the index saw that
// is gone by the time a gate asks does not hold the seat.
func TestSeatWorkIndex_RechecksAHitLive(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
	sessions := beads.NewMemStore()
	work := registerWorkShape(t, cityPath, "split", sessions)
	seat := sessionInfosFromBeads([]beads.Bead{createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)})[0]
	claim := createWork(t, work, "in_progress", seat.ID, nil)
	useSeatWorkPath(t, "index", cityPath, cfg, sessions, nil)

	if has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, sessions, nil, seat); !has || err != nil {
		t.Fatalf("gate with the claim = %v, %v; want held", has, err)
	}
	closeWork(t, work, claim)
	if has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, sessions, nil, seat); has || err != nil {
		t.Fatalf("gate after the claim closed = %v, %v; want the index's stale row re-read and dropped", has, err)
	}
}

// The controller tick's session phases share one seat work index, installed
// for the session phases and removed when the tick ends. hotfix/1.5.1 has no
// runTickPhases, so the tick's demand build (one of its session phases)
// observes the index.
func TestCityRuntimeTick_SharesOneSeatWorkIndexAcrossSessionPhases(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: work,
		wg:                  fixedWispGC{},
		rec:                 events.Discard,
		logPrefix:           "test",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	registerWorkShapeWith(t, cityPath, "single", work, work)
	var seen []*seatWorkIndex
	cr.buildFn = func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
		seen = append(seen, seatWorkIndexFor(cityPath, cr.sessionsBeadStore().Store))
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	if seatWorkIndexFor(cityPath, work) != nil {
		t.Fatal("an index is installed before the tick")
	}
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")

	if len(seen) == 0 {
		t.Fatal("the tick never built demand")
	}
	for i, idx := range seen {
		if idx == nil || idx != seen[0] {
			t.Fatalf("index seen by demand build %d = %p; want one shared index (%p)", i, idx, seen[0])
		}
	}
	if seatWorkIndexFor(cityPath, work) != nil {
		t.Fatal("the tick's index outlived the pass")
	}
}

// An install inside an installed index's span, the reconcile pass inside a tick
// or the startup step, keeps the outer index: the pass shares the outer read,
// and its removal leaves the outer index to the phases after it.
func TestSeatWorkIndex_NestedInstallKeepsTheOuterIndex(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{}
	work := beads.NewMemStore()
	registerWorkShapeWith(t, cityPath, "single", work, work)

	removeOuter := installSeatWorkIndex(cityPath, cfg, work, nil)
	outer := seatWorkIndexFor(cityPath, work)
	if outer == nil {
		t.Fatal("no outer index installed")
	}
	removeInner := installSeatWorkIndex(cityPath, cfg, work, nil)
	if got := seatWorkIndexFor(cityPath, work); got != outer {
		t.Fatalf("index inside the nested install = %p, want the outer index %p", got, outer)
	}
	removeInner()
	if got := seatWorkIndexFor(cityPath, work); got != outer {
		t.Fatalf("index after the nested removal = %p, want the outer index %p", got, outer)
	}
	removeOuter()
	if got := seatWorkIndexFor(cityPath, work); got != nil {
		t.Fatalf("index after the outer removal = %p, want none", got)
	}
}

// A dark work leg fails every gate the index serves closed, exactly as the
// live probes do: no gate may answer "no work" over a leg it could not read.
func TestSeatWorkIndex_DarkLegFailsEveryGateClosed(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
	answers := map[string]string{}
	for _, path := range seatWorkPaths {
		cityPath := t.TempDir()
		sessions := beads.NewMemStore()
		dark := &killRaceStore{Store: beads.NewMemStore(), listErr: map[string]error{"open": errors.New("leg is dark"), "in_progress": errors.New("leg is dark")}}
		registerWorkShapeWith(t, cityPath, "split", sessions, dark)
		seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)
		useSeatWorkPath(t, path, cityPath, cfg, sessions, nil)
		answers[path] = seatWorkAnswers(t, cityPath, cfg, sessions, sessionInfosFromBeads([]beads.Bead{seat})[0])
	}
	if answers["live"] != answers["index"] {
		t.Fatalf("index answered\n  %s\nlive answered\n  %s", answers["index"], answers["live"])
	}
	if strings.Contains(answers["index"], "/false") {
		t.Fatalf("a gate answered without an error over a dark leg: %s", answers["index"])
	}
}

// A leg whose read panics is an unreadable leg, not a crashed controller: the
// gate fails closed with the recovered panic.
func TestSeatWorkIndex_RecoversALegPanic(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "kill-town"}, Agents: []config.Agent{persistentWorker()}}
	sessions := beads.NewMemStore()
	registerWorkShapeWith(t, cityPath, "split", sessions, panickingLegStore{Store: beads.NewMemStore(), panicOnList: true})
	seat := createCanonicalPoolSession(t, sessions, &cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)
	useSeatWorkPath(t, "index", cityPath, cfg, sessions, nil)

	if has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, sessions, nil, sessionInfosFromBeads([]beads.Bead{seat})[0]); has || err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("gate over a panicking leg = %v, %v; want the recovered panic as the error", has, err)
	}
}
