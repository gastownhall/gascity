package session

import (
	"strconv"
	"time"
)

// Recovery for sessions left alive but inert after a provider transport failure
// (ga-qox / incident ci-emg). A live-turn failure — a DNS/WebSocket/HTTPS/stream
// drop mid-turn — can abort the turn and return the interactive CLI to its idle
// prompt with the process still alive. The reconciler then sees the session
// awake and does nothing, so durable hooked/in-progress work stalls until a
// human nudges.
//
// This is a pure decision ladder over caller-gathered facts, mirroring
// DecideSessionExit: the caller owns the provider peek, the transport-failure
// classification, the liveness/eligibility gathering, and the marker writes;
// this package owns the observe→nudge→backoff→give-up state machine and its
// metadata patch shape. The recovery action is a continuation NUDGE (not a restart), which
// forensics proved is the minimal recovery — and, being capped, it cannot form
// a restart storm. It is provider-neutral and role-neutral: no provider name or
// role name appears in the ladder.

// Session-bead metadata keys for the inert-recovery state machine. The state is
// persisted on the session's own bead so it survives a controller restart — an
// in-memory grace map would replay on every restart and re-nudge-storm.
const (
	// InertRecoveryFingerprintKey holds the coarse reason token of the failure
	// episode currently being recovered.
	InertRecoveryFingerprintKey = "inert_recovery_fingerprint"
	// InertRecoveryAttemptsKey holds the count of continuation-nudge attempts
	// reserved for that episode. The caller persists an attempt before delivery,
	// so provider errors and controller restarts cannot replay it unboundedly.
	InertRecoveryAttemptsKey = "inert_recovery_count"
	// InertRecoveryAtKey holds the RFC3339 time of the last observation or nudge.
	InertRecoveryAtKey = "inert_recovery_at"
)

// InertRecoveryOutcome is the classification of one session on one tick.
type InertRecoveryOutcome int

const (
	// RecoverNone means take no action and leave any marker untouched.
	RecoverNone InertRecoveryOutcome = iota
	// RecoverClear means wipe the persisted marker: the session recovered, lost
	// its work, or died, so the episode is over.
	RecoverClear
	// RecoverObserve means a new failure episode: start the grace clock (record
	// the fingerprint with zero attempts) without nudging yet, so a self-healing
	// CLI can recover on its own first.
	RecoverObserve
	// RecoverWait means hold: still inside the observe grace or the retry backoff.
	RecoverWait
	// RecoverNudge means deliver a continuation nudge and advance the attempt
	// count.
	RecoverNudge
	// RecoverExhausted means the attempt cap was reached for this episode; give
	// up quietly (manual re-nudge remains the escape hatch) so retries cannot
	// storm.
	RecoverExhausted
)

// InertRecoveryFacts are the inputs for classifying one session's recovery.
type InertRecoveryFacts struct {
	// Alive reports the session process is running.
	Alive bool
	// Eligible reports the session is one the orchestrator wants making progress
	// — a desired/live session (an always-on named role or human-turn
	// session, or a pool worker with routed work). Durable assigned work is a
	// sufficient signal but not a necessary one: a named control session with
	// no assignee bead is still eligible. The caller decides eligibility; this
	// package only gates recovery on it so a session the orchestrator is not
	// keeping alive is never nudged.
	Eligible bool
	// TransportFailure reports the caller classified the current pane as an inert
	// provider transport failure (ClassifyInertTransportFailure).
	TransportFailure bool
	// Fingerprint is the coarse reason token for the current failure ("" when no
	// failure is present).
	Fingerprint string
	// MarkedFingerprint is the persisted fingerprint from a prior tick ("" when
	// no episode is being tracked).
	MarkedFingerprint string
	// Attempts is the persisted continuation-nudge count for the marked episode.
	Attempts int
	// LastActionAt is the persisted time of the last observation or nudge.
	LastActionAt time.Time
	// Now is the tick clock reading.
	Now time.Time
	// Grace is the observe-before-first-nudge window.
	Grace time.Duration
	// Backoff is the wait between retries after a delivered nudge did not take.
	Backoff time.Duration
	// MaxAttempts caps the continuation nudges per episode.
	MaxAttempts int
}

// DecideInertRecovery classifies one session's inert-recovery state. It performs
// no I/O; the caller gathers the liveness, eligibility, and screen facts.
func DecideInertRecovery(f InertRecoveryFacts) InertRecoveryOutcome {
	// Recovery condition: an eligible (desired/live) session sitting on an inert
	// transport failure. Anything else ends the episode.
	if !f.Alive || !f.Eligible || !f.TransportFailure || f.Fingerprint == "" {
		if f.MarkedFingerprint != "" {
			return RecoverClear
		}
		return RecoverNone
	}
	// Attachment is deliberately NOT an exemption: a human attached to a frozen
	// dead turn cannot revive it (they are the ones waiting on it), so an
	// attached session is recovered on the same terminal-error + quiet/activity
	// gating as any other. Guarding against nudging a genuinely-busy session is
	// the caller's quiet/activity gate and the inert-transport-failure
	// classification, not attachment.
	//
	// First sighting of this episode, or a genuinely different failure class:
	// (re-)arm by starting the grace clock.
	if f.MarkedFingerprint != f.Fingerprint {
		return RecoverObserve
	}
	switch {
	case f.Attempts == 0:
		if f.Now.Sub(f.LastActionAt) < f.Grace {
			return RecoverWait
		}
		return RecoverNudge
	case f.Attempts >= f.MaxAttempts:
		return RecoverExhausted
	default:
		if f.Now.Sub(f.LastActionAt) < f.Backoff {
			return RecoverWait
		}
		return RecoverNudge
	}
}

// InertRecoveryObservePatch records the first sighting of a failure episode:
// the fingerprint with zero attempts and the observation time. It starts the
// grace clock without nudging.
func InertRecoveryObservePatch(fingerprint string, now time.Time) MetadataPatch {
	return inertRecoveryMarker(fingerprint, 0, now)
}

// InertRecoveryNudgePatch reserves a continuation-nudge attempt: the
// fingerprint, the new attempt count, and the attempt time. Callers persist it
// before delivery so the retry bound remains durable even when delivery fails.
func InertRecoveryNudgePatch(fingerprint string, attempts int, now time.Time) MetadataPatch {
	return inertRecoveryMarker(fingerprint, attempts, now)
}

func inertRecoveryMarker(fingerprint string, attempts int, now time.Time) MetadataPatch {
	return MetadataPatch{
		InertRecoveryFingerprintKey: fingerprint,
		InertRecoveryAttemptsKey:    strconv.Itoa(attempts),
		InertRecoveryAtKey:          now.UTC().Format(time.RFC3339),
	}
}

// InertRecoveryClearPatch wipes the marker so the next failure episode starts a
// fresh grace clock.
func InertRecoveryClearPatch() MetadataPatch {
	return MetadataPatch{
		InertRecoveryFingerprintKey: "",
		InertRecoveryAttemptsKey:    "",
		InertRecoveryAtKey:          "",
	}
}
