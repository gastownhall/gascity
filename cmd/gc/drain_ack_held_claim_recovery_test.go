package main

// The drain-ack held-claim recovery lanes (gastownhall/gascity#2293's follow-up
// contract): a session that drain-acks while still holding an in_progress
// claim is finalized asleep WITHOUT touching the claim (the event is the
// signal), and the claim's release is owned by exactly one later lane per
// shape —
//
//   - an undesired split-store seat: the orphan close's aligned-store
//     release (SESS-082), one tick after the finalize;
//   - a desired seat whose claim is unblocked: the assigned-work wake
//     restarts the same bead with the claim kept;
//   - a desired seat whose claim is blocked: the stranded stamp
//     (SESS-624) then the stranded repair (SESS-625) after its
//     confirmation window.
//
// The tests assert outcomes, not lanes: each names the end state the shape
// converges to and the single event that trail owns.

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// drainAckClearingDops models the production ack lifetime on top of
// fakeDrainOps: providerDrainOps.clearDrain removes GC_DRAIN_ACK together with
// the drain, while fakeDrainOps.clearDrain leaves the ack map set. Without the
// ack clearing, the finalize tick's clearDrain would be invisible and the NEXT
// tick would re-enter the drain-ack branch instead of reaching the recovery
// lanes this file pins.
type drainAckClearingDops struct {
	*fakeDrainOps
}

func (d drainAckClearingDops) clearDrain(sessionName string) error {
	if err := d.fakeDrainOps.clearDrain(sessionName); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.acked, sessionName)
	d.mu.Unlock()
	return nil
}

// recordedEvents returns the events rec recorded of typ.
func drainAckEvents(t *testing.T, rec *events.Fake, typ string) []events.Event {
	t.Helper()
	evts, err := rec.List(events.Filter{Type: typ})
	if err != nil {
		t.Fatalf("listing %s events: %v", typ, err)
	}
	return evts
}

// drainAckSplitEnv is the split-store drain-ack shape: the bindingOrphan
// city (a split city whose graph-class steps are relocated into the binding,
// one rig-scoped pool seat outside the desired set) with the seat ACTIVE on a
// runtime the provider no longer reports, a drain already acknowledged for
// it, and an in_progress step resident in the binding — the seat's claim
// lives in the one store its routed prefix does not name.
type drainAckSplitEnv struct {
	cityPath string
	cfg      *config.City
	binding  beads.Store
	rig      beads.Store
	sp       *runtime.Fake
	dops     drainOps
	dt       *drainTracker
	rec      *events.Fake
	session  beads.Bead
	work     beads.Bead
}

