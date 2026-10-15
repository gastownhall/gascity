package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// restartAsked is Manager.RequestFreshRestart's write, which `gc session
// reset`, `gc runtime request-restart` and `gc handoff` all make.
var restartAsked = []string{"restart_requested", "true", "continuation_reset_pending", "true"}

// converged is scripted over rows, run until the rows' metadata and
// baseline settle, so the arms after A8 decide them.
func converged(t *testing.T, live []string, rows ...beads.Bead) *sim {
	t.Helper()
	s := scripted(t, live, rows...)
	s.settle()
	s.advance(simPatrol)
	s.settle()
	s.settle()
	return s
}

// ask writes kv to the city row gc-1 as an outside writer, and lets the
// cache see it.
func (s *sim) ask(kv ...string) {
	s.legs[0].cache.ApplyEvent("bead.updated", s.operator("gc-1", kv...))
}

// TestSimRestartRequestedRestartsALiveRow (CONTRACT v5.10q, A13; SESS-520,
// 524, 525): a live row an operator or its agent asked to restart is
// stopped, handed off (its key rotated, the reset committed, the marker
// consumed) and started again as a new incarnation. Under handoff and
// request-restart the agent's runtime carries GC_RESTART_REQUESTED too,
// and the handoff's wait returns once the stop lands instead of timing out.
func TestSimRestartRequestedRestartsALiveRow(t *testing.T) {
	for _, path := range []string{"gc session reset", "gc runtime request-restart", "gc handoff"} {
		t.Run(path, func(t *testing.T) {
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
			if path != "gc session reset" {
				if err := newDrainOps(s.sp).setRestartRequested("s-gc-1"); err != nil {
					t.Fatal(err)
				}
			}
			s.ask(restartAsked...)
			s.settle()
			m := s.gc1()
			if m["state"] != "asleep" || m["restart_requested"] != "" || m["session_key"] == "k-1" || m["continuation_reset_pending"] != "true" || m[session.ResetCommittedAtKey] == "" || m["started_config_hash"] != "" {
				t.Fatalf("after the stop: row %v, want asleep, handed off, its key rotated", m)
			}
			if b, _ := s.legs[0].backing.Get("gc-1"); len(s.sp.kills) != 1 || s.sp.rts["s-gc-1"] != nil {
				t.Fatalf("kills %d, runtime %v; want the old runtime stopped (row %v)", len(s.sp.kills), s.sp.rts["s-gc-1"], b.Metadata)
			}
			if path == "gc handoff" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				pane := paneEnv{s.sp}
				if code := waitForControllerRestart(ctx, newDrainOps(pane), pane, "s-gc-1", time.Second, io.Discard); code != 0 {
					t.Fatalf("the handoff's wait = %d, want 0 once the stop landed", code)
				}
			}
			s.advance(simPatrol)
			s.settle()
			s.settle()
			b, err := s.legs[0].backing.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			if m := b.Metadata; m["instance_token"] == "tok-1" || m["generation"] == "1" || !s.livePane(b) || m["restart_requested"] != "" {
				t.Fatalf("after the restart: row %v, live %t; want a new live incarnation", m, s.livePane(b))
			}
			s.noViolations(t)
		})
	}
}

// TestSimRestartRequestedStopsAnAttachedRow (CONTRACT v5.10u): the restart
// is the operator's or agent's intent, so a human attached to the live row
// does not defer it (legacy parity): the row is stopped and handed off.
func TestSimRestartRequestedStopsAnAttachedRow(t *testing.T) {
	s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
	s.sp.mu.Lock()
	s.sp.rts["s-gc-1"].attached = true
	s.sp.mu.Unlock()
	s.ask(restartAsked...)
	s.settle()
	if m := s.gc1(); m["restart_requested"] != "" || m[session.ResetCommittedAtKey] == "" || len(s.sp.kills) != 1 {
		t.Fatalf("row %v, kills %d; want the attached row stopped and handed off", m, len(s.sp.kills))
	}
	s.noViolations(t)
}

