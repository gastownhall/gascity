package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const (
	sessionStartControllerMaxDistinct = 4096
	sessionStartControllerMaxRetries  = 5
	sessionStartSeedPageSize          = 64
)

type sessionStartOwnership uint8

const (
	sessionStartOwnershipLegacy sessionStartOwnership = iota
	sessionStartOwnershipKeyed
	sessionStartOwnershipRequiredBlocked
)

var newCitySessionStartController = newSessionStartController

func (cr *CityRuntime) ensureSessionStartController(ctx context.Context, seed *sessionBeadSnapshot) error {
	if cr == nil {
		return fmt.Errorf("city runtime is nil")
	}
	cr.sessionStartLifecycleMu.Lock()
	defer cr.sessionStartLifecycleMu.Unlock()

	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()

	mode := rollout.ModeUnset
	if cr.cs != nil {
		mode = cr.cs.RolloutFlags().SessionReconciler()
	}
	cr.sessionStartMode = mode
	if cr.sessionStartController != nil && cr.sessionStartOwnership == sessionStartOwnershipKeyed {
		return cr.seedSessionStartController(cr.sessionStartController, seed)
	}

	var (
		stateSnapshot controllerSessionStartSnapshot
		releaseState  func()
		capabilityErr error
	)
	decision, reason := rollout.ResolveCapability(ctx, mode, func(context.Context) (bool, string) {
		if cr.cs == nil {
			return false, "controller state is unavailable"
		}
		if seed == nil {
			return false, "startup session snapshot is unavailable"
		}
		if err := seed.LoadError(); err != nil {
			return false, "startup session snapshot is incomplete: " + err.Error()
		}
		stateSnapshot, releaseState, capabilityErr = cr.cs.acquireSessionStartSnapshot()
		if capabilityErr != nil {
			return false, capabilityErr.Error()
		}
		return true, "coherent config, provider, and session store are available"
	})
	if releaseState != nil {
		defer releaseState()
	}

	switch decision {
	case rollout.UseLegacy:
		cr.sessionStartOwnership = sessionStartOwnershipLegacy
		return nil
	case rollout.DegradeLoud:
		cr.sessionStartOwnership = sessionStartOwnershipLegacy
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start controller unavailable (%s); falling back to legacy reconciliation\n", cr.sessionStartLogPrefix(), reason) //nolint:errcheck // rollout degradation must be loud
		return nil
	case rollout.RefuseClosed:
		cr.sessionStartOwnership = sessionStartOwnershipRequiredBlocked
		return fmt.Errorf("required keyed session-start controller is unavailable: %s", reason)
	case rollout.UseNew:
		// Continue below.
	default:
		cr.sessionStartOwnership = sessionStartOwnershipLegacy
		return fmt.Errorf("unexpected session-start rollout decision %q", decision)
	}

	workers := maxParallelStartsPerTick(stateSnapshot.Config)
	var controller *sessionStartController
	var err error
	controller, err = newCitySessionStartController(sessionStartControllerOptions{
		Workers:     workers,
		MaxDistinct: sessionStartControllerMaxDistinct,
		MaxRetries:  sessionStartControllerMaxRetries,
		Reconcile: func(reconcileCtx context.Context, admission sessionStartAdmission) error {
			snapshot, release, acquireErr := cr.cs.acquireSessionStartSnapshot()
			if acquireErr != nil {
				return acquireErr
			}
			leaseTransferred := false
			defer func() {
				if !leaseTransferred {
					release()
				}
			}()
			startOptions := cr.sessionStartOptions
			if sessionStartAdmissionIsDemand(admission.Source) {
				startOptions = append([]startExecutionOption(nil), startOptions...)
				startOptions = append(startOptions, withAdditionalExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
					cr.recordExactSessionLifecycleStatusApplied(snapshot.Config, result)
					cr.recordExactSessionLifecycleStatusShadow(snapshot.Config, result)
				}))
			}
			// The session class REQUIRES the fence. Every keyed write on a
			// session row — deadline sleep, zombie terminal mark, status heal,
			// drain-ack — is decided from a revision this store handed out and
			// applied under that revision, so a nil writer is not a legacy
			// fallback, it is the fence removed. Asking for the capability
			// instead of resolving the rollout gate is what stops a store that
			// genuinely implements conditional writes from being routed around
			// because some policy value never reached it (ga-f7v2ft.162).
			statusWriter, statusWriterErr := beads.RequiredConditionalWriter(snapshot.Store)
			owner, reconcileErr := reconcileExactSessionStartWithOwner(reconcileCtx, admission, exactSessionStartParams{
				Generation:        snapshot.Generation,
				CityPath:          snapshot.CityPath,
				CityName:          snapshot.CityName,
				Config:            snapshot.Config,
				Provider:          snapshot.Provider,
				Store:             snapshot.Store,
				StatusWriter:      statusWriter,
				StatusWriterError: statusWriterErr,
				Recorder:          snapshot.Recorder,
				Stdout:            cr.sessionStartStdout(),
				Stderr:            cr.sessionStartStderr(),
				StartOptions:      startOptions,
				AsyncStopTracker:  &cr.asyncStops,
				AsyncStopCompletion: func(completion drainAckAsyncStopCompletion) {
					release()
					cr.sessionStartMu.Lock()
					activeController := cr.sessionStartController
					cr.sessionStartMu.Unlock()
					if completion == drainAckAsyncStopYielded {
						if admission.PoolDrainAck != nil && activeController != nil && activeController.YieldPoolDrainAck(*admission.PoolDrainAck) {
							cr.requestLegacySessionStartFallback()
						}
						return
					}
					if completion == drainAckAsyncStopParked {
						if admission.PoolDrainAck != nil && activeController != nil {
							if _, err := activeController.AdmitPoolDrainAck(*admission.PoolDrainAck); err != nil {
								fmt.Fprintf(cr.sessionStartStderr(), "%s: retaining parked drain-ack stop for %s: %v\n", cr.sessionStartLogPrefix(), admission.SessionID, err) //nolint:errcheck
							}
						} else {
							cr.admitDrainAckStopCompletion(admission.SessionID)
						}
						return
					}
					cr.admitDrainAckStopCompletion(admission.SessionID)
				},
				AsyncStopQueued: func() {
					leaseTransferred = true
				},
				RolloutMode:              mode,
				RigStores:                cr.rigBeadStores(),
				DrainOps:                 cr.dops,
				DrainTracker:             cr.sessionDrains,
				IdleTracker:              cr.it,
				MaxSessionAgeTracker:     cr.mat,
				AssignedWorkDeferTracker: cr.adt,
				DesiredSessionNames:      cr.desiredSessionNamesView,
				ProviderHealth:           cr.providerHealthSnapshotView,
				SessionLiveness:          cr.sessionLivenessView,
				SessionWakeEvaluations:   cr.sessionWakeEvaluationsView,
				Trace:                    cr.trace,
				AuthorizePoolStart: func(authorizeCtx context.Context, info sessionpkg.Info, lease routedWorkPoolStartLease) (bool, error) {
					return cr.authorizeRoutedWorkPoolStart(authorizeCtx, snapshot, info, lease)
				},
				AuthorizePoolDrainAck: func(info sessionpkg.Info, lease routedWorkPoolDrainAckLease) (bool, drainAckRefusal, error) {
					return cr.authorizeRoutedWorkPoolDrainAck(snapshot, info, lease)
				},
				RecoverPoolDrainAck: func(info sessionpkg.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
					return cr.recoverRoutedWorkPoolDrainAckLease(snapshot, info)
				},
				ValidateWaitDependencyPoolWitness: func(info sessionpkg.Info, lease sessionWaitDependencyStartLease) bool {
					return cr.sessionWaitDependencyPoolWitnessCurrent(snapshot, info, lease)
				},
				ValidateConfiguredDependencyStart: func(info sessionpkg.Info, lease configuredDependencyStartLease) bool {
					return cr.configuredDependencyStartWitnessCurrent(snapshot, info, lease)
				},
				EnterConfiguredDependencyStart: func(lease configuredDependencyStartLease) bool {
					return controller.enterConfiguredDependencyStart(lease)
				},
				ValidateStrictDefaultPoolWakeStart: func(info sessionpkg.Info, lease strictDefaultPoolWakeStartLease) bool {
					return cr.strictDefaultPoolWakeStartWitnessCurrent(snapshot, info, lease)
				},
				EnterStrictDefaultPoolWakeStart: func(lease strictDefaultPoolWakeStartLease) bool {
					return controller.enterStrictDefaultPoolWakeStart(lease)
				},
				ValidateConfiguredNamedWakeStart: func(info sessionpkg.Info, lease configuredNamedWakeStartLease) bool {
					return cr.configuredNamedWakeStartWitnessCurrent(snapshot, info, lease)
				},
				EnterConfiguredNamedWakeStart: func(lease configuredNamedWakeStartLease) bool {
					return controller.enterConfiguredNamedWakeStart(lease)
				},
				CertifyWakeFamilyStart: func(info sessionpkg.Info, revision int64) bool {
					return cr.certifyWakeFamilyStartForKey(snapshot, info, revision)
				},
			})
			if reconcileErr == nil && owner == exactSessionStartLegacyOwner {
				return errSessionStartLegacyFallbackRequired
			}
			return reconcileErr
		},
		Observer: func(result sessionStartReconcileResult) {
			cr.observeSessionStartReconcile(stateSnapshot.Config, mode, result)
		},
		Stderr: cr.sessionStartStderr(),
	})
	if err != nil {
		return cr.sessionStartActivationFailure(mode, fmt.Errorf("creating child: %w", err))
	}
	if err := controller.Start(ctx); err != nil {
		cr.stopAbandonedSessionStartController(controller)
		return cr.sessionStartActivationFailure(mode, fmt.Errorf("starting child: %w", err))
	}
	admit := func(id string) {
		outcome, admitErr := controller.Admit(id, sessionStartAdmissionInProcess)
		cr.refreshPoolMembershipSession(id)
		if admitErr != nil {
			fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting session-start event for %s: %v\n", cr.sessionStartLogPrefix(), id, admitErr) //nolint:errcheck // admission failure is recoverable via audit
			return
		}
		if outcome == sessionStartAdmissionOverflow {
			fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start admission overflow for %s; authoritative audit requested\n", cr.sessionStartLogPrefix(), id) //nolint:errcheck // bounded queue overflow must be visible
			// This callback is drained before its captured controller stops. Seed
			// that controller directly so shutdown need not release its ownership
			// fence while it waits for admitted callbacks.
			if err := cr.seedSessionStartController(controller, cr.loadSessionBeadSnapshot()); err != nil {
				fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start authoritative audit: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // audit failure remains level-triggered
			}
			// The eager seed consumes auditPending. Re-arm it so the next full
			// reconciliation still verifies the authoritative snapshot.
			controller.RequestAudit()
			cr.requestLegacySessionStartFallback()
		}
	}
	if err := cr.cs.installSessionStartEventAdmission(admit); err != nil {
		cr.stopAbandonedSessionStartController(controller)
		if mode == rollout.Require {
			cr.sessionStartOwnership = sessionStartOwnershipRequiredBlocked
		}
		// An already-installed callback means another keyed admission owner may
		// exist. Auto cannot safely enable legacy in that ambiguous state.
		return fmt.Errorf("installing session-start event admission: %w", err)
	}
	if err := cr.seedSessionStartController(controller, seed); err != nil {
		cr.cs.stopSessionStartEventAdmission()
		cr.stopAbandonedSessionStartController(controller)
		return cr.sessionStartActivationFailure(mode, fmt.Errorf("seeding child: %w", err))
	}

	cr.sessionStartController = controller
	cr.sessionStartOwnership = sessionStartOwnershipKeyed
	return nil
}

