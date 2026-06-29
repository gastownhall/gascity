package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// recordCurrentBeadIDOnWake persists the work bead a session is being woken
// for. The reconciler writes this whenever a session is brought up (asleep
// → awake or alive cycle) so that subsequent reconciler ticks can detect
// when the assignee has been pointed at a different bead. The metadata
// survives session restart, so crash recovery can resume the same bead
// instead of jumping to a sibling assignment.
func recordCurrentBeadIDOnWake(session *beads.Bead, sessFront *sessionpkg.InfoStore, beadID string, stderr io.Writer) {
	if session == nil || sessFront == nil {
		return
	}
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return
	}
	if session.Metadata[sessionpkg.CurrentBeadIDKey] == beadID {
		return
	}
	if err := sessFront.RecordCurrentBead(session.ID, beadID); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: recording %s for %s: %v\n", sessionpkg.CurrentBeadIDKey, session.Metadata["session_name"], err) //nolint:errcheck
		}
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, 1)
	}
	session.Metadata[sessionpkg.CurrentBeadIDKey] = beadID
}

// cycleAliveSessionForFreshReassign tears down a live wake_mode=fresh
// session whose assigned bead has changed, then primes the bead so the
// next reconciler tick wakes the session on a brand-new conversation.
// Returns true when the cycle ran; the caller must `continue` so it does
// not double-process the drain/idle bookkeeping for a session it just
// killed.
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
	session *beads.Bead,
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
) bool {
	if session == nil || store == nil {
		return false
	}
	newBeadID = strings.TrimSpace(newBeadID)
	if newBeadID == "" {
		return false
	}
	prevBeadID := strings.TrimSpace(session.Metadata[sessionpkg.CurrentBeadIDKey])
	if err := workerKillSessionTargetWithConfig("", store, sp, cfg, name); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: stopping fresh-cycle %s: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	if identity := namedSessionIdentity(*session); identity != "" {
		if err := resetSessionCircuitBreakerState(store, session.ID, identity, cb); err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "session reconciler: clearing session circuit breaker for fresh-cycle %s: %v\n", name, err) //nolint:errcheck
			}
			return false
		}
	}
	newSessionKey, hasCapability := freshRestartSessionKey(tp, session.Metadata)
	batch := sessionpkg.RestartRequestPatch(newSessionKey, now)
	if hasCapability && newSessionKey == "" {
		batch["session_key"] = ""
	}
	batch[sessionpkg.CurrentBeadIDKey] = newBeadID
	if err := sessionFrontDoor(store).ApplyPatch(session.ID, batch); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: recording fresh-cycle handoff for %s: %v\n", name, err) //nolint:errcheck
		}
		return false
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for key, value := range batch {
		// The durable reset commit marker is for the next reconciler
		// pass; keeping it out of this tick's in-memory bead mirrors the
		// restart-requested handoff above so on-demand sessions are not
		// force-woken without demand within the same tick.
		if key == sessionpkg.ResetCommittedAtKey {
			continue
		}
		session.Metadata[key] = value
	}
	if stdout != nil {
		fmt.Fprintf(stdout, "Cycled fresh-mode session '%s' for bead reassign: %s → %s\n", name, prevBeadID, newBeadID) //nolint:errcheck
	}
	if trace != nil {
		trace.recordDecision("reconciler.session.bead_reassign_cycle", tp.TemplateName, name, "fresh_cycle", "restart", traceRecordPayload{
			"previous_bead_id": prevBeadID,
			"new_bead_id":      newBeadID,
		}, nil, "")
	}
	return true
}
