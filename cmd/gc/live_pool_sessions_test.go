package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestLivePoolSessionsMatchThePairwiseEquivalence pins the legacy demand
// count's behavior across the one-pass rewrite (perf fix 1): for every
// configured agent, the number of live pool sessions livePoolSessionsByIdentity
// counts for it equals the old walk's, which matched each live pool session
// pairwise by agentTemplateIdentitiesEquivalent. The table spans the cases
// buildDemandTargets' comment names: a legacy bound form of the same identity
// counts; an unqualified base name never counts for a dir-scoped agent; a
// same-base-name pool in another rig counts for itself only; padding is
// trimmed; an empty template, an asleep or drained session, and a session
// that is not pool-managed count for nothing.
func TestLivePoolSessionsMatchThePairwiseEquivalence(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		poolAgent("planner", "", intPtr(5), 0),
		poolAgent("planner", "rig-A", intPtr(5), 0),
		poolAgent("planner", "rig-B", intPtr(5), 0),
		poolAgent("coder", "rig-A", intPtr(3), 0),
	}}
	live := func(template string, meta ...string) session.Info {
		i := session.Info{Template: template, PoolManaged: true, MetadataState: "active"}
		for k := 0; k+1 < len(meta); k += 2 {
			switch meta[k] {
			case "state":
				i.MetadataState = meta[k+1]
			case "pool":
				i.PoolManaged = meta[k+1] == "true"
			}
		}
		return i
	}
	infos := []session.Info{
		live("planner"), live("planner"),
		live("rig-A/planner"), live("rig-A/gc.planner"), // the legacy bound form of rig-A/planner
		live(" rig-B/planner "),
		live("rig-A/coder"), live("rig-A/coder", "state", "asleep"),
		live("rig-A/coder", "pool", "false"),
		live(""), live("   "),
		live("rig-C/ghost"), // no configured agent: counts for nothing configured
	}
	lookup := newAgentLookup(cfg)
	got := livePoolSessionsByIdentity(lookup, infos)
	for i := range cfg.Agents {
		template := cfg.Agents[i].QualifiedName()
		want := 0
		for _, si := range infos {
			if isPoolManagedSessionInfo(si) && poolSessionIsLiveInfo(si) && agentTemplateIdentitiesEquivalent(cfg, si.Template, template) {
				want++
			}
		}
		if n := got.count(lookup, template); n != want {
			t.Errorf("%s: %d live pool sessions, want %d (the pairwise walk's)", template, n, want)
		}
	}
	for template, want := range map[string]int{"planner": 2, "rig-A/planner": 2, "rig-A/gc.planner": 2, "rig-B/planner": 1, "rig-A/coder": 1, "": 0, "  ": 0} {
		if n := got.count(lookup, template); n != want {
			t.Errorf("%q: %d live pool sessions, want %d", template, n, want)
		}
	}
}
