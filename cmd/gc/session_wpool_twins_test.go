package main

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wpoolSessionBead builds an open (or closed) session bead for the W-pool twin
// oracles.
func wpoolSessionBead(id, status, title string, labels []string, meta map[string]string) beads.Bead {
	baseLabels := append([]string{session.LabelSession}, labels...)
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Status:   status,
		Title:    title,
		Labels:   baseLabels,
		Metadata: meta,
	}
}

// wpoolTwinCorpus is a diverse session-bead corpus that reaches every branch of
// the W-pool reuse/creation predicates: open/closed, drained, failed-create,
// asleep, manual (both origins), named, pending/creating (alias-deferred),
// alias-set vs deferred-conflict, pool_slot, dependency_only, and slot-suffixed
// identities. Each twin oracle projects these to session.Info and asserts the Info
// twin agrees with its raw form.
func wpoolTwinCorpus() []beads.Bead {
	return []beads.Bead{
		wpoolSessionBead("gc-open", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-open",
			"pool_managed": "true", "alias": "claude-1", "pool_slot": "1",
		}),
		wpoolSessionBead("gc-closed", "closed", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-closed", "pool_managed": "true",
		}),
		wpoolSessionBead("gc-drained", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-drained",
			"pool_managed": "true", "state": "drained", "session_drainable": "true",
		}),
		wpoolSessionBead("gc-failed", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-failed",
			"pool_managed": "true", "state": string(session.StateFailedCreate),
		}),
		wpoolSessionBead("gc-asleep", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-asleep",
			"pool_managed": "true", "state": "asleep",
		}),
		wpoolSessionBead("gc-manual-origin", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-manual1",
			"session_origin": "manual",
		}),
		wpoolSessionBead("gc-manual-flag", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-manual2",
			"manual_session": "true",
		}),
		wpoolSessionBead("gc-named", "open", "mayor", nil, map[string]string{
			"template": "mayor", "agent_name": "mayor", "session_name": "mayor",
			"configured_named_identity": "mayor", "configured_named_session": "true",
		}),
		wpoolSessionBead("gc-pending", "open", "mayor-1", []string{"agent:mayor-1"}, map[string]string{
			"template": "mayor", "agent_name": "mayor-1", "session_name": "s-pending",
			"pool_managed": "true", "pending_create_claim": "true", "pool_slot": "1",
		}),
		wpoolSessionBead("gc-creating", "open", "mayor-1", []string{"agent:mayor-1"}, map[string]string{
			"template": "mayor", "agent_name": "mayor-1", "session_name": "s-creating",
			"pool_managed": "true", "state": "creating",
		}),
		wpoolSessionBead("gc-deferred", "open", "mayor", nil, map[string]string{
			"template": "mayor", "agent_name": "mayor", "session_name": "s-deferred",
			"pool_managed": "true", "pool_alias_conflict": "mayor", "pool_alias_conflict_count": "2",
		}),
		wpoolSessionBead("gc-startpending", "open", "claude-2", []string{"agent:claude-2"}, map[string]string{
			"template": "claude", "agent_name": "claude-2", "session_name": "s-startpending",
			"pool_managed": "true", "state": string(session.StateStartPending), "pool_slot": "2",
		}),
		wpoolSessionBead("gc-dep", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-dep",
			"dependency_only": "true", "pool_managed": "true",
		}),
		wpoolSessionBead("gc-nosession", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "pool_managed": "true", "alias": "claude-3",
		}),
	}
}

func wpoolTwinAgents() []*config.Agent {
	return []*config.Agent{
		{Name: "claude", MaxActiveSessions: intPtr(5)},                               // multi-slot pool
		{Name: "mayor", MaxActiveSessions: intPtr(1)},                                // singleton pool
		{Name: "claude", MaxActiveSessions: intPtr(3), MinActiveSessions: intPtr(1)}, // bounded pool
	}
}

