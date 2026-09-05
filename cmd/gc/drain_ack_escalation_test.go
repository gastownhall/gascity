package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The seven-seat specimen. Seven live seats sat in
// state=draining/state_reason=drain-ack-stop-pending for two days with
// drain_at from 2026-09-03. Their agents acked correctly (pane env
// GC_DRAIN_ACK=1, source=agent, requester token == GC_INSTANCE_TOKEN ==
// GC_DRAIN_TOKEN) and `gc hook --claim` correctly returned drain, so each agent
// printed NO_ROUTED_WORK, acked, and ended its TURN — but not its RUNTIME.
// finalizeDrainAckStopPendingSessions only closes a row whose runtime observes
// DEAD; while obs.Running || obs.Alive it reminds and re-queues forever, and its
// own comment concedes "the only thing that has ever cleared such a row is an
// operator killing the pane". Each stuck row holds its pool slot name, so
// buildDesiredState reports "pool session name unavailable" and the pool cannot
// mint a replacement.
//
// These tests pin the terminal exit: once the reminder budget is spent and its
// answer window has elapsed, the row escalates to a certified kill and CLOSES,
// because only status=closed releases the name (an OPEN bead owns its
// session_name whatever its state).

// escalationEnv is the wedged seat plus the config/city-path the finalizer needs
// to resolve a worker handle for the certified kill.
type escalationEnv struct {
	*drainReminderEnv
	cfg      *config.City
	cityPath string
	rec      *capturingRecorder
}

func newEscalationEnv(t *testing.T) *escalationEnv {
	t.Helper()
	e := newDrainReminderEnv(t)
	e.setMeta(map[string]string{"work_dir": t.TempDir(), "provider": "claude"})
	return &escalationEnv{
		drainReminderEnv: e,
		cfg:              &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}},
		cityPath:         t.TempDir(),
		rec:              &capturingRecorder{},
	}
}

// spendBudget delivers the full reminder budget and then advances the clock past
// the answer window the last delivered reminder earns, which is exactly the
// durable precondition drainRemindersSpent reports.
func (e *escalationEnv) spendBudget() {
	e.t.Helper()
	for i := 0; i < drainReminderMaxAttempts; i++ {
		e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
		if got := e.remind(); got != drainReminderDelivered {
			e.t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
		}
	}
	e.clk.Time = e.now.Add(drainReminderMaxAttempts * drainReminderInterval)
	bead, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	if !drainRemindersSpent(bead, e.clk.Time) {
		e.t.Fatalf("budget not spent after %d delivered reminders and a full interval; the escalation precondition is not set up", drainReminderMaxAttempts)
	}
}

func (e *escalationEnv) finalize() int {
	e.t.Helper()
	return finalizeDrainAckStopPendingSessions(
		e.cityPath, e.cfg, e.sp, beads.SessionStore{Store: e.store}, nil,
		[]sessionpkg.Info{e.info()}, nil, newDrainTracker(), &asyncStartTracker{},
		e.clk, e.rec, e.out,
	)
}

func (e *escalationEnv) status() string {
	e.t.Helper()
	got, err := e.store.Get(e.bead.ID)
	if err != nil {
		e.t.Fatalf("read session bead: %v", err)
	}
	return got.Status
}

func (e *escalationEnv) escalationEvents() []events.Event {
	var out []events.Event
	for _, ev := range e.rec.events {
		if ev.Type == events.SessionDrainStopEscalated {
			out = append(out, ev)
		}
	}
	return out
}

// THE PIN THAT FAILS TODAY. A drain-ack stop-pending seat whose agent acked but
// whose runtime stays alive must reach a terminal CLOSED state with its name
// released. Today the finalizer loops forever on this row.
func TestFinalizeDrainAckStopPendingEscalatesWedgedSeatToClosed(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()

	if !e.sp.IsRunning(e.name) {
		t.Fatal("specimen is not a live wedge; the runtime must be alive for this pin to mean anything")
	}

	if got := e.finalize(); got != 1 {
		t.Errorf("finalized = %d, want 1", got)
	}

	// Only status=closed frees the pool slot name: an OPEN bead owns its
	// session_name whatever its state, so state=drained/suspended/archived would
	// all keep the pool blocked.
	if got := e.status(); got != "closed" {
		t.Errorf("session bead status = %q, want %q — the pool slot name stays held until the bead closes", got, "closed")
	}
	if e.sp.IsRunning(e.name) {
		t.Errorf("runtime session %q still running; the escalation must certify the kill before closing", e.name)
	}
	// The name is released precisely because the bead is no longer an open
	// occupancy holder.
	if !e.info().Closed {
		t.Errorf("session Info still reads open; buildDesiredState would keep reporting %q", errPoolSessionNameUnavailable)
	}
}

// Observability: a terminal escalation is a kill the operator did not ask for,
// so it must never be silent — otherwise it masks a genuine drain-ack tail.
func TestDrainAckEscalationEmitsCountedTypedEvent(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()

	e.finalize()

	evs := e.escalationEvents()
	if len(evs) != 1 {
		t.Fatalf("escalation events = %d, want 1", len(evs))
	}
	if evs[0].SessionID != e.bead.ID {
		t.Errorf("event SessionID = %q, want %q", evs[0].SessionID, e.bead.ID)
	}
	if evs[0].Payload == nil {
		t.Error("escalation event carries no typed payload")
	}
}

