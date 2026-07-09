package main

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
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

// TestPoolReusePredicateInfoTwinsMatchRaw is the equivalence oracle for the
// session.Info reuse/creation predicate siblings. For every corpus bead × agent it
// asserts each Info twin agrees with its raw form, so a mutation of any twin branch
// (a dropped guard, a wrong Info field, a flipped comparison) fails the build. The
// raw predicates are the reference; the twins read the projected Info.
func TestPoolReusePredicateInfoTwinsMatchRaw(t *testing.T) {
	work := []beads.Bead{
		{ID: "w-1", Assignee: "s-open", Status: "in_progress"},
		{ID: "w-2", Assignee: "gc-creating", Status: "open"},
	}
	for _, agent := range wpoolTwinAgents() {
		agent := agent
		bp := &agentBuildParams{city: &config.City{Agents: []config.Agent{*agent}}, assignedWorkBeads: work}
		for _, b := range wpoolTwinCorpus() {
			info := session.InfoFromPersistedBead(b)

			if got, want := poolRuntimeAliasIsDeferredInfo(info), poolRuntimeAliasIsDeferred(b); got != want {
				t.Errorf("poolRuntimeAliasIsDeferred[%s/%s]: info=%v raw=%v", agent.Name, b.ID, got, want)
			}
			if got, want := staleNonExpandingPoolSessionInfo(agent, info), staleNonExpandingPoolSessionBead(agent, b); got != want {
				t.Errorf("staleNonExpandingPoolSession[%s/%s]: info=%v raw=%v", agent.Name, b.ID, got, want)
			}
			if got, want := reusablePoolSessionInfo(bp, agent, "claude", info, nil), reusablePoolSessionBead(bp, agent, "claude", b, nil); got != want {
				t.Errorf("reusablePoolSession[%s/%s]: info=%v raw=%v", agent.Name, b.ID, got, want)
			}
			if got, want := reusableDependencyPoolSessionInfo(bp, "claude", info), reusableDependencyPoolSessionBead(bp, "claude", b); got != want {
				t.Errorf("reusableDependencyPoolSession[%s/%s]: info=%v raw=%v", agent.Name, b.ID, got, want)
			}
		}
	}
}

// TestClaimDesiredPoolSlotInfoMatchesRaw pins that the Info slot-claim sibling
// returns the same slot AND mutates the used-slots map identically to the raw form
// across the corpus, for both a fresh used map and a pre-claimed one.
func TestClaimDesiredPoolSlotInfoMatchesRaw(t *testing.T) {
	cfg := &config.City{}
	for _, agent := range wpoolTwinAgents() {
		agent := agent
		for _, b := range wpoolTwinCorpus() {
			info := session.InfoFromPersistedBead(b)
			for _, seed := range []map[int]bool{{}, {1: true}, {1: true, 2: true}} {
				rawUsed := cloneIntSet(seed)
				infoUsed := cloneIntSet(seed)
				rawSlot := claimDesiredPoolSlot(cfg, agent, b, rawUsed)
				infoSlot := claimDesiredPoolSlotInfo(cfg, agent, info, infoUsed)
				if rawSlot != infoSlot {
					t.Errorf("claimDesiredPoolSlot[%s/%s] seed=%v: info=%d raw=%d", agent.Name, b.ID, seed, infoSlot, rawSlot)
				}
				if !reflect.DeepEqual(rawUsed, infoUsed) {
					t.Errorf("claimDesiredPoolSlot used-map[%s/%s] seed=%v: info=%v raw=%v", agent.Name, b.ID, seed, infoUsed, rawUsed)
				}
			}
		}
	}
}

