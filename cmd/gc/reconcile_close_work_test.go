package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The close's work guard (L5), the confirmed orphan's release (SESS-082)
// and the stranded marker and repair (SESS-624/625), per the C5c1 close
// rulings.

// failingReads is a read store whose listings fail.
type failingReads struct{ beads.Store }

var errListFailed = errors.New("list failed")

func (failingReads) List(beads.ListQuery) ([]beads.Bead, error) { return nil, errListFailed }
func (failingReads) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, errListFailed
}

func (failingReads) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, errListFailed
}

// assignWork creates in-progress work assigned to the row's runtime name
// rt_x, routed to worker.
func assignWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "work", Type: "task", Assignee: "rt_x", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
	b, err = store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Kills a close that orphans work (SESS-061/081/623): L5 read live right
// before the close refuses has-work while work is assigned, or when the
// read fails; only SESS-080's phantom skips it, and its close keeps the
// identity's work.
func TestCloseWorkGuard(t *testing.T) {
	for _, c := range []struct {
		name    string
		work    bool
		failing bool
		phantom bool
		closes  bool
	}{
		{name: "no work", closes: true},
		{name: "work assigned", work: true},
		{name: "read fails", failing: true},
		{name: "phantom", work: true, phantom: true, closes: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, it, store := admittedClose(t, newCloseLeaf())
			var work beads.Bead
			if c.work {
				work = assignWork(t, store)
			}
			if c.failing {
				p.Reads.City = failingReads{p.Reads.City}
			}
			if c.phantom {
				it.Closing = closeSpec{Kind: closeReleasing, Phantom: true, Preserve: []string{"rt_x"}}
			}
			s := closeEffect(p, it)(context.Background())
			if closed := rowStatus(t, store, it.Key.ID) == "closed"; closed != c.closes || (!c.closes && s.Cause != causeHasWork) {
				t.Fatalf("settlement %+v, closed=%v; want closes=%v (or refused %q)", s, closed, c.closes, causeHasWork)
			}
			if c.phantom {
				if w, _ := store.Get(work.ID); w.Assignee != "rt_x" {
					t.Fatalf("phantom close released the identity's work: assignee %q", w.Assignee)
				}
			}
		})
	}
}

// Kills a confirmed orphan closed over work its release could not move:
// L5 is read again after the release, and work left (here unrouted, which
// the release skips) refuses the close.
func TestConfirmedOrphanRereadsWorkAfterRelease(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf())
	work := assignWork(t, store)
	stuck, err := store.Create(beads.Bead{Title: "unrouted", Type: "task", Assignee: "rt_x"})
	if err != nil {
		t.Fatal(err)
	}
	p.World.Demand.AssignedWork, p.Releasers.Assigned = []beads.Bead{work, stuck}, []beads.Store{store, store}
	it.Closing = closeSpec{Kind: closeReleasing, Orphaned: true}
	s := closeEffect(p, it)(context.Background())
	if rowStatus(t, store, it.Key.ID) != "open" || s.Cause != causeHasWork || len(s.Events) != 1 {
		t.Fatalf("settlement %+v; want the released bead's event, has-work and the row open", s)
	}
}

// Kills SESS-082 dropped or widened: a confirmed orphan whose work holds its
// close releases that work (one bead.dead_assignee_reopened each), reads L5
// again and closes; a suspended seat keeps its work and stays open.
func TestConfirmedOrphanReleasesThenCloses(t *testing.T) {
	for _, orphaned := range []bool{true, false} {
		p, it, store := admittedClose(t, newCloseLeaf())
		work := assignWork(t, store)
		p.World.Demand.AssignedWork, p.Releasers.Assigned = []beads.Bead{work}, []beads.Store{store}
		it.Closing = closeSpec{Kind: closeReleasing, Orphaned: orphaned}
		s := closeEffect(p, it)(context.Background())
		w, _ := store.Get(work.ID)
		closed := rowStatus(t, store, it.Key.ID) == "closed"
		switch {
		case orphaned && (!closed || w.Assignee != "" || len(s.Events) < 1 || s.Events[0].Type != events.BeadDeadAssigneeReopened || s.Events[0].Subject != work.ID):
			t.Fatalf("orphaned: settlement %+v, closed=%v, assignee %q; want the work released, its event, and the close", s, closed, w.Assignee)
		case !orphaned && (closed || w.Assignee != "rt_x" || s.Cause != causeHasWork || len(s.Events) != 0):
			t.Fatalf("suspended: settlement %+v, closed=%v, assignee %q; want the work kept and the row open", s, closed, w.Assignee)
		}
	}
}

