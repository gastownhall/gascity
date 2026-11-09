package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The fresh cycle's defer is ga-pvjbx3 Q1-Q3 (the v2 A13 row): a session that
// holds a claim is never cycled out from under it, the anchor is re-read live,
// and an open previous bead refuses only while the session owns it. Every
// read is made inside the stop effect, so these tests drive the probe and the
// pure decision as the transaction does, over live work legs.

// freshWork is freshCase over live work legs: gw-old (the previous bead)
// closed an hour ago, after this incarnation woke two hours ago, so it alone
// does not defer; gw-new (the anchor) open and assigned to the session.
// Nothing else is held, so every row starts as a cycle that goes on and
// changes one thing.
type freshWork struct {
	t    *testing.T
	c    *healCase
	cfg  *config.City
	work beads.Store
	rigs map[string]beads.Store
	log  *callLog
}

// callLog is the reads a store saw, in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) note(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// loggedStore records every Get and List it answers.
type loggedStore struct {
	beads.Store
	log *callLog
	// failList fails the List of that status (a leg that cannot answer).
	failList string
}

func (s loggedStore) Get(id string) (beads.Bead, error) {
	s.log.note("get:" + id)
	return s.Store.Get(id)
}

func (s loggedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.log.note("list:" + q.Status)
	if s.failList != "" && q.Status == s.failList {
		return nil, errors.New("leg unreachable")
	}
	return s.Store.List(q)
}

// ConditionalWritesResolveTarget keeps the inner store's fenced writes.
func (s loggedStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// getFailStore fails the Get of one id.
type getFailStore struct {
	beads.Store
	id string
}

func (s getFailStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		return beads.Bead{}, errors.New("store unreachable")
	}
	return s.Store.Get(id)
}

// ConditionalWritesResolveTarget keeps the inner store's fenced writes.
func (s getFailStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func freshWorkCfg() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", WakeMode: "fresh", MaxActiveSessions: intPtr(3)}},
	}
}

// newFreshWork is the clean cycle. meta is more row metadata (aliases, a pool
// slot).
func newFreshWork(t *testing.T, meta ...string) *freshWork {
	t.Helper()
	woke := censusNow.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	c := freshCase(t, append([]string{"awake_started_at", woke}, meta...)...)
	f := &freshWork{t: t, c: c, cfg: freshWorkCfg(), log: &callLog{}}
	closedAt := censusNow.Add(-time.Hour).Format(time.RFC3339Nano)
	f.work = beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "gw-old", Type: "task", Status: "closed", Assignee: c.k.ID, Metadata: map[string]string{"closed_at": closedAt}},
		{ID: "gw-new", Type: "task", Status: "open", Assignee: c.k.ID},
	}, nil)
	f.wire()
	return f
}

// wire points the world's work legs and config at f's stores.
func (f *freshWork) wire() {
	f.c.w.Env = &reconcileEnv{Cfg: f.cfg}
	logged := loggedStore{Store: f.work, log: f.log}
	f.c.w.WorkLegs = testWorkLegs(f.c.w.CityPath, f.cfg, logged, f.rigs)
}

// row rewrites the cycle's census row.
func (f *freshWork) row(edit func(*session.Info)) {
	r := f.c.w.Census.Rows[f.c.k]
	edit(&r.Info)
	f.c.w.Census.Rows[f.c.k] = r
}

// id is the session's ID.
func (f *freshWork) id() string { return f.c.k.ID }

// add creates a work bead in the city work store.
func (f *freshWork) add(status, assignee string) beads.Bead {
	f.t.Helper()
	return createWork(f.t, f.work, status, assignee, nil)
}

// set rewrites a bead of the city work store.
func (f *freshWork) set(id string, opts beads.UpdateOpts) {
	f.t.Helper()
	if err := f.work.Update(id, opts); err != nil {
		f.t.Fatal(err)
	}
}

// failList makes the city work leg's list of status fail.
func (f *freshWork) failList(status string) {
	f.c.w.WorkLegs = testWorkLegs(f.c.w.CityPath, f.cfg, loggedStore{Store: f.work, log: f.log, failList: status}, f.rigs)
}

