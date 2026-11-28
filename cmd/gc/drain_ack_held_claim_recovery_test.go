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

// TestDrainAckHeldClaim_FinalizeKeepsBindingStepAndEmitsEvent pins the
// finalize tick on its own (#2293): a drain-acked seat whose runtime is gone
// and which still holds an in_progress claim sleeps as idle WITHOUT closing
// and WITHOUT touching the claim, emitting session.drain_acked_with_assigned_work
// exactly once for the pair. The clock is never advanced: this is one tick.
func TestDrainAckHeldClaim_FinalizeKeepsBindingStepAndEmitsEvent(t *testing.T) {
	env := newDrainAckSplitEnv(t)

	env.tick(t)

	work := env.get(t, env.work.ID)
	if work.Status != "in_progress" || work.Assignee != bindingOrphanSeat {
		t.Fatalf("step = status %q assignee %q, want in_progress on its seat — the finalize must not release the claim", work.Status, work.Assignee)
	}
	session := env.get(t, env.session.ID)
	if session.Status == "closed" {
		t.Fatalf("seat closed on the finalize tick — the drain-ack finalize stops at the event while the seat holds work")
	}
	if session.Metadata["state"] != "asleep" || session.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("seat state=%q sleep_reason=%q, want asleep/idle after the drain-ack finalize",
			session.Metadata["state"], session.Metadata["sleep_reason"])
	}
	evts := drainAckEvents(t, env.rec, events.SessionDrainAckedWithAssignedWork)
	if len(evts) != 1 {
		t.Fatalf("%s events = %d, want exactly 1 on the finalize tick", events.SessionDrainAckedWithAssignedWork, len(evts))
	}
	if !strings.Contains(string(evts[0].Payload), env.session.ID) || !strings.Contains(string(evts[0].Payload), env.work.ID) {
		t.Fatalf("event payload does not name the session %q and step %q: %s", env.session.ID, env.work.ID, evts[0].Payload)
	}
	if dead := drainAckEvents(t, env.rec, events.BeadDeadAssigneeReopened); len(dead) != 0 {
		t.Fatalf("%s events = %d on the finalize tick, want 0 — the release is a later lane's job", events.BeadDeadAssigneeReopened, len(dead))
	}
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

// drainAckDesiredSeat is the single-store drain-ack shape over a DESIRED pool
// seat: the seat drain-acks holding one in_progress claim (blocked or not)
// and the finalize tick lands it asleep/idle with the claim kept.
type drainAckDesiredSeat struct {
	env     *reconcilerTestEnv
	rec     *events.Fake
	session beads.Bead
	work    beads.Bead
}

func newDrainAckDesiredSeat(t *testing.T, blocked bool) *drainAckDesiredSeat {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0)}}}
	rec := events.NewFake()
	env.rec = rec
	env.addDesired("worker", "worker", false)

	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"pool_managed": "true",
		"pool_slot":    "1",
	})
	env.markSessionActive(&session)

	seed := beads.Bead{
		Title:    "step held across the drain-ack",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}
	if blocked {
		seed.IsBlocked = boolPtr(true)
	}
	// MemStore.Create normalizes a new bead's status to open, so the
	// in_progress claim lands through an explicit transition.
	work, err := env.store.Create(seed)
	if err != nil {
		t.Fatalf("Create(step): %v", err)
	}
	inProgress := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set step in_progress: %v", err)
	}
	work, err = env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload step: %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now().Add(-defaultDrainTimeout),
		deadline:   env.clk.Now().Add(-time.Second),
		reason:     "orphaned",
		generation: 1,
		ackSet:     true,
	})

	// The finalize tick: the dead runtime plus the acknowledged drain lands
	// the seat asleep/idle with the claim kept, once.
	woken := reconcileSessionBeadsAtPath(
		context.Background(),
		env.city,
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		[]beads.Bead{work},
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
		withAsyncDrainAckStopTracker(env.stops),
	)
	if woken != 0 {
		t.Fatalf("woken on the finalize tick = %d, want 0", woken)
	}
	return &drainAckDesiredSeat{env: env, rec: rec, session: session, work: work}
}

// assertFinalized pins the shared first tick: asleep/idle, seat open, claim
// untouched, exactly one assigned-work event naming the pair.
func (d *drainAckDesiredSeat) assertFinalized(t *testing.T) {
	t.Helper()
	work := d.mustGet(t, d.work.ID)
	if work.Status != "in_progress" || work.Assignee != d.session.ID {
		t.Fatalf("step = status %q assignee %q, want in_progress on its seat after the finalize", work.Status, work.Assignee)
	}
	session := d.mustGet(t, d.session.ID)
	if session.Status == "closed" {
		t.Fatalf("seat closed on the finalize tick while holding work")
	}
	if session.Metadata["state"] != "asleep" || session.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("seat state=%q sleep_reason=%q, want asleep/idle after the finalize",
			session.Metadata["state"], session.Metadata["sleep_reason"])
	}
	evts := drainAckEvents(t, d.rec, events.SessionDrainAckedWithAssignedWork)
	if len(evts) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", events.SessionDrainAckedWithAssignedWork, len(evts))
	}
	if !strings.Contains(string(evts[0].Payload), d.session.ID) || !strings.Contains(string(evts[0].Payload), d.work.ID) {
		t.Fatalf("event payload does not name the session %q and step %q: %s", d.session.ID, d.work.ID, evts[0].Payload)
	}
}

func (d *drainAckDesiredSeat) mustGet(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := d.env.store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return got
}

