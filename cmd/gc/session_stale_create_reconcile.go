package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// traceReasonPendingCreateLeaseExpired is the reason code legacy's rollback arms
// already fire at TraceSiteReconcilerPendingCreate (session_reconciler.go:1901).
// The keyed handler reuses it verbatim so the WD.15 parity join sees one reason
// vocabulary across both populations, separated only by effect_owner.
const traceReasonPendingCreateLeaseExpired TraceReasonCode = "pending_create_lease_expired"

// sessionStartupTimeoutForConfig reads the startup timeout the pending-create
// lease ladder is measured against. A nil config means no configured timeout,
// which is what the lease helpers already treat as "use the built-in windows".
func sessionStartupTimeoutForConfig(cfg *config.City) time.Duration {
	if cfg == nil {
		return 0
	}
	return cfg.Session.StartupTimeoutDuration()
}

// exactSessionStaleCreateRollbackCandidate is the D-STALE-CREATE seam guard: a
// revisioned, open, named row still holding a pending-create claim whose lease
// has expired by its own durable timestamps. It reads nothing but the row —
// never admission.Source — because a stranded create is level-triggered and the
// controller coalesces admissions on a key (the seam's first rule).
func exactSessionStaleCreateRollbackCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) bool {
	if response.Revision == 0 || info.Closed || !info.PendingCreateClaim {
		return false
	}
	if strings.TrimSpace(info.ID) == "" || strings.TrimSpace(info.SessionNameMetadata) == "" {
		return false
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return pendingCreateLeaseExpiredForRollbackInfo(info, clk, sessionStartupTimeoutForConfig(params.Config))
}

// reconcileExactSessionStaleCreateRollback rolls one crash-stranded pending
// create back by exact key. The runtime never came up, so there is nothing to
// stop: the effect is the SAME rollback the keyed start path already runs for
// its own failed starts (commitStartFailure's rollbackPendingCreate, traced as
// TraceSiteLifecycleStartRollback), applied once behind a revision fence instead
// of behind legacy's per-tick budget of five. The budget retires with the fleet
// pass; a keyed rollback runs on the controller's bounded worker pool, off the
// tick critical path, so a rollback burst can no longer stall it.
func reconcileExactSessionStaleCreateRollback(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (exactSessionStartOwner, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	yieldOrPark := func(cause error) (exactSessionStartOwner, error) {
		if params.RolloutMode == rollout.Auto {
			return exactSessionStartLegacyOwner, fmt.Errorf("%w: %w", errSessionStartLegacyFallbackRequired, cause)
		}
		return exactSessionStartKeyedOwner, cause
	}
	if params.DrainTracker != nil && params.DrainTracker.get(info.ID) != nil {
		return yieldOrPark(errors.New("exact stale pending create has an active legacy drain"))
	}

	name := strings.TrimSpace(info.SessionNameMetadata)
	// Proven absence, not assumed absence. The sweep's names-only ListRunning is
	// a scheduling hint; the rollback closes the row that owns the alias, so the
	// observation is re-paid per key here and fails CLOSED on an unreadable
	// provider exactly as the legacy arm does (session_reconciler.go:1873):
	// "observation unavailable" is not "confirmed dead", and the condition is
	// re-detected next sweep.
	running, livenessErr := workerSessionTargetRunningWithConfig(params.CityPath, params.Store, params.Provider, params.Config, info.ID)
	if livenessErr != nil {
		recordExactSessionStaleCreateTrace(params, admission, info, TraceSiteReconcilerPendingCreate,
			traceReasonPendingCreateLeaseExpired, TraceOutcomeSkippedLivenessError, false,
			map[string]any{"liveness_error": livenessErr.Error()})
		return exactSessionStartKeyedOwner, nil
	}
	if running {
		// The runtime is there after all, so the create was not stranded. A live
		// runtime under a claimed row is D-DRIFT's or D-ORPHAN's condition.
		return exactSessionStartKeyedOwner, nil
	}

	// The fence: re-read the authoritative row and refuse unless it is still the
	// exact incarnation the condition was detected on.
	latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil || latestResponse.Revision != response.Revision || latest.Closed ||
		strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
		strings.TrimSpace(latest.SessionNameMetadata) != name {
		return exactSessionStartKeyedOwner, nil
	}
	if !exactSessionStaleCreateRollbackCandidate(params, latest, latestResponse, clk) {
		if latest.PendingCreateClaim {
			// Re-leased between detection and dispatch: a create is in flight
			// again, so this is a no-op that fires the legacy preserved site with
			// effect_owner=keyed and effect_applied=false.
			recordExactSessionStaleCreateTrace(params, admission, latest, TraceSiteReconcilerPendingCreatePreserved,
				TraceReasonPendingCreate, TraceOutcomeKeptOpen, false,
				map[string]any{"state": latest.MetadataState})
		}
		return exactSessionStartKeyedOwner, nil
	}

	// A rollback is a multi-write Tx plus retired-session cleanup. Do not enter
	// it on a canceled context (controller shutdown): the condition is durable,
	// so the next sweep re-detects it, and returning no error keeps a shutdown
	// out of the retry accounting.
	if ctx != nil && ctx.Err() != nil {
		return exactSessionStartKeyedOwner, nil
	}
	// One rollback, no second implementation. rollbackPendingCreate clears the
	// in-flight lease marker, closes the row failed-create — which clears
	// pending_create_claim inside the same Tx (closeFailedCreateBeadInTx) — and
	// is idempotent against an already-closed bead, so a re-admitted key is a
	// zero-effect no-op rather than a second close.
	applied := rollbackPendingCreate(latest, sessionFrontDoor(params.Store), clk.Now().UTC(), stderr) != nil
	recordExactSessionStaleCreateTrace(params, admission, latest, TraceSiteReconcilerPendingCreate,
		traceReasonPendingCreateLeaseExpired, TraceOutcomeRollback, applied, nil)
	return exactSessionStartKeyedOwner, nil
}

// recordExactSessionStaleCreateTrace fires the SAME legacy trace sites the fleet
// arms fire — PendingCreate and PendingCreatePreserved — with effect_owner=keyed
// and the honest effect_applied. The WD.15 parity join reads exactly these
// fields to separate the legacy, keyed, and detector-shadow populations.
func recordExactSessionStaleCreateTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	site TraceSiteCode,
	reason TraceReasonCode,
	outcome TraceOutcomeCode,
	applied bool,
	extra map[string]any,
) {
	if params.Trace == nil || site == "" {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_stale_create_rollback", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	fields := map[string]any{
		"admission":            string(admission.Source),
		"admission_version":    admission.Version,
		"generation":           params.Generation,
		"instance_token":       info.InstanceToken,
		"pending_create_claim": strings.TrimSpace(info.PendingCreateClaimMetadata),
		"effect_owner":         detectorKeyedEffectOwner,
		"effect_applied":       applied,
	}
	for k, v := range extra {
		fields[k] = v
	}
	cycle.recordKeyedEffect(
		site,
		reason,
		outcome,
		"exact_session_stale_create_rollback",
		template,
		info.ID,
		info.SessionNameMetadata,
		0,
		fields,
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact stale-create rollback trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}