// admitDrainAckStopCompletion sends confirmed or retryable stop completion
// through the current keyed owner. The durable stop marker, not the completed
// controller instance, is the recovery record across a provider reload.
func (cr *CityRuntime) admitDrainAckStopCompletion(id string) {
	if cr == nil {
		return
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		cr.requestLegacySessionStartFallback()
		return
	}
	outcome, err := controller.Admit(id, sessionStartAdmissionInProcess)
	if err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting drain-ack stop completion for %s: %v\n", cr.sessionStartLogPrefix(), id, err) //nolint:errcheck // durable audit remains recovery path
		cr.seedActiveSessionStartController(cr.loadSessionBeadSnapshot())
		controller.RequestAudit()
		return
	}
	if outcome == sessionStartAdmissionOverflow {
		cr.seedActiveSessionStartController(cr.loadSessionBeadSnapshot())
		controller.RequestAudit()
	}
}

func withAdditionalExactSessionLifecycleStatusObserver(observer exactSessionLifecycleStatusObserver) startExecutionOption {
	return func(opts *startExecutionOptions) {
		existing := opts.exactStatusObserver
		opts.exactStatusObserver = func(result exactSessionLifecycleStatusResult) {
			if observer != nil {
				observer(result)
			}
			if existing != nil {
				existing(result)
			}
		}
	}
}

func (cr *CityRuntime) recordExactSessionLifecycleStatusApplied(cfg *config.City, result exactSessionLifecycleStatusResult) {
	if cr == nil || cr.trace == nil || !sessionStartAdmissionIsDemand(result.Admission.Source) ||
		!result.RuntimeLive || result.Disposition != exactSessionLifecycleStatusDispositionCandidate || result.Plan == nil ||
		result.Plan.Outcome != sessionLifecycleStatusHeal || !result.EffectApplied {
		return
	}
	admittedAt := result.Admission.AdmittedAt
	observedAt := result.ObservedAt
	if admittedAt.IsZero() || observedAt.IsZero() || observedAt.Before(admittedAt) {
		return
	}
	trace := cr.trace
	cycle := trace.BeginCycle(TraceTickTriggerControl, "session_lifecycle_status_heal", admittedAt, cfg)
	if cycle == nil {
		return
	}
	cycle.RecordMutation(TraceSiteMutationBeadMetadata, TraceReasonUnknown, TraceOutcomeApplied, "session", result.RequestedID, "update_if_match", map[string]any{
		"session_id":        result.RequestedID,
		"admission":         string(result.Admission.Source),
		"admission_version": result.AdmissionVersion,
		"generation":        result.ControllerGeneration,
		"status_outcome":    exactSessionLifecycleStatusOutcomeTraceValue(result.Plan.Outcome),
		"status_reason":     string(result.Plan.Reason),
		"effect_owner":      detectorKeyedEffectOwner,
		"effect_applied":    true,
	})
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session lifecycle status heal trace: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // tracing must not affect reconciliation
	}
}