// freshVerdict is what the probe and the decision answer for the cycle.
type freshVerdict struct {
	refused bool
	step    txStep
	err     error
}

// view is the transaction's view of the cycle, and the reads it makes.
func (f *freshWork) view() (txView, effectReads) {
	f.t.Helper()
	p, it := f.c.pass(f.t, runtime.NewFake(), nil)
	if it.Reason != decideFreshCycle {
		f.t.Fatalf("decideRow = (%q, %q), want the fresh cycle", it.Kind, it.Reason)
	}
	return txView{It: it, World: p.World, Alloc: p.Alloc, Row: f.c.w.Census.Rows[f.c.k].Info, Meta: f.c.meta(f.t), RT: &txRuntime{Class: rtAbsent}, Now: censusNow}, p.reads
}

// verdict probes the cycle and decides on the answer.
func (f *freshWork) verdict() freshVerdict {
	f.t.Helper()
	v, reads := f.view()
	return decideFresh(v, reads)
}

// decideFresh probes and decides. A panic is a failed verdict, not a crash of
// the whole run: the effect must fail closed, never dereference a nil.
func decideFresh(v txView, reads effectReads) (out freshVerdict) {
	defer func() {
		if r := recover(); r != nil {
			out = freshVerdict{err: fmt.Errorf("panic: %v", r)}
		}
	}()
	prev, err := freshCycleProbe(context.Background(), reads, v)
	step := stopDecideAfterProbe(v, prev, err)
	return freshVerdict{refused: step.Refuse == causeFreshCycleDefer, step: step, err: err}
}

// wantRefused fails unless the cycle is refused with the fresh-cycle defer.
func (f *freshWork) wantRefused(why string) {
	f.t.Helper()
	if got := f.verdict(); !got.refused {
		f.t.Fatalf("%s: step %+v, probe err %v; want %s", why, got.step, got.err, causeFreshCycleDefer)
	}
}

// wantGoesOn fails if the cycle is refused with the fresh-cycle defer. What
// the stop does after that (the fence over a live runtime) is the stop's own
// decision, tested in reconcile_effect_stop_test.go and by the sims.
func (f *freshWork) wantGoesOn(why string) {
	f.t.Helper()
	if got := f.verdict(); got.refused {
		f.t.Fatalf("%s: step %+v, probe err %v; want the cycle to go on", why, got.step, got.err)
	}
}

// TestFreshCycleGoesOnWhenClean (A7): the anchor, open and assigned to the
// session, does not count as a claim; with the previous bead closed after the
// incarnation woke and nothing in progress the cycle goes on.
func TestFreshCycleGoesOnWhenClean(t *testing.T) {
	newFreshWork(t).wantGoesOn("the clean cycle")
}

// TestFreshCycleRefusesAnyClaim (Q1; A1-A5): a session that holds ANY assigned
// in_progress bead is not cycled, on the live read.
func TestFreshCycleRefusesAnyClaim(t *testing.T) {
	t.Run("A1 a claim on a bead that is not the anchor (2026-09-25)", func(t *testing.T) {
		f := newFreshWork(t)
		f.add("in_progress", f.id())
		f.wantRefused("the session holds another bead in progress")
	})
	t.Run("A2 the anchor itself in progress", func(t *testing.T) {
		f := newFreshWork(t)
		f.c.a.Snapshot.Entries[f.c.k].AssignedWork.Claimed = true
		f.set("gw-new", beads.UpdateOpts{Status: ptr("in_progress")})
		f.wantRefused("the anchor is claimed")
	})
	t.Run("A3 a stale snapshot: the entry says unclaimed, the store holds the claim", func(t *testing.T) {
		f := newFreshWork(t)
		f.c.a.Snapshot.Entries[f.c.k].AssignedWork.Claimed = false
		f.add("in_progress", f.id())
		f.wantRefused("the live read decides")
	})
	t.Run("A4 the claim is in a rig store", func(t *testing.T) {
		f := newFreshWork(t)
		f.cfg = servingRigsCity(declaredRigsConfigWith(f.cfg, "frontend"), "frontend/other")
		rig := beads.NewMemStore()
		f.rigs = map[string]beads.Store{"frontend": rig}
		f.wire()
		createWork(t, rig, "in_progress", f.id(), nil)
		f.wantRefused("a rig leg holds an in_progress bead for the session")
	})
	t.Run("A5 another identity's in_progress bead does not refuse", func(t *testing.T) {
		f := newFreshWork(t)
		f.add("in_progress", "gc-other")
		f.wantGoesOn("the claim is someone else's")
	})
}

