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

// In production the in-band pass's sessions store is the one-shot CLI
// emitter (openHookLaneSessionStore), whose journal-row writes ring the
// controller. Inside the drain those rings wait for its last doorbell: the
// take and the Done ring nothing mid-pass, and the rows ring once, after the
// hook output, the legacy ack and the delivery stamp, so a controller that
// answers slowly costs the drain none of the time its own bookkeeping needs.
// The row ring pokes the conversation too, so no plain doorbell follows it.
func TestNudgeDrainInjectLaneEmitterRowsRingLast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall bool
	}{{"controller answers slowly", true}, {"no controller", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			f.queueNudge(t)
			op := f.propose(t, "mail arrived")
			emitting := &emittingClassStore{Store: f.sessions, cityPath: f.cityDir}
			prevStore := hookLaneSessionStore
			hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store { return emitting }
			t.Cleanup(func() { hookLaneSessionStore = prevStore })
			if tc.stall {
				stallControllerSocket(t, f.cityDir)
			}
			var rings []laneRowRing
			prevRing := classStoreJournalRing
			classStoreJournalRing = func(city, conv string, ids []string) {
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
				prevRing(city, conv, ids)
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
			if len(f.pokes) != 0 {
				t.Fatalf("plain doorbells = %v, want none: the row ring pokes the conversation", f.pokes)
			}
		})
	}
}

// hookLaneRing rings the rows the pass wrote whether or not it recorded a
// Done (a released take writes rows too), and adds the plain doorbell after
// a Done only for a conversation no row ring poked.
func TestHookLaneRingRingsDeferredRows(t *testing.T) {
	t.Setenv(hookDeadlineEnv, "")
	for _, tc := range []struct {
		name      string
		rows      map[string][]string
		done      bool
		wantRings []string
		wantPokes []string
	}{
		{name: "rows without a Done", rows: map[string][]string{"gc-1": {"gc-a"}}, wantRings: []string{"gc-1:gc-a"}},
		{name: "rows and a Done", rows: map[string][]string{"gc-1": {"gc-a", "gc-b"}}, done: true, wantRings: []string{"gc-1:gc-a,gc-b"}},
		{name: "a Done and no rows", done: true, wantPokes: []string{"gc-1"}},
		{name: "rows of another conversation and a Done", rows: map[string][]string{"gc-2": {"gc-a"}}, done: true, wantRings: []string{"gc-2:gc-a"}, wantPokes: []string{"gc-1"}},
		{name: "nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rings := recordJournalRings(t, nil)
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