func cloneIntSet(in map[int]bool) map[int]bool {
	out := make(map[int]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// TestSetPoolTemplateRuntimeIdentityInfoMatchesRaw pins that the Info sibling
// applies byte-identical mutations to a TemplateParams (the deferred-alias env
// clear vs the identity stamp) across the corpus.
func TestSetPoolTemplateRuntimeIdentityInfoMatchesRaw(t *testing.T) {
	for _, b := range wpoolTwinCorpus() {
		info := session.InfoFromPersistedBead(b)
		for _, alias := range []string{"claude-1", "mayor", ""} {
			rawTP := TemplateParams{SessionName: "sess", Env: map[string]string{"X": "1"}}
			infoTP := TemplateParams{SessionName: "sess", Env: map[string]string{"X": "1"}}
			setPoolTemplateRuntimeIdentity(&rawTP, alias, b)
			setPoolTemplateRuntimeIdentityInfo(&infoTP, alias, info)
			if !reflect.DeepEqual(rawTP, infoTP) {
				t.Errorf("setPoolTemplateRuntimeIdentity[%s alias=%q]:\n info=%+v\n raw=%+v", b.ID, alias, infoTP, rawTP)
			}
		}
	}
}

// TestReusablePoolSessionInfosMatchRawOrder pins the general-reuse candidate set AND
// its CreatedAt/ID precedence order: the Info lister must return the same session IDs
// in the same order as the raw lister over a shared snapshot (pool + dependency
// variants). This is the "general reuse order by CreatedAt/ID" half of the pool-slot
// selection precedence characterization, over the typed feed.
func TestReusablePoolSessionInfosMatchRawOrder(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	corpus := []beads.Bead{}
	// Three reusable pool candidates with out-of-order CreatedAt + an equal-CreatedAt
	// pair to exercise the ID tiebreak.
	mk := func(id string, dt time.Duration, dep bool) beads.Bead {
		meta := map[string]string{"template": "claude", "agent_name": "claude", "session_name": "s-" + id, "pool_managed": "true"}
		if dep {
			meta["dependency_only"] = "true"
		}
		b := wpoolSessionBead(id, "open", "claude", nil, meta)
		b.CreatedAt = base.Add(dt)
		return b
	}
	corpus = append(corpus,
		mk("gc-c", 3*time.Hour, false),
		mk("gc-a", 1*time.Hour, false),
		mk("gc-b", 1*time.Hour, false), // same CreatedAt as gc-a -> ID tiebreak
		mk("gc-d", 2*time.Hour, false),
		mk("gc-dep1", 5*time.Hour, true),
		wpoolSessionBead("gc-closed2", "closed", "claude", nil, map[string]string{"template": "claude", "agent_name": "claude", "session_name": "s-cl", "pool_managed": "true"}),
	)
	snap := newSessionBeadSnapshot(corpus)
	agent := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(5)}
	bp := &agentBuildParams{sessionBeads: snap}

	rawPoolIDs := beadIDs(reusablePoolSessionBeads(bp, agent, "claude", nil))
	infoPoolIDs := infoIDs(reusablePoolSessionInfos(bp, agent, "claude", nil))
	if !reflect.DeepEqual(rawPoolIDs, infoPoolIDs) {
		t.Errorf("reusablePoolSession order diverged:\n info=%v\n raw=%v", infoPoolIDs, rawPoolIDs)
	}

	rawDep := beadIDs(reusableDependencyPoolSessionBeads(bp, "claude"))
	infoDep := infoIDs(reusableDependencyPoolSessionInfos(bp, "claude"))
	if !reflect.DeepEqual(rawDep, infoDep) {
		t.Errorf("reusableDependencyPoolSession order diverged:\n info=%v\n raw=%v", infoDep, rawDep)
	}
	// The canonical-singleton finders over the typed feed must match the raw finders
	// (both the pool and dependency variants).
	singleton := &config.Agent{Name: "claude", MaxActiveSessions: intPtr(1)}
	rawCanon, rawOK := findReusableCanonicalNonExpandingPoolSessionBead(bp, singleton, "claude", nil)
	infoCanon, infoOK := findReusableCanonicalNonExpandingPoolSessionInfo(bp, singleton, "claude", nil)
	if rawOK != infoOK || rawCanon.ID != infoCanon.ID {
		t.Errorf("findReusableCanonical: info=(%q,%v) raw=(%q,%v)", infoCanon.ID, infoOK, rawCanon.ID, rawOK)
	}
	rawDepCanon, rawDepOK := findReusableCanonicalNonExpandingDependencyPoolSessionBead(bp, singleton, "claude")
	infoDepCanon, infoDepOK := findReusableCanonicalNonExpandingDependencyPoolSessionInfo(bp, singleton, "claude")
	if rawDepOK != infoDepOK || rawDepCanon.ID != infoDepCanon.ID {
		t.Errorf("findReusableCanonicalDependency: info=(%q,%v) raw=(%q,%v)", infoDepCanon.ID, infoDepOK, rawDepCanon.ID, rawDepOK)
	}
}

func beadIDs(bs []beads.Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func infoIDs(is []session.Info) []string {
	out := make([]string, len(is))
	for i, in := range is {
		out[i] = in.ID
	}
	return out
}

// TestNormalizeNonExpandingPoolSessionInfoIsAuthoritative is the LOAD-BEARING pin
// for the riskiest point in W-pool: the singleton pool-identity collapse. The Info
// normalize must (a) issue the byte-identical bp.beadStore.Update the raw form
// issues (verified by re-reading both stores) and (b) return an Info that equals the
// projection of the persisted, collapsed bead — the "normalize-returns-authoritative-
// value" contract. A mutation of the Info fold (a dropped pool_slot clear, wrong
// alias-history, missing label prune) makes the returned Info diverge from the
// persisted projection and fails this test.
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

	rawStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	infoStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	rawBP := &agentBuildParams{cityPath: cityPath, beadStore: rawStore, city: cfg}
	infoBP := &agentBuildParams{cityPath: cityPath, beadStore: infoStore, city: cfg}

	rawBead, err := normalizeNonExpandingPoolSessionBead(rawBP, cfgAgent, seed())
	if err != nil {
		t.Fatalf("raw normalize: %v", err)
	}
	foldedInfo, err := normalizeNonExpandingPoolSessionInfo(infoBP, cfgAgent, session.InfoFromPersistedBead(seed()))
	if err != nil {
		t.Fatalf("info normalize: %v", err)
	}

	// The collapse must actually have happened (guards against a vacuous pass).
	if foldedInfo.Alias != "mayor" || foldedInfo.PoolSlot != "" || foldedInfo.AgentName != "mayor" {
		t.Fatalf("collapse did not trigger: %+v", foldedInfo)
	}

	// (a) The write is byte-identical: the two stores project to the same Info.
	rawPersisted, err := session.NewStore(beads.SessionStore{Store: rawStore}).Get("gm-1")
	if err != nil {
		t.Fatalf("raw store Get: %v", err)
	}
	infoPersisted, err := session.NewStore(beads.SessionStore{Store: infoStore}).Get("gm-1")
	if err != nil {
		t.Fatalf("info store Get: %v", err)
	}
	if !reflect.DeepEqual(rawPersisted, infoPersisted) {
		t.Errorf("persisted state diverged:\n info=%+v\n raw=%+v", infoPersisted, rawPersisted)
	}

	// (b) The returned Info equals the projection of the raw collapsed bead AND the
	// authoritative persisted projection.
	if !reflect.DeepEqual(foldedInfo, session.InfoFromPersistedBead(rawBead)) {
		t.Errorf("folded Info != projection of raw collapsed bead:\n folded=%+v\n rawproj=%+v", foldedInfo, session.InfoFromPersistedBead(rawBead))
	}
	if !reflect.DeepEqual(foldedInfo, infoPersisted) {
		t.Errorf("normalize did not return the authoritative persisted value:\n folded=%+v\n persisted=%+v", foldedInfo, infoPersisted)
	}
}

// TestRecordDeferredNonExpandingPoolAliasConflictInfoMatchesRaw pins the
// deferred-conflict fallback fold: the Info form records the same alias-clear +
// conflict bookkeeping and returns an Info equal to the projection of the raw form's
// bead, modulo the non-deterministic pool_alias_conflict_at timestamp (asserted
// non-empty on both).
func TestRecordDeferredNonExpandingPoolAliasConflictInfoMatchesRaw(t *testing.T) {
	seed := func() beads.Bead {
		return wpoolSessionBead("gm-2", "open", "mayor", nil, map[string]string{
			"template": "mayor", "agent_name": "mayor", "alias": "mayor", "session_name": "s-2",
			"pool_managed": "true", "pool_alias_conflict_count": "1", "alias_history": "mayor-3",
		})
	}
	cfgAgent := &config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	rawStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	infoStore := beads.NewMemStoreFrom(1, []beads.Bead{seed()}, nil)
	rawBP := &agentBuildParams{beadStore: rawStore}
	infoBP := &agentBuildParams{beadStore: infoStore}

	rawBead, err := recordDeferredNonExpandingPoolAliasConflict(rawBP, cfgAgent, seed())
	if err != nil {
		t.Fatalf("raw recordDeferred: %v", err)
	}
	foldedInfo, err := recordDeferredNonExpandingPoolAliasConflictInfo(infoBP, cfgAgent, session.InfoFromPersistedBead(seed()))
	if err != nil {
		t.Fatalf("info recordDeferred: %v", err)
	}
	rawProj := session.InfoFromPersistedBead(rawBead)
	if foldedInfo.PoolAliasConflictAt == "" || rawProj.PoolAliasConflictAt == "" {
		t.Errorf("pool_alias_conflict_at must be stamped: info=%q raw=%q", foldedInfo.PoolAliasConflictAt, rawProj.PoolAliasConflictAt)
	}
	foldedInfo.PoolAliasConflictAt = ""
	rawProj.PoolAliasConflictAt = ""
	if !reflect.DeepEqual(foldedInfo, rawProj) {
		t.Errorf("recordDeferred fold diverged (ignoring timestamp):\n info=%+v\n raw=%+v", foldedInfo, rawProj)
	}
	if foldedInfo.PoolAliasConflict != "mayor" || foldedInfo.PoolAliasConflictCount != "2" {
		t.Errorf("conflict bookkeeping wrong: conflict=%q count=%q", foldedInfo.PoolAliasConflict, foldedInfo.PoolAliasConflictCount)
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
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "gc-add-" + string(rune('a'+i))
			snap.addInfo(session.InfoFromPersistedBead(wpoolSessionBead(id, "open", "claude", nil, map[string]string{
				"template": "worker", "agent_name": "worker", "session_name": "s-" + id,
			})))
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
