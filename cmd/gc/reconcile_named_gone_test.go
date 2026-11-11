package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// PAR-NAMEDGONE: a configured named session whose spec leaves the config.
// Legacy holds a live one through a transient collapse of the spec set
// (#3630: namedSuspendConfirmTicks consecutive absent passes before its drain
// begins) and retires a removed one by archiving it
// (session.RetireNamedSessionPatch). These tests drive the v2 planner through
// the same removals. The spec set collapses by the config's named sessions and
// their agent both leaving one pass's environment, and returning the next.

const goneName = "keeper"

// goneSpecs is the configured named session, and its agent, the scenarios
// remove and restore.
func goneSpecs() ([]config.Agent, []config.NamedSession) {
	return []config.Agent{{Name: goneName, StartCommand: "true"}},
		[]config.NamedSession{{Template: goneName, Mode: "always"}}
}

// goneSim is a run over a configured named session gc-7 under goneName, live
// with a runtime unless dead, and over rows beside it.
func goneSim(t *testing.T, dead bool, rows ...beads.Bead) *sim {
	row := func(s *sim) beads.Bead {
		name := config.NamedSessionRuntimeName("test-city", s.cfg.Workspace, goneName)
		return sessionRow("gc-7",
			"template", goneName, "state", "active", "session_name", name, "alias", goneName, "agent_name", goneName,
			"command", "true", "generation", "1",
			namedSessionMetadataKey, "true", namedSessionIdentityMetadata, goneName, "configured_named_mode", "always",
			"continuity_eligible", "true")
	}
	s := newSim(t, 1, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
		agents, named := goneSpecs()
		s.cfg.Agents = append(s.cfg.Agents, agents...)
		s.cfg.NamedSessions = named
		b := row(s)
		if !dead {
			s.sp.put(b.Metadata["session_name"], b.ID, b.Metadata["generation"], b.Metadata["instance_token"])
		}
		return append([]beads.Bead{b}, rows...), nil
	}})
	s.lag = false
	s.advance(time.Second)
	return s
}

// setSpecs makes the next pass read the config with the named session
// (named) and its agent (agent) present or gone.
func (s *sim) setSpecs(agent, named bool) {
	next := cloneSimCity(s.cfg)
	next.Agents = nil
	for _, a := range s.cfg.Agents {
		if a.Name != goneName {
			next.Agents = append(next.Agents, a)
		}
	}
	next.NamedSessions = nil
	agents, specs := goneSpecs()
	if agent {
		next.Agents = append(next.Agents, agents...)
	}
	if named {
		next.NamedSessions = specs
	}
	s.cfg = next
	s.envNow = &reconcileEnv{Gen: s.envNow.Gen + 1, Cfg: next, SP: s.envNow.SP}
}

// patrol is one controller pass: the clock moves a patrol, the pass runs and
// its effects run out.
func (s *sim) patrol() {
	s.advance(simPatrol)
	s.settle()
}

// goneRow is gc-7 as the city store holds it.
func (s *sim) goneRow() beads.Bead { return s.snapshot()[simCityLeg+"/gc-7"] }

// drainBegun reports that gc-7's drain has begun: its intent is written, or
// its runtime has been stopped.
func (s *sim) drainBegun() bool {
	intent := s.goneRow().Metadata[session.DrainIntentReasonKey]
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	return intent != "" || len(s.sp.kills) > 0
}

// A spec set that collapses for one pass and returns the next leaves a live
// named session untouched: the absent pass begins no drain and stops nothing,
// and the pass after it finds the session as it was (legacy's confirm window,
// #3630).
func TestSimNamedSpecCollapseOnePassDoesNotDrain(t *testing.T) {
	s := goneSim(t, false)
	s.patrol()
	s.patrol()
	if s.drainBegun() {
		t.Fatalf("test premise broken: the named session drains with its spec present (%v)", s.goneRow().Metadata)
	}
	before := s.goneRow().Metadata["instance_token"]

	s.setSpecs(false, false)
	s.patrol()
	if s.drainBegun() {
		t.Fatalf("the named session's drain began on its first pass with no spec (kills %+v, intent %q)", s.sp.kills, s.goneRow().Metadata[session.DrainIntentReasonKey])
	}
	s.setSpecs(true, true)
	s.patrol()
	s.patrol()
	if s.drainBegun() {
		t.Errorf("the named session drained after its spec returned (kills %+v, intent %q)", s.sp.kills, s.goneRow().Metadata[session.DrainIntentReasonKey])
	}
	if got := s.goneRow(); got.Status == "closed" || got.Metadata["instance_token"] != before {
		t.Errorf("the named session is %s with token %q after the collapse, want it open under %q", got.Status, got.Metadata["instance_token"], before)
	}
	s.noViolations(t)
}

