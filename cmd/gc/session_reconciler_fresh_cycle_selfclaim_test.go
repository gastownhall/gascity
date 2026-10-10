package main

// Regression coverage for ga-pvjbx3 (Q1, Q2 and Q3): the self-claim half of
// the #6518 fresh-cycle guard. The guard recognized a self-claim only through
// current_claim_bead_id, which the claim paths builder/investigator actually
// use (`bd update --claim`) never write, and it never re-read the anchor bead
// before killing, so it still cycled rig-scoped fresh-mode sessions out from
// under a live claim or onto an anchor that had already closed. The first five
// tests each encode one live kill shape from 2026-09-25 (supervisor.log) and
// fail without the guard changes in session_bead_cycle.go and
// session_reconciler.go. The tests after them pin the guard's edges: the claim
// scan fails closed, and the Q3 ownership requirement narrows row A only.
//
// The fixtures and assertions are session_reconciler_fresh_cycle_guard_test.go's
// (ga-2weagw), which covers the previous-bead deferral rows.

import (
	"strings"
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

// Q1 is the claim-existence gate with the kill directly behind it, so an
// unreadable scan retains the session (gc-ft31x: retain rather than reap)
// instead of falling through to the cycle: a rig leg that cannot answer may be
// hiding the very claim the scan exists to find. This is the
// BdClaimedNextBeadIsSelfClaim shape with the rig's list queries gone dark
// while Get still answers, so Q2's anchor read would happily say "not closed".
// The failure must also be traced, or a session that never cycles has no
// explanation.
func TestFreshCycleRepro_UnreadableClaimScanRetainsSession(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		"awake_started_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "previous investigation", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	env.setSessionMetadata(&session, map[string]string{sessionpkg.CurrentBeadIDKey: "ga-prev"})
	reproCreate(t, rig, beads.Bead{ID: "ga-new", Title: "claimed via bd update --claim", Type: "task", Status: "in_progress", Assignee: "witness"})
	anchor, _ := rig.Get("ga-new")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": failingListStore{Store: rig}})

	assertNotCycled(t, env, session, sessionName, "the claim scan could not read the rig, so the session may hold a claim the scan could not see")
	if !strings.Contains(env.stderr.String(), "leg is dark") {
		t.Fatalf("stderr = %q, want the claim-scan failure traced", env.stderr.String())
	}
}

// Q3 narrows row A only. Row C (this incarnation started after the previous
// bead closed) stays keyed on the close time alone, so a previous bead closed
// under ANOTHER assignee still defers: that is the normal handoff shape (the
// next owner closes the bead), and the incarnation that started after the
// close is already fresh whoever closed it. The new bead lives only in the
// assigned-work snapshot, so Q1 and Q2 cannot defer it and row C alone decides.
func TestFreshCycleRepro_IncarnationStartedAfterCloseUnderOtherAssigneeDefers(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, nil)
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "handed off, then closed by its new owner", Type: "task", Status: "in_progress", Assignee: "reviewer"})
	reproCloseForReal(t, rig, "ga-prev")
	awake := time.Now().UTC().Add(time.Minute) // started strictly after the close
	env.setSessionMetadata(&session, map[string]string{
		sessionpkg.CurrentBeadIDKey: "ga-prev",
		"awake_started_at":          awake.Format(time.RFC3339Nano),
	})
	anchor := beads.Bead{ID: "ga-new", Title: "new", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "this incarnation started after ga-prev closed, whoever it was assigned to (row C)")
}

// Q3, negative: row A defers only while the previous bead is still assigned to
// THIS session. An open previous bead that was handed to another agent, or
// released with no assignee, will close (if ever) on a timeline unrelated to
// this session's work, so it must not pin the session to its old conversation.
// The new bead lives only in the assigned-work snapshot, so Q1 finds no live
// claim and Q2 finds no closed anchor: the guard must cycle. The positive twin
// (open and still the session's own) is
// TestReconcileSessionBeads_FreshCycleDefers_WhenPreviousBeadStillOpen.
func TestFreshCycleRepro_OpenPreviousBeadNotOwnedBySessionCycles(t *testing.T) {
	for _, tc := range []struct{ name, assignee string }{
		{"assigned to another agent", "reviewer"},
		{"unassigned", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, _ := freshCycleReproEnv(t, map[string]string{
				sessionpkg.CurrentBeadIDKey: "ga-prev",
				"awake_started_at":          time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
			})
			rig := reproMemStore()
			reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "open, but no longer this session's", Type: "task", Status: "open", Assignee: tc.assignee})
			anchor := beads.Bead{ID: "ga-new", Title: "new", Type: "task", Status: "in_progress", Assignee: "witness"}

			reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

			assertCycled(t, env, session, "ga-prev is open but not assigned to this session, so its eventual close is unrelated to this session's work (row A)")
		})
	}
}