// TestFreshCycleClaimScanFailsClosed (Q1; A6): a scan that cannot answer
// refuses; one row each.
func TestFreshCycleClaimScanFailsClosed(t *testing.T) {
	t.Run("a leg's list fails", func(t *testing.T) {
		f := newFreshWork(t)
		f.failList("in_progress")
		f.wantRefused("the claim scan's list failed")
	})
	t.Run("the probe expired", func(t *testing.T) {
		f := newFreshWork(t)
		v, _ := f.view()
		if step := stopDecideAfterProbe(v, freshCyclePrev{}, errProbeExpired); step.Refuse != causeFreshCycleDefer {
			t.Fatalf("step %+v, want %s", step, causeFreshCycleDefer)
		}
	})
	t.Run("the legs are unusable", func(t *testing.T) {
		f := newFreshWork(t)
		v, _ := f.view()
		if got := decideFresh(v, effectReads{}); !got.refused {
			t.Fatalf("step %+v, probe err %v; want %s over unusable legs", got.step, got.err, causeFreshCycleDefer)
		}
	})
	t.Run("no world", func(t *testing.T) {
		f := newFreshWork(t)
		v, reads := f.view()
		v.World = nil
		if got := decideFresh(v, reads); !got.refused {
			t.Fatalf("step %+v, probe err %v; want %s with no world", got.step, got.err, causeFreshCycleDefer)
		}
	})
	t.Run("no environment", func(t *testing.T) {
		f := newFreshWork(t)
		v, reads := f.view()
		w := *v.World
		w.Env = nil
		v.World = &w
		if got := decideFresh(v, reads); !got.refused {
			t.Fatalf("step %+v, probe err %v; want %s with no environment", got.step, got.err, causeFreshCycleDefer)
		}
	})
}

// TestFreshCycleClaimScanReadsLive (Q1; A8): a claim the tick did not see
// refuses, on a single-store and a split-store city: the probe's read is
// live, never the tick's index.
func TestFreshCycleClaimScanReadsLive(t *testing.T) {
	for _, shape := range workShapes {
		t.Run(shape, func(t *testing.T) {
			f := newFreshWork(t)
			if shape == "split" {
				registerWorkShapeWith(t, f.c.w.CityPath, shape, f.c.store, f.work)
			}
			f.wire()
			open := f.add("open", f.id())
			f.c.before = func() { f.set(open.ID, beads.UpdateOpts{Status: ptr("in_progress")}) }
			// (i) open when the tick read the work, in_progress by the probe.
			f.c.before()
			f.wantRefused("a bead that became in_progress after the tick's read")
			// (ii) a bead that appeared already in progress.
			f.set(open.ID, beads.UpdateOpts{Status: ptr("closed")})
			f.add("in_progress", f.id())
			f.wantRefused("a bead that appeared in progress after the tick's read")
		})
	}
}

