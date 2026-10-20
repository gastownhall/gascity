package main

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

// stallControllerSocket listens on cityDir's controller socket and answers
// no command within the doorbell's legs: a controller too busy to reply.
func stallControllerSocket(t *testing.T, cityDir string) {
	t.Helper()
	released := make(chan struct{})
	startFakeUnixSocket(t, controllerSocketPath(cityDir), func(string) string {
		<-released
		return ""
	})
	t.Cleanup(func() { close(released) })
}

// laneRowRing is one journal-row doorbell the drain rang, with what the
// drain had done by then.
type laneRowRing struct {
	conv     string
	ids      []string
	inFlight int
	pending  int
	stamped  bool
}

// answerControllerSocket listens on cityDir's controller socket and answers
// every command with reply.
func answerControllerSocket(reply string) func(t *testing.T, cityDir string) {
	return func(t *testing.T, cityDir string) {
		t.Helper()
		startFakeUnixSocket(t, controllerSocketPath(cityDir), func(string) string { return reply + "\n" })
	}
}

// In production the in-band pass's sessions store is the one-shot CLI
// emitter (openHookLaneSessionStore), whose journal-row writes ring the
// controller. Inside the drain those rings wait for its last doorbell: the
// take and the Done ring nothing mid-pass, and the rows ring once, after the
// hook output, the legacy ack and the delivery stamp, so a controller that
// answers slowly costs the drain none of the time its own bookkeeping needs.
// A row ring the controller answers poked the conversation, so no plain
// doorbell follows it; one no controller answered is followed by the plain
// doorbell after the Done, and only a controller that took the ring without
// answering it is warned of.
func TestNudgeDrainInjectLaneEmitterRowsRingLast(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controller func(t *testing.T, cityDir string)
		wantPokes  int
		wantWarned bool
	}{
		{name: "controller answers", controller: answerControllerSocket(laneReplyOK)},
		{name: "controller answers slowly", controller: stallControllerSocket, wantPokes: 1, wantWarned: true},
		{name: "no controller", wantPokes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			f.queueNudge(t)
			op := f.propose(t, "mail arrived")
			emitting := &emittingClassStore{Store: f.sessions, cityPath: f.cityDir}
			prevStore := hookLaneSessionStore
			hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store { return emitting }
			t.Cleanup(func() { hookLaneSessionStore = prevStore })
			if tc.controller != nil {
				tc.controller(t, f.cityDir)
			}
			var warned []string
			prevWarn := classStoreEmitWarn
			classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
			t.Cleanup(func() { classStoreEmitWarn = prevWarn })
			var rings []laneRowRing
			prevRing := classStoreJournalRing
			classStoreJournalRing = func(city, conv string, ids []string) bool {
				st, _ := nudgequeue.LoadState(f.cityDir)
				stamped := false
				// The drain stamps a delivery that carried legacy nudges on
				// the city's store, after their ack.
				if store, err := openCityStoreAt(f.cityDir); err == nil {
					if b, err := store.Get(f.sessionID); err == nil {
						stamped = b.Metadata[session.MetadataLastNudgeDeliveredAt] != ""
					}
				}
				rings = append(rings, laneRowRing{conv: conv, ids: slices.Clone(ids), inFlight: len(st.InFlight), pending: len(st.Pending), stamped: stamped})
				return prevRing(city, conv, ids)
			}
			t.Cleanup(func() { classStoreJournalRing = prevRing })
			prevStart := hookProcessStart
			hookProcessStart = time.Now().Add(-100 * time.Hour) // the archive margin is its cap
			t.Cleanup(func() { hookProcessStart = prevStart })
			t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(hookDeadlineArchiveMarginMax+800*time.Millisecond)))

			run := f.drain(t)
			if !strings.Contains(run.stdout, "review queued work") || !strings.Contains(run.stdout, "mail arrived") {
				t.Fatalf("combined output missing a part:\n%s\nstderr=%s", run.stdout, run.stderr)
			}
			state, err := nudgequeue.LoadState(f.cityDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Pending)+len(state.InFlight) != 0 {
				t.Fatalf("the legacy nudge was not acked (pending %d, in flight %d); stderr=%s", len(state.Pending), len(state.InFlight), run.stderr)
			}
			if len(rings) != 1 || rings[0].conv != f.sessionID || !slices.Contains(rings[0].ids, op.ID) {
				t.Fatalf("row rings = %+v, want one for %s naming %s", rings, f.sessionID, op.ID)
			}
			if r := rings[0]; r.inFlight+r.pending != 0 || !r.stamped {
				t.Fatalf("the row ring came before the legacy ack or the stamp: %+v", r)
			}
			if len(f.pokes) != tc.wantPokes || (tc.wantPokes > 0 && f.pokes[0] != f.sessionID) {
				t.Fatalf("plain doorbells = %v, want %d for %s", f.pokes, tc.wantPokes, f.sessionID)
			}
			if got := len(warned) > 0; got != tc.wantWarned || (got && !strings.Contains(warned[0], f.sessionID)) {
				t.Fatalf("warnings = %q, want a warning naming %s: %t", warned, f.sessionID, tc.wantWarned)
			}
		})
	}
}

