package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// recordCurrentBeadIDOnWake persists the work bead a session is being woken
// for. The reconciler writes this whenever a session is brought up (asleep
// → awake or alive cycle) so that subsequent reconciler ticks can detect
// when the assignee has been pointed at a different bead. The metadata
// survives session restart, so crash recovery can resume the same bead
// instead of jumping to a sibling assignment.
// recordCurrentBeadIDOnWake returns the metadata patch it applied (the
// currently_processing_bead_id write) so the reconciler can fold it onto the
// infoByID snapshot (write-returns-Info), or nil when it was a no-op. It reads
// the session id and the currently-processing bead off the caller's coherent
// typed Info (Info.ID / Info.CurrentlyProcessingBeadID, both verbatim raw
// mirrors); the fold the caller applies keeps the snapshot in step.
func recordCurrentBeadIDOnWake(info sessionpkg.Info, sessFront *sessionpkg.Store, beadID string, stderr io.Writer) sessionpkg.MetadataPatch {
	if strings.TrimSpace(info.ID) == "" || sessFront == nil {
		return nil
	}
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return nil
	}
	if info.CurrentlyProcessingBeadID == beadID {
		return nil
	}
	if err := sessFront.RecordCurrentBead(info.ID, beadID); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: recording %s for %s: %v\n", sessionpkg.CurrentBeadIDKey, info.SessionNameMetadata, err) //nolint:errcheck
		}
		return nil
	}
	return sessionpkg.MetadataPatch{sessionpkg.CurrentBeadIDKey: beadID}
}

// prevAssignedBeadStatus looks up a single bead by id over the caller's
// residency topology — rather than a single fixed store — and reports
// whether it is still open (non-terminal, via convoycore.IsTerminalStatus),
// who it is assigned to right now, and, when terminal, the time it closed.
// Resolving over the topology (byIDOwnerRowForTopology) rather than one store
// is what lets this answer for a rig-scoped id: every ga-* work bead lives in
// a rig store, and a lookup against the city/session store alone always
// misses it. One store read answers all three questions, so the fresh-cycle
// guard in session_reconciler.go does not need a second lookup to get the
// close time or the live assignee after checking status.
//
// The topology only names which store owns the row. The row is then read from
// that store with its cache layers peeled (uncachedBeadStore), as
// freshAnchorBeadTerminal does: a rig leg is a CachingStore whose Get answers
// a primed, undirtied row without a backing read, and a handoff or close made
// by another process (`bd update --assignee`, `bd close`) reaches the backing
// store alone. A cached row would report the old assignee and keep row A
// deferring on a bead this session no longer owns.
//
// assignee is the live, trimmed assignee. ga-pvjbx3 Q3: the guard defers on an
// open previous bead only while this session still owns it, and a bead that
// has since been handed to another agent (or unassigned) answers that with
// the assignee read here, from the same lookup that reported it open.
//
// closedAt prefers the metadata "closed_at" timestamp when present and
// RFC3339Nano-parseable, but falls back to the bead's UpdatedAt: no real `bd
// close` ever writes a closed_at metadata key (bd keeps closed_at as a
// top-level column that neither bdIssue nor beads.Bead decodes), so without
// this fallback closedAt is always zero for a bead closed the way every real
// close closes it. The UpdatedAt fallback is rounded up to the next whole
// second, since BdStore truncates it to seconds. closedAt is the zero Time
// only when the bead is open.
func prevAssignedBeadStatus(topo storeref.Topology, id string) (open bool, assignee string, closedAt time.Time, err error) {
	owner, err := byIDOwnerRowForTopology(topo, id)
	if err != nil {
		return false, "", time.Time{}, err
	}
	b, err := uncachedBeadStore(owner.Store).Get(id)
	if err != nil {
		return false, "", time.Time{}, err
	}
	assignee = strings.TrimSpace(b.Assignee)
	if !convoycore.IsTerminalStatus(b.Status) {
		return true, assignee, time.Time{}, nil
	}
	return false, assignee, beadClosedAt(b), nil
}

