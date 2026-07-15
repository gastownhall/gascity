package main

import (
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// openPoolSessionCountForTemplate counts the reconciler working-set sessions
// that are still open and resolve to the given agent template. It reads each
// session's typed Info from the coherent snapshot (infoByID), not the raw bead,
// so a session closed earlier in the tick is excluded as soon as its close has
// been refreshed onto the snapshot. It feeds the min-floor idle-worker
// exemption: a pool at or below its floor is entirely always-warm contingent.
//
// The count is order-independent (unique session IDs), so it ranges infoByID
// directly rather than the raw `ordered` beads (Step 5e demotion).
func openPoolSessionCountForTemplate(infoByID map[string]sessionpkg.Info, cfg *config.City, template string) int {
	open := 0
	for _, info := range infoByID {
		if !info.Closed && normalizedSessionTemplateInfo(info, cfg) == template {
			open++
		}
	}
	return open
}

// isMinFloorIdleWorker reports whether a session is a legitimate pool floor
// worker that should be exempt from the progress-stall recycler.
//
// A session is a floor worker when the pool has a configured floor
// (minActiveSessions > 0) AND the number of currently open sessions in the
// pool is at or below that floor. In this state every live session is part of
// the always-warm contingent; none should be recycled for being unclaimed —
// they are waiting for routed work, not parked on an error.
//
// Inputs are in-memory values available to the caller; no I/O required.
func isMinFloorIdleWorker(minActiveSessions, openSessionsInPool int) bool {
	return minActiveSessions > 0 && openSessionsInPool <= minActiveSessions
}

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