// TestPoolReuseTwinsCharacterization is the PERMANENT characterization of the
// W-pool session.Info reuse/creation/slot/stamp siblings, pinned against a golden
// captured over the diverse corpus. It replaced the raw-vs-Info equivalence oracles
// whose raw reference forms retired with the snapshot raw half in WI-7 W-delete. A
// mutation of any twin branch (a dropped guard, a wrong Info field, a flipped
// comparison, a reordered candidate list) changes an output and fails the build.
func TestPoolReuseTwinsCharacterization(t *testing.T) {
	agents := wpoolTwinAgents()
	corpus := wpoolTwinCorpus()
	work := []beads.Bead{
		{ID: "w-1", Assignee: "s-open", Status: "in_progress"},
		{ID: "w-2", Assignee: "gc-creating", Status: "open"},
	}

	gotAliasDeferred := map[string]bool{}
	for _, b := range corpus {
		gotAliasDeferred[b.ID] = poolRuntimeAliasIsDeferredInfo(sessiontest.SeedBead(t, b))
	}
	gotReusable := map[string]bool{}
	gotDepReusable := map[string]bool{}
	gotSlot := map[string]int{}
	for ai, agent := range agents {
		bp := &agentBuildParams{city: &config.City{Agents: []config.Agent{*agent}}, assignedWorkBeads: work}
		cfg := &config.City{}
		for _, b := range corpus {
			info := sessiontest.SeedBead(t, b)
			k := fmt.Sprintf("%d/%s", ai, b.ID)
			gotReusable[k] = reusablePoolSessionInfo(bp, agent, "claude", info, nil)
			gotDepReusable[k] = reusableDependencyPoolSessionInfo(bp, "claude", info)
			used := map[int]bool{1: true}
			gotSlot[k] = claimDesiredPoolSlotInfo(cfg, agent, info, used)
		}
	}
	gotStamp := map[string]string{}
	for _, b := range corpus {
		info := sessiontest.SeedBead(t, b)
		for _, alias := range []string{"claude-1", "mayor", ""} {
			tp := TemplateParams{SessionName: "sess", Env: map[string]string{"X": "1"}}
			setPoolTemplateRuntimeIdentityInfo(&tp, alias, info)
			gotStamp[b.ID+"|"+alias] = fmt.Sprintf("alias=%q stamped=%v env=%v", tp.Alias, tp.EnvIdentityStamped, tp.Env)
		}
	}

	wantAliasDeferred := map[string]bool{"gc-asleep": false, "gc-closed": false, "gc-creating": true, "gc-deferred": true, "gc-dep": false, "gc-drained": false, "gc-failed": false, "gc-manual-flag": false, "gc-manual-origin": false, "gc-named": false, "gc-nosession": false, "gc-open": false, "gc-pending": true, "gc-startpending": true}
	if !reflect.DeepEqual(gotAliasDeferred, wantAliasDeferred) {
		t.Errorf("poolRuntimeAliasIsDeferredInfo drift:\n got=%#v\nwant=%#v", gotAliasDeferred, wantAliasDeferred)
	}
	wantReusable := map[string]bool{"0/gc-asleep": false, "0/gc-closed": false, "0/gc-creating": false, "0/gc-deferred": false, "0/gc-dep": true, "0/gc-drained": false, "0/gc-failed": false, "0/gc-manual-flag": false, "0/gc-manual-origin": false, "0/gc-named": false, "0/gc-nosession": true, "0/gc-open": false, "0/gc-pending": false, "0/gc-startpending": true, "1/gc-asleep": false, "1/gc-closed": false, "1/gc-creating": false, "1/gc-deferred": false, "1/gc-dep": true, "1/gc-drained": false, "1/gc-failed": false, "1/gc-manual-flag": false, "1/gc-manual-origin": false, "1/gc-named": false, "1/gc-nosession": true, "1/gc-open": false, "1/gc-pending": false, "1/gc-startpending": true, "2/gc-asleep": false, "2/gc-closed": false, "2/gc-creating": false, "2/gc-deferred": false, "2/gc-dep": true, "2/gc-drained": false, "2/gc-failed": false, "2/gc-manual-flag": false, "2/gc-manual-origin": false, "2/gc-named": false, "2/gc-nosession": true, "2/gc-open": false, "2/gc-pending": false, "2/gc-startpending": true}
	if !reflect.DeepEqual(gotReusable, wantReusable) {
		t.Errorf("reusablePoolSessionInfo drift:\n got=%#v\nwant=%#v", gotReusable, wantReusable)
	}
	wantDepReusable := map[string]bool{"0/gc-asleep": false, "0/gc-closed": false, "0/gc-creating": false, "0/gc-deferred": false, "0/gc-dep": true, "0/gc-drained": false, "0/gc-failed": false, "0/gc-manual-flag": false, "0/gc-manual-origin": false, "0/gc-named": false, "0/gc-nosession": false, "0/gc-open": false, "0/gc-pending": false, "0/gc-startpending": false, "1/gc-asleep": false, "1/gc-closed": false, "1/gc-creating": false, "1/gc-deferred": false, "1/gc-dep": true, "1/gc-drained": false, "1/gc-failed": false, "1/gc-manual-flag": false, "1/gc-manual-origin": false, "1/gc-named": false, "1/gc-nosession": false, "1/gc-open": false, "1/gc-pending": false, "1/gc-startpending": false, "2/gc-asleep": false, "2/gc-closed": false, "2/gc-creating": false, "2/gc-deferred": false, "2/gc-dep": true, "2/gc-drained": false, "2/gc-failed": false, "2/gc-manual-flag": false, "2/gc-manual-origin": false, "2/gc-named": false, "2/gc-nosession": false, "2/gc-open": false, "2/gc-pending": false, "2/gc-startpending": false}
	if !reflect.DeepEqual(gotDepReusable, wantDepReusable) {
		t.Errorf("reusableDependencyPoolSessionInfo drift:\n got=%#v\nwant=%#v", gotDepReusable, wantDepReusable)
	}
	wantSlot := map[string]int{"0/gc-asleep": 2, "0/gc-closed": 2, "0/gc-creating": 2, "0/gc-deferred": 2, "0/gc-dep": 2, "0/gc-drained": 2, "0/gc-failed": 2, "0/gc-manual-flag": 2, "0/gc-manual-origin": 2, "0/gc-named": 2, "0/gc-nosession": 3, "0/gc-open": 0, "0/gc-pending": 2, "0/gc-startpending": 2, "1/gc-asleep": 0, "1/gc-closed": 0, "1/gc-creating": 0, "1/gc-deferred": 0, "1/gc-dep": 0, "1/gc-drained": 0, "1/gc-failed": 0, "1/gc-manual-flag": 0, "1/gc-manual-origin": 0, "1/gc-named": 0, "1/gc-nosession": 0, "1/gc-open": 0, "1/gc-pending": 0, "1/gc-startpending": 0, "2/gc-asleep": 2, "2/gc-closed": 2, "2/gc-creating": 2, "2/gc-deferred": 2, "2/gc-dep": 2, "2/gc-drained": 2, "2/gc-failed": 2, "2/gc-manual-flag": 2, "2/gc-manual-origin": 2, "2/gc-named": 2, "2/gc-nosession": 3, "2/gc-open": 0, "2/gc-pending": 2, "2/gc-startpending": 2}
	if !reflect.DeepEqual(gotSlot, wantSlot) {
		t.Errorf("claimDesiredPoolSlotInfo drift:\n got=%#v\nwant=%#v", gotSlot, wantSlot)
	}
	wantStamp := map[string]string{"gc-asleep|": "alias=\"\" stamped=false env=map[X:1]", "gc-asleep|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-asleep|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-closed|": "alias=\"\" stamped=false env=map[X:1]", "gc-closed|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-closed|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-creating|": "alias=\"\" stamped=false env=map[X:1]", "gc-creating|claude-1": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-creating|mayor": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-deferred|": "alias=\"\" stamped=false env=map[X:1]", "gc-deferred|claude-1": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-deferred|mayor": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-dep|": "alias=\"\" stamped=false env=map[X:1]", "gc-dep|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-dep|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-drained|": "alias=\"\" stamped=false env=map[X:1]", "gc-drained|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-drained|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-failed|": "alias=\"\" stamped=false env=map[X:1]", "gc-failed|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-failed|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-manual-flag|": "alias=\"\" stamped=false env=map[X:1]", "gc-manual-flag|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-manual-flag|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-manual-origin|": "alias=\"\" stamped=false env=map[X:1]", "gc-manual-origin|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-manual-origin|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-named|": "alias=\"\" stamped=false env=map[X:1]", "gc-named|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-named|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-nosession|": "alias=\"\" stamped=false env=map[X:1]", "gc-nosession|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-nosession|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-open|": "alias=\"\" stamped=false env=map[X:1]", "gc-open|claude-1": "alias=\"claude-1\" stamped=true env=map[GC_AGENT:claude-1 GC_ALIAS:claude-1 X:1]", "gc-open|mayor": "alias=\"mayor\" stamped=true env=map[GC_AGENT:mayor GC_ALIAS:mayor X:1]", "gc-pending|": "alias=\"\" stamped=false env=map[X:1]", "gc-pending|claude-1": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-pending|mayor": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-startpending|": "alias=\"\" stamped=false env=map[X:1]", "gc-startpending|claude-1": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]", "gc-startpending|mayor": "alias=\"\" stamped=false env=map[GC_AGENT:sess GC_ALIAS: X:1]"}
	if !reflect.DeepEqual(gotStamp, wantStamp) {
		t.Errorf("setPoolTemplateRuntimeIdentityInfo drift:\n got=%#v\nwant=%#v", gotStamp, wantStamp)
	}
}

