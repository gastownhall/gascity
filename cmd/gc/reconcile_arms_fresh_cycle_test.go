package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// freshCase is a live Wake row in fresh wake mode processing gw-old, which
// the allocation reassigned to gw-new, its runtime read current.
func freshCase(t *testing.T, meta ...string) *healCase {
	t.Helper()
	base := append([]string{"state", "active", "wake_mode", "fresh", session.CurrentBeadIDKey, "gw-old"}, meta...)
	c := newHealCase(t, livenessAlive, desireWake, base...)
	c.a.Snapshot.Entries[c.k].AssignedWork = &assignedWorkView{BeadID: "gw-new", RequiresFreshCycle: true}
	c.w.Now, c.w.ObsMaxAge = censusNow, observeMaxAge
	id := runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
	c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": {DeadKnown: true, Identity: id}}, completeBackend("tmux", "s-heal"))
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(c.w.Census.Rows[c.k].Info): {TP: TemplateParams{TemplateName: "worker"}}}}
	return c
}

// TestArmFreshCycle (SESS-612; CONTRACT v5.10q): a live, own, Wake row in
// fresh wake mode whose assigned work needs a fresh cycle, and which has not
// claimed that work itself, gets A13's stop with the fresh-cycle finalize;
// any of those missing, or an operator-dormant row, decides no cycle.
func TestArmFreshCycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta []string
		set  func(*healCase)
		want bool
	}{
		{name: "reassigned", want: true},
		{name: "not fresh mode", meta: []string{"wake_mode", "resume"}},
		{name: "self-claimed", meta: []string{"current_claim_bead_id", "gw-new"}},
		{name: "no fresh cycle needed", set: func(c *healCase) { c.a.Snapshot.Entries[c.k].AssignedWork.RequiresFreshCycle = false }},
		{name: "not wanted awake", set: func(c *healCase) { c.a.Snapshot.Entries[c.k].Desired = desireKeep }},
		{name: "a runtime not its own", meta: []string{"instance_token", "tok-other"}},
		{name: "suspended by an operator", meta: []string{"state", "suspended", "sleep_intent", "user-hold"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := freshCase(t, tc.meta...)
			if tc.set != nil {
				tc.set(c)
			}
			it, ok := armRestartRequested(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true})
			if ok != tc.want || ok && (it.Kind != intentStop || it.Reason != decideFreshCycle || it.Finish == nil) {
				t.Fatalf("armRestartRequested = (%q, %q) %v, want a fresh cycle %t", it.Kind, it.Reason, ok, tc.want)
			}
		})
	}
}

// TestFreshCycleDefers (SESS-612): legacy's defer on the previous bead,
// read in the effect: open, or closed before this incarnation woke, defers;
// a lookup that failed or expired defers, as a failed read proves nothing
// (the #7879 review); no previous bead, or one no store holds, cycles.
func TestFreshCycleDefers(t *testing.T) {
	closed := beads.Bead{ID: "gw-old", Status: "closed", Metadata: map[string]string{"closed_at": censusNow.Add(-time.Hour).Format(time.RFC3339Nano)}}
	awakeAfter := session.Info{AwakeStartedAt: censusNow.Add(-time.Minute).Format(time.RFC3339Nano)}
	awakeBefore := session.Info{AwakeStartedAt: censusNow.Add(-2 * time.Hour).Format(time.RFC3339Nano)}
	for _, tc := range []struct {
		name   string
		row    session.Info
		prev   freshCyclePrev
		err    error
		defers bool
	}{
		{"the previous bead open", awakeBefore, freshCyclePrev{found: true, bead: beads.Bead{ID: "gw-old", Status: "in_progress"}}, nil, true},
		{"closed after this incarnation woke", awakeBefore, freshCyclePrev{found: true, bead: closed}, nil, false},
		{"closed before this incarnation woke", awakeAfter, freshCyclePrev{found: true, bead: closed}, nil, true},
		{"its wake time unreadable", session.Info{}, freshCyclePrev{found: true, bead: closed}, nil, false},
		{"the lookup expired", awakeBefore, freshCyclePrev{}, errProbeExpired, true},
		{"the lookup failed", awakeBefore, freshCyclePrev{}, errors.New("store unreachable"), true},
		{"no store holds it", awakeBefore, freshCyclePrev{}, nil, false},
		{"no previous bead", awakeBefore, freshCyclePrev{none: true}, nil, false},
	} {
		if got := freshCycleDefers(tc.row, tc.prev, tc.err); got != tc.defers {
			t.Errorf("%s: freshCycleDefers = %t, want %t", tc.name, got, tc.defers)
		}
	}
}