// beadClosedAt is a terminal bead's close time, as prevAssignedBeadStatus
// reads it: the metadata closed_at when parseable, else UpdatedAt rounded up
// to the next second (BdStore truncates it), so a wake earlier in the same
// second as the close never reads as "after". v2's SESS-612 defer reads it
// too.
func beadClosedAt(b beads.Bead) time.Time {
	var closedAt time.Time
	if !b.UpdatedAt.IsZero() {
		closedAt = b.UpdatedAt.Truncate(time.Second).Add(time.Second)
	}
	if ca := b.Metadata["closed_at"]; ca != "" {
		if t, perr := time.Parse(time.RFC3339Nano, ca); perr == nil {
			closedAt = t
		}
	}
	return closedAt
}

// sessionHasFreshInProgressClaim reports whether this session currently
// holds ANY assigned work bead — anywhere its topology can reach — with
// status == in_progress, on a read of the legs made now. ga-pvjbx3 Q1: the
// fresh-cycle guard's old self-claim check compared the anchor
// (ComputeAwakeSet's assignedAnchor, which discards every candidate but one
// — exact match to the stamped bead, else first-in-order fallback) against
// current_claim_bead_id, but the anchor and this session's real self-claim
// can be two different, unrelated beads. The correct check is anchor-agnostic
// existence, not anchor equality: does this session hold ANY live
// in_progress claim at all, regardless of which bead the anchor separately
// resolved to. It asks the progress-stall-recycle existence check, which
// sweeps the unrestricted store+rig topology and already excludes mail and
// session beads. It does not exclude a blocked bead, which
// workBeadHasAwakeDemand does, so this population is a superset of awake
// demand: an extra claim can only retain the session, never kill it.
//
// The question gates a kill, so it is asked of sw.fresh, never of the tick's
// own read: that read re-checks a cached hit live but never re-lists a miss,
// and drops a row it read as open before the live re-check, so a claim made
// (or an open bead claimed) after the tick read the legs is invisible to it.
//
// An error means the scan could not read a store, never "no claim" — the
// existence probe fails closed on a leg that went dark — and the caller must
// retain the session rather than read it as an absent claim.
func sessionHasFreshInProgressClaim(sw *SeatWork, info sessionpkg.Info) (bool, error) {
	return sessionHasInProgressAssignedWorkForConfig(sw.fresh(releaseScope(info, sw.Legs().cfg)), info)
}

// freshAnchorBeadTerminal fetches the fresh-cycle guard's anchor bead
// (ComputeAwakeSet's decision.AssignedWorkBeadID) with one fresh, uncached
// read over the caller's residency topology and reports whether it is
// already terminal (closed). ga-pvjbx3 Q2: the tick's assigned-work snapshot
// can already be several minutes stale by the time the kill decision runs;
// cycling a session onto an anchor that has since closed is pure loss — there
// is no work left to reassign onto. An error (including a bead no leg holds)
// is "could not tell", and the caller then proceeds to the cycle: Q1 has
// already established that the session holds no claim a kill could cost.
//
// The topology only names which store owns the anchor. The row is then read
// from that store with its cache layers peeled (uncachedBeadStore), and the
// row the owner probe may already have read is discarded: a rig leg is a
// CachingStore whose Get answers a primed, undirtied row without a backing
// read, and a close made by another process (`bd close`) reaches the backing
// store alone. Reading through the cache would report that closed anchor as
// live and cycle the session onto work that no longer exists.
func freshAnchorBeadTerminal(topo storeref.Topology, anchorID string) (bool, error) {
	owner, err := byIDOwnerRowForTopology(topo, anchorID)
	if err != nil {
		return false, err
	}
	anchor, err := uncachedBeadStore(owner.Store).Get(anchorID)
	if err != nil {
		return false, err
	}
	return convoycore.IsTerminalStatus(anchor.Status), nil
}