// hookLaneRing rings the rows the pass wrote whether or not it recorded a
// Done (a released take writes rows too), and adds the plain doorbell after
// a Done only for a conversation no row ring poked: one with no rows, or
// whose row ring the doorbell did not answer with a poke.
func TestHookLaneRingRingsDeferredRows(t *testing.T) {
	t.Setenv(hookDeadlineEnv, "")
	for _, tc := range []struct {
		name      string
		rows      map[string][]string
		done      bool
		unpoked   bool
		wantRings []string
		wantPokes []string
	}{
		{name: "rows without a Done", rows: map[string][]string{"gc-1": {"gc-a"}}, wantRings: []string{"gc-1:gc-a"}},
		{name: "rows and a Done", rows: map[string][]string{"gc-1": {"gc-a", "gc-b"}}, done: true, wantRings: []string{"gc-1:gc-a,gc-b"}},
		{name: "rows whose ring poked nothing and a Done", rows: map[string][]string{"gc-1": {"gc-a"}}, done: true, unpoked: true, wantRings: []string{"gc-1:gc-a"}, wantPokes: []string{"gc-1"}},
		{name: "a Done and no rows", done: true, wantPokes: []string{"gc-1"}},
		{name: "rows of another conversation and a Done", rows: map[string][]string{"gc-2": {"gc-a"}}, done: true, wantRings: []string{"gc-2:gc-a"}, wantPokes: []string{"gc-1"}},
		{name: "nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rings := recordJournalRings(t, func(string, string, []string) bool { return !tc.unpoked })
			var pokes []string
			prevPoke := hookLanePoke
			hookLanePoke = func(_, conv string, _ time.Duration) { pokes = append(pokes, conv) }
			t.Cleanup(func() { hookLanePoke = prevPoke })
			q := &journalRingQueue{}
			for _, conv := range []string{"gc-1", "gc-2"} {
				for _, id := range tc.rows[conv] {
					q.add(conv, id)
				}
			}
			hookLaneRing("city", "gc-1", hookLaneOutcome{done: tc.done}, q)
			if got := rings(); !slices.Equal(got, tc.wantRings) {
				t.Fatalf("row rings = %v, want %v", got, tc.wantRings)
			}
			if !slices.Equal(pokes, tc.wantPokes) {
				t.Fatalf("plain doorbells = %v, want %v", pokes, tc.wantPokes)
			}
		})
	}
}

// A drain whose hook deadline leaves its last doorbells no time rings
// nothing, but the rows it wrote are a missed ring, not an empty queue: the
// ring is warned of, naming the conversation, and no plain doorbell follows
// the Done either.
func TestHookLaneRingWarnsOfRowsTheDeadlineLeavesUnrung(t *testing.T) {
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(-time.Second)))
	var warned []string
	prevWarn := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = prevWarn })
	var pokes []string
	prevPoke := hookLanePoke
	hookLanePoke = func(_, conv string, _ time.Duration) { pokes = append(pokes, conv) }
	t.Cleanup(func() { hookLanePoke = prevPoke })
	q := &journalRingQueue{}
	q.add("gc-1", "gc-a")

	hookLaneRing(t.TempDir(), "gc-1", hookLaneOutcome{done: true}, q)
	if len(warned) != 1 || !strings.Contains(warned[0], "conversation gc-1") || !strings.Contains(warned[0], "deadline") {
		t.Fatalf("warnings = %q, want one naming gc-1's unrung rows", warned)
	}
	if len(pokes) != 0 {
		t.Fatalf("plain doorbells = %v, want none", pokes)
	}
}
