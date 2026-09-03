package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// asyncStartRefreshVerdict is the disposition refreshAsyncStartResult hands the
// commit path. Before ga-6wkhl this was four positional bools; the drift arm
// needed a fifth (rollback) and the escalation needed the drift detail, so the
// dispositions are named instead of counted.
type asyncStartRefreshVerdict struct {
	// commit reports that the refreshed result may proceed to commit. When it
	// is false exactly one of the disposition fields below applies.
	commit bool
	// cleanupRuntime stops the runtime this start just spawned.
	cleanupRuntime bool
	// releaseInFlight clears last_woke_at so the next tick retries the start.
	releaseInFlight bool
	// rollbackPendingCreate closes the pending-create bead, releasing its claim,
	// its session_name and its alias. Only ever set for a create that has not
	// committed — see asyncStartDriftRollbackEligibleInfo.
	rollbackPendingCreate bool
	// current is the fresh front-door read the gates decided against. The
	// rollback writes against this, not the enqueue-time twin.
	current sessionpkg.Info
	// preparedCommand and currentCommand are the two sides of the drift compare,
	// carried for the typed event. They never reach the wire verbatim.
	preparedCommand string
	currentCommand  string
}

// asyncStartDriftRollbackEligibleInfo reports whether discarding a
// command-drifted async start would strand the session forever.
//
// A session whose create has NOT committed carries the stale command as its
// only copy: nothing on the discard path rewrites the persisted "command" of a
// pending create, and only a completed create would. So the drift compare
// re-derives the identical verdict on every subsequent tick while the row keeps
// its pending-create claim and its alias — the row cannot converge and no
// replacement may claim the alias (ga-6wkhl: 111 retries over 2.5 h).
//
// A session whose create HAS committed is deliberately excluded. For a live row
// "desired command changed" correctly means "do not commit this stale start",
// and the config-drift lane owns drain-and-restart; rolling back there would
// close a live session's bead and release a live session's alias.
func asyncStartDriftRollbackEligibleInfo(i sessionpkg.Info) bool {
	if !i.PendingCreateClaim {
		return false
	}
	switch sessionpkg.State(strings.TrimSpace(string(i.State))) {
	case sessionpkg.StateCreating, sessionpkg.StateStartPending:
		return true
	default:
		return false
	}
}

// commandFingerprint returns a short SHA-256 prefix of a resolved session
// command. A resolved command is an argv that routinely carries provider
// credentials, and the drift events land in events.jsonl, the SSE stream and
// the dashboard — so the wire gets a fingerprint, never the command. The prefix
// is stable across ticks, which is what makes "the same drift is repeating"
// distinguishable from "the command changed again".
func commandFingerprint(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])[:12]
}

// asyncStartRefreshPayloadJSON builds the typed wire form for the async-start
// refresh events.
func asyncStartRefreshPayloadJSON(sessionID, template, outcome, preparedCommand, currentCommand string, consecutive int) json.RawMessage {
	b, _ := json.Marshal(api.SessionAsyncStartRefreshPayload{
		SessionID:                  sessionID,
		Template:                   template,
		Outcome:                    outcome,
		PreparedCommandFingerprint: commandFingerprint(preparedCommand),
		CurrentCommandFingerprint:  commandFingerprint(currentCommand),
		ConsecutiveFailures:        consecutive,
	})
	return b
}

// asyncStartFailureEscalationThreshold is the consecutive async-start refresh
// failure count that escalates to an event plus a loud stderr line. Small on
// purpose: the point is to surface a repeating failure early, and the specimen
// repeated 111 times in silence.
const asyncStartFailureEscalationThreshold = 3

// asyncStartFailureTracker counts consecutive async-start refresh failures per
// session so a run of identical failures escalates once instead of scrolling
// past as stderr. It is consecutive-only: any commit clears the run.
type asyncStartFailureTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func newAsyncStartFailureTracker() *asyncStartFailureTracker {
	return &asyncStartFailureTracker{counts: make(map[string]int)}
}