// TestClaimDesiredPoolSlotInfoMarksUsedSlot restores the used-map SIDE-EFFECT pin that
// the retired TestClaimDesiredPoolSlotInfoMatchesRaw carried (across the same three seed
// maps): a claim that returns slot s (>0) MUST mark used[s]. Dropping the used[slot]=true
// write — in either the existing-slot branch or the incrementing loop — lets two
// candidates claim the same slot and mint duplicate pool identities; the return-value
// golden alone (TestPoolReuseTwinsCharacterization, single seed) does NOT catch it. This
// is a self-checking invariant (resulting map == seed ∪ {slot}, and a second claim never
// re-hands the same slot), so it exercises both branches over the full corpus.
func TestClaimDesiredPoolSlotInfoMarksUsedSlot(t *testing.T) {
	cfg := &config.City{}
	seeds := []map[int]bool{{}, {1: true}, {1: true, 2: true}}
	claimed := false
	for ai, agent := range wpoolTwinAgents() {
		for _, b := range wpoolTwinCorpus() {
			info := sessiontest.SeedBead(t, b)
			for si, seed := range seeds {
				used := map[int]bool{}
				for k := range seed {
					used[k] = true
				}
				slot := claimDesiredPoolSlotInfo(cfg, agent, info, used)

				want := map[int]bool{}
				for k := range seed {
					want[k] = true
				}
				if slot > 0 {
					claimed = true
					want[slot] = true
					if !used[slot] {
						t.Errorf("agent%d/%s/seed%d: claimed slot %d NOT marked in used=%v (two candidates would claim it)", ai, b.ID, si, slot, used)
					}
					// A second claim on the resulting map never re-hands the same slot.
					used2 := map[int]bool{}
					for k := range used {
						used2[k] = true
					}
					if slot2 := claimDesiredPoolSlotInfo(cfg, agent, info, used2); slot2 == slot {
						t.Errorf("agent%d/%s/seed%d: second claim re-handed slot %d (used[slot] not persisted)", ai, b.ID, si, slot)
					}
				}
				if !reflect.DeepEqual(used, want) {
					t.Errorf("agent%d/%s/seed%d: resulting used=%v, want seed ∪ {slot} = %v", ai, b.ID, si, used, want)
				}
			}
		}
	}
	if !claimed {
		t.Fatal("no corpus bead × agent claimed a non-zero slot; the used-map side effect was never exercised")
	}
}