func (cr *CityRuntime) recordExactSessionLifecycleStatusShadow(cfg *config.City, result exactSessionLifecycleStatusResult) {
	if cr == nil || cr.trace == nil || !sessionStartAdmissionIsDemand(result.Admission.Source) ||
		!result.RuntimeLive || result.Disposition != exactSessionLifecycleStatusDispositionCandidate || result.Plan == nil ||
		result.Plan.Outcome != sessionLifecycleStatusNoop || result.Plan.Reason != sessionLifecycleStatusReasonConverged || result.EffectApplied {
		return
	}
	admittedAt := result.Admission.AdmittedAt
	observedAt := result.ObservedAt
	if admittedAt.IsZero() || observedAt.IsZero() || observedAt.Before(admittedAt) {
		return
	}
	trace := cr.trace
	cycle := trace.BeginCycle(TraceTickTriggerControl, "session_lifecycle_status_shadow", admittedAt, cfg)
	if cycle == nil {
		return
	}
	cycle.RecordControllerOperation(TraceSiteLifecycleStatusShadow, TraceReasonRetained, TraceOutcomeNoChange, "session_lifecycle_status_shadow", observedAt.Sub(admittedAt), map[string]any{
		"session_id":        result.RequestedID,
		"admission":         string(result.Admission.Source),
		"admission_version": result.AdmissionVersion,
		"generation":        result.ControllerGeneration,
		"status_outcome":    exactSessionLifecycleStatusOutcomeTraceValue(result.Plan.Outcome),
		"status_reason":     string(result.Plan.Reason),
		"effect_applied":    false,
	})
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session lifecycle status shadow trace: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // tracing must not affect reconciliation
	}
}

func exactSessionLifecycleStatusOutcomeTraceValue(outcome sessionLifecycleStatusOutcome) string {
	switch outcome {
	case sessionLifecycleStatusNoop:
		return "noop"
	case sessionLifecycleStatusHeal:
		return "heal"
	case sessionLifecycleStatusPark:
		return "park"
	default:
		return "unknown"
	}
}

// observeSessionStartReconcile is the runtime's observer for keyed
// session-start reconcile results: per-refusal drain-ack diagnostics, the
// throttled admission-bound trace, the deadline release, the named escalation
// crossing, legacy fallback requests, and the nudge re-poke. Extracted from
// the controller wiring so the observable surface is testable on crafted
// results.
func (cr *CityRuntime) observeSessionStartReconcile(cfg *config.City, mode rollout.Mode, result sessionStartReconcileResult) {
	if result.Outcome == sessionStartReconcileRetrying &&
		(result.Admission.PoolDrainAck != nil || result.Admission.PoolDrainAckUncertain) &&
		result.Err != nil && result.Err.Error() != errSessionStartPoolDrainAckPending.Error() {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start drain-ack reconciliation retrying for %s: %v\n", cr.sessionStartLogPrefix(), result.Admission.SessionID, result.Err) //nolint:errcheck // non-exhausting safety retries must retain their cause
	}
	// ga-f7v2ft.112 ruling 1b. Repeated refusals are indistinguishable from
	// transient by construction, so this classifies nothing and changes
	// nothing — it is the one throttled observability escalation, and the
	// deadline release below is what actually bounds the obligation.
	if result.Outcome == sessionStartReconcileRetrying && result.DrainAckRefusals > 0 &&
		result.DrainAckRefusals%drainAckRefusalDiagnosticInterval == 0 {
		cr.recordDrainAckAdmissionBoundTrace(cfg, result, TraceOutcomeRetry)
	}
	// The escalation crossing is loud exactly once per obligation: the
	// controller marks the single bound check on which the obligation's own
	// streak first crossed the threshold (ga-f7v2ft.191), and escalated
	// re-examinations after it are quiet by design — the slow cadence is the
	// bound, the named line is the signal (ga-f7v2ft.173).
	if result.Outcome == sessionStartReconcileDrainAckEscalated &&
		result.DrainAckEscalationCrossing {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start drain-ack reconciliation escalated for %s: unresolvable after %d consecutive refusals: %v; re-examining every %s until the row or runtime changes\n", //nolint:errcheck // the escalation must be visible: it replaces the per-retry storm
			cr.sessionStartLogPrefix(), result.Admission.SessionID, result.DrainAckRefusals, result.Err, drainAckEscalatedRetryInterval)
		cr.recordDrainAckAdmissionBoundTrace(cfg, result, TraceOutcomeEscalated)
	}
	if result.Outcome == sessionStartReconcileDeadlineExceeded {
		// The release is a per-cycle event: its number is the refusals of the
		// deadline cycle it releases, bounded by one cycle's re-examinations,
		// with the obligation's designed cumulative streak alongside
		// (ga-c9m4g — the lifetime count alone read as an unbounded climb).
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start drain-ack reconciliation released %s at the drain deadline after %d consecutive refusals this deadline cycle (obligation total %d): %v; authoritative audit requested\n", //nolint:errcheck // the release must be visible: legacy re-owns the row from here
			cr.sessionStartLogPrefix(), result.Admission.SessionID, result.DrainAckCycleRefusals, result.DrainAckRefusals, result.Err)
		cr.recordDrainAckAdmissionBoundTrace(cfg, result, TraceOutcomeDeadlineExceeded)
		if mode == rollout.Auto {
			cr.requestLegacySessionStartFallback()
		}
	}
	if result.Outcome == sessionStartReconcileSucceeded && result.LegacyFallback {
		if result.Err != nil {
			fmt.Fprintf(cr.sessionStartStderr(), "%s: exact session reconciliation yielded %s to priority legacy fallback: %v\n", cr.sessionStartLogPrefix(), result.Admission.SessionID, result.Err) //nolint:errcheck // fallback cause must remain visible
		}
		if result.Admission.PoolAllocation == nil {
			// Pool allocations have no legacy fallback from Q2 onward:
			// the yielded key is re-detected by the next patrol's
			// declared routed-work view (census-owed re-detection).
			cr.requestLegacySessionStartFallback()
		}
	}
	if result.Outcome == sessionStartReconcileSucceeded {
		// A queued nudge can arrive while this exact session is still
		// starting. Once lifecycle work completes, re-poke the nudge
		// dispatcher; it rereads durable queue authority before any effect.
		cr.signalNudgeKeyWake()
	}
	if result.Outcome == sessionStartReconcileExhausted {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start reconciliation exhausted for %s: %v; authoritative audit requested\n", cr.sessionStartLogPrefix(), result.Admission.SessionID, result.Err) //nolint:errcheck // terminal retry diagnostic
		if result.Admission.PoolAllocation == nil &&
			result.Admission.PoolDrainAck != nil && mode == rollout.Auto {
			cr.requestLegacySessionStartFallback()
		}
	}
}