// record counts one failure for sessionID and reports whether this failure hits
// the escalation threshold. It reports true only on the transition, so a
// session that keeps failing escalates once per run, not once per tick.
func (t *asyncStartFailureTracker) record(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[sessionID]++
	return t.counts[sessionID] == asyncStartFailureEscalationThreshold
}

// count returns the current consecutive-failure count for sessionID.
func (t *asyncStartFailureTracker) count(sessionID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[strings.TrimSpace(sessionID)]
}

// clear ends the current failure run for sessionID.
func (t *asyncStartFailureTracker) clear(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, sessionID)
}

// asyncStartFailures is the controller-wide consecutive-failure counter. The
// reconciler is a long-lived process and the run is per-session, so the state
// lives with the process rather than on the bead.
var asyncStartFailures = newAsyncStartFailureTracker()

// rescuePendingCreateForReset rolls back a session whose create never
// committed, so `gc session reset` can free a row stuck in creating.
//
// Reset used to be orthogonal to this state: it reset the circuit breaker and
// the provider conversation state but never cleared pending_create_claim /
// pending_create_started_at and never released the alias, so the next tick
// re-entered the identical drift compare and the operator's first instinct
// silently did nothing (ga-6wkhl). Routing through the same rollback the drift
// gate uses makes reset able to rescue the state.
//
// It reports whether a rollback was performed. A session that is not a pending
// create is left completely alone: reset keeps its restart-in-place semantics.
func rescuePendingCreateForReset(store beads.Store, sessionID string, now time.Time, stderr io.Writer) (bool, error) {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return false, nil
	}
	sessFront := sessionFrontDoor(store)
	info, _, err := sessFront.GetPersistedResponse(sessionID)
	if err != nil {
		return false, fmt.Errorf("loading session %s: %w", sessionID, err)
	}
	if !asyncStartDriftRollbackEligibleInfo(info) {
		return false, nil
	}
	if rollbackPendingCreate(info, sessFront, now, stderr) == nil {
		return false, nil
	}
	return true, nil
}

// emitAsyncStartDriftRollback records the typed drift-rollback event. The
// rolled-back row is closed and recreated, so this fires once per transition —
// a stuck session is one event, not one per retry.
func emitAsyncStartDriftRollback(rec events.Recorder, name string, verdict asyncStartRefreshVerdict, template, outcome string) {
	if rec == nil {
		rec = events.Discard
	}
	rec.Record(events.Event{
		Type:    events.SessionAsyncStartDriftRolledBack,
		Actor:   "controller",
		Subject: name,
		Message: fmt.Sprintf("session %q rolled back: desired command changed before its create committed", name),
		Payload: asyncStartRefreshPayloadJSON(verdict.current.ID, template, outcome, verdict.preparedCommand, verdict.currentCommand, 0),
	})
}

// emitAsyncStartRefreshStalled records the escalation event for a session whose
// async start keeps failing its pre-commit refresh without reaching the
// rollback arm.
func emitAsyncStartRefreshStalled(rec events.Recorder, name, sessionID, template, outcome string, consecutive int, stderr io.Writer) {
	if rec == nil {
		rec = events.Discard
	}
	rec.Record(events.Event{
		Type:    events.SessionAsyncStartRefreshStalled,
		Actor:   "controller",
		Subject: name,
		Message: fmt.Sprintf("session %q async start failed its pre-commit refresh %d times in a row", name, consecutive),
		Payload: asyncStartRefreshPayloadJSON(sessionID, template, outcome, "", "", consecutive),
	})
	fmt.Fprintf(stderr, "session reconciler: async start for %s has failed its pre-commit refresh %d times in a row (outcome=%s)\n", name, consecutive, outcome) //nolint:errcheck // best-effort diagnostics
}