// cycleAliveSessionForFreshReassign tears down a live wake_mode=fresh
// session whose assigned bead has changed, then primes the bead so the
// next reconciler tick wakes the session on a brand-new conversation.
// Returns (true, fold) when the cycle ran; the caller must `continue` so it
// does not double-process the drain/idle bookkeeping for a session it just
// killed. The fold is the in-memory mirror it applied (RestartRequestPatch
// minus ResetCommittedAtKey), for the reconciler to fold onto the infoByID
// snapshot (write-returns-Info).
//
// The teardown path mirrors the agent-initiated restart handoff
// (`gc runtime request-restart`): kill the process, reset the named-session
// circuit breaker (a cycle is deliberate, not a crash — accumulated breaker
// state must not block the post-cycle wake), optionally rotate session_key
// for providers that accept --session-id, then apply RestartRequestPatch so
// the next wake observes firstStart=true and uses the fresh-wake
// conversation reset. We also update currently_processing_bead_id to the
// new anchor so the divergence check does not refire on the next tick.
func cycleAliveSessionForFreshReassign(
	cityPath string,
	info sessionpkg.Info,
	tp TemplateParams,
	sp runtime.Provider,
	store beads.Store,
	cfg *config.City,
	cb *sessionCircuitBreaker,
	name string,
	newBeadID string,
	now time.Time,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) (bool, sessionpkg.MetadataPatch) {
	if store == nil {
		return false, nil
	}
	newBeadID = strings.TrimSpace(newBeadID)
	if newBeadID == "" {
		return false, nil
	}
	prevBeadID := strings.TrimSpace(info.CurrentlyProcessingBeadID)
	if err := controllerKillSessionRow(cityPath, store, sp, cfg, info); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: stopping fresh-cycle %s: %v\n", name, err) //nolint:errcheck
		}
		return false, nil
	}
	if identity := namedSessionIdentityInfo(info); identity != "" {
		if err := resetSessionCircuitBreakerState(store, info.ID, identity, cb); err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "session reconciler: clearing session circuit breaker for fresh-cycle %s: %v\n", name, err) //nolint:errcheck
			}
			return false, nil
		}
	}
	newSessionKey, hasCapability := freshRestartSessionKeyInfo(tp, info)
	batch := sessionpkg.RestartRequestPatch(newSessionKey, now)
	if hasCapability && newSessionKey == "" {
		batch["session_key"] = ""
	}
	// The kill above already succeeded (the workerKillSessionTargetWithConfig
	// error path above returns early otherwise), so the runtime this bead's
	// metadata describes is now definitely gone.
	// Record that unconditionally in the same patch as the mint: a phantom
	// key nothing ever launches on is worse than a session that looks asleep
	// for one extra tick, and ComputeAwakeSet's reset-pending desire (gated
	// on continuation_reset_pending + reset_committed_at, both already set by
	// RestartRequestPatch above) wakes it again on the very next tick. See
	// ga-2fpf9z.
	batch["state"] = string(sessionpkg.StateAsleep)
	batch[sessionpkg.CurrentBeadIDKey] = newBeadID
	if err := sessionFrontDoor(store).ApplyPatch(info.ID, batch); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: recording fresh-cycle handoff for %s: %v\n", name, err) //nolint:errcheck
		}
		return false, nil
	}
	// The returned fold carries every batch key EXCEPT the durable reset commit
	// marker: keeping ResetCommittedAtKey out of this tick's snapshot mirrors the
	// restart-requested handoff so on-demand sessions are not force-woken without
	// demand within the same tick. The former raw session.Metadata mirror loop is
	// deleted — it wrote the identical key set as this fold, and the caller applies
	// the fold to infoByID before `continue`ing (no later raw read this tick).
	fold := make(sessionpkg.MetadataPatch, len(batch))
	for key, value := range batch {
		if key == sessionpkg.ResetCommittedAtKey {
			continue
		}
		fold[key] = value
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "Cycled fresh-mode session '%s' for bead reassign: %s → %s\n", name, prevBeadID, newBeadID) //nolint:errcheck
	}
	if trace != nil {
		trace.RecordDecision(TraceSiteReconcilerBeadReassignCycle, TraceReasonFreshCycle, TraceOutcomeRestart, tp.TemplateName, name, traceRecordPayload{
			"previous_bead_id": prevBeadID,
			"new_bead_id":      newBeadID,
		})
	}
	return true, fold
}