func (cr *CityRuntime) sessionStartActivationFailure(mode rollout.Mode, err error) error {
	if mode == rollout.Require {
		cr.sessionStartOwnership = sessionStartOwnershipRequiredBlocked
		return err
	}
	cr.sessionStartOwnership = sessionStartOwnershipLegacy
	fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start controller failed (%v); falling back to legacy reconciliation\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // auto degradation must be loud
	return nil
}

func (cr *CityRuntime) seedSessionStartController(controller *sessionStartController, snapshot *sessionBeadSnapshot) error {
	if controller == nil {
		return fmt.Errorf("controller is nil")
	}
	if snapshot == nil {
		return fmt.Errorf("session snapshot is nil")
	}
	if err := snapshot.LoadError(); err != nil {
		return fmt.Errorf("session snapshot is incomplete: %w", err)
	}
	stateSnapshot, err := cr.cs.sessionStartSnapshot()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	cursor := 0
	var page []sessionpkg.Info
	return controller.StartAuthoritativeSeed(func(ctx context.Context) sessionStartAuthoritativeSeedResult {
		for {
			if ctx.Err() != nil {
				return sessionStartAuthoritativeSeedResult{Err: ctx.Err()}
			}
			if len(page) == 0 {
				page, cursor = snapshot.openInfoPage(cursor, sessionStartSeedPageSize)
				if len(page) == 0 {
					return sessionStartAuthoritativeSeedResult{Complete: true}
				}
			}
			info := page[0]
			page = page[1:]
			if validateSessionStartAdmission(info.ID, sessionStartAdmissionAntiEntropy) != nil ||
				!resolveExactSessionStartOrDrainAckStopOwnership(info, stateSnapshot.Config, now) {
				continue
			}
			if isDrainAckStopPendingInfo(info) {
				lease, agentDrainAck, legacyMarker, leaseErr := cr.recoverRoutedWorkPoolDrainAckLease(stateSnapshot, info)
				if leaseErr != nil {
					return sessionStartAuthoritativeSeedResult{
						SessionID:                  info.ID,
						PoolDrainAckUncertain:      true,
						PoolDrainAckUncertainToken: strings.TrimSpace(info.InstanceToken),
					}
				}
				if !agentDrainAck && legacyMarker {
					// A definitely non-agent acknowledgement is legacy-owned. Do
					// not manufacture a keyed STOP admission for it.
					continue
				}
				if !agentDrainAck {
					return sessionStartAuthoritativeSeedResult{
						SessionID:                  info.ID,
						PoolDrainAckUncertain:      true,
						PoolDrainAckUncertainToken: strings.TrimSpace(info.InstanceToken),
					}
				}
				return sessionStartAuthoritativeSeedResult{SessionID: info.ID, PoolDrainAck: &lease}
			}
			return sessionStartAuthoritativeSeedResult{SessionID: info.ID}
		}
	})
}

// stopAbandonedSessionStartController drains a child ensureSessionStartController
// started but will not publish, from inside that call's sessionStartMu critical
// section.
//
// It releases the state lock for the drain and re-takes it before returning, so
// the caller's remaining state writes and its deferred unlock are unchanged.
// Stop waits for in-flight workers, and those workers take sessionStartMu to
// read the published fleet views, so draining under it deadlocks
// (ga-f7v2ft.143). Draining here rather than after the caller returns also keeps
// the old ordering intact: the abandoned child is fully joined BEFORE ownership
// is handed to legacy, so the two can never act on one key at once.
// sessionStartLifecycleMu is held across the gap, so no concurrent ensure or
// stop can observe the released lock.
func (cr *CityRuntime) stopAbandonedSessionStartController(controller *sessionStartController) {
	if controller == nil {
		return
	}
	cr.sessionStartMu.Unlock()
	controller.Stop()
	cr.sessionStartMu.Lock()
}

