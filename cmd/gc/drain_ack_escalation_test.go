package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The seven-seat specimen, built to the PRODUCTION shape.
//
// Seven live seats sat in state=draining / state_reason=drain-ack-stop-pending
// for two days. Their agents behaved correctly: `gc runtime drain-ack` wrote
// GC_DRAIN_ACK=1 and GC_DRAIN_ACK_SOURCE=agent on the pane, and each agent
// printed NO_ROUTED_WORK, acked, and ended its TURN — but not its RUNTIME.
//
// GC_DRAIN_ACK_SOURCE=agent is the load-bearing detail and the one an earlier
// revision of these tests got wrong. The reminder pass refuses to remind a pane
// that already carries an agent-authored ack (drainReminderAckPin), and
// correctly so, so drain_reminder_count can NEVER be written for this
// population. Any escalation gated solely on the reminder budget is therefore
// unreachable for exactly the seats it was written for. The fixture below
// carries source=agent for that reason; the reminder-budget arm is exercised as
// a control, not as the headline.
type escalationEnv struct {
	t        *testing.T
	sp       *runtime.Fake
	store    beads.Store
	clk      *clock.Fake
	bead     beads.Bead
	out      *synchronizedBuffer
	rec      *capturingRecorder
	cfg      *config.City
	cityPath string
	name     string
	now      time.Time
}

func newEscalationEnv(t *testing.T) *escalationEnv {
	t.Helper()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	name := "gc-city-worker-1"
	cityPath := t.TempDir()
	// Two days wedged, matching the incident.
	drainAt := now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)

	bead, err := store.Create(beads.Bead{
		Title: "session",
		Type:  sessionBeadType,
		Metadata: map[string]string{
			"session_name":   name,
			"template":       "worker",
			"generation":     "3",
			"state":          string(sessionpkg.StateDraining),
			"state_reason":   sessionpkg.DrainAckStopPendingReason,
			"drain_at":       drainAt,
			"instance_token": "tok-a",
			"pool_managed":   "true",
			"work_dir":       t.TempDir(),
			"provider":       "claude",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	// The live pane. GC_SESSION_ID/GC_CITY_PATH are what the process-table
	// scanner discovers by, and the city attribution is what keeps a forced
	// termination from reaching a sibling city.
	if err := sp.Start(context.Background(), name, runtime.Config{Env: map[string]string{
		"GC_SESSION_ID": bead.ID,
		"GC_CITY_PATH":  cityPath,
	}}); err != nil {
		t.Fatalf("start fake session: %v", err)
	}
	mustSetMeta(t, sp, name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	mustSetMeta(t, sp, name, "GC_DRAIN_ACK", "1")
	sp.SetActivity(name, now.Add(-30*time.Minute))
	// The defining property of the wedge: the ordinary provider stop does NOT
	// terminate this pane. That is why the pre-existing loop — which re-issues
	// exactly that stop every tick — never clears these rows, and it is why the
	// escalation has to reach for a different mechanism rather than the same one.
	sp.StopLeavesRunning = map[string]bool{name: true}

	// Keep the confirm-dead loop short; production bounds would add 6s per test.
	prevTimeout, prevPoll := drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll
	drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = prevTimeout, prevPoll
	})

	return &escalationEnv{
		t: t, sp: sp, store: store, clk: &clock.Fake{Time: now},
		bead: bead, out: &synchronizedBuffer{}, rec: &capturingRecorder{},
		cfg:      &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}},
		cityPath: cityPath, name: name, now: now,
	}
}

func (e *escalationEnv) info() sessionpkg.Info {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	return seedSessionInfo(got)
}

func (e *escalationEnv) setMeta(kvs map[string]string) {
	e.t.Helper()
	if err := e.store.SetMetadataBatch(e.bead.ID, kvs); err != nil {
		e.t.Fatalf("set session metadata: %v", err)
	}
}

func (e *escalationEnv) status() string {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	return got.Status
}

// finalize runs one reconcile tick's stop-pending pass and waits for any
// detached termination it queued, so assertions see a settled world.
func (e *escalationEnv) finalize() {
	e.t.Helper()
	tracker := &asyncStartTracker{}
	finalizeDrainAckStopPendingSessions(
		e.cityPath, e.cfg, e.sp, beads.SessionStore{Store: e.store}, nil,
		[]sessionpkg.Info{e.info()}, nil, newDrainTracker(), tracker,
		e.clk, e.rec, e.out,
	)
	tracker.wait(10 * time.Second)
}

