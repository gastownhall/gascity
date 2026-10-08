package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
)

// The close effect's tests (CONTRACT v5 C3, F2's corpse exception, F3's
// C8.8 on LL2's fresh read).

// closeLeaf is a hardened tmux-like leaf: fenceLeaf, plus remain-on-exit
// corpses, a fresh read (ObserveLivenessSince) apart from a cached one that
// always reads absent (the legacy socket check that can call a live server
// dead), and kills by exact session object.
type closeLeaf struct {
	*fenceLeaf
	corpses map[string]string // name -> the corpse's session object id
	since   []time.Time       // each fresh read's since
	kills   []string          // each kill's object id
	onKill  func(name string) // runs after a kill, before its answer
	refuse  runtime.SessionObjectKillResult
	killErr error // a kill that fails before a verdict
}

func newCloseLeaf() *closeLeaf {
	return &closeLeaf{fenceLeaf: newFenceLeaf(), corpses: map[string]string{}}
}

// closeCreated is every fake session object's creation time.
const closeCreated = "1700000000"

func (l *closeLeaf) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

func (l *closeLeaf) ObserveLivenessSince(name string, pn []string, since time.Time) (runtime.Liveness, error) {
	l.since = append(l.since, since)
	if id, ok := l.corpses[name]; ok {
		return runtime.Liveness{Corpse: true, ObjectID: id, ObjectCreated: closeCreated}, nil
	}
	return l.fenceLeaf.ObserveLivenessWithError(name, pn)
}

func (l *closeLeaf) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	l.kills = append(l.kills, objectID)
	if l.killErr != nil {
		return runtime.SessionObjectNotKilled, l.killErr
	}
	result := runtime.SessionObjectGone
	switch {
	case l.refuse != runtime.SessionObjectNotKilled:
		result = l.refuse
	case l.corpses[name] == objectID && created == closeCreated:
		delete(l.corpses, name)
		result = runtime.SessionObjectKilled
	}
	if l.onKill != nil {
		l.onKill(name)
	}
	return result, nil
}

func (l *closeLeaf) KillZombieObject(string, string, string, string) (runtime.SessionObjectKillResult, error) {
	return runtime.SessionObjectNotKilled, errors.New("the close never kills a zombie")
}

// admittedClose seeds an open asleep row x with runtime name rt_x on a
// require-stamped SQLite store (the atomic conditional closer a close
// requires), and returns the pass that admitted its orphan
// close over sp, the close, and the store.
func admittedClose(t *testing.T, sp runtime.Provider) (*effectPass, intent, beads.Store) {
	t.Helper()
	store := stampedSQLite(t, gate.Require)
	b, err := store.Create(sessionRow("x", "template", "worker", "session_name", "rt_x", "state", "asleep", "generation", "3", "instance_token", "tok-x"))
	if err != nil {
		t.Fatal(err)
	}
	w := &World{
		Now: gatherNow, CityPath: t.Name(), Env: &reconcileEnv{SP: sp},
		Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), LegStores: map[string]beads.Store{rowLeg: store},
	}
	it := intent{
		Kind: intentClose, Key: rowKey{Leg: rowLeg, ID: b.ID}, Patch: session.ClosePatch(gatherNow, "orphaned"),
		Event: &events.Event{Type: events.SessionStranded, Subject: b.ID},
	}
	return newEffectPass(w, &allocDecision{}), it, store
}

func rowStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Status
}