// tick runs one reconcile over the re-read seat and its claim, with no drain
// ack pending: the post-finalize lanes see a desired pool seat asleep on its
// drain, holding its claim.
func (d *drainAckDesiredSeat) tick(t *testing.T) int {
	t.Helper()
	session := d.mustGet(t, d.session.ID)
	work := d.mustGet(t, d.work.ID)
	return reconcileSessionBeadsAtPath(
		context.Background(),
		d.env.city,
		[]beads.Bead{session},
		d.env.desiredState,
		map[string]bool{"worker": true},
		d.env.cfg,
		d.env.sp,
		d.env.store,
		newFakeDrainOps(),
		[]beads.Bead{work},
		nil,
		nil,
		d.env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		d.env.clk,
		d.env.rec,
		0,
		0,
		&d.env.stdout,
		&d.env.stderr,
		withAsyncDrainAckStopTracker(d.env.stops),
	)
}

// TestDrainAckHeldClaim_DesiredSeatWithUnblockedClaimWakesInPlace pins the
// desired-seat recovery: after the finalize, the assigned-work wake restarts
// the SAME session bead for its unblocked in_progress claim — the claim keeps
// its status and assignee, the seat is not closed, and no dead-assignee
// release fires.
func TestDrainAckHeldClaim_DesiredSeatWithUnblockedClaimWakesInPlace(t *testing.T) {
	d := newDrainAckDesiredSeat(t, false)
	d.assertFinalized(t)

	woken := d.tick(t)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 — a desired seat with an unblocked in_progress claim is demand", woken)
	}
	if !d.env.sp.IsRunning("worker") {
		t.Fatal("provider reports the seat stopped after the wake tick")
	}
	work := d.mustGet(t, d.work.ID)
	if work.Status != "in_progress" || work.Assignee != d.session.ID {
		t.Fatalf("step = status %q assignee %q, want kept on its seat across the wake", work.Status, work.Assignee)
	}
	session := d.mustGet(t, d.session.ID)
	if session.Status == "closed" {
		t.Fatal("seat closed while holding its claim — the wake lane restarts the same bead, it does not retire it")
	}
	if dead := drainAckEvents(t, d.rec, events.BeadDeadAssigneeReopened); len(dead) != 0 {
		t.Fatalf("%s events = %d, want 0 — the wake lane never releases the claim", events.BeadDeadAssigneeReopened, len(dead))
	}
}

// TestDrainAckHeldClaim_DesiredSeatWithBlockedClaimStrandsThenRepairs pins the
// blocked-claim recovery: the finalize's sleep leaves the claim blocked, so
// the seat is not demand. The next tick stamps exactly one session.stranded
// (SESS-624) and starts the confirmation window; the claim stays assigned on
// every tick inside strandedRepairConfirmGrace; the first tick past the grace
// releases the claim (open, unassigned) and closes the seat
// close_reason=stranded-repair (SESS-625).
func TestDrainAckHeldClaim_DesiredSeatWithBlockedClaimStrandsThenRepairs(t *testing.T) {
	d := newDrainAckDesiredSeat(t, true)
	d.assertFinalized(t)

	// The stranded tick: one diagnostic, the window opens, nothing is released.
	if woken := d.tick(t); woken != 0 {
		t.Fatalf("woken = %d, want 0 — a blocked in_progress claim is not demand", woken)
	}
	stranded := drainAckEvents(t, d.rec, events.SessionStranded)
	if len(stranded) != 1 {
		t.Fatalf("%s events = %d, want exactly 1 on the first stranded tick", events.SessionStranded, len(stranded))
	}
	session := d.mustGet(t, d.session.ID)
	if session.Metadata[strandedEventEmittedKey] == "" {
		t.Fatalf("seat metadata lacks %s after the stranded tick: %v", strandedEventEmittedKey, session.Metadata)
	}
	if session.Status == "closed" {
		t.Fatal("seat closed on the stranded tick — the confirmation window must run first")
	}
	if work := d.mustGet(t, d.work.ID); work.Assignee != d.session.ID || work.Status != "in_progress" {
		t.Fatalf("step = status %q assignee %q, want still held inside the confirmation window", work.Status, work.Assignee)
	}

	// Inside the grace: the marker is aged but not past it — nothing changes.
	d.env.clk.Advance(time.Minute)
	if woken := d.tick(t); woken != 0 {
		t.Fatalf("woken inside the grace = %d, want 0", woken)
	}
	if work := d.mustGet(t, d.work.ID); work.Assignee != d.session.ID {
		t.Fatalf("step assignee %q inside the grace, want still held", work.Assignee)
	}
	if session := d.mustGet(t, d.session.ID); session.Status == "closed" {
		t.Fatal("seat closed inside the confirmation window")
	}
	if n := len(drainAckEvents(t, d.rec, events.SessionStranded)); n != 1 {
		t.Fatalf("%s events = %d inside the grace, want the single stamped diagnostic", events.SessionStranded, n)
	}

	// Past the grace: the repair releases the claim and retires the seat.
	d.env.clk.Advance(2 * time.Minute)
	if woken := d.tick(t); woken != 0 {
		t.Fatalf("woken on the repair tick = %d, want 0", woken)
	}
	work := d.mustGet(t, d.work.ID)
	if work.Status != "open" || work.Assignee != "" {
		t.Fatalf("step = status %q assignee %q after the grace, want open/unassigned — the stranded repair reclaims blocked work", work.Status, work.Assignee)
	}
	session = d.mustGet(t, d.session.ID)
	if session.Status != "closed" {
		t.Fatalf("seat status = %q after the grace, want closed by the stranded repair", session.Status)
	}
	if want := sessionpkg.CanonicalCloseReason(strandedRepairCloseReason); session.Metadata["close_reason"] != want {
		t.Fatalf("close_reason = %q, want %q", session.Metadata["close_reason"], want)
	}
}