// Kills a stranded pool slot closed with its work, marked more than once,
// or never repaired (SESS-624/625): the first close finding work stamps the
// marker and records session.stranded with legacy's payload; a second finds
// the marker and records nothing; the repair unclaims the work and closes
// stranded-repair, then prunes.
func TestStrandedPoolSlotMarksThenRepairs(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf(), "sleep_reason", "idle")
	work := assignWork(t, store)
	it.Closing = closeSpec{Kind: closePoolSlot, Template: "worker"}
	pruned := 0
	run := func(it intent) settlement {
		return closeRun{pass: p, it: it, now: time.Now, prune: func(session.Info, string, *config.City, io.Writer) { pruned++ }}.run(context.Background())
	}
	s := run(it)
	row, _ := store.Get(it.Key.ID)
	if s.Cause != causeStranded || len(s.Events) != 1 || row.Metadata[strandedEventEmittedKey] != gatherNow.Format(time.RFC3339) {
		t.Fatalf("first: settlement %+v, marker %q; want refused %q, one event, the marker stamped", s, row.Metadata[strandedEventEmittedKey], causeStranded)
	}
	legacy := strandedLegacyEvent(t, sessionRow("x", "template", "worker", "session_name", "rt_x", "state", "asleep", "generation", "3", "instance_token", "tok-x", "sleep_reason", "idle"))
	if got := s.Events[0]; got.Type != legacy.Type || got.Message != legacy.Message || string(got.Payload) != string(legacy.Payload) {
		t.Fatalf("session.stranded %+v, want legacy's %+v", got, legacy)
	}

	p, it2, _ := admittedCloseOn(t, store, it.Key.ID)
	it2.Closing = it.Closing
	if s := run(it2); s.Cause != causeHasWork || len(s.Events) != 0 {
		t.Fatalf("second: settlement %+v, want refused %q and no second event", s, causeHasWork)
	}
	it2.Closing.Repair = true
	if s := run(it2); s.Outcome != settledLanded || pruned != 1 {
		t.Fatalf("repair: settlement %+v pruned %d, want landed and one prune", s, pruned)
	}
	row, _ = store.Get(it.Key.ID)
	w, _ := store.Get(work.ID)
	if row.Status != "closed" || row.Metadata["state"] != strandedRepairCloseReason || w.Assignee != "" || w.Status != "open" {
		t.Fatalf("row %s/%q, work %s/%q; want closed stranded-repair and the work unclaimed", row.Status, row.Metadata["state"], w.Status, w.Assignee)
	}
}

// strandedLegacyEvent is the session.stranded legacy's diagnostic records
// for row b, holding one unit of work, at gatherNow.
func strandedLegacyEvent(t *testing.T, b beads.Bead) events.Event {
	t.Helper()
	store := stampedSQLite(t, gate.Require)
	b, err := store.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	assignWork(t, store)
	rec := events.NewFake()
	emitSessionStrandedDiagnostic("", nil, store, nil, sessionInfoFromBead(b), nil, "worker", rec, &clock.Fake{Time: gatherNow}, io.Discard)
	if len(rec.Events) != 1 {
		t.Fatalf("legacy recorded %v", rec.Events)
	}
	return rec.Events[0]
}

