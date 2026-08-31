package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// An acknowledgement counts only for the incarnation that wrote it.
//
// Pool chairs are recycled under the same name and the pane environment is
// per-CHAIR state, so a previous incarnation's `gc runtime drain-ack` leaves
// GC_DRAIN_ACK_SOURCE=agent sitting there for whoever sits down next. Read
// unbound, that residue reads as "the agent already answered" about an agent
// that no longer exists — and since a skip writes nothing, it says so in
// silence, leaving no reminder line and no marker while the chair stays wedged.
//
// These pin the binding in both directions, plus the arm that cannot tell.

// ackBindingUnreadableProvider fails exactly one metadata read, so the tests can
// separate "the binding says stale" from "the binding could not be read".
type ackBindingUnreadableProvider struct {
	*runtime.Fake
	key string
}

func (p *ackBindingUnreadableProvider) GetMeta(name, key string) (string, error) {
	if key == p.key {
		return "", errors.New("provider metadata unreadable")
	}
	return p.Fake.GetMeta(name, key)
}

// THE PIN: a prior incarnation's acknowledgement must not suppress this drain's
// reminder. The fixture row's token is "tok-a"; the ack names somebody else.
func TestDrainReminderFiresThroughPriorIncarnationAckResidue(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	mustSetMeta(t, e.sp, e.name, drainAckRequesterInstanceTokenKey, "stale-prior-incarnation-token")

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered: residue from a dead incarnation is not an answer", got)
	}
	if got := len(e.nudges()); got != 1 {
		t.Errorf("nudges = %d, want 1", got)
	}
}

// Control: a GENUINE acknowledgement by the CURRENT incarnation still skips, and
// still writes nothing. Without this the pin above would pass for a reminder
// that simply ignores every agent ack.
func TestDrainReminderStillSkipsCurrentIncarnationAck(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	mustSetMeta(t, e.sp, e.name, drainAckRequesterInstanceTokenKey, "tok-a")
	before := e.beadSnapshot()

	if got := e.remind(); got != drainReminderSkipped {
		t.Fatalf("outcome = %v, want skipped: this incarnation's own acknowledgement stands", got)
	}
	if got := len(e.nudges()); got != 0 {
		t.Errorf("nudges = %d, want 0", got)
	}
	e.assertBeadUnchanged(before)
}

// The arm that cannot tell. `gc runtime drain-ack` stamps the requester from the
// pane's own GC_INSTANCE_TOKEN, so an adopted pane whose environment did not
// survive a restart acknowledges with an empty one. That is neither proven
// current nor proven residue. The reminder keeps asking: it is informational,
// and a redundant nudge is noise against a suppressed one costing a chair for
// hours.
func TestDrainReminderRemindsWhenAckCarriesNoIncarnation(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)

	if got := e.remind(); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered: an unbindable ack is not proof this incarnation answered", got)
	}
}

// An UNREADABLE binding is different from an absent one: it holds, and unlike
// the old silent skip it leaves a breadcrumb. The silent decline is what hid
// this bug.
func TestDrainReminderHoldsWithBreadcrumbWhenAckBindingUnreadable(t *testing.T) {
	e := newDrainReminderEnv(t)
	mustSetMeta(t, e.sp, e.name, reconcilerDrainAckSourceKey, drainAckSourceAgentValue)
	sp := &ackBindingUnreadableProvider{Fake: e.sp, key: drainAckRequesterInstanceTokenKey}

	if got := e.remindWith(sp); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held: an unreadable binding is not evidence the ack is stale", got)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldAckUnknown {
		t.Fatalf("hold breadcrumb = %q, want %q", got, drainReminderHoldAckUnknown)
	}
}

// The acknowledging agent records which incarnation it was, so the readers above
// have something to bind against.
func TestSetDrainAckStampsTheAcknowledgingIncarnation(t *testing.T) {
	t.Setenv("GC_INSTANCE_TOKEN", "tok-a")
	sp := runtime.NewFake()
	ops := &providerDrainOps{sp: sp}

	if err := ops.setDrainAck("gc-city-worker-1"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	if got, _ := sp.GetMeta("gc-city-worker-1", drainAckRequesterInstanceTokenKey); got != "tok-a" {
		t.Errorf("%s = %q, want %q", drainAckRequesterInstanceTokenKey, got, "tok-a")
	}
	if got, _ := sp.GetMeta("gc-city-worker-1", reconcilerDrainAckSourceKey); got != drainAckSourceAgentValue {
		t.Errorf("ack source = %q, want %q", got, drainAckSourceAgentValue)
	}
}
