package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// #9320 on v2: the allocation's snapshot can still assign the anchor to the
// session after the session released it (`bd update --assignee "" --status
// open`) or handed it to another session, and before its hand-back sling
// runs. The stop effect's probe reads that anchor live, once, past any cache,
// and the cycle goes on only when that one row is open and still the
// session's under its ReleaseScope. These tests drive the probe and the pure
// decision as the transaction does, over live work legs.

// anchorCycle is freshCase over live work legs: gw-old (the previous bead)
// closed an hour ago, after this incarnation woke two hours ago, so it alone
// does not defer; gw-new (the anchor) is open and assigned to the session's
// ID. Unedited, the cycle goes on.
type anchorCycle struct {
	t       *testing.T
	c       *healCase
	cfg     *config.City
	backing *beads.MemStore
}

func newAnchorCycle(t *testing.T, meta ...string) *anchorCycle {
	t.Helper()
	woke := censusNow.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	c := freshCase(t, append([]string{"awake_started_at", woke}, meta...)...)
	a := &anchorCycle{
		t: t,
		c: c,
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents:    []config.Agent{{Name: "worker", StartCommand: "true", WakeMode: "fresh", MaxActiveSessions: intPtr(3)}},
		},
	}
	closedAt := censusNow.Add(-time.Hour).Format(time.RFC3339Nano)
	a.backing = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "gw-old", Type: "task", Status: "closed", Assignee: c.k.ID, Metadata: map[string]string{"closed_at": closedAt}},
		{ID: "gw-new", Type: "task", Status: "open", Assignee: c.k.ID},
	}, nil)
	a.readFrom(a.backing)
	return a
}

// readFrom points the world's config and work legs at store.
func (a *anchorCycle) readFrom(store beads.Store) {
	a.c.w.Env = &reconcileEnv{Cfg: a.cfg}
	a.c.w.WorkLegs = testWorkLegs(a.c.w.CityPath, a.cfg, store, nil)
}

// edit rewrites gw-new on the backing store.
func (a *anchorCycle) edit(opts beads.UpdateOpts) {
	a.t.Helper()
	if err := a.backing.Update("gw-new", opts); err != nil {
		a.t.Fatal(err)
	}
}

// assign sets gw-new's assignee on the backing store.
func (a *anchorCycle) assign(assignee string) {
	a.t.Helper()
	a.edit(beads.UpdateOpts{Assignee: &assignee})
}

// view is the transaction's view of the cycle, over the session's own live
// runtime, and the reads it makes.
func (a *anchorCycle) view() (txView, effectReads) {
	a.t.Helper()
	p, it := a.c.pass(a.t, runtime.NewFake(), nil)
	if it.Reason != decideFreshCycle {
		a.t.Fatalf("decideRow = (%q, %q), want the fresh cycle", it.Kind, it.Reason)
	}
	rt := &txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: a.c.k.ID, Epoch: "3", Token: "tok-3"}}
	return txView{It: it, World: p.World, Alloc: p.Alloc, Row: a.c.w.Census.Rows[a.c.k].Info, Meta: a.c.meta(a.t), RT: rt, Now: censusNow}, p.reads
}

// anchorStep is what the probe and the decision answer for the cycle.
type anchorStep struct {
	step txStep
	err  error
}

func (s anchorStep) refused() bool { return s.step.Refuse == causeFreshCycleDefer }

// probeAnchorCycle probes v and decides on the answer. A panic is a failed
// step, not a crash of the run: the effect must fail closed.
func probeAnchorCycle(v txView, reads effectReads) (out anchorStep) {
	defer func() {
		if r := recover(); r != nil {
			out = anchorStep{err: fmt.Errorf("panic: %v", r)}
		}
	}()
	prev, err := freshCycleProbe(context.Background(), reads, v)
	return anchorStep{step: stopDecideAfterProbe(v, prev, err), err: err}
}

// step probes and decides the cycle as a's world now reads.
func (a *anchorCycle) step() anchorStep {
	a.t.Helper()
	v, reads := a.view()
	return probeAnchorCycle(v, reads)
}

// wantRefused fails unless the cycle is refused with the fresh-cycle defer.
func (a *anchorCycle) wantRefused(why string) {
	a.t.Helper()
	if got := a.step(); !got.refused() {
		a.t.Fatalf("%s: step %+v, probe err %v; want %s", why, got.step, got.err, causeFreshCycleDefer)
	}
}

// wantGoesOn fails unless the cycle passes the defer to the stop's own
// decision (stopDecide: A4 again, then the fence), which still decides the
// stop rather than redeciding it.
func (a *anchorCycle) wantGoesOn(why string) {
	a.t.Helper()
	v, reads := a.view()
	got := probeAnchorCycle(v, reads)
	if got.err != nil || got.refused() || !reflect.DeepEqual(got.step, stopDecide(v)) {
		a.t.Fatalf("%s: step %+v, probe err %v; want stopDecide's %+v", why, got.step, got.err, stopDecide(v))
	}
	if got.step.Refuse == causeRedecided {
		a.t.Fatalf("%s: step %+v; want the stop still decided", why, got.step)
	}
}