// TestFreshCycleClaimScanScope (Q1, 4.8; A9, A10): the scan names every
// identity the seat may hold work under, its prior aliases included, and
// never a prior transient pool slot alias.
func TestFreshCycleClaimScanScope(t *testing.T) {
	named := []string{"configured_named_session", "true", "configured_named_identity", "mayor", "alias", "nux", "alias_history", "morsov"}
	for _, form := range []struct {
		name string
		by   func(f *freshWork) string
	}{
		{"session ID", func(f *freshWork) string { return f.id() }},
		{"session name", func(*freshWork) string { return "s-heal" }},
		{"configured named identity", func(*freshWork) string { return "mayor" }},
		{"current stable alias", func(*freshWork) string { return "nux" }},
		{"a prior alias from alias_history", func(*freshWork) string { return "morsov" }},
	} {
		t.Run("A9 "+form.name, func(t *testing.T) {
			f := newFreshWork(t, named...)
			f.add("in_progress", form.by(f))
			f.wantRefused("work held under the seat's " + form.name)
		})
	}
	pool := []string{"pool_managed", "true", "pool_slot", "1", "alias", "frontend/worker-1", "alias_history", "frontend/worker-0"}
	t.Run("A10 a slot-aliased pool seat's current alias refuses", func(t *testing.T) {
		f := newFreshWork(t, pool...)
		f.add("in_progress", "frontend/worker-1")
		f.wantRefused("work held under the pool seat's current alias")
	})
	t.Run("A10 a prior transient pool slot alias does not", func(t *testing.T) {
		f := newFreshWork(t, pool...)
		f.add("in_progress", "frontend/worker-0")
		f.wantGoesOn("a chair, not an owner")
	})
}

// TestFreshCycleAnchor (Q2; B1-B5): the anchor is read live before the stop.
func TestFreshCycleAnchor(t *testing.T) {
	t.Run("B1 closed on the live read", func(t *testing.T) {
		f := newFreshWork(t)
		f.set("gw-new", beads.UpdateOpts{Status: ptr("closed")})
		f.wantRefused("the allocation points at finished work")
	})
	t.Run("B2 the read fails", func(t *testing.T) {
		f := newFreshWork(t)
		f.c.w.WorkLegs = testWorkLegs(f.c.w.CityPath, f.cfg, getFailStore{Store: f.work, id: "gw-new"}, nil)
		f.wantRefused("the anchor cannot be read")
	})
	t.Run("B3 no leg holds it", func(t *testing.T) {
		f := newFreshWork(t)
		f.c.a.Snapshot.Entries[f.c.k].AssignedWork.BeadID = "gw-gone"
		f.wantRefused("the anchor is held by no leg")
	})
	t.Run("B4 no anchor id", func(t *testing.T) {
		for name, set := range map[string]func(*freshWork){
			"no entry":         func(f *freshWork) { delete(f.c.a.Snapshot.Entries, f.c.k) },
			"no assigned work": func(f *freshWork) { f.c.a.Snapshot.Entries[f.c.k].AssignedWork = nil },
			"an empty id":      func(f *freshWork) { f.c.a.Snapshot.Entries[f.c.k].AssignedWork.BeadID = " " },
		} {
			f := newFreshWork(t)
			v, reads := f.view()
			set(f)
			if got := decideFresh(v, reads); !got.refused {
				t.Errorf("%s: step %+v, probe err %v; want %s", name, got.step, got.err, causeFreshCycleDefer)
			}
		}
		f := newFreshWork(t)
		v, reads := f.view()
		v.Alloc = nil
		if got := decideFresh(v, reads); !got.refused {
			t.Errorf("no allocation: step %+v, probe err %v; want %s", got.step, got.err, causeFreshCycleDefer)
		}
		v.Alloc = &allocDecision{}
		if got := decideFresh(v, reads); !got.refused {
			t.Errorf("no snapshot: step %+v, probe err %v; want %s", got.step, got.err, causeFreshCycleDefer)
		}
	})
	t.Run("B5 open and not claimed by the session: the cycle goes on", func(t *testing.T) {
		f := newFreshWork(t)
		f.set("gw-new", beads.UpdateOpts{Assignee: ptr("gc-other")})
		f.wantGoesOn("a live, non-terminal anchor")
	})
}