func newDrainAckSplitEnv(t *testing.T) *drainAckSplitEnv {
	t.Helper()
	cityPath := t.TempDir()
	writeCityTOML(t, cityPath, "test-city", "gc.implementation-worker")
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session:   config.SessionConfig{Provider: "fake"},
		Rigs:      []config.Rig{{Name: "beads", Path: filepath.Join(cityPath, "beads")}},
		Agents: []config.Agent{{
			Name:              "gc.implementation-worker",
			Dir:               "beads",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}

	// Type + label matter: the reconciler resolves a seat's liveness through
	// session.ResolveSessionRecordByExactID, which only recognizes a typed (or
	// labeled and untyped) bead as a session record. The seat is ACTIVE and
	// its last wake is aged past wakeUndesiredGrace so the later orphan close
	// is not deferred by the undesired-wake grace.
	session, err := binding.Create(beads.Bead{
		Title:  "gc.implementation-worker",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"session_name":       bindingOrphanSeat,
			"template":           bindingOrphanTemplate,
			"agent_name":         "gc.implementation-worker",
			"state":              "active",
			"last_woke_at":       time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "tok-drain-ack",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work := createWork(t, binding, "in_progress", bindingOrphanSeat, map[string]string{
		beadmeta.RoutedToMetadataKey: bindingOrphanTemplate,
		"gc.root_store_ref":          "rig:beads",
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck(bindingOrphanSeat); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	now := time.Now().UTC()
	dt := newDrainTracker()
	dt.set(session.ID, &drainState{
		startedAt:  now.Add(-defaultDrainTimeout),
		deadline:   now.Add(-time.Second),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})

	return &drainAckSplitEnv{
		cityPath: cityPath,
		cfg:      cfg,
		binding:  binding,
		rig:      beads.NewMemStore(),
		sp:       runtime.NewFake(),
		dops:     drainAckClearingDops{dops},
		dt:       dt,
		rec:      events.NewFake(),
		session:  session,
		work:     work,
	}
}

func (e *drainAckSplitEnv) rigStores() map[string]beads.Store {
	return map[string]beads.Store{"beads": e.rig}
}

func (e *drainAckSplitEnv) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := e.binding.Get(id)
	if err != nil {
		t.Fatalf("binding.Get(%s): %v", id, err)
	}
	return got
}

// tick runs one controller bead-reconcile tick over the seat, re-reading both
// beads first so each tick observes the previous tick's writes. The
// aligned-store DesiredStateResult travels the whole production route:
// wake filter → start options → drain-ack finalize → orphan close.
func (e *drainAckSplitEnv) tick(t *testing.T) {
	t.Helper()
	session := e.get(t, e.session.ID)
	work := e.get(t, e.work.ID)
	cr := &CityRuntime{
		cityPath:            e.cityPath,
		cityName:            "test-city",
		cfg:                 e.cfg,
		sp:                  e.sp,
		dops:                e.dops,
		standaloneCityStore: e.binding,
		standaloneRigStores: e.rigStores(),
		sessionDrains:       e.dt,
		rec:                 e.rec,
		logPrefix:           "gc",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{work},
		AssignedWorkStores:    []beads.Store{e.binding},
		AssignedWorkStoreRefs: []string{""},
	}, newSessionBeadSnapshot([]beads.Bead{session}), nil, false)
}

// TestDrainAckHeldClaim_UndesiredSplitSeatReleasesBindingStep is the split-store
// recovery end state: after the finalize tick, ONE more tick closes the seat
// as the orphan it now is, and the orphan close's aligned-store release
// (SESS-082) frees the binding-resident step — open, unassigned, named by
// exactly one bead.dead_assignee_reopened. No clock advance between ticks.
func TestDrainAckHeldClaim_UndesiredSplitSeatReleasesBindingStep(t *testing.T) {
	env := newDrainAckSplitEnv(t)

	env.tick(t) // finalize: asleep/idle, event, claim kept
	env.tick(t) // orphan close: aligned-store release, then close

	work := env.get(t, env.work.ID)
	if work.Status != "open" || work.Assignee != "" {
		t.Fatalf("step = status %q assignee %q, want open/unassigned — the orphan close must release the binding-resident claim", work.Status, work.Assignee)
	}
	session := env.get(t, env.session.ID)
	if session.Status != "closed" {
		t.Fatalf("seat status = %q, want closed — with the held step released the close guard stops refusing", session.Status)
	}
	dead := drainAckEvents(t, env.rec, events.BeadDeadAssigneeReopened)
	if len(dead) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", events.BeadDeadAssigneeReopened, len(dead))
	}
	if !strings.Contains(string(dead[0].Payload), env.work.ID) {
		t.Fatalf("dead-assignee event payload does not name the step %q: %s", env.work.ID, dead[0].Payload)
	}
	// The finalize's event stays at one: the second tick releases, it does not re-finalize.
	if evts := drainAckEvents(t, env.rec, events.SessionDrainAckedWithAssignedWork); len(evts) != 1 {
		t.Fatalf("%s events = %d across both ticks, want 1", events.SessionDrainAckedWithAssignedWork, len(evts))
	}
}