// TestFreshCycleFinishes (SESS-612): the cycle's finalize is the restart
// handoff (the conversation reset, the row asleep, the key rotated or
// cleared), anchored to the new bead, and never a close.
func TestFreshCycleFinishes(t *testing.T) {
	f := finalizeFacts{Now: censusNow, CloseIfUnassigned: true, AssignedBead: "gw-new"}
	for name, c := range map[string]finalizeChoice{"mint": freshCycleMintKey(f), "clear": freshCycleClearKey(f)} {
		if c.CloseSeat || c.Patch[session.CurrentBeadIDKey] != "gw-new" || c.Patch["state"] != "asleep" || c.Patch["continuation_reset_pending"] != "true" {
			t.Fatalf("%s: %+v, want the asleep handoff anchored to gw-new", name, c)
		}
	}
	if k := freshCycleMintKey(f).Patch["session_key"]; k == "" {
		t.Fatal("mint: no new session key")
	}
	if k, ok := freshCycleClearKey(f).Patch["session_key"]; !ok || k != "" {
		t.Fatalf("clear: session_key %q (%t), want cleared", k, ok)
	}
}

// freshSim is a converged sim city whose live pool row gc-1, in fresh wake
// mode and awake since two hours ago, processes gw-old, while gc-1 is now
// assigned gw-new (open, unclaimed).
func freshSim(t *testing.T, old beads.Bead) *sim {
	t.Helper()
	row := poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-2*time.Hour),
		"wake_mode", "fresh", "awake_started_at", rowAt(-2*time.Hour), session.CurrentBeadIDKey, "gw-old")
	next := beads.Bead{ID: "gw-new", Title: "work", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}
	s := scripted(t, []string{"gc-1"}, row, old, next)
	s.cfg.Agents[0].WakeMode = "fresh" // the template's, which A7 keeps on the row
	s.settle()
	s.advance(simPatrol)
	s.settle()
	s.settle()
	return s
}

// TestSimFreshCycleOnReassign (SESS-612): the reassigned row whose previous
// bead closed after it woke is stopped once, handed off and started again
// as a new incarnation on a fresh conversation, anchored to the new bead,
// and is not cycled again.
func TestSimFreshCycleOnReassign(t *testing.T) {
	old := beads.Bead{ID: "gw-old", Title: "done", Type: "task", Status: "closed", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker", "closed_at": rowAt(-time.Hour)}}
	s := freshSim(t, old)
	for range 6 {
		s.settle()
		s.advance(simPatrol)
	}
	b, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	s.sp.mu.Lock()
	kills := len(s.sp.kills)
	s.sp.mu.Unlock()
	if m := b.Metadata; kills != 1 || m[session.CurrentBeadIDKey] != "gw-new" || m["instance_token"] == "tok-1" || !s.livePane(b) {
		t.Fatalf("kills %d, row %v; want one cycle onto gw-new into a new incarnation", kills, m)
	}
	s.noViolations(t)
}

// TestSimFreshCycleDefersWhilePreviousBeadOpen (SESS-612): while the
// previous bead is still open the cycle defers in the effect, writes
// nothing, stops nothing, and is not re-proposed every pass (its row
// backoff).
func TestSimFreshCycleDefersWhilePreviousBeadOpen(t *testing.T) {
	old := beads.Bead{ID: "gw-old", Title: "open", Type: "task", Status: "in_progress", Assignee: "gc-other", Metadata: map[string]string{"gc.routed_to": "worker"}}
	s := freshSim(t, old)
	count := func() (cycles, deferred int) {
		for _, r := range s.round {
			if r.it.Reason == decideFreshCycle {
				cycles++
				if r.res.Outcome == settledRefused && r.res.Cause == causeFreshCycleDefer {
					deferred++
				}
			}
		}
		return cycles, deferred
	}
	before, _ := count()
	for range 4 { // within the defer's row backoff: no pass re-proposes it
		s.settle()
	}
	cycles, deferred := count()
	if before == 0 || cycles != before || deferred != cycles || len(s.sp.kills) != 0 || s.gc1()[session.CurrentBeadIDKey] != "gw-old" {
		t.Fatalf("cycles %d then %d (deferred %d), kills %d, row %v; want deferred cycles only, none re-proposed in the backoff, nothing stopped or written", before, cycles, deferred, len(s.sp.kills), s.gc1())
	}
	s.noViolations(t)
}

// TestFreshCycleProbeReadsOnlyForTheCycle (SESS-612): the stop verb's first
// section reads the previous bead only for the fresh cycle; any other stop
// reads nothing, so a stop never waits on, or refuses over, a work read it
// does not decide on.
func TestFreshCycleProbeReadsOnlyForTheCycle(t *testing.T) {
	row := session.Info{CurrentlyProcessingBeadID: "gw-old"}
	got, err := freshCycleProbe(context.Background(), effectReads{}, txView{It: intent{Reason: decideStopVerb + "idle"}, Row: row})
	if err != nil || got.none || got.found || got.bead.ID != "" {
		t.Fatalf("another stop's probe = %+v, %v; want no read", got, err)
	}
	if _, err := freshCycleProbe(context.Background(), effectReads{}, txView{It: intent{Reason: decideFreshCycle}, Row: row}); err == nil {
		t.Fatal("the cycle's probe read nothing over unusable legs, want the read's error")
	}
}

// TestFreshCycleBackoffHoldsOnlyTheCycle (the #7879 review): the cycle
// settles on its own backoff key, and while that record is live A13 falls
// through past the cycle (so the row's later arms decide), and is decided
// again when it releases.
func TestFreshCycleBackoffHoldsOnlyTheCycle(t *testing.T) {
	c := freshCase(t)
	if got := settledBackoffKey(intentStop, decideFreshCycle, c.k); got != freshCycleBackoffKey(c.k) || got == rowBackoffKey(c.k) {
		t.Fatalf("the cycle settles on %q, want its own key", got)
	}
	c.w.Backoff = map[string]backoffRecord{freshCycleBackoffKey(c.k): {Consecutive: 1, Until: censusNow.Add(time.Minute), Cause: causeFreshCycleDefer}}
	r := &rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}
	if it, ok := armRestartRequested(r); ok {
		t.Fatalf("armRestartRequested = (%q, %q) under a live cycle backoff, want a fall-through", it.Kind, it.Reason)
	}
	if !r.next.Equal(censusNow.Add(time.Minute)) {
		t.Fatalf("next %v, want the backoff's release %v", r.next, censusNow.Add(time.Minute))
	}
	c.w.Backoff[freshCycleBackoffKey(c.k)] = backoffRecord{Consecutive: 1, Until: censusNow.Add(-time.Second)}
	if _, ok := armRestartRequested(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}); !ok {
		t.Fatal("no cycle once the backoff released")
	}
}