// TestSimRestartRequestedHandsOffAGoneRow (A13; SESS-526): a row whose
// runtime is already gone is handed off without a stop, ahead of its start,
// and started a pass later.
func TestSimRestartRequestedHandsOffAGoneRow(t *testing.T) {
	s := scripted(t, nil, poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-1", "started_config_hash", "h-1"), gc1Work())
	s.ask(restartAsked...)
	handedOff := false
	for range 6 {
		s.settle()
		if m := s.gc1(); m["restart_requested"] == "" && !handedOff {
			handedOff = true
			if m[session.ResetCommittedAtKey] == "" || m["started_config_hash"] != "" || len(s.sp.kills) != 0 {
				t.Fatalf("row %v, kills %d; want it handed off with nothing stopped", m, len(s.sp.kills))
			}
		}
		s.advance(simPatrol)
	}
	b, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !handedOff || !s.livePane(b) {
		t.Fatalf("row %v, handed off %t, live %t; want it handed off, then started", b.Metadata, handedOff, s.livePane(b))
	}
	s.noViolations(t)
}

// TestSimRestartRequestedLeavesHeldRows (A13; SESS-521, A2, A4): a pinned
// named row with no explicit reset only has the marker cleared and keeps
// its runtime; an operator-dormant row and a kill-fenced row are never
// restarted.
func TestSimRestartRequestedLeavesHeldRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		meta  []string
		asked []string
		clear bool // the marker is consumed
	}{
		{"a pinned named row, a collateral request", []string{"configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true"}, []string{"restart_requested", "true"}, true},
		{"suspended by an operator", []string{"state", "suspended", "sleep_intent", "user-hold"}, restartAsked, false},
		{"kill-fenced", []string{"state", "asleep", "state_reason", session.KillPendingReason, "sleep_reason", string(session.SleepReasonKilled), "slept_at", rowAt(0)}, restartAsked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := append([]string{"instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-time.Hour)}, tc.meta...)
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", meta...), gc1Work())
			s.ask(tc.asked...)
			s.settle()
			s.settle()
			m := s.gc1()
			if m["session_key"] != "k-1" || m[session.ResetCommittedAtKey] != "" {
				t.Fatalf("row %v, want no restart handoff", m)
			}
			if tc.clear != (m["restart_requested"] == "") {
				t.Fatalf("restart_requested %q, want consumed %t", m["restart_requested"], tc.clear)
			}
			if tc.clear && len(s.sp.kills) != 0 {
				t.Fatalf("%d kills, want the pinned runtime left running", len(s.sp.kills))
			}
			s.noViolations(t)
		})
	}
}

// TestRestartHandoffFinishes (A13; SESS-524): the restart's finalize hands
// off as legacy does, rotating the key for a provider that takes one and
// clearing it for any other, marks the row asleep, and never closes.
func TestRestartHandoffFinishes(t *testing.T) {
	f := finalizeFacts{Now: censusNow, CloseIfUnassigned: true}
	mint, cleared := restartHandoffMintKey(f), restartHandoffClearKey(f)
	for name, c := range map[string]finalizeChoice{"mint": mint, "clear": cleared} {
		if c.CloseSeat || c.AsSleep || c.Patch["restart_requested"] != "" || c.Patch["state"] != "asleep" || c.Patch["continuation_reset_pending"] != "true" {
			t.Fatalf("%s: %+v, want the asleep handoff", name, c)
		}
	}
	if k, ok := mint.Patch["session_key"]; !ok || k == "" {
		t.Fatalf("mint: session_key %q, want a new key", k)
	}
	if k, ok := cleared.Patch["session_key"]; !ok || k != "" {
		t.Fatalf("clear: session_key %q (%t), want cleared", k, ok)
	}
}

// paneEnv is the sim's provider with its env living in the pane, as tmux's
// does: a gone runtime answers its env reads session-gone, and runs no
// longer.
type paneEnv struct{ *simProvider }

func (p paneEnv) GetMeta(name, key string) (string, error) {
	if !p.running(name) {
		return "", fmt.Errorf("reading %s: %w", key, runtime.ErrSessionNotFound)
	}
	return p.simProvider.GetMeta(name, key)
}

