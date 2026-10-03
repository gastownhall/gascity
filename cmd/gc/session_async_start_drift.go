package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// asyncStartRefreshVerdict is what refreshAsyncStartResult hands the async
// commit path. When commit is false, the other fields say what to do instead.
type asyncStartRefreshVerdict struct {
	// commit reports that the refreshed result may proceed to commit.
	commit bool
	// cleanupRuntime stops the runtime this start spawned, subject to the
	// attribution probe in stopStaleAsyncStartRuntime.
	cleanupRuntime bool
	// releaseInFlight clears last_woke_at so the next tick retries the start.
	releaseInFlight bool
	// rollbackPendingCreate closes the pending-create bead as failed-create,
	// which releases its claim, its session_name and its alias. It is set only
	// for a command-drifted create that cannot converge by retrying; see
	// asyncStartDriftRollbackEligibleInfo.
	rollbackPendingCreate bool
	// current is the fresh front-door read the gates decided against.
	current sessionpkg.Info
}

// asyncStartDriftRollbackEligibleInfo reports whether a command-drifted async
// start must roll its pending create back instead of discarding the result and
// retrying.
//
// Discard-and-retry only converges when the drift happened during startup,
// that is, when the persisted command moved after this start was prepared.
// When the persisted command is the same one the start was prepared against,
// the mismatch was already there before the start and the next tick prepares
// the same start and finds the same mismatch. Nothing on the discard path
// rewrites the command of a create that never committed, so the row stays in
// creating, keeps its pending-create claim and keeps its alias on every tick,
// and no replacement can take the alias. A named session sat in that loop for
// 2.5 hours across 111 retries.
//
// Every condition below guards against rolling back a create that could still
// succeed or that belongs to someone else:
//   - the row still holds pending_create_claim and is still creating or
//     start-pending, so its create never committed (a committed row keeps the
//     discard, and the config-drift lane owns drain-and-restart for it);
//   - the row is still the incarnation this attempt started (the identity
//     fence), so a late attempt can never close the bead of a newer one;
//   - the persisted command has not moved since the start was prepared, so a
//     retry would not converge.
func asyncStartDriftRollbackEligibleInfo(prepared, current sessionpkg.Info) bool {
	if current.Closed || !current.PendingCreateClaim {
		return false
	}
	if !pendingCreateQueuedOrCreatingState(current.MetadataState) {
		return false
	}
	if !asyncStartIdentityMatchesInfo(prepared, current) {
		return false
	}
	return strings.TrimSpace(prepared.Command) == strings.TrimSpace(current.Command)
}