// NEGATIVE CONTROL (the class that bit the 7g review). A seat that still holds
// work claimed under its ALIAS must never be killed or closed. The narrow
// identifier set ({ID, session_name, configured_named_identity}) cannot see an
// alias claim; the probe must use session.AssigneeIdentities, which carries
// alias + alias_history.
func TestDrainAckEscalationNeverKillsSeatHoldingAliasClaimedWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		assignee string
	}{
		{
			name:     "work claimed under the current alias",
			metadata: map[string]string{"alias": "nux"},
			assignee: "nux",
		},
		{
			name:     "work claimed under a prior alias in alias_history",
			metadata: map[string]string{"alias": "nux", "alias_history": "slit,morsov"},
			assignee: "morsov",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEscalationEnv(t)
			e.setMeta(tc.metadata)
			e.spendBudget()

			// The live agent's claim: in_progress work owned under the alias.
			if _, err := e.store.Create(beads.Bead{
				Title:    "alias-claimed work",
				Status:   "in_progress",
				Assignee: tc.assignee,
			}); err != nil {
				t.Fatalf("create work bead: %v", err)
			}

			e.finalize()

			if !e.sp.IsRunning(e.name) {
				t.Errorf("runtime %q was killed while the agent still held work claimed as %q — "+
					"the assigned-work probe is blind to the alias identity class", e.name, tc.assignee)
			}
			if got := e.status(); got == "closed" {
				t.Errorf("session bead closed over a live agent that still owns work claimed as %q (7g §3.5)", tc.assignee)
			}
			if n := len(e.escalationEvents()); n != 0 {
				t.Errorf("escalation events = %d, want 0", n)
			}
		})
	}
}

// NEGATIVE CONTROL. A seat inside the bound is untouched: the budget is not yet
// spent, so the row keeps its existing remind-and-requeue behavior.
func TestDrainAckEscalationLeavesInsideBoundSeatUntouched(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*escalationEnv)
	}{
		{
			name:  "no reminders delivered yet",
			setup: func(*escalationEnv) {},
		},
		{
			name: "budget spent but the answer window has not elapsed",
			setup: func(e *escalationEnv) {
				for i := 0; i < drainReminderMaxAttempts; i++ {
					e.clk.Time = e.now.Add(time.Duration(i) * drainReminderInterval)
					if got := e.remind(); got != drainReminderDelivered {
						e.t.Fatalf("attempt %d outcome = %v, want delivered", i+1, got)
					}
				}
				e.clk.Time = e.now.Add(drainReminderMaxAttempts*drainReminderInterval - time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEscalationEnv(t)
			tc.setup(e)

			e.finalize()

			if !e.sp.IsRunning(e.name) {
				t.Errorf("runtime %q killed before the bound elapsed", e.name)
			}
			if got := e.status(); got == "closed" {
				t.Error("session bead closed before the bound elapsed")
			}
			if n := len(e.escalationEvents()); n != 0 {
				t.Errorf("escalation events = %d, want 0", n)
			}
		})
	}
}

// NEGATIVE CONTROL. The token fence: if the runtime name has been taken over by
// a re-woken replacement carrying a different GC_INSTANCE_TOKEN, the escalation
// must not kill it. The row we meant to stop is already gone.
func TestDrainAckEscalationHonorsTheTokenFence(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()
	mustSetMeta(t, e.sp, e.name, "GC_INSTANCE_TOKEN", "tok-replacement")

	e.finalize()

	if !e.sp.IsRunning(e.name) {
		t.Errorf("runtime %q killed despite a definite instance-token mismatch — that is a live replacement", e.name)
	}
	if got := e.status(); got == "closed" {
		t.Error("session bead closed over a name now owned by a live replacement")
	}
	if n := len(e.escalationEvents()); n != 0 {
		t.Errorf("escalation events = %d, want 0", n)
	}
}

// NEGATIVE CONTROL. A non-pool row is outside this pass entirely: closing it
// releases no pool name, and the reminder budget it would need is never spent on
// one (drainReminderEligible requires a pool seat).
func TestDrainAckEscalationSkipsNonPoolSeats(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()
	e.setMeta(map[string]string{"pool_managed": "false"})

	e.finalize()

	if !e.sp.IsRunning(e.name) {
		t.Errorf("runtime %q killed on a non-pool row", e.name)
	}
	if n := len(e.escalationEvents()); n != 0 {
		t.Errorf("escalation events = %d, want 0", n)
	}
}

// NEGATIVE CONTROL. A survivor that outlives the kill is not certified dead, so
// the bead must stay open — closing it would free the name while the agent
// still runs, which is the exact failure the confirm-dead contract exists to
// prevent.
func TestDrainAckEscalationDoesNotCloseAnUncertifiedKill(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()
	e.sp.StopLeavesRunning = map[string]bool{e.name: true}

	prevTimeout, prevPoll := drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll
	drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = prevTimeout, prevPoll
	})

	e.finalize()

	if got := e.status(); got == "closed" {
		t.Error("session bead closed while its runtime survived the kill — the slot would be freed under a live agent")
	}
	if n := len(e.escalationEvents()); n != 0 {
		t.Errorf("escalation events = %d, want 0; nothing was escalated to a terminal state", n)
	}
}

// Control for the whole pass: a row whose runtime is already gone still takes
// the ordinary finalize arm, with no kill and no escalation event.
func TestDrainAckStopPendingDeadRuntimeStillFinalizesWithoutEscalation(t *testing.T) {
	e := newEscalationEnv(t)
	e.spendBudget()
	if err := e.sp.Stop(e.name); err != nil {
		t.Fatalf("stop fake session: %v", err)
	}

	if got := e.finalize(); got != 1 {
		t.Errorf("finalized = %d, want 1", got)
	}
	if got := e.status(); got != "closed" {
		t.Errorf("session bead status = %q, want closed", got)
	}
	if n := len(e.escalationEvents()); n != 0 {
		t.Errorf("escalation events = %d, want 0; this row needed no escalation", n)
	}
}