func (p paneEnv) IsRunning(name string) bool { return p.running(name) }

func (p paneEnv) running(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rts[name]
	return rt != nil && !rt.corpse
}

// TestArmRestartRequested (A13; SESS-520, 521, 526): a row asked to restart
// gets the stop verb with the restart finalize, its key minted for a
// provider that takes one and cleared otherwise; a gone runtime is
// finalized without a stop; an operator-dormant row, and a row AL1 drains,
// are not A13's; a pinned
// named row with its runtime up and no explicit reset only has the marker
// cleared, and is restarted with one, or with its runtime gone.
func TestArmRestartRequested(t *testing.T) {
	pinned := []string{"configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true"}
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		meta     []string
		mints    bool
		want     string // the reason, "" for no decision
		finalize bool
		drain    bool // AL1 drains the row
	}{
		{name: "not asked", liveness: livenessAlive, meta: []string{"state", "active"}},
		{name: "asked, live", liveness: livenessAlive, meta: append([]string{"state", "active"}, restartAsked...), want: decideRestartRequested},
		{name: "asked, live, its provider takes an id", liveness: livenessAlive, meta: append([]string{"state", "active"}, restartAsked...), mints: true, want: decideRestartRequested},
		{name: "asked, a dead pane", liveness: livenessDead, meta: append([]string{"state", "active"}, restartAsked...), want: decideRestartRequested},
		{name: "asked, gone", liveness: livenessGone, meta: append([]string{"state", "asleep"}, restartAsked...), want: decideRestartRequested, finalize: true},
		{name: "asked, suspended by an operator", liveness: livenessGone, meta: append([]string{"state", "suspended", "sleep_intent", "user-hold"}, restartAsked...)},
		{name: "asked, killed", liveness: livenessGone, meta: append([]string{"state", "asleep", "sleep_reason", "killed"}, restartAsked...)},
		{name: "asked, drained", liveness: livenessAlive, meta: append([]string{"state", "active"}, restartAsked...), drain: true},
		{name: "pinned, collateral, live", liveness: livenessAlive, meta: append([]string{"state", "active", "restart_requested", "true"}, pinned...), want: decideRestartPinnedClear},
		{name: "pinned, collateral, a dead pane", liveness: livenessDead, meta: append([]string{"state", "active", "restart_requested", "true"}, pinned...), want: decideRestartPinnedClear},
		{name: "pinned, an explicit reset", liveness: livenessAlive, meta: append(append([]string{"state", "active"}, restartAsked...), pinned...), want: decideRestartRequested},
		{name: "pinned, collateral, gone", liveness: livenessGone, meta: append([]string{"state", "asleep", "restart_requested", "true"}, pinned...), want: decideRestartRequested, finalize: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, map[bool]desire{false: desireWake, true: desireDrain}[tc.drain], tc.meta...)
			info := c.w.Census.Rows[c.k].Info
			tp := TemplateParams{TemplateName: "worker"}
			if tc.mints {
				tp.ResolvedProvider = &config.ResolvedProvider{Name: "claude", SessionIDFlag: "--session-id"}
			}
			c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			it, ok := armRestartRequested(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true})
			if got := map[bool]string{true: it.Reason}[ok]; got != tc.want {
				t.Fatalf("armRestartRequested = %q (%v), want %q", got, ok, tc.want)
			}
			switch {
			case !ok:
			case tc.want == decideRestartPinnedClear:
				if it.Kind != intentRowHeal || len(it.Patch) != 1 || it.Patch["restart_requested"] != "" {
					t.Fatalf("intent %+v, want the marker's clear alone", it)
				}
			default:
				fin := it.Finish(finalizeFacts{Now: censusNow})
				if it.Kind != intentStop || it.Finalize != tc.finalize || fin.Patch["restart_requested"] != "" || (fin.Patch["session_key"] != "") != tc.mints {
					t.Fatalf("intent %+v, finalize %v; want the restart stop (finalize %t, mint %t)", it, fin.Patch, tc.finalize, tc.mints)
				}
			}
		})
	}
}

