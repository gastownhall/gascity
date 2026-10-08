package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// confirmsOn returns the reaper calls of phase on name, as method(key)=result.
func confirmsOn(eff reaperEffects, phase, name string) []string {
	var out []string
	for _, c := range eff.calls {
		if c.phase == phase && c.name == name && c.method != "ListRunning" {
			out = append(out, c.method+"("+c.key+")="+c.result)
		}
	}
	return out
}

// freshListings counts the corpse cleaner's own ListRunning calls.
func freshListings(eff reaperEffects) int {
	n := 0
	for _, c := range eff.calls {
		if c.phase == "corpse" && c.method == "ListRunning" {
			n++
		}
	}
	return n
}

// Kills: serving a view older than two patrol intervals. A corpse that
// appeared after the lane's last pass is found by the live listing once the
// pass is stale, and not a moment before.
func TestDeadRuntimeCleanup_StaleViewFallsBackToLive(t *testing.T) {
	seen := reaperFixtureState([]beads.Bead{reaperRow("s-late", "late", "active")}, nil)
	now := reaperFixtureState([]beads.Bead{reaperRow("s-late", "late", "active")},
		map[string]reaperRuntime{"late": {incarnation: "late:1", dead: true, sessionID: "s-late"}})

	lr := newLaneReaperRun(t, seen)
	clk := &clock.Fake{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	useInventoryClock(lr.cr, clk)
	lr.pass(seen)

	clk.Advance(2 * inventoryLaneInterval)
	if inv := lr.cr.inventoryViewForTick(); inv == nil {
		t.Fatal("no view at exactly two intervals: the bound is inclusive")
	}
	clk.Advance(time.Millisecond)
	eff, inv := lr.reap(t, now)
	if inv != nil {
		t.Fatalf("view served %v after its pass finished", 2*inventoryLaneInterval+time.Millisecond)
	}
	if eff.source != inventorySourceLive || freshListings(eff) != 1 {
		t.Fatalf("source = %s with %d fresh listing(s), want one live listing", eff.source, freshListings(eff))
	}
	if !reflect.DeepEqual(eff.stops, []string{"late"}) || eff.closes["s-late"] != "dead-runtime" {
		t.Fatalf("stops=%v closes=%v, want the post-pass corpse reaped live", eff.stops, eff.closes)
	}
}

// Kills: a live-pane filter that also skips a corpse, a name the inventory
// did not cover, or a name whose live pane was seen by an earlier pass only.
// Each of those gets its fresh death check; only the pass's own live pane
// skips it.
func TestDeadRuntimeCleanup_LivePaneFilterOnlySkipsLivePanes(t *testing.T) {
	rows := []beads.Bead{
		reaperRow("s-live", "live", "active"),
		reaperRow("s-corpse", "corpse", "active"),
		reaperRow("s-uncovered", "uncovered", "active"),
		reaperRow("s-earlier", "earlier", "active"),
	}
	first := reaperFixtureState(rows, map[string]reaperRuntime{
		"live":      {incarnation: "live:1"},
		"corpse":    {incarnation: "corpse:1", dead: true},
		"uncovered": {incarnation: "uncovered:1", uninventoried: true},
		"earlier":   {incarnation: "earlier:1"},
	})
	// The second pass's inventory misses earlier: its live pane is pass 1's.
	second := first.clone()
	second.runtimes["earlier"] = reaperRuntime{incarnation: "earlier:1", uninventoried: true}
	// By the tick, uncovered and earlier have died.
	now := first.clone()
	now.runtimes["uncovered"] = reaperRuntime{incarnation: "uncovered:1", dead: true}
	now.runtimes["earlier"] = reaperRuntime{incarnation: "earlier:1", dead: true}

	lr := newLaneReaperRun(t, first)
	lr.pass(first)
	lr.pass(second)
	eff, inv := lr.reap(t, now)
	if inv == nil {
		t.Fatal("no view after a fresh pass")
	}
	if got := confirmsOn(eff, "corpse", "live"); len(got) != 0 {
		t.Fatalf("live pane confirmed: %v", got)
	}
	for _, name := range []string{"corpse", "uncovered", "earlier"} {
		if got := confirmsOn(eff, "corpse", name); len(got) == 0 || got[0] != "IsDeadRuntimeSession()=true" {
			t.Fatalf("%s: calls %v, want a fresh death check that finds it dead", name, got)
		}
	}
	if want := []string{"corpse", "earlier", "uncovered"}; !reflect.DeepEqual(eff.stops, want) {
		t.Fatalf("stops = %v, want %v", eff.stops, want)
	}
	if s := inv.corpses; s.candidates != 4 || s.filtered != 1 || s.confirms != 3 {
		t.Fatalf("corpse stats = %+v, want 4 candidates, 1 filtered, 3 confirms", s)
	}
}

// Kills: a pre-boot close on a stale ServerAbsent. The lane's absent server
// only sends the cleaner to a fresh listing; the pre-boot reap runs only if
// that listing finds the server absent too.
func TestDeadRuntimeCleanup_ServerAbsentFromLaneReconfirmsFresh(t *testing.T) {
	boot := time.Now().Add(-10 * time.Minute)
	withHostBootTime(t, boot, nil)
	pre := reaperRow("s-preboot", "w-1", "active")
	pre.CreatedAt = boot.Add(-time.Hour)
	absent := reaperFixtureState([]beads.Bead{pre}, nil)
	absent.list = reaperListServerAbsent

	t.Run("server back by the tick", func(t *testing.T) {
		back := reaperFixtureState([]beads.Bead{pre}, nil)
		eff := runLaneReapers(t, absent, back)
		if len(eff.closes) != 0 {
			t.Fatalf("closed %v on the lane's absent server; the fresh listing found it running", eff.closes)
		}
		if eff.source != inventorySourceLive || freshListings(eff) != 1 {
			t.Fatalf("source = %s with %d fresh listing(s), want one fresh re-listing", eff.source, freshListings(eff))
		}
	})
	t.Run("still absent", func(t *testing.T) {
		eff := runLaneReapers(t, absent, absent)
		if !reflect.DeepEqual(eff.closes, map[string]string{"s-preboot": "stale-session"}) {
			t.Fatalf("closes = %v, want the pre-boot row reaped on the fresh absent listing", eff.closes)
		}
	})
}

// Kills: a closed-bead Stop on the lane's attribution alone. The lane saw the
// runtime bound to a closed bead; the fresh GetMeta cannot be read, so the
// runtime is left alone, exactly as without a lane.
func TestReapClosedBoundRuntimes_CachedOwnerNeverAuthorizesStop(t *testing.T) {
	rows := []beads.Bead{closedReaperRow("gm-closed")}
	seen := reaperFixtureState(rows, map[string]reaperRuntime{"mayor": {incarnation: "m:1", sessionID: "gm-closed"}})
	now := seen.clone()
	now.runtimes["mayor"] = reaperRuntime{incarnation: "m:1", sessionID: "gm-closed", metaErr: true}

	lr := newLaneReaperRun(t, seen)
	lr.pass(seen)
	if inv := lr.cr.inventoryViewForTick(); inv == nil {
		t.Fatal("no view")
	} else if owner, ok := inv.owner("mayor"); !ok || owner != "gm-closed" {
		t.Fatalf("owner(mayor) = %q, %v; want the lane's attribution gm-closed", owner, ok)
	}
	eff, _ := lr.reap(t, now)
	if len(eff.stops) != 0 {
		t.Fatalf("stopped %v with no fresh attribution; calls=%v", eff.stops, eff.calls)
	}
	if got := confirmsOn(eff, "closed", "mayor"); !reflect.DeepEqual(got, []string{"GetMeta(GC_SESSION_ID)=error"}) {
		t.Fatalf("closed-bead calls on mayor = %v, want one fresh GetMeta", got)
	}
}

// Kills: acting on a stale owner after a name is reused. The lane saw the
// name's runtime bound to a closed bead; by the tick the name runs a new
// incarnation for an open bead, and the fresh read wins.
func TestReapClosedBoundRuntimes_FreshOwnerOverridesCache(t *testing.T) {
	rows := []beads.Bead{closedReaperRow("gm-closed"), reaperRow("gm-open", "mayor", "active")}
	seen := reaperFixtureState(rows, map[string]reaperRuntime{"mayor": {incarnation: "m:1", sessionID: "gm-closed"}})
	now := reaperFixtureState(rows, map[string]reaperRuntime{"mayor": {incarnation: "m:2", sessionID: "gm-open"}})

	eff := runLaneReapers(t, seen, now)
	if len(eff.stops) != 0 {
		t.Fatalf("stopped %v on the previous incarnation's owner; calls=%v", eff.stops, eff.calls)
	}
	if got := confirmsOn(eff, "closed", "mayor"); !reflect.DeepEqual(got, []string{"GetMeta(GC_SESSION_ID)=gm-open"}) {
		t.Fatalf("closed-bead calls on mayor = %v, want one fresh GetMeta naming gm-open", got)
	}
}

// Kills: an owner or live pane served from a pass whose listing cannot vouch
// for it. A stale, unprimed or partial snapshot filters nothing, and every
// effect it leads to is one legacy takes on the current state.
func TestRuntimeReapers_StaleUnprimedOrPartialViewNominatesNothingDestructive(t *testing.T) {
	rows := []beads.Bead{reaperRow("s-w", "w", "active"), closedReaperRow("gm-closed")}
	// What the lane saw: a corpse bound to a closed bead. What is true at
	// the tick: a live pane bound to the open row.
	alarming := reaperFixtureState(rows, map[string]reaperRuntime{"w": {incarnation: "w:1", dead: true, sessionID: "gm-closed"}})
	calm := reaperFixtureState(rows, map[string]reaperRuntime{"w": {incarnation: "w:2", sessionID: "s-w"}})
	// The reverse: the lane saw a live pane owned by the open row; by the
	// tick the name holds a corpse bound to a closed bead.
	reassuring := calm.clone()
	reassuring.runtimes["w"] = reaperRuntime{incarnation: "w:1", sessionID: "s-w"}
	doomed := reaperFixtureState(rows, map[string]reaperRuntime{"w": {incarnation: "w:2", dead: true, sessionID: "gm-closed"}})

	cases := []struct {
		name string
		mod  func(reaperState) reaperState
	}{
		{name: "unprimed (unattested backend)", mod: func(s reaperState) reaperState { s.unattested = true; return s }},
		{name: "partial (never complete)", mod: func(s reaperState) reaperState {
			s.list, s.hidden = reaperListPartial, map[string]bool{"other": true}
			return s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, pair := range []struct {
				what      string
				seen, now reaperState
			}{
				{"alarming view, calm world", alarming, calm},
				{"reassuring view, doomed world", reassuring, doomed},
			} {
				lr := newLaneReaperRun(t, tc.mod(pair.seen.clone()))
				lr.pass(tc.mod(pair.seen.clone()))
				inv := lr.cr.inventoryViewForTick()
				if inv == nil {
					t.Fatalf("%s: no view", pair.what)
				}
				if inv.livePane("w") {
					t.Fatalf("%s: livePane(w) served from an unprimed pass", pair.what)
				}
				if owner, ok := inv.owner("w"); ok {
					t.Fatalf("%s: owner(w) = %q served from an unprimed pass", pair.what, owner)
				}
				lane, _ := lr.reap(t, pair.now)
				legacy := runLegacyReapers(t, pair.now)
				if !reflect.DeepEqual(lane.stops, legacy.stops) || !reflect.DeepEqual(lane.closes, legacy.closes) {
					t.Fatalf("%s: lane stops=%v closes=%v, legacy stops=%v closes=%v", pair.what, lane.stops, lane.closes, legacy.stops, legacy.closes)
				}
			}
		})
	}
	t.Run("stale", func(t *testing.T) {
		lr := newLaneReaperRun(t, alarming)
		clk := &clock.Fake{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
		useInventoryClock(lr.cr, clk)
		lr.pass(alarming)
		clk.Advance(2*inventoryLaneInterval + time.Second)
		eff, inv := lr.reap(t, calm)
		if inv != nil || len(eff.stops) != 0 || len(eff.closes) != 0 {
			t.Fatalf("stale view = %v, stops=%v closes=%v; want no view and no effect", inv != nil, eff.stops, eff.closes)
		}
	})
}

// Kills: a respawned runtime judged by its predecessor's owner. When the
// pass that lists the new incarnation has not attributed it (its inventory
// failed, or the attribution read failed), the previous incarnation's owner
// must not filter it, so the fresh GetMeta finds it bound to a closed bead.
func TestRuntimeReapers_RespawnNeverJudgedByPredecessorOwner(t *testing.T) {
	rows := []beads.Bead{reaperRow("gm-open", "mayor", "active"), closedReaperRow("gm-closed")}
	predecessor := reaperFixtureState(rows, map[string]reaperRuntime{"mayor": {incarnation: "m:1", sessionID: "gm-open"}})
	successor := reaperFixtureState(rows, map[string]reaperRuntime{"mayor": {incarnation: "m:2", sessionID: "gm-closed"}})

	for _, tc := range []struct {
		name string
		mod  func(reaperState) reaperState
	}{
		{name: "inventory failed", mod: func(s reaperState) reaperState { s.inventoryErr = true; return s }},
		{name: "attribution unread", mod: func(s reaperState) reaperState { return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lr := newLaneReaperRun(t, predecessor)
			lr.pass(predecessor)
			if owner, ok := lr.cr.inventoryViewForTick().owner("mayor"); !ok || owner != "gm-open" {
				t.Fatalf("first pass owner(mayor) = %q, %v; want gm-open", owner, ok)
			}
			if tc.name == "attribution unread" {
				// The successor's attribution read fails this pass.
				lr.cr.inventoryLane.attributing = true
				t.Cleanup(func() { lr.cr.inventoryLane.attributing = false })
			}
			lr.pass(tc.mod(successor.clone()))
			eff, inv := lr.reap(t, successor)
			if owner, ok := inv.owner("mayor"); ok {
				t.Fatalf("owner(mayor) = %q after a respawn the pass did not attribute", owner)
			}
			if !reflect.DeepEqual(eff.stops, []string{"mayor"}) {
				t.Fatalf("stops = %v, want the successor bound to a closed bead reaped; calls=%v", eff.stops, eff.calls)
			}
		})
	}
}

// Kills: a view that serves a name its pass did not list. A partial pass on a
// primed backend keeps an unlisted name's Listed=Yes from the pass before;
// neither its owner nor its live pane may be served as this pass's.
func TestRuntimeInventoryView_ServesOnlyNamesThisPassListed(t *testing.T) {
	rows := []beads.Bead{reaperRow("gm-open", "w", "active")}
	first := reaperFixtureState(rows, map[string]reaperRuntime{"w": {incarnation: "w:1", sessionID: "gm-open"}})
	second := first.clone()
	second.list, second.hidden = reaperListPartial, map[string]bool{"w": true}

	lr := newLaneReaperRun(t, first)
	lr.pass(first)
	if owner, ok := lr.cr.inventoryViewForTick().owner("w"); !ok || owner != "gm-open" {
		t.Fatalf("first pass owner(w) = %q, %v; want gm-open", owner, ok)
	}
	lr.pass(second)
	inv := lr.cr.inventoryViewForTick()
	if obs := inv.snap.ByName["w"]; obs.Listed.Value != ObsYes || !inv.snap.Primed[""] {
		t.Fatalf("setup: w Listed=%v primed=%v, want the earlier Yes kept on a primed backend", obs.Listed.Value, inv.snap.Primed[""])
	}
	if owner, ok := inv.owner("w"); ok {
		t.Fatalf("owner(w) = %q from a pass that did not list w", owner)
	}
	if inv.livePane("w") {
		t.Fatal("livePane(w) from a pass that did not list w")
	}
}