// finalizeOnTick runs the pass WITHOUT waiting, returning how long the
// synchronous tick itself took.
func (e *escalationEnv) finalizeOnTick() time.Duration {
	e.t.Helper()
	tracker := &asyncStartTracker{}
	start := time.Now()
	finalizeDrainAckStopPendingSessions(
		e.cityPath, e.cfg, e.sp, beads.SessionStore{Store: e.store}, nil,
		[]sessionpkg.Info{e.info()}, nil, newDrainTracker(), tracker,
		e.clk, e.rec, e.out,
	)
	elapsed := time.Since(start)
	tracker.wait(10 * time.Second)
	return elapsed
}

func (e *escalationEnv) escalations() []events.Event {
	var out []events.Event
	for _, ev := range e.rec.events {
		if ev.Type == events.SessionDrainStopEscalated {
			out = append(out, ev)
		}
	}
	return out
}

func (e *escalationEnv) terminateCalls() int {
	n := 0
	for _, c := range e.sp.SnapshotCalls() {
		if c.Method == "TerminateRuntime" {
			n++
		}
	}
	return n
}

// spendReminderBudget drives the reminder arm to exhaustion. Only reachable
// when the pane is NOT agent-acked, which is exactly the point.
func (e *escalationEnv) spendReminderBudget() {
	e.t.Helper()
	mustSetMeta(e.t, e.sp, e.name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := remindStopPendingDrain(e.sp, e.store, e.info(), e.clk, e.out); got != drainReminderDelivered {
			e.t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
		}
	}
	e.clk.Time = e.now.Add(drainReminderMaxAttempts * drainReminderInterval)
}

// THE PIN. An AGENT-ACKED seat past its bound must escalate. This is the
// population the fix exists for, and the arm that serves it cannot be the
// reminder budget: with source=agent, drain_reminder_count is never written, so
// a budget-only gate refuses forever and the row loops exactly as it did before.
func TestEscalationFiresForTheAgentAckedWedgedSeat(t *testing.T) {
	e := newEscalationEnv(t)

	// Precondition: the reminder budget is, and stays, unspendable here.
	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if drainRemindersSpent(bead, e.clk.Time) {
		t.Fatal("fixture is not the agent-acked population: its reminder budget reads as spent")
	}

	e.finalize()

	evs := e.escalations()
	if len(evs) != 1 {
		t.Fatalf("escalation events = %d, want 1 — an agent-acked seat past its bound never escalates", len(evs))
	}
	if evs[0].SessionID != e.bead.ID {
		t.Errorf("event SessionID = %q, want %q", evs[0].SessionID, e.bead.ID)
	}
	if evs[0].Payload == nil {
		t.Error("escalation event carries no typed payload")
	}
	// The ordinary provider stop is what has already failed on this row every
	// tick, so the escalation must apply the force the ordinary path lacks.
	if e.terminateCalls() == 0 {
		t.Error("no process-table termination attempted; the escalation re-sent the same stop that has been failing")
	}
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Metadata[drainAckEscalationAtKey] == "" {
		t.Error("no durable escalation attempt recorded; the attempt would be re-paid every tick")
	}
}

// Control for the pin above: the reminder-budget arm still serves the
// non-agent-acked population.
func TestEscalationFiresForTheReminderExhaustedSeat(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendReminderBudget()

	e.finalize()

	if n := len(e.escalations()); n != 1 {
		t.Fatalf("escalation events = %d, want 1", n)
	}
	if e.terminateCalls() == 0 {
		t.Error("no process-table termination attempted")
	}
}

// The escalation must NOT close the bead. Closing is the finalizer's own
// fresh-observation arm on a later tick: confirmDrainAckRuntimeDead reports true
// on a definite token MISMATCH (meaning "another runtime owns this name now"),
// and closing on that would free the bead while a live pane still holds the
// runtime name — the pool's next create fails the same way, and the event would
// claim a kill that never happened.
func TestEscalationNeverClosesTheBeadItself(t *testing.T) {
	e := newEscalationEnv(t)

	e.finalize()

	if got := e.status(); got == "closed" {
		t.Error("the escalation closed the bead while its runtime was still alive; the name is not released and the pool cannot reuse it")
	}
}

