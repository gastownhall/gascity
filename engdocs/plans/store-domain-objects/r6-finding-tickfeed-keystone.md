# R6 structural finding: the raw-half deletion is DOWNSTREAM of the tick-feed refactor + pool typing

The R6 agent (grounded at 1b93614da, corroborated by an independent verifier + the code's OWN in-line
sanctions at session_bead_snapshot.go:273-284 and build_desired_state.go:4063-4069) proved the raw
sessionBeadSnapshot half cannot be deleted in isolation. It serves THREE load-bearing consumer classes:

(a) RECONCILER-TICK FEEDS — Open() produces the raw `sessions []beads.Bead` the reconciler MUTATES IN PLACE
    (Phase-0 heal session_reconciler.go:1187; dedup :1208) and projects at the WI-7 tick edges (:1342/:1419).
    Sites: cmd_start.go:929/943 → reconcileSessionBeadsAtPathWithNamedDemand(open); city_runtime.go:2252 →
    reconcileSessionBeadsTracedWithNamedDemand(open); city_runtime.go:1159 → finalizeDrainAckStopPendingSessions
    (itself the finalize tick edge, session_reconciler.go:556/582); city_runtime.go:3122→2962→2976.

(b) POOL SELECTION/CREATION PATH — raw beads are REUSED/CREATED, not just read: findOpenSessionBeadByID
    (build_desired_state.go:3607) → selectOrPlanPoolSessionBead (returns/reuses beads.Bead); reusablePoolSessionBeads
    (:3836) + reusableDependencyPoolSessionBeads (:4474) → normalizeNonExpandingPoolSessionBeadForSelection /
    createPoolSessionBeadWithGuardedAlias. add() inserts freshly-created/reopened raw beads for parallel pool
    realization at TWO sites (session_name_lookup.go:301, session_template_start.go:110). In-code sanction at
    build_desired_state.go:4064: "stays raw until that whole path is typed in WI-6."

(c) RAW SYNC/HEAL PATH — syncSessionBeadsWithSnapshotAndRigStores MUTATES raw openBeads in place
    (openBeads[idx].Status="closed") and REBUILDS newSessionBeadSnapshot(openBeads) (session_beads.go:1768) as
    the snapshot the reconciler+pool then consume; loadSessionBeads callers 1-3 (snapshotOrLoadSessionBeads,
    findOpenSessionBeadBySessionName, loadVisibleBySessionName) live here.

## Census is BLOCKED at every target until the above are typed
- ListAllSessionBeads (3 sites): session_bead_snapshot.go:90 (loadSessionBeadSnapshot must keep producing a
  bead-backed snapshot for a/b), session_beads.go:40 (loadSessionBeads, needed by class-c raw sync callers),
  doctor_session_model.go:149 (mixed session+work raw union, IncludeClosed:true).
- InfoFromPersistedBead: session_bead_snapshot.go (3) = the call inside newSessionBeadSnapshot (can't delete)
  + 2 comments; session_hash.go (1) = sessionCoreConfigForHash inside the class-c raw openBeads loop (feeding
  it Info needs an InfoFromPersistedBead in session_beads.go = a census INCREASE).

## Correct sequencing (the R6 agent's Option 1, recommended)
1. TICK-FEED refactor (session.Store.ListAllForReconcile() []Info + reshape the reconciler tick to hold Info
   from the edge) — frees class (a) + (c) at the reconciler side; drives InfoFromPersistedBead :1342/:1419 → 0.
   NUANCE (design §5c): orderedBeads []beads.Bead is used for BOTH session AND work-class scans + in-place
   Phase-0 heal mutation. Reshape so SESSION beads come as Info from the edge; WORK-class beads stay raw
   (ClassWork). The in-place heal must be re-expressed as a fold on the Info snapshot (ApplyPatchInfo).
2. POOL-path typing — type the pool selection/creation/reuse path (class b) + add(info). This is a real
   refactor (beads are created/reused, not just read).
3. RAW-HALF deletion — now mechanical; delete Open()/FindByID/newSessionBeadSnapshot(beads)/open-slice; zero
   ListAllSessionBeads (3) + session_bead_snapshot InfoFromPersistedBead (3→0) + session_hash (1→0).
4. FRONT-DOOR flip (design §5b: class_store.go + api.State → domain stores).
5. UNEXPORT the codecs + guard→permanent-zero-pins (design §5e).

The pure-read accessor migrations (city_runtime.go:2499 FindByID→FindInfoByID; providers.go:539
FindSessionNameByNamedIdentity→FindInfoByNamedIdentity; providers.go:232 / city_runtime.go:2969 /
cmd_citystatus.go:393/449 newSessionBeadSnapshot→FromInfos) are achievable anytime but move NO census needle
and would delete the raw/Info equivalence PIN tests that guard the still-live raw half — so do them WITH the
raw-half deletion (step 3), not before.