// A live named session with no spec on two consecutive passes drains: the
// first pass confirms, the second begins the drain.
func TestSimNamedSpecAbsentTwoPassesDrains(t *testing.T) {
	s := goneSim(t, false)
	s.patrol()
	s.setSpecs(false, false)
	s.patrol()
	if s.drainBegun() {
		t.Fatalf("the drain began on the first absent pass")
	}
	s.patrol()
	if !s.drainBegun() {
		t.Errorf("no drain began on the second consecutive absent pass (row %v)", s.goneRow().Metadata)
	}
	s.noViolations(t)
}

// A restart resets the absent count, as legacy's drainTracker does: one absent
// pass before the restart and one after are not two consecutive passes the new
// controller saw.
func TestSimNamedSpecAbsentCountResetsOnRestart(t *testing.T) {
	s := goneSim(t, false)
	s.patrol()
	s.setSpecs(false, false)
	s.patrol()
	if s.drainBegun() {
		t.Fatalf("the drain began on the first absent pass")
	}
	s.restartController()
	s.patrol()
	if s.drainBegun() {
		t.Fatalf("the drain began on the first absent pass after a restart")
	}
	s.patrol()
	if !s.drainBegun() {
		t.Errorf("no drain began on the second absent pass after the restart (row %v)", s.goneRow().Metadata)
	}
	s.noViolations(t)
}

// A named session with no spec whose runtime is gone is not held by the
// window: its bead ends on the first pass, as legacy ends a dead one at once.
func TestSimNamedSpecAbsentDeadBeadEndsAtOnce(t *testing.T) {
	s := goneSim(t, true)
	s.setSpecs(false, false)
	s.patrol()
	m := s.goneRow()
	if m.Status != "closed" && m.Metadata["archived_at"] == "" {
		t.Errorf("the dead named session is %s with no spec, want it ended on the first pass (%v)", m.Status, m.Metadata)
	}
	if m.Metadata["alias"] == goneName && m.Status != "closed" {
		t.Errorf("the dead named session still holds alias %q", goneName)
	}
	s.noViolations(t)
}

// A live named session with open assigned work and no spec is held for its
// work, however many passes the spec stays away, as legacy holds it.
func TestSimNamedSpecAbsentWithLiveWorkIsHeld(t *testing.T) {
	work := beads.Bead{ID: "gw-gc-7", Title: "work", Type: "task", Status: "in_progress", Assignee: "gc-7", Metadata: map[string]string{"gc.routed_to": goneName}}
	s := goneSim(t, false, work)
	s.patrol()
	s.setSpecs(false, false)
	for i := range 4 {
		s.patrol()
		if s.drainBegun() {
			t.Fatalf("pass %d with no spec: the drain began on a session holding live work (row %v)", i+1, s.goneRow().Metadata)
		}
	}
	if got := s.goneRow(); got.Status == "closed" {
		t.Errorf("the named session holding live work was closed")
	}
	s.noViolations(t)
}

// A removed configured named session ends with its identity free: neither its
// alias nor its session name is held by an open row, a same-name config added
// back starts under that identity, and the work assigned to it can be given to
// another session (legacy's RetireNamedSessionPatch archive frees all three).
func TestSimRemovedNamedSessionFreesItsIdentity(t *testing.T) {
	work := beads.Bead{ID: "gw-gc-7", Title: "work", Type: "task", Status: "open", Assignee: "gc-7", Metadata: map[string]string{"gc.routed_to": goneName}}
	s := goneSim(t, true, work)
	s.setSpecs(false, false)
	for range 3 {
		s.patrol()
	}
	name := config.NamedSessionRuntimeName("test-city", s.cfg.Workspace, goneName)
	for id, b := range s.snapshot() {
		if b.Type != session.BeadType || b.Status == "closed" && b.Metadata["archived_at"] == "" {
			continue
		}
		if b.Metadata["alias"] == goneName || b.Metadata["session_name"] == name {
			t.Errorf("%s still holds alias %q / session name %q after its spec was removed (status %s, %v)", id, b.Metadata["alias"], b.Metadata["session_name"], b.Status, b.Metadata)
		}
	}
	if w := s.snapshot()[simCityLeg+"/gw-gc-7"]; w.Assignee == "gc-7" || w.Assignee == goneName {
		t.Errorf("work gw-gc-7 is %s assigned to %q (routed to %q), want it reassignable; row gc-7 is %s", w.Status, w.Assignee, w.Metadata["gc.routed_to"], s.goneRow().Status)
	}

	s.setSpecs(true, true)
	for range 4 {
		s.patrol()
	}
	started := false
	for _, st := range s.sp.starts {
		if st.Name == name && st.Created {
			started = true
		}
	}
	if !started {
		t.Errorf("starts %+v: the re-added %q was not started under its runtime name %q", s.sp.starts, goneName, name)
	}
	s.noViolations(t)
}