// MAJOR: the kill must not run on the reconcile tick. The confirm-dead loop plus
// the process-table walk are seconds each; paid inline for N wedged rows they
// would stall every controller tick and starve the pool respawn, order dispatch
// and health patrol that the wedge is already starving.
func TestEscalationDoesNotBlockTheReconcileTick(t *testing.T) {
	e := newEscalationEnv(t)
	drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = 3*time.Second, 50*time.Millisecond

	elapsed := e.finalizeOnTick()

	if elapsed > time.Second {
		t.Errorf("synchronous tick took %v; the kill and its confirm-dead loop must run off-tick", elapsed)
	}
}

// MAJOR: the attempt is durably recorded and paced, so a row that will never die
// costs a bounded number of escalations rather than one per tick forever.
func TestEscalationIsPacedAndNotRepaidEveryTick(t *testing.T) {
	e := newEscalationEnv(t)

	e.finalize()
	if n := len(e.escalations()); n != 1 {
		t.Fatalf("first tick escalations = %d, want 1", n)
	}
	// Same tick-adjacent time: the retry interval has not elapsed.
	e.finalize()
	if n := len(e.escalations()); n != 1 {
		t.Errorf("escalations after a second immediate tick = %d, want 1 — the attempt is being re-paid every tick", n)
	}

	// Past the retry interval it may try again.
	e.clk.Time = e.now.Add(drainAckEscalationRetryInterval + time.Minute)
	e.finalize()
	if n := len(e.escalations()); n != 2 {
		t.Errorf("escalations after the retry interval = %d, want 2", n)
	}
}

// NEGATIVE CONTROL (the class that bit the 7g review). A seat still holding work
// claimed under its ALIAS must never be terminated. The narrow identifier set
// cannot see an alias claim; the probe must use session.AssigneeIdentities.
func TestEscalationNeverTerminatesSeatHoldingAliasClaimedWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		assignee string
	}{
		{"current alias", map[string]string{"alias": "nux"}, "nux"},
		{"prior alias in alias_history", map[string]string{"alias": "nux", "alias_history": "slit,morsov"}, "morsov"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEscalationEnv(t)
			e.setMeta(tc.metadata)
			if _, err := e.store.Create(beads.Bead{
				Title: "alias-claimed work", Status: "in_progress", Assignee: tc.assignee,
			}); err != nil {
				t.Fatalf("create work bead: %v", err)
			}

			e.finalize()

			if n := len(e.escalations()); n != 0 {
				t.Errorf("escalations = %d, want 0 — escalated over a live agent holding work claimed as %q", n, tc.assignee)
			}
			if e.terminateCalls() != 0 {
				t.Errorf("force-terminated a live agent that still held work claimed as %q (7g §3.5)", tc.assignee)
			}
			if got := e.status(); got == "closed" {
				t.Errorf("closed over work claimed as %q", tc.assignee)
			}
		})
	}
}