// TestFreshCycleAnchorOwnerDecides (#9320, AC-5, AC-6): the anchor's live
// row decides the cycle. Released, blank, handed to another session or to an
// alias-history name (outside ReleaseScope), or finished: refused. Open and
// assigned to the session's ID, session name or stable alias: goes on.
func TestFreshCycleAnchorOwnerDecides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		meta   []string
		edit   func(*anchorCycle)
		goesOn bool
	}{
		{name: "the session ID", edit: func(*anchorCycle) {}, goesOn: true},
		{name: "the session name", edit: func(a *anchorCycle) { a.assign("s-heal") }, goesOn: true},
		{name: "the stable alias", meta: []string{"alias", "worker-alias"}, edit: func(a *anchorCycle) { a.assign("worker-alias") }, goesOn: true},
		{name: "released", edit: func(a *anchorCycle) {
			open := "open"
			released := ""
			a.edit(beads.UpdateOpts{Assignee: &released, Status: &open})
		}},
		{name: "blank assignee", edit: func(a *anchorCycle) { a.assign(" \t ") }},
		{name: "another session", edit: func(a *anchorCycle) { a.assign("gc-other") }},
		{name: "an alias-history name", meta: []string{"alias", "worker-alias", "alias_history", "old-alias"}, edit: func(a *anchorCycle) { a.assign("old-alias") }},
		{name: "closed", edit: func(a *anchorCycle) {
			closed := "closed"
			a.edit(beads.UpdateOpts{Status: &closed})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAnchorCycle(t, tc.meta...)
			tc.edit(a)
			if tc.goesOn {
				a.wantGoesOn("the live anchor is the session's open work")
				return
			}
			a.wantRefused("the live anchor is not the session's open work")
		})
	}
}

// anchorLiveCounter is a store whose live reader counts the Gets of one id,
// and can fail them, so a test sees exactly which live reads the probe made.
// Every other read passes through.
type anchorLiveCounter struct {
	beads.Store
	id    string
	gets  *int
	fails error
}

// Handles keeps the inner store's cached reads and wraps its live reader.
func (s anchorLiveCounter) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	h.Live = anchorLiveCounterReader{LiveReader: h.Live, s: s}
	return h
}

type anchorLiveCounterReader struct {
	beads.LiveReader
	s anchorLiveCounter
}

// Get counts a live read of the watched id, and fails it when told to.
func (r anchorLiveCounterReader) Get(id string) (beads.Bead, error) {
	if id == r.s.id {
		*r.s.gets++
		if r.s.fails != nil {
			return beads.Bead{}, r.s.fails
		}
	}
	return r.LiveReader.Get(id)
}

// TestFreshCycleAnchorReadsPastPrimedCache (#9320, AC-5): the controller's
// legs are CachingStores, whose Get answers a primed, undirtied row. A
// release made by another process reaches the backing store alone; the probe
// reads the backing row, and refuses.
func TestFreshCycleAnchorReadsPastPrimedCache(t *testing.T) {
	a := newAnchorCycle(t)
	cached := beads.NewCachingStore(a.backing, nil)
	if err := cached.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	gets := 0
	a.readFrom(anchorLiveCounter{Store: cached, id: "gw-new", gets: &gets})
	a.assign("")
	if stale, err := cached.Get("gw-new"); err != nil || stale.Assignee != a.c.k.ID {
		t.Fatalf("precondition: the primed cache row = %+v (err %v), want it still assigned to %s", stale, err, a.c.k.ID)
	}
	a.wantRefused("the backing row is released behind a stale cache row")
	if gets != 1 {
		t.Fatalf("live anchor Gets = %d, want exactly one", gets)
	}
}

// TestFreshCycleAnchorGoesOnPastStaleCacheRow (#9320, AC-6): the converse of
// a stale release. The primed cache still holds the anchor as another
// session's, while the backing row is the session's again; the probe reads
// the backing row, once, and the cycle goes on.
func TestFreshCycleAnchorGoesOnPastStaleCacheRow(t *testing.T) {
	a := newAnchorCycle(t)
	a.assign("gc-other")
	cached := beads.NewCachingStore(a.backing, nil)
	if err := cached.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	gets := 0
	a.readFrom(anchorLiveCounter{Store: cached, id: "gw-new", gets: &gets})
	a.assign(a.c.k.ID)
	if stale, err := cached.Get("gw-new"); err != nil || stale.Assignee != "gc-other" {
		t.Fatalf("precondition: the primed cache row = %+v (err %v), want it still gc-other's", stale, err)
	}
	a.wantGoesOn("the backing row is the session's again behind a stale cache row")
	if gets != 1 {
		t.Fatalf("live anchor Gets = %d, want exactly one", gets)
	}
}

// TestFreshCycleAnchorReadOnlyAfterPreviousGoesOn (#9320, AC-6): the anchor
// is read only once the previous bead alone lets the cycle go on; a previous
// bead that defers it costs no anchor read.
func TestFreshCycleAnchorReadOnlyAfterPreviousGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		meta     []string
		previous beads.UpdateOpts
		goesOn   bool
	}{
		{name: "previous bead still open", previous: beads.UpdateOpts{Status: strPtr("open")}},
		{name: "awake since after the previous bead closed", meta: []string{"awake_started_at", censusNow.Add(-time.Minute).Format(time.RFC3339Nano)}},
		{name: "previous bead closed before this incarnation woke", goesOn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAnchorCycle(t, tc.meta...)
			if err := a.backing.Update("gw-old", tc.previous); err != nil {
				t.Fatal(err)
			}
			gets := 0
			a.readFrom(anchorLiveCounter{Store: a.backing, id: "gw-new", gets: &gets})
			want := 0
			if tc.goesOn {
				want = 1
				a.wantGoesOn("the previous bead and the anchor both let the cycle go on")
			} else {
				a.wantRefused("the previous bead defers the cycle")
			}
			if gets != want {
				t.Fatalf("live anchor Gets = %d, want %d", gets, want)
			}
		})
	}
}