// TestRestartRequestedGoneRowIsA13s (the #7823 review): a committed row
// asked to restart whose runtime is gone, or whose dead pane A12
// classified, is A13's, not A6's dead-runtime heal (whose composed start
// would bring up a runtime with the marker still set), whatever AL1 wants
// of it short of a drain.
func TestRestartRequestedGoneRowIsA13s(t *testing.T) {
	for _, d := range []desire{desireWake, desireKeep, desireSleep} {
		for _, live := range []rowLiveness{livenessGone, livenessDead} {
			t.Run(fmt.Sprintf("%v, %v", d, live), func(t *testing.T) {
				c := newHealCase(t, live, d, append([]string{"state", "active", "session_key", "k-1", "last_woke_at", rowAt(-10 * time.Second)}, restartAsked...)...)
				if live == livenessDead { // A12 classified the dead pane
					id := runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
					c.w.Observed = map[rowKey]rowObservation{c.k: {Liveness: livenessDead, Identity: id}}
					c.w.Classified = map[rowKey]classifiedRuntime{c.k: {Token: c.k.ID + "/tok-3"}}
				}
				if it := c.decide(); it.Reason != decideRestartRequested || it.Heal != nil {
					t.Fatalf("decideRow = (%q, %q), heal %v; want A13's restart alone", it.Kind, it.Reason, it.Heal != nil)
				}
			})
		}
	}
}

// TestSimRestartRequestedOverAGoneRuntimeStartsOnce (the #7823 review): a
// committed Wake row whose runtime is gone when its restart is asked (an
// agent that exits after `gc runtime request-restart` on a provider that
// leaves no pane) is handed off and started once: no runtime is stopped,
// and no wake failure or churn accrues, at any wake age.
func TestSimRestartRequestedOverAGoneRuntimeStartsOnce(t *testing.T) {
	for _, age := range []time.Duration{10 * time.Second, 2 * time.Minute, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-age)), gc1Work())
			s.ask(restartAsked...)
			s.sp.mu.Lock()
			s.sp.drop("s-gc-1")
			s.sp.mu.Unlock()
			for range 6 {
				s.settle()
				s.advance(simPatrol)
			}
			s.sp.mu.Lock()
			starts, kills := len(s.sp.starts), len(s.sp.kills)
			s.sp.mu.Unlock()
			m := s.gc1()
			if starts != 1 || kills != 0 || m["restart_requested"] != "" {
				t.Fatalf("starts %d, kills %d, row %v; want one start and no stop", starts, kills, m)
			}
			if a, c := m["wake_attempts"], m["churn_count"]; a != "" && a != "0" || c != "" && c != "0" {
				t.Fatalf("row %v: the requested restart accrued a death", m)
			}
			s.noViolations(t)
		})
	}
}

// TestRestartStopKeepsTheTokenLeg (CONTRACT v5.10u, the #7823 review W2):
// A13's restart waives only the attach leg; L2, the row's own runtime
// read Current, stays, so a runtime whose identity read failed is never
// stopped by name.
func TestRestartStopKeepsTheTokenLeg(t *testing.T) {
	legs := stopLegs(intent{Kind: intentStop, Reason: decideRestartRequested})
	if legs&legToken == 0 || legs&legAttach != 0 {
		t.Fatalf("stopLegs = %b, want L2 kept and L3 waived", legs)
	}
}

// TestRestartRequestedHealComposesNoStart (the #7823 review): any other A6
// heal on a gone row asked to restart (here an expired hold's clear) lands
// alone, with no start composed beside it: A13 hands the row off on the
// next pass, and the start follows the handoff.
func TestRestartRequestedHealComposesNoStart(t *testing.T) {
	c := newHealCase(t, livenessGone, desireWake, append([]string{"state", "asleep", "held_until", rowAt(-time.Minute)}, restartAsked...)...)
	if it := c.decide(); it.Kind != intentRowHeal || it.Heal != nil {
		t.Fatalf("decideRow = (%q, %q), heal %v; want the hold's clear alone, no start", it.Kind, it.Reason, it.Heal != nil)
	}
}

