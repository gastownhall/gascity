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
// whether it is still open (non-terminal, via convoycore.IsTerminalStatus)
// and, when terminal, the time it closed. Resolving over the topology
// (byIDBeadForTopology) rather than one store is what lets this answer for a
// rig-scoped id: every ga-* work bead lives in a rig store, and a lookup
// against the city/session store alone always misses it. One store round
// trip answers both questions, so the fresh-cycle guard in
// session_reconciler.go does not need a second lookup to get the close time
// after checking status.
//
// closedAt prefers the metadata "closed_at" timestamp when present and
// RFC3339Nano-parseable, but falls back to the bead's UpdatedAt: no real `bd
// close` ever writes a closed_at metadata key (bd keeps closed_at as a
// top-level column that neither bdIssue nor beads.Bead decodes), so without
// this fallback closedAt is always zero for a bead closed the way every real
// close closes it. closedAt is the zero Time only when the bead is open.
func prevAssignedBeadStatus(topo storeref.Topology, id string) (open bool, closedAt time.Time, err error) {
	b, err := byIDBeadForTopology(topo, id)
	if err != nil {
		return false, time.Time{}, err
	}
	if !convoycore.IsTerminalStatus(b.Status) {
		return true, time.Time{}, nil
	}
	closedAt = b.UpdatedAt
	if ca := b.Metadata["closed_at"]; ca != "" {
		if t, perr := time.Parse(time.RFC3339Nano, ca); perr == nil {
			closedAt = t
		}
	}
	return false, closedAt, nil
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
	if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
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
