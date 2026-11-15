package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ga-f4ovjm.76: the WaitForIdle record veto at the conversation lane's
// quiescence boundary. The second shadow (ga-f4ovjm.72) saw this boundary
// read a running Codex turn idle and type into it 54 times of 350 (the
// typed delivery aborted the turn), and a fresh record read right before the
// typing saw the turn every time, harming none. Under program_status = "on",
// a provider declaring hold_mid_turn has its record read fresh at the
// boundary and again right before Deliver types; a live working turn holds
// the delivery. It is the hold #8840 put on the starved bypass and #8862 on
// the lane's starved boundary, extended to the quiescence boundary: one
// mechanism (laneRecordTypingHold, runtime.ProgramRecordTypingHold). Under
// off and shadow nothing is read and the boundary decides as before.

// recordHoldPass runs one lane pass with the pane quiet (the quiescence rule
// reads it idle) and returns op as the pass left it; before, when set, runs
// ahead of each record-hold look, given its count.
func recordHoldPass(t *testing.T, s *starvedLane, op opjournal.Op, before func(look int)) opjournal.Op {
	t.Helper()
	s.f.sp.SetActivity(s.f.name, s.f.clk.Now().Add(-time.Minute))
	v, row := s.f.view(t)
	e := newConversationLaneExec(s.deps, row)
	quiescenceOnFixtureClock(s.f, e)
	hold := e.recordHold
	looks := 0
	e.recordHold = func(tg nudgeTarget, now time.Time) bool {
		looks++
		if before != nil {
			before(looks)
		}
		return hold(tg, now)
	}
	if _, err := s.l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return s.f.get(t, s.st, op.ID)
}

// newRecordHoldLane is a lane over one quiet session of provider, under the
// city cfg, whose screen shows no turn and whose record reads st under the
// switch on (as the leaf reads it fresh), with one boundary prompt
// proposed.
func newRecordHoldLane(t *testing.T, provider string, cfg *config.City, st runtime.ProgramState, opts ...laneFixtureOpt) (*starvedLane, opjournal.Op) {
	t.Helper()
	s := newStarvedLane(t, provider, opts...)
	s.deps.cfg = cfg
	s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, st, time.Now()))
	return s, s.prompt(t, "k-record-hold", "typed at the quiescence boundary")
}

// TestLaneQuietBoundaryHonorsTheCodexRecordHold: record-says-working versus
// scan-says-idle, for every switch value. Under "on" a Codex session whose
// quiet pane and screen read idle is held while its record says a turn is
// working, and the prompt goes once the record reaches a boundary; a record
// that says done lets it go at once. Under "off", "shadow" and an unset
// switch the record is never read and the quiescence rule delivers at once,
// whatever the record says.
func TestLaneQuietBoundaryHonorsTheCodexRecordHold(t *testing.T) {
	for _, kind := range []struct {
		name string
		row  laneFixtureOpt
		city func(mode string) *config.City
	}{
		{"agent session", func(map[string]string) {}, codexAgentCity},
		{"provider session", providerSessionRow, codexCatalogCity},
	} {
		t.Run(kind.name, func(t *testing.T) {
			t.Run("on, record working", func(t *testing.T) {
				s, op := newRecordHoldLane(t, "codex", kind.city("on"), runtime.ProgramWorking, kind.row)
				for range 3 {
					if got := recordHoldPass(t, s, op, nil); got.Status == opjournal.StatusClosed {
						t.Fatalf("op closed %+v while the record says working; want it held", got.Outcome)
					}
					s.f.clk.Advance(laneBusyMax)
				}
				if typed := s.f.nudges(); len(typed) != 0 {
					t.Fatalf("typed %+v while the record says working; want nothing", typed)
				}
				if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n < 3 {
					t.Fatalf("%d fresh record reads over 3 passes; want one a pass at least", n)
				}
				s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramIdle, time.Now()))
				if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("after the record says idle: op %s %+v; want closed succeeded", got.Status, got.Outcome)
				}
				if typed := s.f.nudges(); len(typed) != 1 {
					t.Fatalf("typed %+v; want the prompt once", typed)
				}
			})
			t.Run("on, record done", func(t *testing.T) {
				s, op := newRecordHoldLane(t, "codex", kind.city("on"), runtime.ProgramDone, kind.row)
				if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("op %s %+v; want closed succeeded on a done record", got.Status, got.Outcome)
				}
			})
			for _, sw := range []string{"off", "shadow", ""} {
				t.Run("switch "+sw+", record working", func(t *testing.T) {
					s, op := newRecordHoldLane(t, "codex", kind.city(sw), runtime.ProgramWorking, kind.row)
					if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
						t.Fatalf("switch %q: op %s %+v; want the quiescence rule to deliver as before", sw, got.Status, got.Outcome)
					}
					if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n != 0 {
						t.Fatalf("switch %q: %d record reads; want none", sw, n)
					}
				})
			}
		})
	}
}

// TestLaneQuietBoundaryRecordHoldIsCodexOnly: a provider that does not
// declare hold_mid_turn (Claude) is neutral: under "on", with its pane
// carrying a working record that even declares the hold, the quiescence
// boundary delivers as before and its record is never read.
func TestLaneQuietBoundaryRecordHoldIsCodexOnly(t *testing.T) {
	cfg := &config.City{Session: config.SessionConfig{ProgramStatus: "on"}}
	s, op := newRecordHoldLane(t, "claude", cfg, runtime.ProgramWorking)
	if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op %s %+v; want claude delivered as before", got.Status, got.Outcome)
	}
	if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n != 0 {
		t.Fatalf("%d record reads for a provider that declares no hold; want none", n)
	}
}

