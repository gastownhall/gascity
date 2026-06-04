package main

import "time"

// sessionProgressStalled reports whether a desired, alive session has stopped
// making progress and should be recycled with a fresh restart. It is the
// progress-aware half of the liveness predicate (ADR-0013 Amendment A1, move
// 3b): a live process is necessary but not sufficient for "healthy" — a session
// can be alive yet parked (for example, its turn ended on a provider auth error)
// and will not self-recover, so the reconciler must restart it.
//
// It returns true only when ALL of the following hold:
//   - threshold > 0: the feature is opt-in; an unset/zero timeout disables it.
//   - !holdsClaim: a claimed-but-hung session is the stall-reaper's domain.
//     This targets the claim-less parked case the reaper cannot see (the session
//     parked before it could claim work).
//   - providerHealthy: never recycle a session whose provider cannot currently
//     serve; while a provider is unhealthy the session is left alone until it
//     recovers (composes with the provider-health respawn gate, move 3a).
//   - !exempt: the session is not attached, awaiting interaction, or within its
//     startup grace window.
//   - lastProgress is known and older than threshold.
//
// lastProgress is the most recent provider-reported activity timestamp the
// caller resolved. A zero value means progress is unknown, in which case the
// predicate is conservative and returns false rather than recycle a session
// whose liveness it cannot assess.
func sessionProgressStalled(threshold time.Duration, holdsClaim, providerHealthy, exempt bool, lastProgress, now time.Time) bool {
	if threshold <= 0 || holdsClaim || !providerHealthy || exempt {
		return false
	}
	if lastProgress.IsZero() {
		return false
	}
	return now.Sub(lastProgress) > threshold
}