// The escalation's KILL gate is the wide one. The close gate deliberately stays
// narrow (transient pool slot aliases rebind and must never register as
// ownership — TestAssignmentGuardsIgnoreTransientPoolSlotAliases), so the wide
// set lives here, in front of the destructive act, where over-refusing is safe
// and under-refusing ends a live agent's turn.
func TestEscalationKillGateSeesAliasClaimedWork(t *testing.T) {
	e := newEscalationEnv(t)
	e.setMeta(map[string]string{"alias": "nux", "alias_history": "morsov"})
	if _, err := e.store.Create(beads.Bead{
		Title: "alias-claimed work", Status: "in_progress", Assignee: "nux",
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForEscalation(e.cityPath, e.cfg, e.store, nil, e.info())
	if err != nil {
		t.Fatalf("escalation work probe: %v", err)
	}
	if !has {
		t.Error("the escalation kill gate cannot see work claimed under the session's alias; it would authorize killing a live agent mid-turn")
	}
}

// NEGATIVE CONTROL. Inside the bound, nothing happens.
func TestEscalationLeavesInsideBoundSeatUntouched(t *testing.T) {
	e := newEscalationEnv(t)
	// Freshly stop-pending rather than two days wedged.
	e.setMeta(map[string]string{"drain_at": e.now.Add(-time.Minute).UTC().Format(time.RFC3339)})

	e.finalize()

	if n := len(e.escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0 — escalated before the grace elapsed", n)
	}
	if e.terminateCalls() != 0 {
		t.Error("force-terminated a seat inside its bound")
	}
}

// NEGATIVE CONTROL. A definite instance-token mismatch means the name belongs to
// a re-woken replacement; the seat we meant to stop is already gone.
func TestEscalationHonorsTheTokenFence(t *testing.T) {
	e := newEscalationEnv(t)
	mustSetMeta(t, e.sp, e.name, "GC_INSTANCE_TOKEN", "tok-replacement")

	e.finalize()

	if n := len(e.escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0 — escalated onto a live replacement", n)
	}
	if e.terminateCalls() != 0 {
		t.Error("force-terminated a live replacement holding the same name")
	}
}

// NEGATIVE CONTROL. The /proc scan is supervisor-wide, so a runtime that is not
// positively attributed to THIS city must never be terminated — doing so would
// SIGKILL a sibling city's healthy session.
func TestEscalationNeverTerminatesAnotherCitysRuntime(t *testing.T) {
	e := newEscalationEnv(t)
	// Re-home the pane onto a DIFFERENT city: the /proc scan is supervisor-wide,
	// so city attribution is the only thing standing between this pass and a
	// sibling city's healthy session.
	e.sp.StopLeavesRunning = nil
	if err := e.sp.Stop(e.name); err != nil {
		t.Fatalf("reset session: %v", err)
	}
	if err := e.sp.Start(context.Background(), e.name, runtime.Config{Env: map[string]string{
		"GC_SESSION_ID": e.bead.ID,
		"GC_CITY_PATH":  t.TempDir(),
	}}); err != nil {
		t.Fatalf("restart fake session: %v", err)
	}
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	e.sp.StopLeavesRunning = map[string]bool{e.name: true}

	e.finalize()

	if e.terminateCalls() != 0 {
		t.Error("force-terminated a runtime attributed to a different city")
	}
}

// NEGATIVE CONTROL. A non-pool row is outside this pass: closing it releases no
// pool name.
func TestEscalationSkipsNonPoolSeats(t *testing.T) {
	e := newEscalationEnv(t)
	e.setMeta(map[string]string{"pool_managed": "false"})

	e.finalize()

	if n := len(e.escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0", n)
	}
	if e.terminateCalls() != 0 {
		t.Error("force-terminated a non-pool row")
	}
}

// Control for the whole pass: a row whose runtime is already gone takes the
// ordinary finalize arm — closed, with nothing escalated.
func TestDeadRuntimeStillFinalizesWithoutEscalation(t *testing.T) {
	e := newEscalationEnv(t)
	e.sp.StopLeavesRunning = nil
	if err := e.sp.Stop(e.name); err != nil {
		t.Fatalf("stop fake session: %v", err)
	}

	e.finalize()

	if got := e.status(); got != "closed" {
		t.Errorf("session bead status = %q, want closed", got)
	}
	if n := len(e.escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0; this row needed no escalation", n)
	}
}

// MAJOR: an all-undeliverable budget must still earn its answer window. `failed`
// counts any sp.Nudge transport error (ssh/k8s exec, tmux send-keys), which is
// not evidence of an input-dead pane, and this gates a kill — so an undelivered
// reminder must not be treated more harshly than a refused one.
func TestUndeliverableReminderBudgetStillEarnsItsAnswerWindow(t *testing.T) {
	e := newEscalationEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
	deaf := &nudgeFailingProvider{Fake: e.sp}

	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		maybeRemindDrainingSession(deaf, e.store, e.info(), e.clk, e.out)
	}
	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	last := e.now.Add((drainReminderMaxAttempts - 1) * drainReminderInterval)
	if drainRemindersSpent(bead, last.Add(drainReminderInterval-time.Second)) {
		t.Error("an all-undeliverable budget authorized a kill with no answer window; a transport failure is not an input-dead pane")
	}
	if !drainRemindersSpent(bead, last.Add(drainReminderInterval)) {
		t.Error("an all-undeliverable budget never becomes spent even after its answer window")
	}
}