// TestLaneQuietBoundaryRecordHoldReadsFresh: the hold decides on a fresh
// read of the pane, never the state cache's snapshot (the 2 s cached
// reading the shadow measured harming the Claude pre-read). A cache that
// still says done over a pane whose record says working holds; a cache that
// still says working over a pane at a boundary lets the prompt go.
func TestLaneQuietBoundaryRecordHoldReadsFresh(t *testing.T) {
	for _, c := range []struct {
		name          string
		cached, fresh runtime.ProgramState
		delivered     bool
	}{
		{"stale cache done, fresh working", runtime.ProgramDone, runtime.ProgramWorking, false},
		{"stale cache working, fresh idle", runtime.ProgramWorking, runtime.ProgramIdle, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, op := newRecordHoldLane(t, "codex", codexAgentCity("on"), c.fresh)
			s.f.sp.SetProgramCachedPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, c.cached, time.Now()))
			got := recordHoldPass(t, s, op, nil)
			if delivered := got.Status == opjournal.StatusClosed; delivered != c.delivered {
				t.Fatalf("op %s %+v; want delivered %v on the fresh %s record", got.Status, got.Outcome, c.delivered, c.fresh)
			}
		})
	}
}

// TestLaneQuietBoundaryRecordHoldsUndecidedNothing: under "on", a fresh
// read that decides nothing (a pane that does not declare the hold or is not
// armed, a lost reader, a silent or cleared record, a working record past
// its hold bound) holds nothing at the quiescence boundary: the rule
// decides as before, unlike the starved boundary, which never types into
// such a provider on the screen's look alone.
func TestLaneQuietBoundaryRecordHoldsUndecidedNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane func(runtime.ProgramPaneRow) runtime.ProgramPaneRow
	}{
		{"pane undeclared", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.HoldDeclared = false; return r }},
		{"pane not armed", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.Tap = ""; return r }},
		{"reader lost", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow { r.PanePipe = false; return r }},
		{"record silent", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow {
			r.Record = runtime.ProgramStatus{Nonce: r.Tap}
			return r
		}},
		{"record cleared", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow {
			r.Record = runtime.ProgramStatus{Nonce: r.Tap, Reported: true}
			return r
		}},
		{"working past the hold bound", func(r runtime.ProgramPaneRow) runtime.ProgramPaneRow {
			r.Record.Since = time.Now().Add(-runtime.ProgramStatusHoldLimit - time.Minute)
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, op := newRecordHoldLane(t, "codex", codexAgentCity("on"), runtime.ProgramWorking)
			s.f.sp.SetProgramPane(s.f.name, tc.pane(holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now())))
			if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("op %s %+v; want the quiescence rule to deliver on a reading that decides nothing", got.Status, got.Outcome)
			}
		})
	}
}

// TestLaneQuietBoundaryRecordHoldVetoedAtTheSendLook: the shadow's decision
// reading was taken right before the lane typed, so Deliver reads the
// record again before it types, as the starved boundary's send look does. A
// Codex turn that starts between the probe's read and Deliver's (Codex
// picking up a follow-up it queued) holds the delivery: nothing is typed,
// and the op is handed back transient, uncharged and not delivered. A later
// pass at a boundary types the prompt once.
func TestLaneQuietBoundaryRecordHoldVetoedAtTheSendLook(t *testing.T) {
	s, op := newRecordHoldLane(t, "codex", codexAgentCity("on"), runtime.ProgramIdle)
	got := recordHoldPass(t, s, op, func(look int) {
		if look == 2 {
			s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
		}
	})
	if typed := s.f.nudges(); len(typed) != 0 {
		t.Fatalf("typed %+v though Deliver's read held; want nothing", typed)
	}
	if n := s.f.sp.CountCalls("ProgramStatusHoldRead", s.f.name); n != 2 {
		t.Fatalf("%d record reads; want the probe's and Deliver's", n)
	}
	if got.Status != opjournal.StatusAccepted || got.Failures != 0 || got.MaybeDelivered != 0 || !strings.Contains(got.LastError, "before typing") {
		t.Fatalf("op %s, %d failures, maybe delivered %d, last error %q; want it handed back uncharged and not delivered, saying why",
			got.Status, got.Failures, got.MaybeDelivered, got.LastError)
	}
	s.f.sp.SetProgramPane(s.f.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
	s.f.clk.Advance(laneBusyMax)
	if got := recordHoldPass(t, s, op, nil); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("once the record is at a boundary: op %s %+v; want closed succeeded", got.Status, got.Outcome)
	}
	if typed := s.f.nudges(); len(typed) != 1 {
		t.Fatalf("typed %+v; want the prompt once", typed)
	}
}

// Kills a quiescence boundary the probe opens on a live working record (the
// send look alone would then hold every delivery, taking the op each pass):
// under "on" the probe's own boundary reads No for a Codex session whose
// record says a turn is working, and Yes under "off".
func TestLaneProbeClosesTheQuietBoundaryOnAWorkingRecord(t *testing.T) {
	for _, c := range []struct {
		sw   string
		want opjournal.Tri
	}{{"on", opjournal.TriNo}, {"off", opjournal.TriYes}} {
		t.Run(c.sw, func(t *testing.T) {
			s, _ := newRecordHoldLane(t, "codex", codexAgentCity(c.sw), runtime.ProgramWorking)
			s.f.sp.SetActivity(s.f.name, s.f.clk.Now().Add(-time.Minute))
			v, row := s.f.view(t)
			e := newConversationLaneExec(s.deps, row)
			quiescenceOnFixtureClock(s.f, e)
			p, err := e.Probe(context.Background(), v)
			if err != nil {
				t.Fatal(err)
			}
			if p.Boundary != c.want {
				t.Fatalf("switch %s: probe boundary %v; want %v", c.sw, p.Boundary, c.want)
			}
		})
	}
}