// admittedCloseOn is admittedClose's pass over store's row id, as the next
// pass reads it.
func admittedCloseOn(t *testing.T, store beads.Store, id string) (*effectPass, intent, beads.Store) {
	t.Helper()
	w := &World{
		Now: gatherNow, CityPath: t.Name(), Env: &reconcileEnv{SP: newCloseLeaf(), Cfg: &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}},
		Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), LegStores: map[string]beads.Store{rowLeg: store}, SessionsStore: store,
	}
	it := intent{Kind: intentClose, Key: rowKey{Leg: rowLeg, ID: id}, Patch: session.ClosePatch(gatherNow, "idle")}
	return newEffectPass(w, &allocDecision{}), it, store
}

// Kills a stranded repair that closes while its work is still assigned
// (SESS-625): an unclaim that fails on any leg refuses unclaim-failed and
// leaves the row open for the next try.
func TestStrandedRepairRefusesAFailedUnclaim(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf())
	assignWork(t, store)
	p.Releasers.City = failingReads{store}
	it.Closing = closeSpec{Kind: closePoolSlot, Repair: true}
	if s := closeEffect(p, it)(context.Background()); s.Cause != causeReleaseFailed || rowStatus(t, store, it.Key.ID) != "open" {
		t.Fatalf("settlement %+v; want refused %q with the row open", s, causeReleaseFailed)
	}
}

// Kills a stranded marker stamped on a row the close no longer stands on
// (v5.6 C3): the stamp's CAS requires a freeable pool slot whose lifecycle
// facts are the pass's, so a row that is not freeable (no sleep reason and
// no slept_at), or whose sleep reason moved, gets no marker and no event,
// and the close refuses stranded-unstamped.
func TestStrandedStampRequiresTheFreeableRowThePassRead(t *testing.T) {
	for _, c := range []struct {
		name  string
		meta  []string
		moved map[string]string
	}{
		{name: "not freeable"},
		{name: "lifecycle moved", meta: []string{"sleep_reason", "idle"}, moved: map[string]string{"sleep_reason": "idle-timeout"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, it, store := admittedClose(t, newCloseLeaf(), c.meta...)
			assignWork(t, store)
			if c.moved != nil {
				if err := store.SetMetadataBatch(it.Key.ID, c.moved); err != nil {
					t.Fatal(err)
				}
			}
			it.Closing = closeSpec{Kind: closePoolSlot, Template: "worker"}
			s := closeEffect(p, it)(context.Background())
			row, _ := store.Get(it.Key.ID)
			if s.Cause != causeUnstamped || len(s.Events) != 0 || row.Metadata[strandedEventEmittedKey] != "" {
				t.Fatalf("settlement %+v, marker %q; want refused %q, no event, no marker", s, row.Metadata[strandedEventEmittedKey], causeUnstamped)
			}
		})
	}
}

// Kills a marker that holds a slot with no work: a marked pool slot whose
// L5 finds no work closes with its sleep reason; the marker only gates the
// repair.
func TestMarkedPoolSlotWithNoWorkCloses(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf(), "sleep_reason", "idle", strandedEventEmittedKey, "yesterday")
	it.Closing = closeSpec{Kind: closePoolSlot, Template: "worker"}
	if s := closeEffect(p, it)(context.Background()); s.Outcome != settledLanded || rowStatus(t, store, it.Key.ID) != "closed" {
		t.Fatalf("settlement %+v, want the close landed", s)
	}
}

// Kills release events dropped by a close that then fails: a confirmed
// orphan's release lands, a wake then fails the close's premise, and the
// settlement still carries each bead.dead_assignee_reopened.
func TestReleaseEventsSurviveAFailedClose(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf())
	work := assignWork(t, store)
	p.World.Demand.AssignedWork, p.Releasers.Assigned = []beads.Bead{work}, []beads.Store{store}
	it.Closing = closeSpec{Kind: closeReleasing, Orphaned: true}
	if err := store.SetMetadataBatch(it.Key.ID, map[string]string{"state": "active"}); err != nil {
		t.Fatal(err)
	}
	s := closeEffect(p, it)(context.Background())
	if s.Cause != causeSuperseded || len(s.Events) != 1 || s.Events[0].Type != events.BeadDeadAssigneeReopened {
		t.Fatalf("settlement %+v, want refused %q carrying the release's event", s, causeSuperseded)
	}
}