// TestSimSuspendDuringADeferredFreshCycle (the #7879 review): an operator's
// suspend of a row whose fresh cycle is deferred stops its runtime on the
// first pass after it: the deferral holds nothing but the cycle.
func TestSimSuspendDuringADeferredFreshCycle(t *testing.T) {
	old := beads.Bead{ID: "gw-old", Title: "open", Type: "task", Status: "in_progress", Assignee: "gc-other", Metadata: map[string]string{"gc.routed_to": "worker"}}
	s := freshSim(t, old)
	deferred := false
	for _, r := range s.round {
		deferred = deferred || r.it.Reason == decideFreshCycle && r.res.Cause == causeFreshCycleDefer
	}
	if !deferred {
		t.Fatal("the cycle was not deferred, so the probe proves nothing")
	}
	s.ask("state", "suspended", "sleep_intent", "user-hold")
	s.settle()
	if len(s.sp.kills) != 1 {
		t.Fatalf("kills %d after the suspend's first pass, want its runtime stopped", len(s.sp.kills))
	}
	s.noViolations(t)
}

// TestPinnedRuntimeMarkerHoldsNoCycle (the #7879 review): a pinned
// configured named row with no explicit reset ignores its runtime's
// restart marker, so the marker is no pending restart: the pass clears it
// (mc-nm6yt), and SESS-612's cycle decides once it is gone.
func TestPinnedRuntimeMarkerHoldsNoCycle(t *testing.T) {
	c := freshCase(t, "configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true")
	id := runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
	c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": {DeadKnown: true, Identity: id, RestartRequested: true}}, completeBackend("tmux", "s-heal"))
	r := &rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}
	if r.restartPending() {
		t.Fatal("a pinned row's runtime marker reads as a pending restart")
	}
	if it, ok := armRestartRequested(r); !ok || it.Kind != intentMarkerClear {
		t.Fatalf("armRestartRequested = (%q, %q) %v, want the marker cleared first (mc-nm6yt)", it.Kind, it.Reason, ok)
	}
	c.w.Obs = newObserveCache().publish(censusNow, map[string]InventoryAttrs{"s-heal": {DeadKnown: true, Identity: id}}, completeBackend("tmux", "s-heal"))
	r = &rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}
	if it, ok := armRestartRequested(r); !ok || it.Reason != decideFreshCycle {
		t.Fatalf("armRestartRequested = (%q, %q) %v, want the fresh cycle once the marker is gone", it.Kind, it.Reason, ok)
	}
}
