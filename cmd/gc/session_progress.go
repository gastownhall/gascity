package main

import (
	"strings"
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

// isWarmFloorCandidate reports whether a session Info currently occupies a warm
// min_active_sessions floor slot: an open session in a genuinely-live runtime
// state (on its way to, or currently, active). It is an explicit ALLOW-LIST —
// only StateNone (freshly stamped, not yet transitioned), StateActive and
// StateAwake (running; Awake is the reconciler healState alias for Active —
// session_reconcile.go / lifecycle_projection.go RuntimeProjectionAlive),
// StateCreating, and StateStartPending count.
//
// It adopts countMinActiveCovered's allow-list SHAPE (compute_awake_set.go)
// rather than diverging into a deny-list — but deliberately not its state SET.
// countMinActiveCovered admits only active/creating (plus an asleep bead an
// earlier pass already marked desired-awake); this predicate additionally
// admits Awake, StartPending and StateNone, and never admits asleep. The two
// answer different questions: this one ranks the CURRENT warm occupancy of one
// session, so anything on its way to (or already) running counts and a dormant
// bead never does, while countMinActiveCovered counts coverage of the floor
// including the asleep sessions this tick's wake pass will revive.
//
// Why an allow-list (Doc adversarial re-review, sc-sabwwn): every other state —
// Asleep, Suspended, Draining, Drained, Archived, FailedCreate, Quarantined,
// Closed — is a dormant, terminal, or non-runnable bead that can persist with
// Closed==false yet is NOT a warm occupant. Counting any such stale low-id bead
// would inflate the floor rank in isMinFloorExemptIdleSession and mask the ACTUAL
// live low-id floor session out of the deterministic exempt set, getting that
// warm session idle-killed and cold-recreated — the exact 0<->1 oscillation this
// fix (sc-5mtyhy) exists to eliminate. A deny-list silently reopened that gap for
// every state it forgot (Quarantined/FailedCreate/Drained/Archived did leak); the
// allow-list fails closed — an unknown or newly-added state is excluded, never a
// spurious floor occupant. The info.Closed guard stays first: a closed bead has
// State=="" (StateNone), which the allow-list admits, so occupancy must be
// rejected on Closed before the state switch.
func isWarmFloorCandidate(info sessionpkg.Info) bool {
	if info.Closed {
		return false
	}
	switch info.State {
	case sessionpkg.StateNone, sessionpkg.StateActive, sessionpkg.StateAwake,
		sessionpkg.StateCreating, sessionpkg.StateStartPending:
		return true
	default:
		return false
	}
}

// isMinFloorExemptIdleSession reports whether sessionID is one of the
// deterministic min_active_sessions floor members for template — the minSess
// lowest-bead-id warm pool sessions — which the idle-timeout path keeps WARM
// (exempt from the idle kill) instead of killing and cold-recreating it every
// tick (sc-5mtyhy). This is the per-session, deterministic complement to the
// count-based isMinFloorIdleWorker the progress-stall recycler uses.
//
// The exempt set is the POOL-MANAGED floor members only. Ranking mirrors
// isMinActivePoolBead (compute_awake_set.go), the predicate that defines which
// beads the min_active_sessions guarantee covers: configured-named, manual and
// dependency-only sessions are excluded because they carry their own keep-awake
// rules and never participate in the floor. Excluding them matters in both
// directions — a lower-id non-pool session must not consume a floor rank (which
// would push the real pool member out of the exempt set and no-op this fix in a
// mixed-identity template), and a non-pool session must never become
// idle-exempt itself. Like isMinActivePoolBead the filter is exclusion-based:
// it does NOT require a positive pool-managed flag, so legacy pool beads
// predating that projection still rank.
//
// Determinism (spec acceptance 4): the exempt set is the minSess lowest-id warm
// same-template pool sessions, mirroring cityStopPoolBeads' bead-ID ordering, so
// the same concrete sessions stay warm tick over tick with no flapping over
// which one is exempt. sessionID is exempt exactly when it is itself a warm
// same-template pool session AND fewer than minSess such sessions sort below its
// ID. Elastic sessions ABOVE the floor are never exempt and idle-reclaim
// normally (acceptance 2); max_active_sessions still caps the total because this
// only defers idle kills for the bottom minSess sessions — it never creates any.
//
// Selection reads the coherent infoByID snapshot and cfg in memory; no I/O.
func isMinFloorExemptIdleSession(infoByID map[string]sessionpkg.Info, cfg *config.City, template, sessionID string) bool {
	if cfg == nil || sessionID == "" {
		return false
	}
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil {
		return false
	}
	minFloor := cfgAgent.EffectiveMinActiveSessions()
	if minFloor <= 0 {
		return false
	}
	self := false
	lower := 0
	for sid, info := range infoByID {
		if !isWarmFloorCandidate(info) || normalizedSessionTemplateInfo(info, cfg) != template {
			continue
		}
		// Pool-managed identities only, mirroring isMinActivePoolBead. The
		// filter runs before the self-check below, so an excluded identity
		// neither consumes a floor rank nor becomes exempt itself.
		if isNamedSessionInfo(info) || strings.TrimSpace(info.ConfiguredNamedIdentity) != "" ||
			isManualSessionInfo(info) || info.DependencyOnly {
			continue
		}
		switch {
		case sid == sessionID:
			self = true
		case sid < sessionID:
			lower++
		}
	}
	return self && lower < minFloor
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

// sessionClaimHolderStalled reports whether a confirmed claim-holder has made
// no provider-reported progress long enough to require a fresh restart. It is
// deliberately separate from sessionProgressStalled: claim-less sessions are
// safe to recycle sooner, while recycling a holder risks interrupting work.
// Unknown activity, provider failure, and protected sessions always suppress
// the recycle.
func sessionClaimHolderStalled(threshold time.Duration, holdsClaim, providerHealthy, exempt bool, lastProgress, now time.Time) bool {
	if threshold <= 0 || !holdsClaim || !providerHealthy || exempt || lastProgress.IsZero() {
		return false
	}
	return now.Sub(lastProgress) > threshold
}

// minPositiveDuration returns the smaller positive duration, or zero when both
// are disabled. It lets the reconciler defer its more expensive checks until
// either stall policy could apply.
func minPositiveDuration(first, second time.Duration) time.Duration {
	switch {
	case first <= 0:
		return second
	case second <= 0:
		return first
	case first < second:
		return first
	default:
		return second
	}
}
