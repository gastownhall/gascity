package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A21's closes (CONTRACT v5.6 C3, A21; the C5c1 close rulings).

// closeFacts is decideRow's facts for one row of b under entry e, with the
// inventory's observation o.
func closeFacts(b beads.Bead, e selectionEntry, o rowObservation) *rowFacts {
	k := rowKey{Leg: rowLeg, ID: b.ID}
	w := &World{
		Now: gatherNow, Env: &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}},
		Observed: map[rowKey]rowObservation{k: o},
	}
	e.Liveness = o.Liveness
	return &rowFacts{w: w, k: k, row: censusRow{Key: k, Info: sessionInfoFromBead(b)}, found: true, entry: &e}
}

var (
	failedCreateEntry = selectionEntry{Desired: desireNone, Reason: reasonFailedCreate}
	poolSleepEntry    = selectionEntry{Desired: desireSleep, Reason: reasonNoWake, Template: "worker"}
	orphanEntry       = selectionEntry{Desired: desireDrain, Reason: reasonNoWake, DrainReason: drainOrphaned}
)

func failedCreateRow() beads.Bead {
	return sessionRow("f", "template", "worker", "session_name", "s-f", "state", "failed-create")
}

func idlePoolRow(meta ...string) beads.Bead {
	return poolRow("p", "worker", 1, "asleep", append([]string{"sleep_reason", "idle"}, meta...)...)
}

func orphanRow(state string) beads.Bead {
	return sessionRow("o", "template", "gone-agent", "session_name", "s-o", "state", state)
}

// Kills a close on a runtime that may still be live, or that the row still
// claims: each close fires on a gone runtime and on a corpse of a row that
// no longer claims a live one, and never on a zombie, a corpse the row
// claims (C5c2's MAINT-031), an occupied, alive or unknown row, on Keep, on
// an undesired pending create (A10's rollback), or on a partial pass (P-3).
func TestCloseArmsFireOnlyOnClosableRuntime(t *testing.T) {
	gone := rowObservation{Liveness: livenessGone}
	corpse := rowObservation{Liveness: livenessDead, Corpse: true}
	zombie := rowObservation{Liveness: livenessDead}
	for _, c := range []struct {
		name    string
		row     beads.Bead
		entry   selectionEntry
		obs     rowObservation
		partial bool
		want    closeKind
		closes  bool
	}{
		{name: "failed create, gone", row: failedCreateRow(), entry: failedCreateEntry, obs: gone, want: closeFailedCreate, closes: true},
		{name: "failed create, corpse", row: failedCreateRow(), entry: failedCreateEntry, obs: corpse, want: closeFailedCreate, closes: true},
		{name: "failed create, partial", row: failedCreateRow(), entry: failedCreateEntry, obs: gone, partial: true},
		{name: "pool slot, gone", row: idlePoolRow(), entry: poolSleepEntry, obs: gone, want: closePoolSlot, closes: true},
		{name: "pool slot, zombie", row: idlePoolRow(), entry: poolSleepEntry, obs: zombie},
		{name: "pool slot, partial", row: idlePoolRow(), entry: poolSleepEntry, obs: gone, partial: true},
		{name: "pool slot, user-hold", row: poolRow("p", "worker", 1, "asleep", "sleep_reason", "user-hold"), entry: poolSleepEntry, obs: gone},
		{name: "orphan, gone", row: orphanRow("asleep"), entry: orphanEntry, obs: gone, want: closeReleasing, closes: true},
		{name: "orphan, corpse", row: orphanRow("asleep"), entry: orphanEntry, obs: corpse, want: closeReleasing, closes: true},
		{name: "orphan, corpse it claims live", row: orphanRow("active"), entry: orphanEntry, obs: corpse},
		{name: "orphan, zombie", row: orphanRow("asleep"), entry: orphanEntry, obs: zombie},
		{name: "orphan, alive", row: orphanRow("asleep"), entry: orphanEntry, obs: rowObservation{Liveness: livenessAlive}},
		{name: "orphan, occupied", row: orphanRow("asleep"), entry: orphanEntry, obs: rowObservation{Liveness: livenessOccupied}},
		{name: "orphan, unknown", row: orphanRow("asleep"), entry: orphanEntry, obs: rowObservation{}},
		{name: "orphan, Keep", row: orphanRow("asleep"), entry: selectionEntry{Desired: desireKeep, Reason: reasonPartialRetain}, obs: gone},
		{name: "orphan, pending create", row: sessionRow("o", "template", "gone-agent", "session_name", "s-o", "state", "creating", "pending_create_claim", "true"), entry: orphanEntry, obs: gone},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := closeFacts(c.row, c.entry, c.obs)
			r.partial = c.partial
			it, ok := armClose(r)
			if closes := ok && it.Kind == intentClose; closes != c.closes || (closes && it.Closing.Kind != c.want) {
				t.Fatalf("armClose = %+v ok=%v, want closes=%v kind %d", it, ok, c.closes, c.want)
			}
		})
	}
}

// Kills a close that drops legacy's close reasons: failed-create's terminal
// patch, a pool slot's sleep reason (drained when empty), and an orphan's
// drain reason, with a confirmed orphan's release armed only for orphaned
// (a suspended seat keeps its work for its return, SESS-082).
func TestCloseArmsCarryLegacysReasons(t *testing.T) {
	gone := rowObservation{Liveness: livenessGone}
	for _, c := range []struct {
		name     string
		row      beads.Bead
		entry    selectionEntry
		state    string
		orphaned bool
	}{
		{"failed create", failedCreateRow(), failedCreateEntry, "failed-create", false},
		{"idle pool slot", idlePoolRow(), poolSleepEntry, "idle", false},
		{"drained pool slot", poolRow("p", "worker", 1, "drained"), poolSleepEntry, "drained", false},
		{"orphaned", orphanRow("asleep"), orphanEntry, "orphaned", true},
		{"suspended", orphanRow("asleep"), selectionEntry{Desired: desireDrain, DrainReason: drainSuspended}, "suspended", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			it, _ := armClose(closeFacts(c.row, c.entry, gone))
			if it.Patch["state"] != c.state || it.Closing.Orphaned != c.orphaned {
				t.Fatalf("close %+v, want state %q orphaned=%v", it, c.state, c.orphaned)
			}
		})
	}
}

