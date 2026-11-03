package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-vixyn5.2: the reconfigured-named gate declines to close a named session
// bead that holds assigned work. The decline is a typed event, once per
// episode: nothing on the ticks that follow while the bead stays blocked, and
// nothing for a bead that closes cleanly. The fixtures are ga-k88yuh's
// (session_reconfigured_pool_carveout_test.go): an adopted canonical-singleton
// pool bead whose session_name is not the name the named spec reserves.

// namedBlockedSync runs one sync tick of store with a SeatWork that carries
// the controller's refusal log and rec, as CityRuntime.newSeatWork wires them;
// log is the tick's stderr.
func namedBlockedSync(t *testing.T, store beads.Store, cfg *config.City, sp runtime.Provider, ds map[string]TemplateParams, refusals *closeRefusalLog, rec events.Recorder, log io.Writer) {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 9, 24, 20, 30, 0, 0, time.UTC)}
	sw := testSeatWork("", cfg, store, nil)
	sw.refusals = refusals
	sw.rec = rec
	syncSessionBeadsWithSnapshotAndRigStores(
		"", beads.SessionStore{Store: store}, sw, ds, sp, allConfiguredDS(ds), cfg, clk, log, false, nil, nil,
	)
}

func namedBlockedEvents(t *testing.T, fake *events.Fake) []events.Event {
	t.Helper()
	got, err := fake.List(events.Filter{Type: events.SessionNamedReconfiguredBlocked})
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	return got
}

func TestNamedReconfiguredBlocked_EmitsOnceOnFirstDecline(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)
	fake := events.NewFake()

	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), &closeRefusalLog{}, fake, io.Discard)

	got := namedBlockedEvents(t, fake)
	if len(got) != 1 {
		t.Fatalf("session.named_reconfigured_blocked events = %d, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Subject != k88Identity {
		t.Errorf("Subject = %q, want the named identity %q", e.Subject, k88Identity)
	}
	if e.SessionID != b.ID {
		t.Errorf("SessionID = %q, want the bead %q", e.SessionID, b.ID)
	}
	if e.Actor != "gc" {
		t.Errorf("Actor = %q, want gc", e.Actor)
	}
}

func TestNamedReconfiguredBlocked_NoReemitWhileStillBlocked(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)
	fake := events.NewFake()
	refusals := &closeRefusalLog{}

	for tick := 1; tick <= 3; tick++ {
		namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), refusals, fake, io.Discard)
		if n := len(namedBlockedEvents(t, fake)); n != 1 {
			t.Fatalf("after tick %d: %d events, want exactly 1 (transition only, not per tick)", tick, n)
		}
	}
}

// The episode is the controller's refusal log, not the stderr writer: a tick
// that logs to a real writer and one that discards share one episode.
func TestNamedReconfiguredBlocked_EpisodeIsIndependentOfStderr(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)
	fake := events.NewFake()
	refusals := &closeRefusalLog{}

	var buf bytes.Buffer
	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), refusals, fake, io.Discard)
	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), refusals, fake, &buf)
	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), refusals, fake, io.Discard)

	if n := len(namedBlockedEvents(t, fake)); n != 1 {
		t.Fatalf("events = %d, want 1 across a discarded, a logged and a discarded tick", n)
	}
}

func TestNamedReconfiguredBlocked_CleanCloseEmitsNothing(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	b := k88SeedSessionBead(t, store, k88PoolName, true, nil)
	fake := events.NewFake()

	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), &closeRefusalLog{}, fake, io.Discard)

	if got, _ := store.Get(b.ID); got.Status != "closed" {
		t.Fatalf("precondition: bead status=%q, want closed (no assigned work)", got.Status)
	}
	if got := namedBlockedEvents(t, fake); len(got) != 0 {
		t.Fatalf("clean close emitted %d session.named_reconfigured_blocked events: %+v", len(got), got)
	}
}

// A tick without a refusal log (a one-shot command) has no episode to
// dedup against, so it emits nothing rather than once per call.
func TestNamedReconfiguredBlocked_NilRefusalLogEmitsNothing(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	sp := runtime.NewFake()
	k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)
	fake := events.NewFake()

	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), nil, fake, io.Discard)
	namedBlockedSync(t, store, cfg, sp, k88NamedDesired(k88PoolName), nil, fake, io.Discard)

	if got := namedBlockedEvents(t, fake); len(got) != 0 {
		t.Fatalf("nil refusal log emitted %d events, want 0: %+v", len(got), got)
	}
}

// Only the reconfigured-named gate raises it: a different close refused for
// the same cause (the has-work drain) is not this event.
func TestNamedReconfiguredBlocked_OtherCloseReasonsDoNotEmit(t *testing.T) {
	store := beads.NewMemStore()
	cfg := k88Config()
	b := k88SeedSessionBead(t, store, k88PoolName, true, nil)
	k88SeedAssignedWork(t, store)
	fake := events.NewFake()

	sw := testSeatWork("", cfg, store, nil)
	sw.refusals = &closeRefusalLog{}
	sw.rec = fake
	now := time.Date(2026, 9, 24, 20, 30, 0, 0, time.UTC)
	info := sessionInfoFromBead(b)
	if closeSeat(store, sw, info, closeWhy{reason: "idle", gate: closeGated}, now, io.Discard) {
		t.Fatal("precondition: close of a seat holding assigned work succeeded")
	}

	if got := namedBlockedEvents(t, fake); len(got) != 0 {
		t.Fatalf("a non-reconfigured refusal emitted %d events, want 0: %+v", len(got), got)
	}
}