// TestReusablePoolSessionInfosOrder pins the general-reuse candidate set AND its
// CreatedAt/ID precedence order over the typed feed (the "general reuse order by
// CreatedAt/ID" half of the pool-slot selection precedence characterization).
func TestReusablePoolSessionInfosOrder(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, dt time.Duration, dep bool) beads.Bead {
		meta := map[string]string{"template": "claude", "agent_name": "claude", "session_name": "s-" + id, "pool_managed": "true"}
		if dep {
			meta["dependency_only"] = "true"
		}
		b := wpoolSessionBead(id, "open", "claude", nil, meta)
		b.CreatedAt = base.Add(dt)
		return b
	}
	corpus := []beads.Bead{
		mk("gc-c", 3*time.Hour, false),
		mk("gc-a", 1*time.Hour, false),
		mk("gc-b", 1*time.Hour, false), // same CreatedAt as gc-a -> ID tiebreak
		mk("gc-d", 2*time.Hour, false),
		mk("gc-dep1", 5*time.Hour, true),
		wpoolSessionBead("gc-closed2", "closed", "claude", nil, map[string]string{"template": "claude", "agent_name": "claude", "session_name": "s-cl", "pool_managed": "true"}),
	}
	snap := newSessionBeadSnapshot(corpus)
	agent := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{sessionBeads: snap}

	// General pool reuse: all open non-dependency + dependency candidates, ordered
	// CreatedAt asc with the ID tiebreak (gc-a before gc-b at 1h), closed excluded.
	if got, want := infoIDs(reusablePoolSessionInfos(bp, agent, "claude", nil)), []string{"gc-a", "gc-b", "gc-d", "gc-c", "gc-dep1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reusablePoolSessionInfos order = %v, want %v (CreatedAt asc, ID tiebreak; closed excluded)", got, want)
	}
	if got, want := infoIDs(reusableDependencyPoolSessionInfos(bp, "claude")), []string{"gc-dep1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reusableDependencyPoolSessionInfos = %v, want %v", got, want)
	}
	// The canonical-singleton finder over the typed feed resolves the earliest-created
	// reusable candidate (gc-a) for a singleton agent.
	singleton := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(1)}
	canon, ok := findReusableCanonicalNonExpandingPoolSessionInfo(bp, singleton, "claude", nil)
	if !ok || canon.ID != "gc-a" {
		t.Errorf("findReusableCanonicalNonExpandingPoolSessionInfo = (%q, %v), want (gc-a, true)", canon.ID, ok)
	}
}