// TestFreshCycleOwnedPreviousBead (Q3; C1-C5, C7): an open previous bead
// refuses only while its live assignee is one of the seat's identities.
func TestFreshCycleOwnedPreviousBead(t *testing.T) {
	named := []string{"alias", "nux", "alias_history", "morsov"}
	open := func(f *freshWork, assignee string) {
		f.set("gw-old", beads.UpdateOpts{Status: ptr("open"), Assignee: ptr(assignee)})
	}
	t.Run("C1 open and the session's, under each of two identity forms", func(t *testing.T) {
		for _, by := range []string{"s-heal", "nux", "morsov"} {
			f := newFreshWork(t, named...)
			open(f, by)
			f.wantRefused("the previous bead is open and assigned to " + by)
		}
	})
	t.Run("C2 open and another identity's: the cycle goes on", func(t *testing.T) {
		f := newFreshWork(t)
		open(f, "gc-other")
		f.wantGoesOn("another identity holds the previous bead")
	})
	t.Run("C3 open and unassigned: the cycle goes on", func(t *testing.T) {
		f := newFreshWork(t)
		open(f, "")
		f.wantGoesOn("nobody holds the previous bead")
	})
	t.Run("C4 the assignee is compared trimmed", func(t *testing.T) {
		f := newFreshWork(t)
		open(f, "  s-heal \t")
		f.wantRefused("surrounding whitespace around the session's own name")
		g := newFreshWork(t)
		open(g, "  gc-other ")
		g.wantGoesOn("surrounding whitespace around another identity")
	})
	t.Run("C5 closed after this incarnation woke, under another assignee", func(t *testing.T) {
		f := newFreshWork(t)
		f.row(func(i *session.Info) { i.AwakeStartedAt = censusNow.Add(-time.Minute).Format(time.RFC3339Nano) })
		f.set("gw-old", beads.UpdateOpts{Assignee: ptr("gc-other")})
		f.wantRefused("the incarnation woke after the close, whatever the assignee")
	})
	t.Run("C7 open, assigned under a prior transient pool slot alias: not owned", func(t *testing.T) {
		f := newFreshWork(t, "pool_managed", "true", "pool_slot", "1", "alias", "frontend/worker-1", "alias_history", "frontend/worker-0")
		open(f, "frontend/worker-0")
		f.wantGoesOn("a chair, not an owner")
	})
}

// TestFreshCycleDefers (C6; SESS-612): every answer the previous bead alone
// gave before Q1-Q3 keeps its answer; the fixture carries a clean scan and a
// live non-terminal anchor, and the open previous bead is the session's own.
func TestFreshCycleDefers(t *testing.T) {
	woke := func(f *freshWork, ago time.Duration) {
		f.row(func(i *session.Info) { i.AwakeStartedAt = censusNow.Add(-ago).Format(time.RFC3339Nano) })
	}
	for _, tc := range []struct {
		name   string
		set    func(f *freshWork)
		defers bool
	}{
		{"the previous bead open and the session's", func(f *freshWork) {
			f.set("gw-old", beads.UpdateOpts{Status: ptr("open"), Assignee: ptr(f.id())})
		}, true},
		{"closed after this incarnation woke", func(f *freshWork) { woke(f, 2*time.Hour) }, false},
		{"closed before this incarnation woke", func(f *freshWork) { woke(f, time.Minute) }, true},
		{"its wake time unreadable", func(f *freshWork) { f.row(func(i *session.Info) { i.AwakeStartedAt = "" }) }, false},
		{"the lookup failed", func(f *freshWork) {
			f.c.w.WorkLegs = testWorkLegs(f.c.w.CityPath, f.cfg, getFailStore{Store: f.work, id: "gw-old"}, nil)
		}, true},
		{"no store holds it", func(f *freshWork) { f.row(func(i *session.Info) { i.CurrentlyProcessingBeadID = "gw-gone" }) }, false},
		{"no previous bead", func(f *freshWork) { f.row(func(i *session.Info) { i.CurrentlyProcessingBeadID = "" }) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFreshWork(t)
			tc.set(f)
			if got := f.verdict(); got.refused != tc.defers {
				t.Fatalf("refused %t (%+v, probe err %v), want %t", got.refused, got.step, got.err, tc.defers)
			}
		})
	}
}