func (cr *CityRuntime) seedActiveSessionStartController(snapshot *sessionBeadSnapshot) {
	if cr == nil {
		return
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	active := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !active || controller == nil {
		return
	}
	// Every legacy full tick already carries an authoritative session snapshot.
	// Reusing it here adds no store enumeration and permanently recovers missed
	// event hooks, queue loss, overflow, and retry exhaustion. TakeAuditRequest
	// clears the urgent bit; the periodic seed remains unconditional.
	controller.TakeAuditRequest()
	if err := cr.seedSessionStartController(controller, snapshot); err != nil {
		controller.RequestAudit()
		fmt.Fprintf(cr.sessionStartStderr(), "%s: session-start authoritative audit: %v\n", cr.sessionStartLogPrefix(), err) //nolint:errcheck // audit failure remains level-triggered
	}
}

// stopSessionStartController shuts the keyed child down and hands session-start
// ownership back to legacy.
//
// The drain runs OUTSIDE sessionStartMu (ga-f7v2ft.143). controller.Stop()
// blocks in ShutDownWithDrain until every in-flight worker finishes, and those
// workers take sessionStartMu to read the published fleet views
// (desiredSessionNamesView, providerHealthSnapshotView, sessionLivenessView), so
// holding it across the drain is a lock-order inversion: cleanup waits for the
// worker, the worker waits for the lock. Snapshot what the shutdown needs under
// the lock, release, drain, then re-take it to retire the pointer and flip
// ownership.
//
// The ownership flip stays AFTER the drain on purpose. Legacy must not re-own a
// key while a keyed worker is still finishing it, so the yield is published only
// once nothing keyed is in flight. sessionStartLifecycleMu is what keeps that
// safe without the state lock: it holds ensure out for the whole shutdown, so
// releasing sessionStartMu around the drain cannot let a second controller be
// built beside the one still draining.
func (cr *CityRuntime) stopSessionStartController() {
	if cr == nil {
		return
	}
	cr.sessionStartLifecycleMu.Lock()
	defer cr.sessionStartLifecycleMu.Unlock()

	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	cs := cr.cs
	cr.sessionStartMu.Unlock()

	if cs != nil {
		cs.stopSessionStartEventAdmission()
	}
	if controller != nil {
		controller.Stop()
	}

	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	cr.sessionStartController = nil
	if cr.sessionStartMode == rollout.Require {
		cr.sessionStartOwnership = sessionStartOwnershipRequiredBlocked
	} else {
		cr.sessionStartOwnership = sessionStartOwnershipLegacy
	}
}

func (cr *CityRuntime) restartSessionStartController(ctx context.Context) error {
	if cr == nil {
		return fmt.Errorf("city runtime is nil")
	}
	return cr.ensureSessionStartController(ctx, cr.loadSessionBeadSnapshot())
}

func (cr *CityRuntime) sessionStartOwnershipState() sessionStartOwnership {
	if cr == nil {
		return sessionStartOwnershipLegacy
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.sessionStartOwnership
}

// sessionStartSocketFallback records why the exact socket handoff yielded to
// the established legacy poke path. It deliberately does not change the reply
// or admission semantics.
func (cr *CityRuntime) sessionStartSocketFallback(sessionID, reason string) sessionStartSocketReply {
	fmt.Fprintf(cr.sessionStartStderr(), "%s: exact session-start socket fallback for %s: %s\n", cr.sessionStartLogPrefix(), sessionID, reason) //nolint:errcheck // fallback diagnostics must not affect admission
	return sessionStartSocketReplyFallback
}

func (cr *CityRuntime) configuredDependencyStartWitnessCurrent(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease configuredDependencyStartLease,
) bool {
	if cr == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil ||
		validateConfiguredDependencyStartLease(lease) != nil {
		return false
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	return configCurrent && snapshot.Generation == lease.ControllerGeneration &&
		configuredDependencyStartTargetMatches(info, snapshot.Config, lease) &&
		configuredDependencyStartDependencyMatches(snapshot.Store, snapshot.Config, snapshot.CityName, lease) &&
		allDependenciesAliveForTemplateWithClock(
			lease.TargetTemplate, snapshot.Config, nil, snapshot.Provider, snapshot.CityName, snapshot.Store, clock.Real{},
		)
}

func (cr *CityRuntime) strictDefaultPoolWakeStartWitnessCurrent(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease strictDefaultPoolWakeStartLease,
) bool {
	if cr == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil ||
		validateStrictDefaultPoolWakeStartLease(lease) != nil {
		return false
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	if !configCurrent || snapshot.Generation != lease.ControllerGeneration ||
		!strictDefaultPoolWakeIdentityMatches(info, snapshot.Config, lease) {
		return false
	}
	// Clause 2 of the uniform predicate contract (ga-f7v2ft.116 Q1). Eligibility
	// moved to supported(), which admits agent-capped pools for the first time,
	// so the cap itself now has to be checked where the action can change the
	// active count instead of being smuggled into the eligibility reason. Waking
	// a member that already holds its own occupancy adds no member, so its own
	// occupancy is excluded — the same self-exclusion the bounded wait-dependency
	// resume uses.
	agent := findAgentByTemplate(snapshot.Config, lease.PoolTarget)
	if agent == nil {
		return false
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates)
	if policy.maxActiveSessions < 0 {
		return true
	}
	observation, selfOccupied := cr.poolMembershipShadow.observeOccupiedMember(lease.PoolTarget, lease.SessionID)
	if !observation.certified {
		return false
	}
	return strictDefaultPoolWakeCapacityAvailable(policy.maxActiveSessions, observation.occupied, selfOccupied)
}

func (cr *CityRuntime) configuredNamedWakeStartWitnessCurrent(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	lease configuredNamedWakeStartLease,
) bool {
	if cr == nil || snapshot.Config == nil || snapshot.Provider == nil || snapshot.Store == nil ||
		validateConfiguredNamedWakeStartLease(lease) != nil {
		return false
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	return configCurrent && snapshot.Generation == lease.ControllerGeneration &&
		configuredNamedWakeIdentityMatches(info, snapshot.Config, snapshot.CityName, lease)
}

func (cr *CityRuntime) admitSessionStartSocketKey(sessionID string) sessionStartSocketReply {
	if cr == nil {
		return cr.sessionStartSocketFallback(sessionID, "controller runtime is nil")
	}
	if err := validateSessionStartAdmission(sessionID, sessionStartAdmissionSocket); err != nil {
		return sessionStartSocketReplyInvalid
	}

	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	mode := cr.sessionStartMode
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, "controller unavailable or not keyed")
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("acquiring controller snapshot: %v", err))
	}
	defer release()
	info, revision, err := getAuthoritativeSessionStartRecord(snapshot.Store, sessionID)
	if err != nil {
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("reading authoritative session row: %v", err))
	}
	lease, agentDrainAck, leaseRefusal, leaseErr := cr.newRoutedWorkPoolDrainAckLease(snapshot, info)
	if leaseErr != nil {
		if mode == rollout.Require {
			fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting exact pool drain acknowledgement for %s: %v (refusal=%s); required path refused closed\n", cr.sessionStartLogPrefix(), sessionID, leaseErr, leaseRefusal) //nolint:errcheck // required refusal must remain visible
			return sessionStartSocketReplyBlocked
		}
		fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting exact pool drain acknowledgement for %s: %v (refusal=%s); priority legacy fallback requested\n", cr.sessionStartLogPrefix(), sessionID, leaseErr, leaseRefusal) //nolint:errcheck // admission uncertainty must remain visible
		return cr.sessionStartSocketFallback(sessionID, "pool drain acknowledgement admission uncertainty")
	}
	if agentDrainAck {
		outcome, admitErr := controller.AdmitPoolDrainAck(lease)
		if admitErr != nil || outcome == sessionStartAdmissionOverflow {
			if mode == rollout.Require {
				fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting exact pool drain acknowledgement for %s: outcome=%s err=%v; required path refused closed\n", cr.sessionStartLogPrefix(), sessionID, outcome, admitErr) //nolint:errcheck // required refusal must remain visible
				return sessionStartSocketReplyBlocked
			}
			fmt.Fprintf(cr.sessionStartStderr(), "%s: admitting exact pool drain acknowledgement for %s: outcome=%s err=%v; priority legacy fallback requested\n", cr.sessionStartLogPrefix(), sessionID, outcome, admitErr) //nolint:errcheck // queue rejection must remain visible
			return cr.sessionStartSocketFallback(sessionID, "pool drain acknowledgement admission rejected")
		}
		return sessionStartSocketReplyOK
	}
	now := time.Now().UTC()
	if exactUserHoldSuspendCurrent(info, now) && !controller.ownsPoolDrainAckStop(info.ID, info.InstanceToken) {
		outcome, admitErr := controller.Admit(sessionID, sessionStartAdmissionSocket)
		if admitErr == nil && outcome != sessionStartAdmissionOverflow {
			return sessionStartSocketReplyOK
		}
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("exact suspend admission rejected (outcome=%s err=%v)", outcome, admitErr))
	}
	if revision != 0 && exactOrdinaryResetCurrent(info, snapshot.Config, now) &&
		!controller.ownsPoolDrainAckStop(info.ID, info.InstanceToken) {
		outcome, admitErr := controller.Admit(sessionID, sessionStartAdmissionSocket)
		if admitErr == nil && outcome != sessionStartAdmissionOverflow {
			return sessionStartSocketReplyOK
		}
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("exact reset admission rejected (outcome=%s err=%v)", outcome, admitErr))
	}
	_, _, owner := classifyExactSessionStartOwnership(info, snapshot.Config, now)
	if owner != exactSessionStartKeyedOwner {
		admitted, admitErr := cr.admitCertifiedWakeFamilyStart(
			controller, snapshot, info, revision, sessionStartAdmissionSocket, now,
		)
		if admitted.Certified {
			if admitErr == nil && admitted.Outcome != sessionStartAdmissionOverflow {
				return sessionStartSocketReplyOK
			}
			if mode == rollout.Require {
				return sessionStartSocketReplyBlocked
			}
			return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("%s admission rejected (outcome=%s err=%v)", admitted.Family, admitted.Outcome, admitErr))
		}
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, "clean legacy ownership classification")
	}
	outcome, err := controller.Admit(sessionID, sessionStartAdmissionSocket)
	if err != nil || outcome == sessionStartAdmissionOverflow {
		if mode == rollout.Require {
			return sessionStartSocketReplyBlocked
		}
		return cr.sessionStartSocketFallback(sessionID, fmt.Sprintf("exact session-start admission rejected (outcome=%s err=%v)", outcome, err))
	}
	return sessionStartSocketReplyOK
}