func infoIDs(is []session.Info) []string {
	out := make([]string, len(is))
	for i, in := range is {
		out[i] = in.ID
	}
	return out
}

// TestNormalizeNonExpandingPoolSessionInfoIsAuthoritative is the LOAD-BEARING pin for
// the riskiest point in W-pool: the singleton pool-identity collapse. The Info
// normalize must (a) persist the collapse (verified by re-reading the store) and
// (b) return an Info that equals the projection of the persisted, collapsed bead — the
// "normalize-returns-authoritative-value" contract. A mutation of the Info fold (a
// dropped pool_slot clear, wrong alias-history, missing label prune) makes the returned
// Info diverge from the persisted projection and fails this test.
func TestNormalizeNonExpandingPoolSessionInfoIsAuthoritative(t *testing.T) {
	seed := func() beads.Bead {
		return wpoolSessionBead("gm-1", "open", "mayor-1", []string{"agent:mayor-1"}, map[string]string{
			"template": "mayor", "agent_name": "mayor-1", "alias": "mayor-1",
			"pool_slot": "1", "session_name": "s-mayor-1", "pool_managed": "true",
			"alias_history": "mayor-9",
		})
	}
	cfgAgent := &config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{Agents: []config.Agent{*cfgAgent}}
	cityPath := t.TempDir()

	infoStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	infoBP := &agentBuildParams{cityPath: cityPath, beadStore: infoStore, city: cfg}

	foldedInfo, err := normalizeNonExpandingPoolSessionInfo(infoBP, cfgAgent, sessiontest.SeedBead(t, seed()))
	if err != nil {
		t.Fatalf("info normalize: %v", err)
	}

	// The collapse must actually have happened (guards against a vacuous pass).
	if foldedInfo.Alias != "mayor" || foldedInfo.PoolSlot != "" || foldedInfo.AgentName != "mayor" {
		t.Fatalf("collapse did not trigger: %+v", foldedInfo)
	}

	// normalize returns the authoritative persisted value.
	infoPersisted, err := session.NewStore(beads.SessionStore{Store: infoStore}).Get("gm-1")
	if err != nil {
		t.Fatalf("info store Get: %v", err)
	}
	if !reflect.DeepEqual(foldedInfo, infoPersisted) {
		t.Errorf("normalize did not return the authoritative persisted value:\n folded=%+v\n persisted=%+v", foldedInfo, infoPersisted)
	}
}

