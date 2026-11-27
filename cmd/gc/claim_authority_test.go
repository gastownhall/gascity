package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func claimAuthorityIdentities(entries []beads.ClaimAuthorityEntry) map[string]beads.ClaimAuthoritySource {
	out := make(map[string]beads.ClaimAuthoritySource, len(entries))
	for _, entry := range entries {
		out[entry.Identity] = entry.Source
	}
	return out
}

// AC-2: the authority set holds every assignee identity of every open
// session in the snapshot (the identities the orphan sweep accepts as
// owned), including a pool session's runtime name, and nothing of a closed
// session.
func TestClaimAuthorityEntriesCoverOpenSessionIdentities(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "gc"}}
	pool := session.Info{ID: "gc-s1", SessionNameMetadata: "pool-gc-s1", Alias: "rig/pool-1", AliasHistory: []string{"rig/pool-old"}}
	closed := session.Info{ID: "gc-s2", SessionNameMetadata: "pool-gc-s2", Alias: "rig/pool-2", Closed: true}

	got := claimAuthorityIdentities(claimAuthorityEntries(cfg, []session.Info{pool, closed}))
	for _, identity := range sessionBeadAssigneeIdentitiesForConfigInfo(pool, cfg) {
		if got[identity] != beads.ClaimAuthoritySession {
			t.Errorf("authority set lacks open session identity %q (got %v)", identity, got)
		}
	}
	for _, identity := range []string{"gc-s2", "pool-gc-s2", "rig/pool-2"} {
		if _, ok := got[identity]; ok {
			t.Errorf("authority set holds closed session identity %q", identity)
		}
	}
}

// AC-3: the authority set holds every assignee form of a configured named
// session that is not running (its qualified identity and its runtime name),
// each of which the orphan sweep keeps through
// assigneePreservesNamedSessionRoute, plus every [beads]
// claim_authority_allow identity.
func TestClaimAuthorityEntriesCoverNamedSessionsAndAllowlist(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc"},
		Rigs:      []config.Rig{{Name: "gascity", Path: cityPath + "/gascity"}},
		Agents: []config.Agent{{
			Name:         "patrol",
			Dir:          "gascity",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{Template: "patrol", Dir: "gascity", Mode: "on_demand"}},
		Beads:         config.BeadsConfig{ClaimAuthorityAllow: []string{"operator"}},
	}
	got := claimAuthorityIdentities(claimAuthorityEntries(cfg, nil))

	const identity = "gascity/patrol"
	spec, ok := findNamedSessionSpec(cfg, cfg.EffectiveCityName(), identity)
	if !ok {
		t.Fatalf("test premise broken: %q is not a configured named session", identity)
	}
	forms := []string{identity, spec.SessionName}
	if forms[0] == forms[1] {
		t.Fatalf("test premise broken: identity and runtime name are both %q", identity)
	}
	for _, form := range forms {
		if got[form] != beads.ClaimAuthorityNamed {
			t.Errorf("authority set lacks named session form %q (got %v)", form, got)
		}
	}
	for form, source := range got {
		if source != beads.ClaimAuthorityNamed {
			continue
		}
		if !assigneePreservesNamedSessionRoute(cfg, cityPath, namedSessionBackingTemplate(spec), form, "", false) {
			t.Errorf("published named form %q is one the orphan sweep would release", form)
		}
	}
	if got["operator"] != beads.ClaimAuthorityAllow {
		t.Errorf("authority set lacks allowlisted identity operator (got %v)", got)
	}
}