// TestSimRuntimeRestartMarkerRestartsALiveRow (CONTRACT v5.10q, C7a-2b;
// SESS-520): when request-restart's row write failed, the runtime's own
// marker, read by the inventory, restarts the live row once: stopped,
// handed off, and started as a new incarnation on a fresh conversation (its
// continuation epoch advanced) that carries no marker, so nothing restarts
// it again.
func TestSimRuntimeRestartMarkerRestartsALiveRow(t *testing.T) {
	s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "started_config_hash", "h-1", "last_woke_at", rowAt(-time.Hour)), gc1Work())
	if err := newDrainOps(s.sp).setRestartRequested("s-gc-1"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		s.settle()
		s.advance(simPatrol)
	}
	b, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	s.sp.mu.Lock()
	starts, kills := len(s.sp.starts), len(s.sp.kills)
	marked := s.sp.rts["s-gc-1"] != nil && s.sp.rts["s-gc-1"].restart
	s.sp.mu.Unlock()
	if m := b.Metadata; starts != 1 || kills != 1 || marked || m["instance_token"] == "tok-1" || !s.livePane(b) || m["continuation_epoch"] != "2" {
		t.Fatalf("starts %d, kills %d, marked %t, row %v; want one restart into a new unmarked incarnation on a fresh conversation", starts, kills, marked, m)
	}
	s.noViolations(t)
}

// TestSimRuntimeRestartMarkerSparesAPinnedRow (C7a-2b, mc-nm6yt; SESS-521):
// a pinned named row whose runtime alone carries the marker, with no
// explicit reset on the row, is not killed, and the marker is cleared, so
// a later unpin over the same runtime restarts nothing.
func TestSimRuntimeRestartMarkerSparesAPinnedRow(t *testing.T) {
	s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-time.Hour),
		"configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true"), gc1Work())
	if err := newDrainOps(s.sp).setRestartRequested("s-gc-1"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		s.settle()
		s.advance(simPatrol)
	}
	if m := s.gc1(); len(s.sp.kills) != 0 || m["session_key"] != "k-1" {
		t.Fatalf("kills %d, row %v; want the pinned runtime left running", len(s.sp.kills), m)
	}
	// Legacy's pinned skip clears the runtime's marker (mc-nm6yt), so an
	// unpin over the same runtime fires no old request.
	s.sp.mu.Lock()
	marked := s.sp.rts["s-gc-1"] != nil && s.sp.rts["s-gc-1"].restart
	s.sp.mu.Unlock()
	if marked {
		t.Fatal("the pinned runtime still carries the restart marker")
	}
	s.ask("pin_awake", "")
	for range 3 {
		s.settle()
		s.advance(simPatrol)
	}
	if m := s.gc1(); len(s.sp.kills) != 0 || m["session_key"] != "k-1" {
		t.Fatalf("after the unpin: kills %d, row %v; want no old restart fired", len(s.sp.kills), m)
	}
	s.noViolations(t)
}

