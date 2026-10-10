package main

import (
	"context"
	"errors"
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

// mc-3ixn3.16 review: the close-time re-read. A claim assigned to a seat after
// the tick's seat work index read it refuses a gate-decided close, on every
// leg kind, and the re-read never reads the remote work store live.

// closeReadCity is a split city whose work store is cached (a CachingStore
// over a counted backing, as a controller's is), with a local binding and a
// declared native rig.
type closeReadCity struct {
	path     string
	cfg      *config.City
	sessions beads.Store
	backing  *workCallCounter
	work     *beads.CachingStore
	rig      beads.Store
	rigs     map[string]beads.Store
	seat     session.Info
}

func newCloseReadCity(t *testing.T) *closeReadCity {
	t.Helper()
	c := &closeReadCity{path: t.TempDir(), sessions: beads.NewMemStore(), backing: &workCallCounter{Store: beads.NewMemStore()}, rig: beads.NewMemStore()}
	c.cfg = &config.City{
		Workspace: config.Workspace{Name: "kill-town"},
		Rigs:      []config.Rig{{Name: "riga", Path: filepath.Join(c.path, "riga")}},
		Agents:    []config.Agent{persistentWorker()},
	}
	c.work = beads.NewCachingStore(c.backing, nil)
	if err := c.work.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	c.rigs = map[string]beads.Store{"riga": c.rig}
	registerWorkShapeWith(t, c.path, "split", c.sessions, c.work)
	c.seat = sessionInfosFromBeads([]beads.Bead{createCanonicalPoolSession(t, c.sessions, &c.cfg.Agents[0], seatWorkNow.Add(-time.Hour), 1)})[0]
	return c
}

// readIndex installs the tick's index and makes it read, as the tick's first
// gate does.
func (c *closeReadCity) readIndex(t *testing.T) {
	t.Helper()
	useSeatWorkPath(t, "index", c.path, c.cfg, c.sessions, c.rigs)
	if has, err := sessionHasOpenAssignedWorkForReachableStore(c.path, c.cfg, c.sessions, c.rigs, c.seat); has || err != nil {
		t.Fatalf("gate before the late claim = %v, %v", has, err)
	}
}

func (c *closeReadCity) leg(kind string) beads.Store {
	return map[string]beads.Store{"binding": c.sessions, "rig": c.rig, "work": c.work}[kind]
}

// A claim assigned after the index read refuses the gate-decided close on the
// binding, a native rig and the cached work store; with none, the seat closes.
// Either way the close reads the work store's backing not once.
func TestCloseRead_LateClaimRefusesTheClose(t *testing.T) {
	for _, kind := range []string{"none", "binding", "rig", "work"} {
		t.Run(kind, func(t *testing.T) {
			c := newCloseReadCity(t)
			c.readIndex(t)
			var claim beads.Bead
			if kind != "none" {
				claim = createWork(t, c.leg(kind), "open", c.seat.ID, nil) // unrouted: nothing else would ever release it
			}

			before := c.backing.n()
			closed := closeSessionBeadIfReachableStoreUnassigned(c.path, c.cfg, c.sessions, c.rigs, c.seat, "drained", seatWorkNow, io.Discard, false)

			if want := kind == "none"; closed != want {
				t.Fatalf("close = %v, want %v", closed, want)
			}
			if kind != "none" {
				assertWork(t, c.leg(kind), claim.ID, "open", c.seat.ID)
			}
			if got := c.backing.n() - before; got != 0 {
				t.Fatalf("the close read the work store's backing %d times, want 0 (cache only)", got)
			}
		})
	}
}

// A close that releases the seat's work anyway (a corpse) releases what was
// assigned after the index read too, on every leg kind.
func TestCloseRead_ACorpseCloseReleasesALateClaim(t *testing.T) {
	for _, kind := range []string{"binding", "rig", "work"} {
		t.Run(kind, func(t *testing.T) {
			c := newCloseReadCity(t)
			c.readIndex(t)
			claim := createWork(t, c.leg(kind), "in_progress", c.seat.ID, nil)

			if !closeBead(c.sessions, workLegs{c.path, c.cfg, c.rigs}, c.seat, "dead-runtime", seatWorkNow, io.Discard) {
				t.Fatal("corpse close refused")
			}
			assertWork(t, c.leg(kind), claim.ID, "open", "")
		})
	}
}

// The idle-timeout kill re-reads the seat's claims before it kills: a claim
// made after the tick's gather read the index defers the kill.
func TestCloseRead_IdleKillDefersForALateClaim(t *testing.T) {
	env := newReconcilerTestEnv()
	binding := &killRaceStore{Store: env.store}
	env.store = binding
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	seat := env.createSessionBead("worker", "worker")
	env.markSessionActive(&seat)
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", seat.ID); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	registerWorkShapeWith(t, cityPath, "split", binding, beads.NewMemStore())
	// The claim lands right after the index has listed the binding.
	var mu sync.Mutex
	indexLists := 0
	binding.afterList = func(q beads.ListQuery, _ []beads.Bead) {
		mu.Lock()
		defer mu.Unlock()
		if q.Live && q.Assignee == "" && q.TierMode == beads.TierBoth && q.Type == "" && q.Label == "" {
			if indexLists++; indexLists == len(seatWorkStatuses) {
				createWork(t, binding.Store, "in_progress", seat.ID, nil)
			}
		}
	}
	it := newFakeIdleTracker()
	it.idle["worker"] = true

	reconcileSessionBeadsAtPath(context.Background(), cityPath, []beads.Bead{seat}, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
		env.cfg, env.sp, env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)

	if indexLists < len(seatWorkStatuses) {
		t.Fatalf("the index read the binding %d times; the fixture never reached the idle gather", indexLists)
	}
	if !env.sp.IsRunning("worker") {
		t.Fatalf("idle worker killed holding a claim made after the tick's read; stderr=%q", env.stderr.String())
	}
}

// The drain-finalize close re-reads with its gate's own filter: the seat's own
// drain step, made after the index read, does not refuse the close.
func TestCloseRead_DrainFinalizeIgnoresTheOwnDrainStep(t *testing.T) {
	c := newCloseReadCity(t)
	c.readIndex(t)
	root, err := c.sessions.Create(beads.Bead{Title: "root", Type: "task", Ref: "mol-do-work"})
	if err != nil {
		t.Fatal(err)
	}
	createWork(t, c.sessions, "in_progress", c.seat.ID, map[string]string{beadmeta.StepRefMetadataKey: "mol-do-work.drain", beadmeta.RootBeadIDMetadataKey: root.ID})

	if !closeSessionBeadIfReachableStoreUnassigned(c.path, c.cfg, c.sessions, c.rigs, c.seat, "drained", seatWorkNow, io.Discard, true) {
		t.Fatal("the drain-finalize close refused over the seat's own drain step")
	}
}

// assigneeListFails is a local leg whose per-identity lists fail: the tick's
// index (status lists) reads it, and the close-time re-read cannot.
type assigneeListFails struct{ beads.Store }

func (s assigneeListFails) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Assignee != "" {
		return nil, errors.New("binding unreadable")
	}
	return s.Store.List(q)
}

