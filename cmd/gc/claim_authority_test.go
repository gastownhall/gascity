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