// TestRuntimeRestartMarkerReadsOnlyARunningOwnRuntime (C7a-2b): A13 takes
// the runtime's marker only on the row's Current runtime while it runs
// (alive, or a zombie's live pane); a corpse's, another incarnation's, or a
// row with no marker anywhere decides nothing.
func TestRuntimeRestartMarkerReadsOnlyARunningOwnRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		attrs    InventoryAttrs
		want     bool
	}{
		{"alive, marked", livenessAlive, InventoryAttrs{DeadKnown: true, RestartRequested: true}, true},
		{"alive, unmarked", livenessAlive, InventoryAttrs{DeadKnown: true}, false},
		{"a zombie, marked", livenessDead, InventoryAttrs{DeadKnown: true, ProcessKnown: true, RestartRequested: true}, true},
		{"a corpse, marked", livenessDead, InventoryAttrs{DeadKnown: true, AllPanesDead: true, RestartRequested: true}, false},
		{"another incarnation, marked", livenessAlive, InventoryAttrs{DeadKnown: true, RestartRequested: true, Identity: runtimeIdentity{Known: true, Token: "tok-other"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newHealCase(t, tc.liveness, desireWake, "state", "active")
			a := tc.attrs
			if !a.Identity.Known {
				a.Identity = runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
			} else {
				a.Identity.SessionID, a.Identity.Epoch = c.k.ID, "3"
			}
			c.w.ObsMaxAge = observeMaxAge
			c.w.Obs = newObserveCache().publish(c.w.Now, map[string]InventoryAttrs{"s-heal": a}, completeBackend("tmux", "s-heal"))
			r := &rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}
			if got := r.restartPending(); got != tc.want {
				t.Fatalf("restartPending = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestSidecarRestartMarkerFallback (C7a-2b): a name on a local sidecar
// leaf has its restart marker read through GetMeta every pass, and one that
// errors reads unset; a batched leaf's names are left to its inventory.
func TestSidecarRestartMarkerFallback(t *testing.T) {
	side := sidecarProvider([]string{"asked", "quiet", "unreadable"}, map[string]map[string]string{"asked": {"GC_RESTART_REQUESTED": "1"}, "unreadable": {"GC_RESTART_REQUESTED": "1"}})
	side.GetMetaErrors = map[string]map[string]error{"unreadable": {"GC_RESTART_REQUESTED": errors.New("sidecar unreadable")}}
	batched := &simProvider{Fake: runtime.NewFake(), rts: map[string]*simRuntime{}}
	if err := batched.Fake.SetMeta("other", "GC_RESTART_REQUESTED", "1"); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]InventoryAttrs{"asked": {}, "quiet": {}, "unreadable": {}, "other": {}}
	hosts := map[string]runtime.Provider{"asked": side, "quiet": side, "unreadable": side, "other": batched}
	readSidecarRestartMarkers(context.Background(), attrs, hosts, 2)
	if !attrs["asked"].RestartRequested || attrs["quiet"].RestartRequested || attrs["unreadable"].RestartRequested || attrs["other"].RestartRequested {
		t.Fatalf("attrs %+v, want only the sidecar's readable marked name set", attrs)
	}
}

// TestRestartStopClearsTheRuntimeMarker (C7a-2b): A13's confirmed stop
// clears the runtime's restart marker, as legacy's clearRestartRequested
// does, so a runtime the stop leaves (a corpse, a sidecar that outlives
// its process) restarts nothing again; any other stop leaves it alone.
func TestRestartStopClearsTheRuntimeMarker(t *testing.T) {
	for _, restart := range []bool{true, false} {
		sp := &simProvider{Fake: runtime.NewFake(), rts: map[string]*simRuntime{}, changed: map[string]uint64{}, now: func() time.Time { return plannerT0 }}
		caps := txCaps{stop: stopCaps{sp: sp, clock: newFakePlannerClock(plannerT0)}}
		out, err := stopCall(context.Background(), caps, stopPlan{name: "s-gone", now: plannerT0, restart: restart})
		if err != nil || !out.Confirmed {
			t.Fatalf("restart %t: stopCall = %+v, %v; want the gone runtime confirmed", restart, out, err)
		}
		if got := sp.CountCalls("RemoveMeta", "s-gone"); got != map[bool]int{true: 1, false: 0}[restart] {
			t.Fatalf("restart %t: %d marker clears", restart, got)
		}
	}
}

// TestRestartMarkerFlipIsAnObservationChange (C7a-2b): a runtime's restart
// marker turning on is a change the planner is signaled for, though no
// fact or identity moved.
func TestRestartMarkerFlipIsAnObservationChange(t *testing.T) {
	old := RuntimeObservation{SessionName: "s"}
	next := old
	next.RestartRequested = true
	if !observationChanged(old, next, true) {
		t.Fatal("a marker flip read as no change")
	}
}

// TestPinnedRuntimeMarkerIsCleared (C7a-2b, mc-nm6yt; SESS-521): a pinned
// named row with no explicit reset, whose runtime alone carries the marker,
// gets legacy's pinned skip: the marker cleared from its runtime, no kill,
// and no write of a row marker it does not hold. A runtime not its own, or
// a dead pane, decides no clear.
func TestPinnedRuntimeMarkerIsCleared(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs InventoryAttrs
		want  bool
	}{
		{"its own live runtime", InventoryAttrs{DeadKnown: true, RestartRequested: true}, true},
		{"a dead pane", InventoryAttrs{DeadKnown: true, AllPanesDead: true, RestartRequested: true}, false},
		{"another incarnation", InventoryAttrs{DeadKnown: true, RestartRequested: true, Identity: runtimeIdentity{Known: true, Token: "tok-other"}}, false},
		{"no marker", InventoryAttrs{DeadKnown: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			liveness := livenessAlive
			if tc.attrs.AllPanesDead {
				liveness = livenessDead
			}
			c := newHealCase(t, liveness, desireWake, "state", "active", "configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true")
			c.w.ObsMaxAge = observeMaxAge
			a := tc.attrs
			if !a.Identity.Known {
				a.Identity = runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
			} else {
				a.Identity.SessionID, a.Identity.Epoch = c.k.ID, "3"
			}
			c.w.Obs = newObserveCache().publish(c.w.Now, map[string]InventoryAttrs{"s-heal": a}, completeBackend("tmux", "s-heal"))
			it, ok := armRestartRequested(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true})
			if got := ok && it.Kind == intentMarkerClear && it.Reason == decideRestartMarkerClear && len(it.Patch) == 0; got != tc.want {
				t.Fatalf("armRestartRequested = (%q, %q) %v %v, want a marker clear %t", it.Kind, it.Reason, ok, it.Patch, tc.want)
			}
		})
	}
}

// pinnedMarkerCase is a pinned named row whose own live runtime alone
// carries the restart marker, its fresh read and identity answering live.
func pinnedMarkerCase(t *testing.T, meta ...string) (*healCase, *freshObserver) {
	t.Helper()
	base := append([]string{"state", "active", "configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true"}, meta...)
	c := newHealCase(t, livenessAlive, desireWake, base...)
	c.w.ObsMaxAge = observeMaxAge
	id := runtimeIdentity{Known: true, SessionID: c.k.ID, Epoch: "3", Token: "tok-3"}
	c.w.Obs = newObserveCache().publish(c.w.Now, map[string]InventoryAttrs{"s-heal": {DeadKnown: true, RestartRequested: true, Identity: id}}, completeBackend("tmux", "s-heal"))
	live := &freshObserver{
		Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"},
		env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_RUNTIME_EPOCH": "3", "GC_INSTANCE_TOKEN": "tok-3"},
	}
	return c, live
}

// TestPinnedMarkerClearEffect (mc-nm6yt): the clear's effect removes the
// runtime's marker from its Current runtime and writes no row; a row
// unpinned between the pass and the effect is refused, its marker kept for
// A13's restart; a runtime not its own on the fresh read is left.
func TestPinnedMarkerClearEffect(t *testing.T) {
	c, live := pinnedMarkerCase(t)
	it, s := c.run(t, live.tmux(), nil)
	if it.Kind != intentMarkerClear || s.Outcome != settledNoop || s.Cause != "" || live.Fake.CountCalls("RemoveMeta", "s-heal") != 1 {
		t.Fatalf("decided %q, settled %+v, %d marker clears; want one clear and no row write (a no-op settlement)", it.Kind, s, live.Fake.CountCalls("RemoveMeta", "s-heal"))
	}

	c, live = pinnedMarkerCase(t)
	c.before = func() {
		if err := c.store.SetMetadata(c.k.ID, "pin_awake", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, s := c.run(t, live.tmux(), nil); s.Outcome != settledRefused || live.Fake.CountCalls("RemoveMeta", "s-heal") != 0 {
		t.Fatalf("settled %+v, %d marker clears; want refused, the marker kept", s, live.Fake.CountCalls("RemoveMeta", "s-heal"))
	}

	c, live = pinnedMarkerCase(t)
	live.env["GC_INSTANCE_TOKEN"] = "tok-other"
	if _, s := c.run(t, live.tmux(), nil); live.Fake.CountCalls("RemoveMeta", "s-heal") != 0 {
		t.Fatalf("settled %+v, cleared a runtime not its own", s)
	}
}

// TestPinnedRowMarkerClearsFirst (SESS-521): a pinned row holding the row
// marker too has the row marker cleared first (A13's row write); the
// runtime's follows on a later pass.
func TestPinnedRowMarkerClearsFirst(t *testing.T) {
	c, _ := pinnedMarkerCase(t, "restart_requested", "true")
	it, ok := armRestartRequested(&rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true})
	if !ok || it.Kind != intentRowHeal || it.Reason != decideRestartPinnedClear {
		t.Fatalf("armRestartRequested = (%q, %q) %v, want the row marker's clear first", it.Kind, it.Reason, ok)
	}
}

// TestSimFailingMarkerClearHoldsNoSuspend (the #7893 review, its probe):
// a pinned row's marker clear that keeps failing (a sidecar or a tmux
// option the controller cannot remove) backs off its own record, so an
// operator's suspend of the row stops its runtime on the first pass, as it
// does when the clear lands.
func TestSimFailingMarkerClearHoldsNoSuspend(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprintf("failing %t", failing), func(t *testing.T) {
			s := converged(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-time.Hour),
				"configured_named_session", "true", "configured_named_identity", "worker", "pin_awake", "true"), gc1Work())
			if err := newDrainOps(s.sp).setRestartRequested("s-gc-1"); err != nil {
				t.Fatal(err)
			}
			if failing {
				s.sp.mu.Lock()
				s.sp.RemoveMetaErrors = map[string]map[string]error{"s-gc-1": {"GC_RESTART_REQUESTED": errors.New("sim: permission denied")}}
				s.sp.mu.Unlock()
			}
			for range 40 {
				s.settle()
				s.advance(simPatrol)
			}
			s.ask("state", "suspended", "sleep_intent", "user-hold", "suspended_at", s.clk.Now().Format(time.RFC3339))
			s.settle()
			s.sp.mu.Lock()
			gone := s.sp.rts["s-gc-1"] == nil
			s.sp.mu.Unlock()
			if !gone {
				t.Fatalf("failing %t: the suspend's first pass left the runtime; trace %q", failing, s.p.rowTrace[rowKey{Leg: rowLeg, ID: "gc-1"}])
			}
			s.noViolations(t)
		})
	}
}