// Kills a stranded repair that fires before its confirmation window, or a
// marker that pins its row (v5.6 C3): no marker, or an unparseable one,
// proposes the plain close (whose L5 decides); a fresh marker holds until it
// is 2 minutes old, with that deadline; so does one up to 2 minutes in the
// future (skew); an aged marker, or one further in the future, proposes the
// repair.
func TestStrandedMarkerHoldsThenRepairs(t *testing.T) {
	gone := rowObservation{Liveness: livenessGone}
	for _, c := range []struct {
		name         string
		marker       string
		hold, repair bool
		next         time.Time
	}{
		{name: "no marker"},
		{name: "fresh", marker: rowAt(-time.Minute), hold: true, next: gatherNow.Add(time.Minute)},
		{name: "aged", marker: rowAt(-2 * time.Minute), repair: true},
		{name: "skew band", marker: rowAt(time.Minute), hold: true, next: gatherNow.Add(3 * time.Minute)},
		{name: "skew band edge", marker: rowAt(2 * time.Minute), hold: true, next: gatherNow.Add(4 * time.Minute)},
		{name: "far future", marker: rowAt(2*time.Minute + time.Second), repair: true},
		{name: "unparseable", marker: "yesterday"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := closeFacts(idlePoolRow(strandedEventEmittedKey, c.marker), poolSleepEntry, gone)
			it, ok := armClose(r)
			switch {
			case !ok:
				t.Fatal("the pool-slot arm did not decide")
			case c.hold && (it.Kind != "" || it.Reason != decideStrandedHold || !r.next.Equal(c.next)):
				t.Fatalf("%+v next %v, want a stranded hold until %v", it, r.next, c.next)
			case !c.hold && (it.Kind != intentClose || it.Closing.Repair != c.repair):
				t.Fatalf("%+v, want a pool-slot close with repair=%v", it, c.repair)
			}
		})
	}
}

// Kills a claimless failed-create row read as a rollback candidate (ruling
// Q3; A10 itself is C5b's): the allocation reads it None(failed-create), and
// decideRow proposes A21's failed-create close for it. The inventory's corpse
// fact reaches the arm: a dead pane is a corpse, a dead agent under a live
// pane is not.
func TestClaimlessFailedCreateRowGoesToA21(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "failed-create", "pending_create_started_at", allocNow.Add(-time.Hour).Format(time.RFC3339)),
		poolRow("gc-2", "worker", 2, "asleep", "sleep_reason", "idle"),
		poolRow("gc-3", "worker", 3, "asleep", "sleep_reason", "idle"),
	).corpse("s-gc-2").alive("s-gc-3", InventoryAttrs{ProcessProbed: true, ProcessKnown: true, ProcessAlive: false})
	in := f.inputs()
	a := mustDecide(t, in)
	w := &World{Now: in.Now, Env: &reconcileEnv{Cfg: cfg}, Census: in.Census, Mislabelled: map[rowKey]bool{}, Observed: observeCensus(in.Obs, in.Census, in.Now, in.ObsMaxAge)}
	k := func(id string) rowKey { return rowKey{Leg: allocSessionsLeg, ID: id} }
	if e := a.Snapshot.Entries[k("gc-1")]; e.Desired != desireNone || e.Reason != reasonFailedCreate {
		t.Fatalf("claimless failed-create row = %s/%s, want None(failed-create)", e.Desired, e.Reason)
	}
	if it, _ := decideRow(w, &a, k("gc-1")); it.Kind != intentClose || it.Closing.Kind != closeFailedCreate {
		t.Fatalf("decideRow = %+v, want A21's failed-create close", it)
	}
	if o := w.Observed[k("gc-2")]; o.Liveness != livenessDead || !o.Corpse {
		t.Fatalf("a dead pane observes %+v, want a dead corpse", o)
	}
	if o := w.Observed[k("gc-3")]; o.Liveness != livenessDead || o.Corpse {
		t.Fatalf("a dead agent under a live pane observes %+v, want dead, not a corpse", o)
	}
}

// Kills a phantom close that releases the configured identity's work or
// reads L5 on it (SESS-080): a dead row squatting a configured named
// identity is closed as a phantom, keeping that identity's assignees.
func TestOrphanArmMarksTheNamedPhantom(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "chat"}},
		NamedSessions: []config.NamedSession{{Template: "chat", Mode: "always"}},
	}
	runtimeName := config.NamedSessionRuntimeName("city", cfg.Workspace, "chat")
	row := sessionRow("ph", "template", "chat", "state", "asleep", "session_name", runtimeName)
	r := closeFacts(row, orphanEntry, rowObservation{Liveness: livenessGone})
	r.w.Env.Cfg, r.w.CityName = cfg, "city"
	if _, ok := session.RecyclableDeadConfiguredNamePhantomInfo(r.row.Info, cfg, "city"); !ok {
		t.Fatal("the fixture must be a phantom of chat")
	}
	it, _ := armClose(r)
	if !it.Closing.Phantom || len(it.Closing.Preserve) == 0 || it.Closing.Preserve[0] != "chat" {
		t.Fatalf("close %+v, want a phantom keeping the identity chat", it.Closing)
	}
}
