package main

// Regression coverage for ga-pvjbx3 (Q1 + Q2): the self-claim half of the
// #6518 fresh-cycle guard. The guard recognized a self-claim only through
// current_claim_bead_id, which the claim paths builder/investigator actually
// use (`bd update --claim`) never write, and it never re-read the anchor bead
// before killing, so it still cycled rig-scoped fresh-mode sessions out from
// under a live claim or onto an anchor that had already closed. Each test
// encodes one live kill shape from 2026-09-25 (supervisor.log) and fails
// without the guard changes in session_bead_cycle.go and session_reconciler.go.
//
// The fixtures and assertions are session_reconciler_fresh_cycle_guard_test.go's
// (ga-2weagw), which covers the previous-bead deferral rows.

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Cases 2, 3 and 5 (gascity--investigator). The session worked the previous
// bead, closed it, and claimed its next bead itself with `bd update --claim`
// (the investigator prompt's documented claim path). That path never writes
// current_claim_bead_id, so row E (self-claimed) cannot fire, and rows D/F
// cycle it. Q1's existence check finds the newly-claimed bead directly
// (in_progress, assigned to this session) without ever consulting
// current_claim_bead_id.
func TestFreshCycleRepro_BdClaimedNextBeadIsSelfClaim(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "previous investigation", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-prev"})
	reproCreate(t, rig, beads.Bead{ID: "ga-new", Title: "claimed via bd update --claim", Type: "task", Status: "in_progress", Assignee: "witness"})
	anchor, _ := rig.Get("ga-new")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "the session claimed ga-new itself (bd update --claim); no current_claim_bead_id is ever written on that path")
}

// The builder's live shape: current_claim_bead_id is STALE (an unrelated
// unassigned bead) because a raw bd release does not clear it, so it never
// equals any anchor. Q1's existence check does not depend on the stamp at
// all, so a stale stamp cannot hide the session's real, live claim.
func TestFreshCycleRepro_StaleSelfClaimStampDoesNotHideRealClaim(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at":                     time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		beadmeta.CurrentClaimBeadIDMetadataKey: "ga-stale",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-stale", Title: "released long ago", Type: "task", Status: "open"})
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "previous step", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-prev"})
	reproCreate(t, rig, beads.Bead{ID: "ga-new", Title: "claimed step", Type: "task", Status: "in_progress", Assignee: "witness"})
	anchor, _ := rig.Get("ga-new")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "current_claim_bead_id is a stale unrelated bead; the session's real claim is ga-new")
}

// Mayor's ask (a): the session self-claimed the ROOT (source bead) while the
// reconciler's anchor is one of its STEPS. Q1's existence check is
// anchor-agnostic — it finds ga-src itself still in_progress+assigned in the
// same candidate population, regardless of what the anchor separately
// resolved to, so no molecule-membership comparison is needed.
func TestFreshCycleRepro_SelfClaimOfRootWhileAnchorIsStep(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at":                     time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		beadmeta.CurrentClaimBeadIDMetadataKey: "ga-src",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-mol", Title: "molecule", Type: "molecule", Status: "open"})
	reproCreate(t, rig, beads.Bead{ID: "ga-src", Title: "source bead", Type: "task", Status: "in_progress", Assignee: "witness", Metadata: map[string]string{"molecule_id": "ga-mol"}})
	reproCreate(t, rig, beads.Bead{ID: "ga-step1", Title: "RED", Type: "step", Status: "in_progress", Assignee: "witness", ParentID: "ga-mol"})
	reproCloseForReal(t, rig, "ga-step1")
	reproCreate(t, rig, beads.Bead{ID: "ga-step2", Title: "GREEN", Type: "step", Status: "in_progress", Assignee: "witness", ParentID: "ga-mol"})
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-step1"})
	step2, _ := rig.Get("ga-step2")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{step2}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "anchor ga-step2 is a step of the molecule whose source bead ga-src the session claimed")
}

// Mayor's ask (b), and live case 4 (23:05:15, ga-851h6i -> ga-g4odhq): the
// session claimed the next STEP while the anchor falls back to the
// molecule's own ROOT/source bead. Symmetric to the root/step case above.
func TestFreshCycleRepro_SelfClaimOfStepWhileAnchorIsRoot(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at":                     time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		beadmeta.CurrentClaimBeadIDMetadataKey: "ga-step2",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-mol", Title: "molecule", Type: "molecule", Status: "open"})
	reproCreate(t, rig, beads.Bead{ID: "ga-src", Title: "source bead", Type: "task", Status: "in_progress", Assignee: "witness", Metadata: map[string]string{"molecule_id": "ga-mol"}})
	reproCreate(t, rig, beads.Bead{ID: "ga-step1", Title: "RED", Type: "step", Status: "in_progress", Assignee: "witness", ParentID: "ga-mol"})
	reproCloseForReal(t, rig, "ga-step1")
	reproCreate(t, rig, beads.Bead{ID: "ga-step2", Title: "GREEN", Type: "step", Status: "in_progress", Assignee: "witness", ParentID: "ga-mol"})
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-step1"})
	src, _ := rig.Get("ga-src")
	step2, _ := rig.Get("ga-step2")

	// Anchor falls back to the FIRST matching work bead: the source bead.
	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{src, step2}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "anchor ga-src is the source bead of the molecule whose step ga-step2 the session claimed")
}

// Case 5 (23:18:04, gascity--investigator ga-knges1 -> ga-1sss9q): the tick
// decided on a ~4-minute-old assigned-work snapshot. The anchor it cycled the
// session ONTO was already closed by kill time and the session had already
// self-claimed its next bead. Q2's live re-read of the anchor sees it closed
// and skips the cycle.
func TestFreshCycleRepro_AnchorAlreadyClosedDoesNotCycle(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "previous", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	reproCreate(t, rig, beads.Bead{ID: "ga-anchor", Title: "already finished", Type: "task", Status: "in_progress", Assignee: "witness"})
	staleSnapshot, _ := rig.Get("ga-anchor") // what the slow tick still holds
	reproCloseForReal(t, rig, "ga-anchor")   // live truth at kill time
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-prev"})

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{staleSnapshot}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "the anchor ga-anchor is already closed on a live read; cycling onto it is pure loss")
}