// TestMarkerClearBackoffHoldsOnlyTheClear (the #7893 review): the clear
// settles on its own backoff key, and while that record is live A13 falls
// through past the clear, its release the row's next deadline.
func TestMarkerClearBackoffHoldsOnlyTheClear(t *testing.T) {
	c, _ := pinnedMarkerCase(t)
	if got := settledBackoffKey(intentMarkerClear, decideRestartMarkerClear, c.k); got != markerClearBackoffKey(c.k) || got == rowBackoffKey(c.k) {
		t.Fatalf("the clear settles on %q, want its own key", got)
	}
	c.w.Backoff = map[string]backoffRecord{markerClearBackoffKey(c.k): {Consecutive: 1, Until: c.w.Now.Add(time.Minute)}}
	r := &rowFacts{w: c.w, k: c.k, entry: c.a.Snapshot.Entries[c.k], row: c.w.Census.Rows[c.k], found: true}
	if it, ok := armRestartRequested(r); ok && it.Kind == intentMarkerClear {
		t.Fatalf("armRestartRequested = (%q, %q) under a live clear backoff, want a fall-through", it.Kind, it.Reason)
	}
	if !r.next.Equal(c.w.Now.Add(time.Minute)) {
		t.Fatalf("next %v, want the backoff's release", r.next)
	}
}

// TestMarkerClearNeedsOneSessionObject (the #7893 review nit): a fresh read
// that sees another session object under the name between its reads clears
// nothing.
func TestMarkerClearNeedsOneSessionObject(t *testing.T) {
	c, live := pinnedMarkerCase(t)
	n := 0
	live.read = func() { n++; live.l.ObjectID = fmt.Sprintf("$%d", n) }
	if _, s := c.run(t, live.tmux(), nil); live.Fake.CountCalls("RemoveMeta", "s-heal") != 0 {
		t.Fatalf("settled %+v, cleared over a moving session object", s)
	}
}