// TestFreshCycleReadOrder (D1, D3): the previous bead, then the claim scan,
// then the anchor; a refusal from earlier evidence issues no later read.
func TestFreshCycleReadOrder(t *testing.T) {
	kind := func(calls []string) string {
		var out []string
		for _, c := range calls {
			switch {
			case c == "get:gw-old":
				out = append(out, "previous")
			case c == "get:gw-new":
				out = append(out, "anchor")
			case strings.HasPrefix(c, "list:"):
				if len(out) == 0 || out[len(out)-1] != "scan" {
					out = append(out, "scan")
				}
			default:
				out = append(out, c)
			}
		}
		return strings.Join(out, ",")
	}
	t.Run("D3 a clean cycle reads previous, scan, anchor", func(t *testing.T) {
		f := newFreshWork(t)
		f.wantGoesOn("the clean cycle")
		if got := kind(f.log.all()); got != "previous,scan,anchor" {
			t.Fatalf("reads %q (%v), want previous,scan,anchor", got, f.log.all())
		}
	})
	for _, tc := range []struct {
		name string
		set  func(f *freshWork)
	}{
		{"open and the session's", func(f *freshWork) {
			f.set("gw-old", beads.UpdateOpts{Status: ptr("open"), Assignee: ptr(f.id())})
		}},
		{"closed and woke after", func(f *freshWork) {
			f.row(func(i *session.Info) { i.AwakeStartedAt = censusNow.Add(-time.Minute).Format(time.RFC3339Nano) })
		}},
		{"unreadable", func(f *freshWork) {
			f.c.w.WorkLegs = testWorkLegs(f.c.w.CityPath, f.cfg, loggedStore{Store: getFailStore{Store: f.work, id: "gw-old"}, log: f.log}, nil)
		}},
	} {
		t.Run("D1 a refusal from the previous bead issues no scan and no anchor read: "+tc.name, func(t *testing.T) {
			f := newFreshWork(t)
			tc.set(f)
			f.wantRefused(tc.name)
			if got := kind(f.log.all()); got != "previous" && got != "" {
				t.Fatalf("reads %q (%v), want only the previous bead's", got, f.log.all())
			}
		})
	}
	t.Run("D1 a claim-scan refusal issues no anchor read", func(t *testing.T) {
		f := newFreshWork(t)
		f.add("in_progress", f.id())
		f.wantRefused("a claim")
		if got := kind(f.log.all()); got != "previous,scan" {
			t.Fatalf("reads %q (%v), want previous,scan", got, f.log.all())
		}
	})
}

// TestFreshCycleProbeReadsNothingElse (D2): any other stop reason reads
// nothing, so a stop never waits on a work read it does not decide on.
func TestFreshCycleProbeReadsNothingElse(t *testing.T) {
	f := newFreshWork(t)
	v, reads := f.view()
	v.It.Reason = decideStopVerb + "idle"
	f.log.note("start")
	if got, err := freshCycleProbe(context.Background(), reads, v); err != nil || got.found || got.none || got.bead.ID != "" {
		t.Fatalf("another stop's probe = %+v, %v; want no read", got, err)
	}
	if calls := f.log.all(); len(calls) != 1 {
		t.Fatalf("reads %v, want none", calls[1:])
	}
}

// TestFreshCycleRefusalSettles (E1): the whole effect, over a held claim,
// settles refused on the fresh-cycle defer and stops nothing.
func TestFreshCycleRefusalSettles(t *testing.T) {
	f := newFreshWork(t)
	f.add("in_progress", f.id())
	sp := runtime.NewFake()
	_, s := f.c.run(t, sp, nil)
	if s.Outcome != settledRefused || s.Cause != causeFreshCycleDefer {
		t.Fatalf("settlement %+v, want refused %s", s, causeFreshCycleDefer)
	}
	if got := f.c.meta(t)["session_key"]; got != "" && f.c.meta(t)["continuation_reset_pending"] != "" {
		t.Fatalf("row %v written, want nothing written", f.c.meta(t))
	}
}

// declaredRigsConfigWith is cfg with the named rigs declared.
func declaredRigsConfigWith(cfg *config.City, names ...string) *config.City {
	c := *cfg
	c.Rigs = append(c.Rigs, declaredRigsConfig(names...).Rigs...)
	return &c
}