// pendingCreateRuntimeClearedForRollback stops the runtime a drifted pending
// create spawned and reports whether it is confirmed gone, so the caller may
// release the session's identifiers.
//
// The rollback frees the alias. Freeing it while a process still holds the
// runtime name would leave a live agent that no bead owns, and every
// replacement start would fail on the name. So this fails closed:
// stopStaleAsyncStartRuntime alone is not enough, because it returns silently
// when it cannot attribute the runtime and it swallows Stop errors.
func pendingCreateRuntimeClearedForRollback(result startResult, sp runtime.Provider, stderr io.Writer) bool {
	if sp == nil {
		// No provider, so this start never reached a runtime this process can
		// see. There is nothing to strand.
		return true
	}
	info := result.prepared.candidate.info
	name := strings.TrimSpace(result.prepared.candidate.name())
	if name == "" {
		return false
	}
	if staleAsyncStartRuntimeAttribution(info, name, sp) != pendingCreateRuntimeOurs {
		// Nothing of ours is there, or the identity probe could not tell. Only
		// the first is safe to act on, and this attempt may not stop a runtime
		// it cannot attribute, so require positive absence.
		return pendingCreateRuntimeAbsenceConfirmed(name, sp, stderr)
	}
	if err := sp.Stop(name); err != nil && !runtime.IsSessionGone(err) {
		fmt.Fprintf(stderr, "session reconciler: stopping runtime %s before releasing its identifiers: %v\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	obs, err := runtime.ObserveLivenessWithError(sp, name, nil)
	if err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		fmt.Fprintf(stderr, "session reconciler: cannot confirm runtime %s stopped (%v); keeping its bead so the alias is not stranded\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	if err == nil && obs.Running {
		fmt.Fprintf(stderr, "session reconciler: runtime %s survived its stop; keeping its bead so the alias is not stranded\n", name) //nolint:errcheck // best-effort diagnostics
		return false
	}
	// A provider without the error-bearing liveness capability answers the
	// probe above with a bare IsRunning, which cannot tell "gone" from "could
	// not look". ListRunning reports an observation failure as an error, so
	// consult its error too. Only the error: a stopped runtime may stay listed
	// for a while (a terminating k8s pod keeps phase Running), and this branch
	// already holds an attributed runtime and a clean Stop.
	//
	// A server-absent error is the one error accepted here. Stopping the only
	// session on a tmux server takes the server down with it, so this stop
	// produces that answer itself, and a session cannot outlive its server.
	if _, err := sp.ListRunning(name); err != nil && !runtime.IsRuntimeServerAbsent(err) {
		fmt.Fprintf(stderr, "session reconciler: cannot confirm runtime %s stopped (%v); keeping its bead so the alias is not stranded\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	return true
}

// pendingCreateRuntimeAbsenceConfirmed reports whether the provider positively
// confirms that nothing is running under name, without stopping anything. It
// is fail-closed: "could not observe" answers false, the same as "running".
//
// It asks both probes the provider offers. The liveness observation is
// three-valued (running, not running, observation error) on providers that
// implement runtime.LivenessObserverWithError, and falls back to a bare
// IsRunning elsewhere. ListRunning carries an error on every provider, and a
// listed name is positive evidence of presence. Any error from it refuses,
// including server-absent: this path has neither attributed nor stopped the
// runtime, so it has no independent proof to weigh that answer against.
func pendingCreateRuntimeAbsenceConfirmed(name string, sp runtime.Provider, stderr io.Writer) bool {
	if sp == nil {
		return true
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	obs, err := runtime.ObserveLivenessWithError(sp, name, nil)
	if err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		fmt.Fprintf(stderr, "session reconciler: cannot confirm runtime %s is gone (%v); keeping its bead so its alias is not stranded\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	if err == nil && obs.Running {
		return false
	}
	running, err := sp.ListRunning(name)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: cannot confirm runtime %s is gone (%v); keeping its bead so its alias is not stranded\n", name, err) //nolint:errcheck // best-effort diagnostics
		return false
	}
	for _, listed := range running {
		if strings.TrimSpace(listed) == name {
			return false
		}
	}
	return true
}

// pendingCreateRollbackOutcome is the confirmed result of a pending-create
// rollback attempt.
type pendingCreateRollbackOutcome int

const (
	// pendingCreateRolledBack means this call closed the bead as failed-create.
	pendingCreateRolledBack pendingCreateRollbackOutcome = iota
	// pendingCreateRollbackSuperseded means the row moved on before the
	// rollback could land: it closed, its claim cleared, or a different
	// incarnation owns it now. The caller must not write to it.
	pendingCreateRollbackSuperseded
	// pendingCreateRollbackFailed means the row is still this incarnation's
	// pending create and the rollback did not land. It still holds its claim
	// and its alias.
	pendingCreateRollbackFailed
)

// rollbackPendingCreateConfirmed rolls back the pending create observed as
// current and reports what actually happened.
//
// rollbackPendingCreate returns nil for opposite results: its fence refused
// because the row moved on, or its transaction failed and the row still holds
// its claim and alias. Callers that release a lease or report success on the
// strength of that nil would report a rollback that never happened, so a nil
// is resolved by re-reading the row.
func rollbackPendingCreateConfirmed(expected, current sessionpkg.Info, sessFront *sessionpkg.Store, now time.Time, stderr io.Writer) pendingCreateRollbackOutcome {
	if sessFront == nil || strings.TrimSpace(current.ID) == "" {
		return pendingCreateRollbackFailed
	}
	if batch := rollbackPendingCreate(current, sessFront, now, stderr); batch != nil {
		return pendingCreateRolledBack
	}
	reread, _, err := sessFront.GetPersistedResponse(current.ID)
	if err != nil {
		return pendingCreateRollbackFailed
	}
	if reread.Closed || !asyncStartIdentityMatchesInfo(expected, reread) {
		return pendingCreateRollbackSuperseded
	}
	// A store whose Tx is not atomic can persist the failed-create metadata,
	// which clears the claim, and then fail the close. That row is still this
	// incarnation's unfinished rollback, not a row that moved on.
	if !reread.PendingCreateClaim && sessionpkg.State(strings.TrimSpace(reread.MetadataState)) != sessionpkg.StateFailedCreate {
		return pendingCreateRollbackSuperseded
	}
	return pendingCreateRollbackFailed
}

// rescuePendingCreateForReset rolls back a session whose create never
// committed and is no longer in flight, so `gc session reset` can free a row
// stuck in creating.
//
// An in-place restart cannot rescue that row. handle.Reset refreshes the
// provider conversation state but leaves pending_create_claim,
// pending_create_started_at and the alias in place, so the controller
// re-enters the same failing start on the next tick. Reset is what operators
// reach for first, and it silently did nothing.
//
// It reports whether it rolled the row back. False means "proceed with the
// ordinary in-place restart" and covers every row this must not destroy: one
// that is not a pending create, one whose create is still within its start
// lease (three seconds into a spawn is exactly when an operator reaches for
// reset), one whose runtime is alive or cannot be observed (a live agent must
// not lose its bead), and one that moved on while this ran. An eligible row
// whose rollback did not land is an error, not false: reporting success while
// the row still holds its claim and alias is the silent no-op this exists to
// remove.
func rescuePendingCreateForReset(store beads.Store, sp runtime.Provider, startupTimeout time.Duration, sessionID string, clk clock.Clock, stderr io.Writer) (bool, error) {
	if store == nil || clk == nil || strings.TrimSpace(sessionID) == "" {
		return false, nil
	}
	sessFront := sessionFrontDoor(store)
	info, _, err := sessFront.GetPersistedResponse(sessionID)
	if err != nil {
		return false, fmt.Errorf("loading session %s: %w", sessionID, err)
	}
	if info.Closed || !info.PendingCreateClaim || !pendingCreateQueuedOrCreatingState(info.MetadataState) {
		return false, nil
	}
	// Lease first, staleness second, the same order as the reconciler and
	// sessionWakeCreateAbandonedInfo. This passes a real clock on purpose: the
	// nil-clock sweep wrapper reports "still leased" for any row that carries
	// last_woke_at.
	if pendingCreateLeaseActiveInfo(info, clk, startupTimeout) || !pendingCreateAttemptStaleInfo(info, clk) {
		return false, nil
	}
	// Passive probe. Reset decides about a runtime somebody else started, so
	// it must not stop one to satisfy the gate: a live runtime means the
	// ordinary in-place restart applies.
	if !pendingCreateRuntimeAbsenceConfirmed(info.SessionNameMetadata, sp, stderr) {
		return false, nil
	}
	switch rollbackPendingCreateConfirmed(info, info, sessFront, clk.Now().UTC(), stderr) {
	case pendingCreateRolledBack:
		return true, nil
	case pendingCreateRollbackSuperseded:
		return false, nil
	default:
		return false, fmt.Errorf("rolling back the unfinished create of %s did not land; it still holds its pending-create claim and alias", sessionID)
	}
}