// TestRecordDeferredNonExpandingPoolAliasConflictInfoFold pins the deferred-conflict
// fallback fold: the Info form clears the alias, bumps the conflict bookkeeping, stamps
// pool_alias_conflict_at, and returns the authoritative persisted projection.
func TestRecordDeferredNonExpandingPoolAliasConflictInfoFold(t *testing.T) {
	seed := func() beads.Bead {
		return wpoolSessionBead("gm-2", "open", "mayor", nil, map[string]string{
			"template": "mayor", "agent_name": "mayor", "alias": "mayor", "session_name": "s-2",
			"pool_managed": "true", "pool_alias_conflict_count": "1", "alias_history": "mayor-3",
		})
	}
	cfgAgent := &config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	infoStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	infoBP := &agentBuildParams{beadStore: infoStore}

	foldedInfo, err := recordDeferredNonExpandingPoolAliasConflictInfo(infoBP, cfgAgent, sessiontest.SeedBead(t, seed()))
	if err != nil {
		t.Fatalf("info recordDeferred: %v", err)
	}
	if foldedInfo.PoolAliasConflictAt == "" {
		t.Errorf("pool_alias_conflict_at must be stamped: %q", foldedInfo.PoolAliasConflictAt)
	}
	if foldedInfo.PoolAliasConflict != "mayor" || foldedInfo.PoolAliasConflictCount != "2" {
		t.Errorf("conflict bookkeeping wrong: conflict=%q count=%q", foldedInfo.PoolAliasConflict, foldedInfo.PoolAliasConflictCount)
	}
	infoPersisted, err := session.NewStore(beads.SessionStore{Store: infoStore}).Get("gm-2")
	if err != nil {
		t.Fatalf("info store Get: %v", err)
	}
	if !reflect.DeepEqual(foldedInfo, infoPersisted) {
		t.Errorf("recordDeferred did not return the authoritative persisted value:\n folded=%+v\n persisted=%+v", foldedInfo, infoPersisted)
	}
}

// TestSnapshotAddInfoConcurrentAndCoherent reruns the parallel-create add() safety
// contract (gastownhall/gascity#2319) against the new addInfo: concurrent addInfo
// calls must not race or drop entries, and after all adds the snapshot's typed half
// (OpenInfos / OpenForReconcile / the id lookups) is coherent. Run with -race.
func TestSnapshotAddInfoConcurrentAndCoherent(t *testing.T) {
	snap := newSessionBeadSnapshot([]beads.Bead{
		wpoolSessionBead("gc-seed", "open", "claude", nil, map[string]string{
			"template": "claude", "agent_name": "claude", "session_name": "s-seed", "pool_managed": "true",
		}),
	})
	const n = 16
	// Pre-project the fixtures on the test goroutine: sessiontest.SeedBead can
	// t.Fatalf, which is only valid from the test goroutine, so only the
	// concurrency under test (snap.addInfo) runs inside the spawned goroutines.
	added := make([]session.Info, n)
	for i := 0; i < n; i++ {
		id := "gc-add-" + string(rune('a'+i))
		added[i] = sessiontest.SeedBead(t, wpoolSessionBead(id, "open", "claude", nil, map[string]string{
			"template": "worker", "agent_name": "worker", "session_name": "s-" + id,
		}))
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snap.addInfo(added[i])
		}(i)
	}
	wg.Wait()

	if got := len(snap.OpenInfos()); got != n+1 {
		t.Fatalf("OpenInfos len = %d, want %d", got, n+1)
	}
	if got := len(snap.OpenForReconcile()); got != n+1 {
		t.Fatalf("OpenForReconcile len = %d, want %d", got, n+1)
	}
	// OpenForReconcile stays lockstep with OpenInfos, and every added id resolves.
	rows := snap.OpenForReconcile()
	infos := snap.OpenInfos()
	for i := range infos {
		if rows[i].Info.ID != infos[i].ID {
			t.Fatalf("row %d: OpenForReconcile id %q != OpenInfos id %q", i, rows[i].Info.ID, infos[i].ID)
		}
	}
	for i := 0; i < n; i++ {
		id := "gc-add-" + string(rune('a'+i))
		if _, ok := snap.FindInfoByID(id); !ok {
			t.Errorf("FindInfoByID(%q) missing after concurrent addInfo", id)
		}
	}
}