// Fail closed: an unreadable local leg refuses a gate-decided close.
func TestCloseRead_AnUnreadableLocalLegRefusesTheClose(t *testing.T) {
	c := newCloseReadCity(t)
	c.readIndex(t)
	c.rigs["riga"] = assigneeListFails{Store: c.rig}

	if closeSessionBeadIfReachableStoreUnassigned(c.path, c.cfg, c.sessions, c.rigs, c.seat, "drained", seatWorkNow, io.Discard, false) {
		t.Fatal("the close went ahead over a local leg the re-read could not read")
	}
}

// The drain-deadline retire re-reads with its gate's filter: a wedged drain
// still holding its own drain step, on the local binding, is retired rather
// than refused by the re-read on every tick for good.
func TestCloseRead_DrainDeadlineRetireIgnoresTheOwnDrainStep(t *testing.T) {
	env := poolSeatEnv()
	seat := stuckDrainedPoolSeat(t, env, "drained", poolSlotDrainRetireDeadline+time.Minute)
	cityPath := t.TempDir()
	registerWorkShapeWith(t, cityPath, "split", env.store, beads.NewMemStore())
	root, err := env.store.Create(beads.Bead{Title: "root", Type: "task", Ref: "mol-do-work"})
	if err != nil {
		t.Fatal(err)
	}
	createWork(t, env.store, "in_progress", seat.ID, map[string]string{beadmeta.StepRefMetadataKey: "mol-do-work.drain", beadmeta.RootBeadIDMetadataKey: root.ID})

	reconcileSessionBeadsAtPath(context.Background(), cityPath, []beads.Bead{seat}, env.desiredState, map[string]bool{"worker": true},
		env.cfg, env.sp, env.store, newFakeDrainOps(), nil, nil, nil, env.dt, nil, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr)

	got, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("wedged seat holding only its own drain step was not retired; stderr=%q", env.stderr.String())
	}
}

// A bd rig is a remote store: the re-read takes it from its cache, never
// live, and a bd rig with no cache to answer is not read at all. A local rig
// is read live.
func TestCloseRead_BdRigsAreReadFromTheCacheOnly(t *testing.T) {
	var bdCalls int
	bd := beads.NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		bdCalls++
		return []byte("[]"), nil
	})
	q := beads.ListQuery{Assignee: "seat", Status: "open", TierMode: beads.TierBoth}

	if list := closeTimeLister(bd, false); list != nil {
		t.Fatal("an uncached bd rig got a lister; it would be read live")
	}
	cached := beads.NewCachingStore(bd, nil)
	list := closeTimeLister(wrapStoreWithBeadPolicies(cached, &config.City{}), false)
	if list == nil {
		t.Fatal("a cached bd rig got no lister")
	}
	before := bdCalls
	if _, err := list(q); err != nil {
		t.Fatal(err)
	}
	if bdCalls != before {
		t.Fatalf("the cached bd rig was read live: %d bd calls", bdCalls-before)
	}
	local := &workCallCounter{Store: beads.NewMemStore()}
	if _, err := closeTimeLister(local, false)(q); err != nil || local.n() != 1 {
		t.Fatalf("local rig reads = %d, %v; want one live list", local.n(), err)
	}
}