// admitCertifiedWakeFamilyStart is the ONE place a wake certificate is minted
// and admitted. Every entry point that can reach a wake target uses it — the CLI
// socket, the detector sweep's D-WAKE routing seam, and the pre-lease ownership
// seam inside the keyed handler — so the three families are certified in one
// fixed order against one authoritative row, and no entry point can drift into
// admitting a shape another one refuses.
//
// The families are tried named → strict-default pool → configured dependency,
// which is the order the socket path has always used and is disjoint by
// construction: named rows fail the pool predicates, slotized rows fail the
// named and singleton predicates, and canonical singletons fail the slot
// predicate.
type certifiedWakeAdmission struct {
	// Family names the certifying family for diagnostics only. Empty means
	// nothing certified — the row is a wake target no lease can own, which is a
	// legitimate answer, not an error.
	Family    string
	Outcome   sessionStartAdmissionOutcome
	Certified bool
}

func (cr *CityRuntime) admitCertifiedWakeFamilyStart(
	controller *sessionStartController,
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	revision int64,
	source sessionStartAdmissionSource,
	now time.Time,
) (certifiedWakeAdmission, error) {
	if cr == nil || controller == nil || snapshot.Config == nil {
		return certifiedWakeAdmission{}, nil
	}
	if lease, certified := certifyConfiguredNamedWakeStartLease(info, revision, snapshot.Config, snapshot.CityName, snapshot.Generation, now); certified {
		outcome, err := controller.AdmitConfiguredNamedWake(lease, source)
		return certifiedWakeAdmission{Family: "configured named wake", Outcome: outcome, Certified: true}, err
	}
	if lease, certified := certifyStrictDefaultPoolWakeStartLease(info, revision, snapshot.Config, snapshot.Generation, now); certified &&
		cr.strictDefaultPoolWakeStartWitnessCurrent(snapshot, info, lease) {
		outcome, err := controller.AdmitStrictDefaultPoolWake(lease, source)
		return certifiedWakeAdmission{Family: "strict-default pool wake", Outcome: outcome, Certified: true}, err
	}
	if lease, certified := certifyConfiguredDependencyStartLease(info, snapshot.Config, snapshot.Provider, snapshot.CityName, snapshot.Store, snapshot.Generation, now); certified {
		outcome, err := controller.AdmitConfiguredDependency(lease, source)
		return certifiedWakeAdmission{Family: "configured-dependency", Outcome: outcome, Certified: true}, err
	}
	return certifiedWakeAdmission{}, nil
}

// certifyWakeFamilyStartForKey backs exactSessionStartParams.CertifyWakeFamilyStart:
// the pre-lease ownership seam. The keyed handler calls it with the row it has
// already read, immediately before it would have surrendered that row to legacy
// and asked for the fallback poke that consumes the durable wake cause. A true
// return means a certified lease now names this exact key and the key has been
// re-admitted under it.
func (cr *CityRuntime) certifyWakeFamilyStartForKey(
	snapshot controllerSessionStartSnapshot,
	info sessionpkg.Info,
	revision int64,
) bool {
	if cr == nil {
		return false
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		return false
	}
	admitted, err := cr.admitCertifiedWakeFamilyStart(
		controller, snapshot, info, revision, sessionStartAdmissionWakeFill, time.Now().UTC(),
	)
	return admitted.Certified && err == nil && admitted.Outcome != sessionStartAdmissionOverflow
}

// detectorWakeAdmitFunc hands the detector sweep D-WAKE's certified-lease
// admission. Unlike every other acting family, D-WAKE cannot ride the bare
// Admit(id, source) entry: a wake IS a start, and the keyed start path fences a
// start with a lease, so the routing seam must certify.
//
// Recorded §3 delta: certification runs at the ROUTING seam, not in detection.
// The sweep itself stays read-only and pays no new reads; the seam pays one
// authoritative row read per ROUTED wake key, which is the same read the socket
// path pays for the same decision and is bounded by the wake-target set (rows the
// awake set wants and the liveness probe found dead), not by the fleet.
//
// An EMPTY outcome with a nil error is the seam's "no lease can own this row"
// answer — a pool-fill member, or one whose dependency is not alive. It is not a
// failure: the row stays legacy's and the level-triggered sweep re-detects it.
func (cr *CityRuntime) detectorWakeAdmitFunc() func(string) (sessionStartAdmissionOutcome, error) {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		return nil
	}
	return func(sessionID string) (sessionStartAdmissionOutcome, error) {
		snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
		if err != nil {
			return "", fmt.Errorf("acquiring wake admission snapshot for %q: %w", sessionID, err)
		}
		defer release()
		info, revision, err := getAuthoritativeSessionStartRecord(snapshot.Store, sessionID)
		if err != nil {
			return "", fmt.Errorf("reading wake target %q: %w", sessionID, err)
		}
		admitted, admitErr := cr.admitCertifiedWakeFamilyStart(
			controller, snapshot, info, revision, sessionStartAdmissionWakeFill, time.Now().UTC(),
		)
		if !admitted.Certified {
			return "", nil
		}
		return admitted.Outcome, admitErr
	}
}

// detectorPoolAllocationEnqueueFunc hands the detector sweep the existing
// pool-allocation admission. It is D-WAKE's pool-under-min FILL sink: the arm
// has no session row to admit, so its exact key is the routed work's
// (workID, poolTarget, sourceStore) triple and the handler behind it is
// handleRoutedWorkPoolAllocation, unchanged.
//
// Nil unless keyed ownership is live and the hint channel exists, which keeps a
// legacy-owned city's sweep read-only whatever its act constants say.
func (cr *CityRuntime) detectorPoolAllocationEnqueueFunc() func(readyRoutedWorkEntry) bool {
	if cr == nil || cr.routedWorkPoolAllocationCh == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned {
		return nil
	}
	return func(entry readyRoutedWorkEntry) bool {
		return cr.enqueueRoutedWorkPoolAllocation(readyRoutedWorkDemandContribution{
			WorkID:      entry.WorkID,
			PoolTarget:  entry.PoolTarget,
			SourceStore: entry.SourceStore,
			SourceActor: detectorSweepDemandActor,
			DecidedAt:   time.Now().UTC(),
		})
	}
}

