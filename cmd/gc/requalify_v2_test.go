package main

// The v2 reconciler's half of the requalification contract
// (gastownhall/gascity#5957): planNamed selects the bare row for the
// requalified qualified spec, and the v2 arms rename it in place — identity
// and alias to the qualified form, the bare form in alias_history, the drift
// baseline rebaselined — with no kill, no start, no second row.

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// requalifyBareRow is the live row the pre-adopt config minted, in the census
// shape: bare identity, alias and session name everywhere, started, with the
// stale baseline a config-drift compare would trip over.
func requalifyBareRow(id string, state string, meta ...string) beads.Bead {
	base := []string{
		"template", requalifyBare, "state", state,
		"session_name", requalifyBare, "alias", requalifyBare, "agent_name", requalifyBare,
		"command", "true",
		namedSessionMetadataKey, "true",
		namedSessionIdentityMetadata, requalifyBare,
		"continuity_eligible", "true",
		"started_config_hash", "v6:stale-core",
		"started_live_hash", "v6:stale-live",
		"live_hash", "v6:stale-live",
		"started_provision_hash", "v6:stale-prov",
		"started_launch_hash", "v6:stale-launch",
		"core_hash_breakdown", "{}",
	}
	return sessionRow(id, append(base, meta...)...)
}

// AC-2, allocator half: planNamed selects the bare row as the requalified
// spec's canonical row, under the qualified identity, with no createNamed
// plan beside it and no drain of the row.
func TestAllocatorPlanNamedSelectsTheRequalifiedBareRow(t *testing.T) {
	cfg := requalifyCity("always")
	d := newAllocFixture(t, cfg).sessions(requalifyBareRow("gc-1", "active", "generation", "1", "configured_named_mode", "always")).decide()
	e := entryOf(t, d, "gc-1")
	if !e.InDesired || e.Config == nil || e.Config.NamedIdentity != requalifyQualified {
		t.Fatalf("the bare row = %s/%s in-desired=%v cfg=%+v, want it selected under %q", e.Desired, e.Reason, e.InDesired, e.Config, requalifyQualified)
	}
	if e.Desired == desireDrain {
		t.Fatalf("the canonical row = %s/%s, want it retained, not drained", e.Desired, e.DrainReason)
	}
	if hasNamedPlan(d) {
		t.Fatalf("plans %+v: a named plan was made beside the canonical row", d.Plans)
	}
}

