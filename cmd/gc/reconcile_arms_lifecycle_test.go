package main

import (
	"context"
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

// ask writes kv to the city row id as an outside writer, and lets the
// cache see it.
func (s *sim) ask(id string, kv ...string) {
	s.legs[0].cache.ApplyEvent("bead.updated", s.operator(id, kv...))
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
			s.ask("gc-1", restartAsked...)
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

// TestSimRestartRequestedHandsOffAGoneRow (A13; SESS-526): a row whose
// runtime is already gone is handed off without a stop, ahead of its start,
// and started a pass later.
func TestSimRestartRequestedHandsOffAGoneRow(t *testing.T) {
	s := scripted(t, nil, poolRow("gc-1", "worker", 1, "asleep", "instance_token", "tok-1", "started_config_hash", "h-1"), gc1Work())
	s.ask("gc-1", restartAsked...)
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
			s.ask("gc-1", tc.asked...)
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