// detectorSweepDemandActor names the sweep as the origin of a routed-work
// contribution. Event-carried work names the writing actor; census-owed
// re-detection has none, and naming the sweep is what lets the WD.15 parity join
// separate the two populations.
const detectorSweepDemandActor = "detector-sweep"

// detectorAdmitFunc hands the detector sweep the existing session-start
// controller's Admit entry. It is nil unless keyed ownership is live, so a
// legacy-owned city's sweep stays read-only no matter which detector family has
// flipped to act.
func (cr *CityRuntime) detectorAdmitFunc() func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	cr.sessionStartMu.Unlock()
	if !owned || controller == nil {
		return nil
	}
	return controller.Admit
}

// publishDesiredSessionNames records the desired-session view of the tick that
// is about to run the sweep, so the keyed D-ORPHAN close handler re-derives
// undesiredness from the SAME view that raised the condition. Keeping the view
// on the runtime rather than in the admission is what keeps the seam's rule
// intact: the handler still answers from durable state plus the fleet's own
// inputs, never from the detector's reason.
func (cr *CityRuntime) publishDesiredSessionNames(desired map[string]TemplateParams) {
	if cr == nil {
		return
	}
	names := make(map[string]bool, len(desired))
	for name := range desired {
		names[name] = true
	}
	cr.sessionStartMu.Lock()
	cr.desiredSessionNames = names
	cr.sessionStartMu.Unlock()
}

// detectorSuspendDeferrals returns this runtime's named spec-absence
// confirmation window for the detector sweep, creating it on first use. Only
// the patrol/boot tick calls it: the control dispatcher and `gc start` run
// narrowed sweeps, and a second sweep counting the same window on the same tick
// would confirm a suspend twice as fast as legacy does.
func (cr *CityRuntime) detectorSuspendDeferrals() *detectorSuspendDeferralTracker {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	if cr.orphanSuspendDeferrals == nil {
		cr.orphanSuspendDeferrals = newDetectorSuspendDeferralTracker()
	}
	return cr.orphanSuspendDeferrals
}

// detectorIdleProbes returns this runtime's D-SLEEP round-robin probe cursor
// for the detector sweep, creating it on first use. Only the patrol/boot tick
// calls it: the control dispatcher and `gc start` run narrowed sweeps, and a
// second sweep spending the same per-tick probe budget would double the fleet's
// idle-probe rate.
func (cr *CityRuntime) detectorIdleProbes() *detectorIdleProbeCursor {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	if cr.sleepIdleProbeCursor == nil {
		cr.sleepIdleProbeCursor = newDetectorIdleProbeCursor()
	}
	return cr.sleepIdleProbeCursor
}

// desiredSessionNamesView returns the last published desired-session view, or
// nil before the first patrol/boot tick has published one.
func (cr *CityRuntime) desiredSessionNamesView() map[string]bool {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.desiredSessionNames
}

// providerHealthSnapshotView returns the provider-health snapshot the last
// patrol/boot tick published for its sweep. A nil return falls the keyed gates
// back to their own file read, which is what a controller-free entry point and
// a city whose first tick has not run yet both need.
func (cr *CityRuntime) providerHealthSnapshotView() *providerHealthSnapshot {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.providerHealthSnap
}

// publishProviderHealthSnapshot records the registry view of the tick that is
// about to run the sweep, so every key that tick produces answers the ADR-0013
// respawn gate from ONE file read rather than one per key. It is the health
// half of publishDesiredSessionNames, and it keeps the same property: keyed and
// legacy answer the gate from one fleet input, never two.
func (cr *CityRuntime) publishProviderHealthSnapshot(snap *providerHealthSnapshot) {
	if cr == nil {
		return
	}
	cr.sessionStartMu.Lock()
	cr.providerHealthSnap = snap
	cr.sessionStartMu.Unlock()
}

// sessionLivenessView returns the two-bit observation the last patrol/boot
// sweep made. A nil return declines the D-ZOMBIE guard rather than making it
// probe, which is the fail-safe direction: the condition is level-triggered, so
// the next sweep re-detects.
func (cr *CityRuntime) sessionLivenessView() map[string]detectorLivenessBits {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.sessionLiveness
}

// publishSessionLiveness records the sweep's two-bit observation for the keyed
// D-ZOMBIE guard. Only the patrol/boot sweep calls it, for the reason
// publishDesiredSessionNames has the same restriction: a narrowed sweep would
// overwrite the fleet view with a partial one.
func (cr *CityRuntime) publishSessionLiveness(liveness map[string]detectorLivenessBits) {
	if cr == nil {
		return
	}
	cr.sessionStartMu.Lock()
	cr.sessionLiveness = liveness
	cr.sessionStartMu.Unlock()
}

// sessionWakeEvaluationsView returns the wake verdicts the last patrol/boot
// sweep derived. A nil return declines D-DRAIN's third cancel arm, which leaves
// the drain to its other arms: the arm can only ever spare a session, so
// declining is the direction that cannot rescue one nothing wants awake.
func (cr *CityRuntime) sessionWakeEvaluationsView() map[string]wakeEvaluation {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	defer cr.sessionStartMu.Unlock()
	return cr.sessionWakeEvals
}

// publishSessionWakeEvaluations records the sweep's wake verdicts for the keyed
// D-DRAIN advance. Only the patrol/boot sweep calls it, and here the restriction
// is load-bearing rather than merely tidy: a narrowed sweep publishes verdicts
// for a subset of the fleet, and a row absent from that subset reads as "no
// reason to be awake" — the exact input that lets the drain run to its deadline
// and force-stop a session legacy would have rescued.
func (cr *CityRuntime) publishSessionWakeEvaluations(evals map[string]wakeEvaluation) {
	if cr == nil {
		return
	}
	cr.sessionStartMu.Lock()
	cr.sessionWakeEvals = evals
	cr.sessionStartMu.Unlock()
}

func (cr *CityRuntime) requestLegacySessionStartFallback() {
	if cr == nil {
		return
	}
	if cr.pokeCh != nil {
		select {
		case cr.pokeCh <- struct{}{}:
		default:
		}
		return
	}
	if cr.cs != nil {
		cr.cs.Poke()
	}
}