// AC-2, arms half: the v2 pass renames the requalified row in place. The same
// bead keeps session_name, gains the qualified identity and alias, records
// the bare form in alias_history, rebaselines its drift baseline, and the
// seat is never killed or restarted across the settles.
func TestSimRequalifiedNamedSessionRenamesInPlace(t *testing.T) {
	row := requalifyBareRow("gc-1", "active", "generation", "1", "configured_named_mode", "always", "instance_token", "tok-gc-1")
	s := newSim(t, 1, simOpts{rows: func(s *sim) (city, rig []beads.Bead) {
		// The post-adopt config: the entry `gc rig add --adopt` requalified
		// golden-hon to, live in the same pass's config.
		s.cfg.Rigs = append(s.cfg.Rigs, config.Rig{Name: "contentbuild", Path: t.TempDir()})
		s.cfg.Agents = append(s.cfg.Agents, config.Agent{Name: requalifyBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"})
		s.cfg.NamedSessions = []config.NamedSession{{Template: requalifyBare, Dir: "contentbuild", BindingName: "lane", Mode: "always"}}
		s.sp.put(row.Metadata["session_name"], row.ID, row.Metadata["generation"], row.Metadata["instance_token"])
		return []beads.Bead{row}, nil
	}})
	s.lag = false
	s.advance(simPatrol)
	s.settle()
	if reason, _, _ := strings.Cut(s.p.rowTrace[rowKey{Leg: rowLeg, ID: "gc-1"}], "/"); reason != decideRowIdentity && reason != decideRowAlias {
		t.Fatalf("first pass decided %q, want armRowMetadata's identity write (%q or %q)", reason, decideRowIdentity, decideRowAlias)
	}
	for range 3 {
		s.advance(simPatrol)
		s.settle()
	}
	m := s.gc1()
	if m[namedSessionIdentityMetadata] != requalifyQualified {
		t.Errorf("configured_named_identity = %q, want %q", m[namedSessionIdentityMetadata], requalifyQualified)
	}
	if m["alias"] != requalifyQualified {
		t.Errorf("alias = %q, want %q", m["alias"], requalifyQualified)
	}
	if !strings.Contains(m["alias_history"], requalifyBare) {
		t.Errorf("alias_history = %q, want it to contain %q", m["alias_history"], requalifyBare)
	}
	if m["session_name"] != requalifyBare {
		t.Errorf("session_name = %q, want the row to keep %q", m["session_name"], requalifyBare)
	}
	if !strings.HasPrefix(m["started_config_hash"], runtime.FingerprintVersion+":") || strings.Contains(m["started_config_hash"], "stale") {
		t.Errorf("started_config_hash = %q, want a fresh %s-prefixed rebaseline", m["started_config_hash"], runtime.FingerprintVersion)
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 {
		t.Errorf("kills %v starts %v, want the seat left running", s.sp.kills, s.sp.starts)
	}
	for _, b := range s.snapshot() {
		if b.ID != "gc-1" && strings.Contains(b.Metadata[namedSessionIdentityMetadata], "golden-hon") {
			t.Errorf("a second row %s took the identity: %v", b.ID, b.Metadata)
		}
	}
	s.noViolations(t)
}

// AC-4, arms half: an ambiguous leaf leaves the bare row unselected under the
// v2 reconciler too. Neither qualified spec adopts the row (no migration, no
// createNamed beside it), and the drain arm's work gate keeps the live seat
// running with its work still claimed.
func TestSimAmbiguousLeafKeepsTheBareRowUnselected(t *testing.T) {
	row := requalifyBareRow("gc-1", "active", "generation", "1", "configured_named_mode", "on_demand", "instance_token", "tok-gc-1")
	work := beads.Bead{ID: "gw-gc-1", Title: "work", Type: "task", Status: "in_progress", Assignee: requalifyBare, Metadata: map[string]string{"gc.routed_to": "worker"}}
	s := newSim(t, 2, simOpts{rows: func(s *sim) (city, rig []beads.Bead) {
		// Two bindings claim the bare leaf: neither direction may resolve it.
		s.cfg.Rigs = append(s.cfg.Rigs, config.Rig{Name: "contentbuild", Path: t.TempDir()})
		s.cfg.Agents = append(s.cfg.Agents,
			config.Agent{Name: requalifyBare, Dir: "contentbuild", BindingName: "lane", StartCommand: "true"},
			config.Agent{Name: requalifyBare, Dir: "contentbuild", BindingName: "ops", StartCommand: "true"})
		s.cfg.NamedSessions = []config.NamedSession{
			{Template: requalifyBare, Dir: "contentbuild", BindingName: "lane", Mode: "on_demand"},
			{Template: requalifyBare, Dir: "contentbuild", BindingName: "ops", Mode: "on_demand"},
		}
		s.sp.put(row.Metadata["session_name"], row.ID, row.Metadata["generation"], row.Metadata["instance_token"])
		return []beads.Bead{row, work}, nil
	}})
	s.lag = false
	for range 4 {
		s.advance(simPatrol)
		s.settle()
	}
	m := s.gc1()
	if m[namedSessionIdentityMetadata] != requalifyBare || m["alias"] != requalifyBare {
		t.Errorf("the row was migrated: identity=%q alias=%q, want both bare", m[namedSessionIdentityMetadata], m["alias"])
	}
	if len(s.sp.kills) != 0 || len(s.sp.starts) != 0 {
		t.Errorf("kills %v starts %v, want the seat left alone", s.sp.kills, s.sp.starts)
	}
	for _, b := range s.snapshot() {
		if b.ID != "gc-1" && strings.Contains(b.Metadata[namedSessionIdentityMetadata], "golden-hon") {
			t.Errorf("a second row %s took the identity: %v", b.ID, b.Metadata)
		}
	}
	if w := s.snapshot()[simCityLeg+"/gw-gc-1"]; w.Status != "in_progress" || w.Assignee != requalifyBare {
		t.Errorf("work = %s/%q, want in_progress assigned %q", w.Status, w.Assignee, requalifyBare)
	}
	s.noViolations(t)
}