// Kills a close on unconfirmed absence (R27): a bool-only answer, an
// incomplete or erring read, and the cached read (which says absent while the
// fresh one shows a live pane) never confirm, and the close never issues a
// Stop of its own whose nil return could stand in for one. Only a fresh read
// begun after the effect began, showing no live pane, closes the row.
func TestCloseRequiresThreeOutcomeAbsent(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*closeLeaf) runtime.Provider
		cause string
	}{
		{"bool-only", func(*closeLeaf) runtime.Provider { return boolLeaf{runtime.NewFake()} }, causeLivenessUnsupported},
		{"incomplete", func(l *closeLeaf) runtime.Provider {
			l.LivenessErrors["rt_x"] = runtime.ErrRuntimeUnavailable
			return l
		}, causeLivenessUnknown},
		{"erred", func(l *closeLeaf) runtime.Provider {
			l.LivenessErrors["rt_x"] = errors.New("tmux: server busy")
			return l
		}, causeLivenessUnknown},
		{"live pane the cache misses", func(l *closeLeaf) runtime.Provider {
			startRuntime(t, l.Fake, "rt_x", nil)
			return l
		}, causeLivePane},
		{"absent", func(l *closeLeaf) runtime.Provider { return l }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			leaf := newCloseLeaf()
			p, it, store := admittedClose(t, c.setup(leaf))
			began := time.Now()
			s := closeEffect(p, it)(context.Background())
			if c.cause != "" {
				if s.Outcome != settledRefused || s.Cause != c.cause || rowStatus(t, store, it.Key.ID) != "open" {
					t.Fatalf("settlement %+v, row %s; want refused %q with the row open", s, rowStatus(t, store, it.Key.ID), c.cause)
				}
			} else if s.Outcome != settledLanded || s.Event == nil || rowStatus(t, store, it.Key.ID) != "closed" {
				t.Fatalf("settlement %+v, row %s; want landed with its event and the row closed", s, rowStatus(t, store, it.Key.ID))
			}
			for _, since := range leaf.since {
				if since.Before(began) {
					t.Fatalf("a fresh read accepted a refresh from %v, before the effect began at %v", since, began)
				}
			}
			if leaf.CountCalls("Stop", "rt_x") != 0 {
				t.Fatal("the close issued a provider Stop")
			}
		})
	}
}

// Kills a token-less corpse that livelocks a close (I23), a corpse removed
// while attached, a kill that is not by the observed session object, a
// refused or failed kill (a multi-pane corpse) retried by backoff, and a
// close over the session an attach re-created under the name before the
// kill (R54).
func TestCorpseCountsAsStoppedAndIsRemovedWhenDetached(t *testing.T) {
	for _, c := range []struct {
		name     string
		attached bool
		refuse   runtime.SessionObjectKillResult
		killErr  error
		recreate bool
		kills    int
		corpse   bool // left in place
		closed   bool
	}{
		{name: "detached", kills: 1, closed: true},
		{name: "attached", attached: true, corpse: true, closed: true},
		{name: "multi-pane", refuse: runtime.SessionObjectLive, kills: 1, corpse: true, closed: true},
		{name: "kill failed", killErr: errors.New("tmux: server busy"), kills: 1, corpse: true, closed: true},
		{name: "attach re-created it", refuse: runtime.SessionObjectGone, recreate: true, kills: 1, corpse: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			leaf := newCloseLeaf()
			leaf.corpses["rt_x"] = "$7" // no GC_INSTANCE_TOKEN: identity never reads
			leaf.SetAttached("rt_x", c.attached)
			leaf.refuse, leaf.killErr = c.refuse, c.killErr
			if c.recreate {
				leaf.onKill = func(name string) {
					delete(leaf.corpses, name)
					startRuntime(t, leaf.Fake, name, nil)
				}
			}
			p, it, store := admittedClose(t, leaf)
			s := closeEffect(p, it)(context.Background())
			if got := rowStatus(t, store, it.Key.ID) == "closed"; got != c.closed || (c.closed && s.Outcome != settledLanded) || (!c.closed && s.Cause != causeLivePane) {
				t.Fatalf("settlement %+v, closed=%v; want closed=%v", s, got, c.closed)
			}
			if len(leaf.kills) != c.kills || (c.kills > 0 && leaf.kills[0] != "$7") {
				t.Fatalf("kills %v, want %d of the observed object $7", leaf.kills, c.kills)
			}
			if _, left := leaf.corpses["rt_x"]; left != c.corpse {
				t.Fatalf("corpse left in place = %v, want %v", left, c.corpse)
			}
		})
	}
}