func (cr *CityRuntime) sessionStartLegacyExclusionOption() startExecutionOption {
	cr.sessionStartMu.Lock()
	state := cr.sessionStartOwnership
	controller := cr.sessionStartController
	cr.sessionStartMu.Unlock()

	// The detector-family yields are deliberately NOT folded into the start
	// predicate: that one answers "does keyed own this row's START family",
	// which is true for rows legacy must stay free to idle-kill or de-duplicate,
	// and false for the lifecycle-terminal rows a stale create leaves behind.
	// Each family's bridge answers its own narrow question — is an effect for
	// this exact key in flight right now — and they are installed whenever a
	// controller exists, including the bounded handoff windows where the start
	// predicate stands down, because an admitted key outlives those windows.
	// D-ORPHAN's close and drain arms carry SEPARATE predicates for the same
	// reason one predicate never covers several arms: it would make each arm's
	// legacy counterpart stand down for rows another keyed arm owns.
	var familyOptions []startExecutionOption
	if controller != nil {
		familyOptions = append(familyOptions,
			withLegacyDeadlineStopExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsDeadlineStop(info.ID)
			}),
			withLegacyOrphanCloseExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsOrphanClose(info.ID)
			}),
			withLegacyOrphanDrainExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsOrphanDrain(info.ID)
			}),
			withLegacyStaleCreateRollbackExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsStaleCreateRollback(info.ID)
			}),
			withLegacyConfigDriftConvergeExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsConfigDriftConverge(info.ID)
			}),
			withLegacyConfigDriftDeferExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsConfigDriftDefer(info.ID)
			}),
			withLegacyDuplicateRetireExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsDuplicateNamedRetire(info.ID)
			}),
			withLegacySleepDrainExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsSleepDrain(info.ID)
			}),
			withLegacyDrainAdvanceExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsDrainAdvance(info.ID)
			}),
			withLegacyProgressStallRecycleExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsProgressStallRecycle(info.ID)
			}),
			withLegacyStrandedRepairExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsStrandedRepair(info.ID)
			}),
			withLegacyZombieMarkExclusion(func(info sessionpkg.Info) bool {
				return controller.ownsZombieMark(info.ID)
			}),
		)
	}
	familyOption := combineStartExecutionOptions(familyOptions...)
	excluded := cr.sessionStartLegacyExclusionPredicate()
	if excluded == nil {
		return familyOption
	}
	startOption := combineStartExecutionOptions(withLegacyStartExclusion(excluded), familyOption)
	if state != sessionStartOwnershipKeyed {
		return startOption
	}
	snapshot, err := cr.cs.sessionStartSnapshot()
	if err != nil {
		return startOption
	}
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(snapshot.Store)
	if statusWriter == nil && statusWriterErr == nil {
		return startOption
	}
	statusOption := withLegacyStatusHealExclusion(func(info sessionpkg.Info) bool {
		return validateSessionStartAdmission(info.ID, sessionStartAdmissionInProcess) == nil &&
			(resolveExactSessionStartOwnership(info, snapshot.Config, time.Now().UTC()) ||
				(controller != nil && (controller.ownsPoolAllocationStart(info.ID, info.InstanceToken) ||
					controller.ownsConfiguredDependencyStart(info.ID) ||
					controller.ownsStrictDefaultPoolWakeStart(info.ID) ||
					controller.ownsConfiguredNamedWakeStart(info.ID))))
	})
	return combineStartExecutionOptions(startOption, statusOption)
}

// sessionStartLegacyExclusionPredicate is the single ownership predicate used
// by legacy start and drain-ack stop entry points while keyed reconciliation is
// active. Keyed reconciliation owns drain-ack finalization when the provider
// can produce a fresh observation; Auto otherwise leaves it to legacy.
func (cr *CityRuntime) sessionStartLegacyExclusionPredicate() func(sessionpkg.Info) bool {
	if cr == nil {
		return nil
	}
	cr.sessionStartMu.Lock()
	state := cr.sessionStartOwnership
	mode := cr.sessionStartMode
	controller := cr.sessionStartController
	cr.sessionStartMu.Unlock()
	if state != sessionStartOwnershipKeyed && state != sessionStartOwnershipRequiredBlocked {
		return nil
	}
	if state == sessionStartOwnershipKeyed && mode == rollout.Auto && cr.cs != nil && cr.cs.configMutationPending.Load() {
		// updateWithPendingConfigMutation only sets this marker after the
		// generation fence has drained old keyed work. New keyed snapshots fail
		// closed until the runtime loop applies the same revision, so legacy is
		// the sole available owner during this bounded handoff.
		return nil
	}
	return func(info sessionpkg.Info) bool {
		if validateSessionStartAdmission(info.ID, sessionStartAdmissionInProcess) != nil {
			return false
		}
		if cr.ownsSessionWaitDependencyStart(info.ID) {
			return true
		}
		if controller != nil && controller.ownsConfiguredDependencyStart(info.ID) {
			return true
		}
		if controller != nil && controller.ownsStrictDefaultPoolWakeStart(info.ID) {
			return true
		}
		if controller != nil && controller.ownsConfiguredNamedWakeStart(info.ID) {
			return true
		}
		if controller != nil && controller.ownsPoolDrainAckStop(info.ID, info.InstanceToken) {
			return true
		}
		snapshot, err := cr.cs.sessionStartSnapshot()
		if err != nil {
			if mode == rollout.Require {
				return !info.Closed
			}
			// Once ownership has transferred, an incoherent state generation must
			// stall its start family rather than let both writers enter.
			input := sessionpkg.LifecycleInputFromInfo(info)
			input.Now = time.Now().UTC()
			input.CreatedAt = info.CreatedAt
			input.StaleCreatingAfter = staleCreatingStateTimeout
			lifecycle := sessionpkg.ProjectLifecycle(input)
			return !info.Closed && (!lifecycle.Terminal || isDrainAckStopPendingInfo(info)) &&
				(isDrainAckStopPendingInfo(info) || lifecycle.HasWakeCause(sessionpkg.WakeCausePendingCreate) || lifecycle.HasWakeCause(sessionpkg.WakeCauseExplicit))
		}
		if mode == rollout.Require {
			name := strings.TrimSpace(info.SessionNameMetadata)
			if name != "" {
				source, sourceErr := snapshot.Provider.GetMeta(name, reconcilerDrainAckSourceKey)
				if sourceErr != nil || source == drainAckSourceAgentValue {
					return !info.Closed
				}
			}
		}
		if isDrainAckStopPendingInfo(info) {
			name := strings.TrimSpace(info.SessionNameMetadata)
			if name == "" {
				return true
			}
			source, sourceErr := snapshot.Provider.GetMeta(name, reconcilerDrainAckSourceKey)
			if sourceErr == nil && source == reconcilerDrainAckSourceValue {
				return false
			}
			// Agent, missing, or unreadable provenance is never a reason to
			// let legacy enter the destructive stop path.
			return true
		}
		if controller != nil && controller.ownsPoolAllocationStart(info.ID, info.InstanceToken) {
			return true
		}
		return resolveExactSessionStartOrDrainAckStopOwnership(info, snapshot.Config, time.Now().UTC())
	}
}

func (cr *CityRuntime) sessionStartStdout() io.Writer {
	if cr != nil && cr.stdout != nil {
		return cr.stdout
	}
	return io.Discard
}

func (cr *CityRuntime) sessionStartStderr() io.Writer {
	if cr != nil && cr.stderr != nil {
		return cr.stderr
	}
	return io.Discard
}

func (cr *CityRuntime) sessionStartLogPrefix() string {
	if cr != nil && cr.logPrefix != "" {
		return cr.logPrefix
	}
	return "gc controller"
}
