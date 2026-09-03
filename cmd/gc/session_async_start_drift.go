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
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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

// pendingCreateRuntimeClearedForRollback stops the runtime a pending create
// spawned and reports whether it is CONFIRMED gone, so the caller may release
// the session's identifiers.
//
// Every other rollback site in the reconciler gates on liveness and fails CLOSED
// when the observation is unavailable; the pending-create rollback frees an
// alias, so it must do the same. stopStaleAsyncStartRuntime is not sufficient on
// its own: it returns silently when runningSessionMatchesPendingCreateInfo
// cannot confirm identity (both GetMeta probes erroring on a transient provider
// blip), and it swallows a non-IsSessionGone Stop error. Releasing the alias in
// either case strands a live agent holding the chair name with no bead owning
// it, after which every replacement start fails with ErrSessionExists.
func pendingCreateRuntimeClearedForRollback(info sessionpkg.Info, name string, sp runtime.Provider, stderr io.Writer) bool {
	if sp == nil {
		// No provider to probe against: this start never reached a runtime this
		// process can see, so there is nothing to strand.
		return true
	}
	if strings.TrimSpace(name) == "" {
		return false
	}
	if !runningSessionMatchesPendingCreateInfo(info, name, sp) {
		// Either no runtime of ours is there, or the identity probe could not
		// confirm one. Only the first is safe to act on, so defer to the
		// provider's own existence check and fail closed while it still sees a
		// session under this name.
		return !sp.IsRunning(name)
	}
	if err := sp.Stop(name); err != nil && !runtime.IsSessionGone(err) {
		fmt.Fprintf(stderr, "session reconciler: stopping runtime %s before releasing its identifiers: %v\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	// Confirm with the provider's existence check, not the identity probe: a
	// stopped session's metadata often outlives it (the tmux pane environment,
	// and runtime.Fake, both still answer GetMeta after Stop), so re-running the
	// identity probe here would report every successful stop as a survivor.
	if sp.IsRunning(name) {
		fmt.Fprintf(stderr, "session reconciler: runtime %s survived its stop; keeping its bead so the alias is not stranded\n", name) //nolint:errcheck // best-effort diagnostics
		return false
	}
	return true
}

// rollbackPendingCreateConfirmed performs the pending-create rollback and
// reports whether the row actually left the pending-create state.
//
// rollbackPendingCreate returns nil for two opposite outcomes: an already-closed
// bead (an idempotent no-op — the row is already gone, which is success) and a
// FAILED transaction (the row still holds its claim and its alias, which is
// not). Callers that report an outcome or release a lease on the strength of
// that nil would report a rollback that never happened, so the result is
// confirmed by re-reading rather than inferred.
func rollbackPendingCreateConfirmed(info sessionpkg.Info, sessFront *sessionpkg.Store, now time.Time, stderr io.Writer) bool {
	if sessFront == nil || strings.TrimSpace(info.ID) == "" {
		return false
	}
	if batch := rollbackPendingCreate(info, sessFront, now, stderr); batch != nil {
		return true
	}
	current, _, err := sessFront.GetPersistedResponse(info.ID)
	if err != nil {
		return false
	}
	return current.Closed || !current.PendingCreateClaim
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
	if _, tracked := t.counts[sessionID]; !tracked {
		t.evictLocked()
	}
	t.counts[sessionID]++
	return t.counts[sessionID] == asyncStartFailureEscalationThreshold
}

// evictLocked keeps the map bounded before a new session is admitted. The
// counter is a diagnostic, not a ledger: a controller runs for weeks over
// churning pool session IDs, and beads are closed by lanes that never reach
// this file (the stale-creating reaper, gc session close, the orphan sweep), so
// entries would otherwise accumulate for IDs that no longer exist. Which entry
// is dropped does not matter — only that the map cannot grow without bound.
func (t *asyncStartFailureTracker) evictLocked() {
	for len(t.counts) >= asyncStartFailureTrackerMaxEntries {
		for id := range t.counts {
			delete(t.counts, id)
			break
		}
	}
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

// asyncStartFailureTrackerMaxEntries bounds how many sessions the
// controller-wide counter tracks at once. See evictLocked.
const asyncStartFailureTrackerMaxEntries = 1024

// forget drops sessionID's entry because its bead is gone. Distinct from clear
// only in intent: clear ends a run that a commit resolved, forget releases the
// slot of a session that no longer exists.
func (t *asyncStartFailureTracker) forget(sessionID string) { t.clear(sessionID) }

// size reports how many sessions currently have a failure run.
func (t *asyncStartFailureTracker) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.counts)
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
// It reports whether a rollback was performed. Reporting false means "reset
// should proceed with its ordinary in-place restart", and is returned for a
// session that is not a pending create, one whose create is still legitimately
// in flight, and one whose runtime is still alive. An eligible session whose
// rollback did NOT land returns an error rather than false: reporting success
// while the row still holds its claim and its alias is the silent no-op this
// path exists to remove.
func rescuePendingCreateForReset(store beads.Store, sp runtime.Provider, startupTimeout time.Duration, sessionID string, clk clock.Clock, stderr io.Writer) (bool, error) {
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
	// Operators reach for reset exactly when a create looks stuck, which is also
	// when a perfectly healthy spawn is mid-flight. Mirror the sibling CLI gate
	// (sessionWakeCreateAbandonedInfo): the pending-create lease is checked
	// FIRST, staleness second — checking staleness alone rejects a create the
	// reconciler still protects.
	// pendingCreateLeaseActiveInfo, not its nil-clock sweep wrapper: the wrapper
	// reports "still leased" for ANY row carrying last_woke_at, because
	// pendingCreateAttemptStaleInfo returns false on a nil clock.
	if pendingCreateLeaseActiveInfo(info, clk, startupTimeout) || !isStaleCreatingInfo(info) {
		return false, nil
	}
	// A live runtime means this is not an abandoned create at all — the
	// documented post-start ApplyPatch failure leaves a serving agent with its
	// row parked in creating for warm reuse. Closing that bead would orphan the
	// runtime under its chair name, so leave it to the ordinary in-place reset.
	if !pendingCreateRuntimeClearedForRollback(info, info.SessionNameMetadata, sp, stderr) {
		return false, nil
	}
	if !rollbackPendingCreateConfirmed(info, sessFront, clk.Now().UTC(), stderr) {
		return false, fmt.Errorf("rolling back the pending create for %s did not land; it still holds its claim and alias", sessionID)
	}
	asyncStartFailures.forget(sessionID)
	return true, nil
}

// sessionResetRescueBudget returns the configured session startup budget the
// reset rescue leases against.
func sessionResetRescueBudget(cfg *config.City) time.Duration {
	if cfg == nil {
		return (&config.SessionConfig{}).StartupTimeoutDuration()
	}
	return cfg.Session.StartupTimeoutDuration()
}

// pendingCreateStartedAtForWake returns the pending-create start marker a wake
// should carry forward, or empty when this wake opens a NEW episode.
//
// The marker is preserved across the retries of one episode so the stale-create
// bound measures the episode rather than the latest attempt (ga-6wkhl). It must
// not be inherited across episodes: a row that left creating — healed to asleep,
// slept, or parked — starts a fresh episode on its next wake, and inheriting an
// aged marker there would make the freshly started row read instantly stale and
// be flapped back to asleep mid-start.
func pendingCreateStartedAtForWake(info sessionpkg.Info) string {
	if !pendingCreateQueuedOrCreatingState(info.MetadataState) {
		return ""
	}
	return info.PendingCreateStartedAt
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
//
// The command fingerprints are carried here too, and they are what make the
// event actionable: this arm covers a store read failure AND command drift on a
// row the rollback declined, and both report the same outcome string. Without
// the fingerprints a consumer cannot tell "the same drift is repeating" from
// "the store is flaky" — the exact disambiguation they were introduced for.
func emitAsyncStartRefreshStalled(rec events.Recorder, name, sessionID, template, outcome string, consecutive int, preparedCommand, currentCommand string, stderr io.Writer) {
	if rec == nil {
		rec = events.Discard
	}
	rec.Record(events.Event{
		Type:    events.SessionAsyncStartRefreshStalled,
		Actor:   "controller",
		Subject: name,
		Message: fmt.Sprintf("session %q async start failed its pre-commit refresh %d times in a row", name, consecutive),
		Payload: asyncStartRefreshPayloadJSON(sessionID, template, outcome, preparedCommand, currentCommand, consecutive),
	})
	fmt.Fprintf(stderr, "session reconciler: async start for %s has failed its pre-commit refresh %d times in a row (outcome=%s)\n", name, consecutive, outcome) //nolint:errcheck // best-effort diagnostics
}