// Kills closing a woken session: a wake that lands between the pass's read
// and the close fails the premise, so the close refuses superseded, writes
// nothing, and the next pass decides on the woken row.
func TestCloseLostFenceRedecides(t *testing.T) {
	p, it, store := admittedClose(t, newCloseLeaf())
	if err := store.SetMetadataBatch(it.Key.ID, map[string]string{"state": "active", "last_woke_at": rowAt(0)}); err != nil {
		t.Fatal(err)
	}
	s := closeEffect(p, it)(context.Background())
	if s.Outcome != settledRefused || s.Cause != causeSuperseded {
		t.Fatalf("settlement %+v, want refused %q", s, causeSuperseded)
	}
	b, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" || b.Metadata["state"] != "active" {
		t.Fatalf("row %s state %q, want the woken row open and untouched", b.Status, b.Metadata["state"])
	}
}

// Kills a close that skips the name lock (P3): a name another effect holds
// refuses name-busy, with nothing read or written.
func TestCloseRefusesABusyName(t *testing.T) {
	leaf := newCloseLeaf()
	p, it, store := admittedClose(t, leaf)
	unlock := runtimeNames.tryLock(p.World.CityPath, "rt_x")
	defer unlock()
	if s := closeEffect(p, it)(context.Background()); s.Outcome != settledRefused || s.Cause != causeNameBusy {
		t.Fatalf("settlement %+v, want refused %q", s, causeNameBusy)
	}
	if len(leaf.since) != 0 || rowStatus(t, store, it.Key.ID) != "open" {
		t.Fatal("a close read or wrote under a busy name")
	}
}

// Kills confirming, or removing a corpse, through a stale route
// (mc-zndi7.24): routed to ACP, a runtime live on the default backend is
// seen through the composite and refuses the close; and an attached corpse
// there is never killed on the strength of the ACP leaf's attach answer.
func TestStaleRouteNeverConfirmsOrStopsOnWrongBackend(t *testing.T) {
	t.Run("live on the other backend", func(t *testing.T) {
		tmux, acp := newCloseLeaf(), newFenceLeaf()
		startRuntime(t, tmux.Fake, "rt_x", nil)
		sp := auto.New(tmux, acp)
		sp.SeedRoutes([]string{"rt_x"})
		p, it, store := admittedClose(t, sp)
		if s := closeEffect(p, it)(context.Background()); s.Cause != causeLivePane || rowStatus(t, store, it.Key.ID) != "open" {
			t.Fatalf("settlement %+v, want refused %q with the row open", s, causeLivePane)
		}
	})
	t.Run("attached corpse on the other backend", func(t *testing.T) {
		tmux, acp := newCloseLeaf(), newFenceLeaf()
		tmux.corpses["rt_x"] = "$7"
		tmux.SetAttached("rt_x", true)
		sp := auto.New(tmux, acp)
		sp.SeedRoutes([]string{"rt_x"})
		p, it, _ := admittedClose(t, sp)
		closeEffect(p, it)(context.Background())
		if len(tmux.kills) != 0 {
			t.Fatalf("kills %v: an attached corpse was killed through a stale route", tmux.kills)
		}
	})
}

// Kills C8.8 on the cached read (ORCH LL2: the legacy socket check can call
// a live server dead): confirmStopped reads fresh, since the stop.
func TestConfirmedStopReadsFreshSinceTheStop(t *testing.T) {
	leaf := newCloseLeaf()
	startRuntime(t, leaf.Fake, "rt_x", nil)
	since := time.Now()
	if confirmStopped(context.Background(), leaf, "rt_x", since) {
		t.Fatal("a stop was confirmed from the cached read while the fresh read shows a live pane")
	}
	if len(leaf.since) != 1 || !leaf.since[0].Equal(since) {
		t.Fatalf("fresh reads since %v, want one since %v", leaf.since, since)
	}
	leaf.corpses["rt_x"] = "$1"
	if !confirmStopped(context.Background(), leaf, "rt_x", since) {
		t.Fatal("a corpse (no live pane) did not confirm the stop")
	}
}
