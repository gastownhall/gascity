package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestAgentLookupMatchesFindAgentByTemplate (Gate S fix 3) pins the index
// against findAgentByTemplate as an oracle, agent pointer for agent pointer:
// qualified names, an unbound agent's dir and name, a bound agent's bare name
// (no exact match: legacy fallback), legacy bound and unbound forms,
// duplicate identities (the earlier agent wins), padding, empty and unknown
// templates, and a nil config.
func TestAgentLookupMatchesFindAgentByTemplate(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "planner"},
		{Name: "planner", Dir: "rig-A"},
		{Name: "coder", Dir: "rig-A", BindingName: "gc"},   // rig-A/gc.coder
		{Name: "coder", Dir: "rig-A"},                      // same dir/name, unbound: rig-A/coder
		{Name: "reviewer", Dir: "rig-B", BindingName: "x"}, // only bound: rig-B/x.reviewer
		{Name: "planner", Dir: "rig-A"},                    // a duplicate: the earlier wins
		{Name: "planner"},                                  // a duplicate unbound root: the earlier wins
		{Name: "dup", Dir: "rig-C", BindingName: "one"},
		{Name: "dup", Dir: "rig-C", BindingName: "two"}, // two bound agents: a bare rig-C/dup is ambiguous
		{Name: "twin", Dir: "rig-D", BindingName: "b", Session: "first"},
		{Name: "twin", Dir: "rig-D", BindingName: "b", Session: "second"}, // a duplicate bound identity: the earlier wins
	}}
	templates := []string{
		"planner", " planner ", "rig-A/planner", "rig-A/coder", "rig-A/gc.coder",
		"rig-A/gc.planner", // legacy bound form of the unbound rig-A/planner
		"rig-B/reviewer",   // legacy unbound form of the bound rig-B/x.reviewer
		"rig-B/x.reviewer", "rig-C/dup", "rig-C/one.dup", "rig-C/two.dup",
		"rig-Z/planner", "ghost", "", "   ", "rig-A/", "/planner", "rig-D/b.twin",
	}
	l := newAgentLookup(cfg)
	for _, tmpl := range templates {
		want := findAgentByTemplate(cfg, tmpl)
		if got := l.find(tmpl); got != want {
			t.Errorf("find(%q) = %v, want findAgentByTemplate's %v", tmpl, got, want)
		}
		if got, want := l.normalize(tmpl), normalizeAgentTemplateIdentity(cfg, tmpl); got != want {
			t.Errorf("normalize(%q) = %q, want %q", tmpl, got, want)
		}
	}
	if got := newAgentLookup(nil).find("planner"); got != nil {
		t.Errorf("nil config: %v, want nil", got)
	}
}
