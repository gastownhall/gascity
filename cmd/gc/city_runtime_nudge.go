package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/rollout"
)

const nudgeKeyControllerMaxRetries = 5

var (
	newCityNudgeKeyController = newNudgeKeyController
	loadKeyedNudgeQueueState  = nudgequeue.LoadState
)

// ensureNudgeKeyController enables exact-session nudge scheduling only when
// the existing session-reconciler rollout is enabled. The legacy dispatcher
// remains the byte-for-byte default when the rollout is off.
func (cr *CityRuntime) ensureNudgeKeyController(ctx context.Context) error {
	if cr == nil || !nudgeDispatcherIsSupervisor(cr.cfg) {
		return nil
	}
	cr.nudgeKeyMu.Lock()
	defer cr.nudgeKeyMu.Unlock()
	if cr.nudgeKeyController != nil {
		return nil
	}
	mode := rollout.ModeUnset
	if cr.cs != nil {
		mode = cr.cs.RolloutFlags().SessionReconciler()
	}
	var activationSnapshot controllerSessionStartSnapshot
	decision, reason := rollout.ResolveCapability(ctx, mode, func(context.Context) (bool, string) {
		if cr.cs == nil {
			return false, "controller state is unavailable"
		}
		snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
		if release != nil {
			release()
		}
		if err != nil {
			return false, err.Error()
		}
		if snapshot.NudgeStore == nil {
			return false, "coherent nudge store is unavailable"
		}
		activationSnapshot = snapshot
		return true, "coherent runtime stores are available"
	})
	switch decision {
	case rollout.UseLegacy:
		return nil
	case rollout.DegradeLoud:
		fmt.Fprintf(cr.stderr, "%s: keyed nudge controller unavailable (%s); using legacy dispatcher\n", cr.logPrefix, reason) //nolint:errcheck
		return nil
	case rollout.RefuseClosed:
		return fmt.Errorf("required keyed nudge controller is unavailable: %s", reason)
	case rollout.UseNew:
	default:
		return fmt.Errorf("unexpected nudge rollout decision %q", decision)
	}

	controller, err := newCityNudgeKeyController(nudgeKeyControllerOptions{
		Workers:     maxParallelStartsPerTick(activationSnapshot.Config),
		MaxDistinct: nudgeKeyControllerMaxDistinct,
		MaxRetries:  nudgeKeyControllerMaxRetries,
		Reconcile: func(reconcileCtx context.Context, sessionID string) error {
			snapshot, release, acquireErr := cr.cs.acquireSessionStartSnapshot()
			if acquireErr != nil {
				return acquireErr
			}
			defer release()
			return reconcileExactQueuedNudge(reconcileCtx, sessionID, exactQueuedNudgeParams{
				CityPath: snapshot.CityPath, Config: snapshot.Config, Provider: snapshot.Provider,
				SessionStore: snapshot.Store, NudgeStore: snapshot.NudgeStore,
			})
		},
		Observer: func(result sessionStartReconcileResult) {
			if result.Outcome != sessionStartReconcileExhausted {
				return
			}
			cr.requestNudgeKeyAudit()
			if mode == rollout.Auto {
				cr.markNudgeKeyLegacyFallback(result.Admission.SessionID)
			}
			fmt.Fprintf(cr.stderr, "%s: keyed nudge reconciliation exhausted for %s: %v; authoritative audit requested\n", cr.logPrefix, result.Admission.SessionID, result.Err) //nolint:errcheck
		},
	})
	if err != nil {
		return cr.nudgeKeyActivationFailure(mode, fmt.Errorf("creating child: %w", err))
	}
	cr.nudgeKeyController = controller
	cr.nudgeKeyMode = mode
	cr.nudgeKeyFallback = make(map[string]struct{})
	if err := controller.Start(ctx); err != nil {
		cr.nudgeKeyController = nil
		controller.Stop()
		return cr.nudgeKeyActivationFailure(mode, fmt.Errorf("starting child: %w", err))
	}
	return nil
}

func (cr *CityRuntime) nudgeKeyActivationFailure(mode rollout.Mode, err error) error {
	if mode == rollout.Auto {
		fmt.Fprintf(cr.stderr, "%s: keyed nudge controller unavailable (%v); using legacy dispatcher\n", cr.logPrefix, err) //nolint:errcheck
		return nil
	}
	return fmt.Errorf("required keyed nudge controller is unavailable: %w", err)
}

func (cr *CityRuntime) stopNudgeKeyController() {
	if cr == nil {
		return
	}
	cr.nudgeKeyMu.Lock()
	controller := cr.nudgeKeyController
	cr.nudgeKeyController = nil
	cr.nudgeKeyMu.Unlock()
	if controller != nil {
		controller.Stop()
	}
}

func (cr *CityRuntime) markNudgeKeyLegacyFallback(id string) {
	if cr == nil || id == "" {
		return
	}
	cr.nudgeKeyMu.Lock()
	if cr.nudgeKeyFallback == nil {
		cr.nudgeKeyFallback = make(map[string]struct{})
	}
	cr.nudgeKeyFallback[id] = struct{}{}
	cr.nudgeKeyMu.Unlock()
	select {
	case cr.nudgeWakeCh <- struct{}{}:
	default:
	}
}

func (cr *CityRuntime) admitDueExactNudges(now time.Time) (map[string]struct{}, bool, nudgequeue.State, error) {
	if cr == nil {
		return nil, true, nudgequeue.State{}, nil
	}
	cr.nudgeKeyMu.Lock()
	controller := cr.nudgeKeyController
	mode := cr.nudgeKeyMode
	cr.nudgeKeyMu.Unlock()
	if controller == nil {
		return nil, true, nudgequeue.State{}, nil
	}
	// Every wake and patrol reads queue authority below, so it is also the
	// authoritative audit requested by overflow or exhausted retries.
	controller.TakeAuditRequest()
	state, err := loadKeyedNudgeQueueState(cr.cityPath)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: keyed nudge queue load: %v\n", cr.logPrefix, err) //nolint:errcheck
		return nil, false, nudgequeue.State{}, fmt.Errorf("loading keyed nudge queue: %w", err)
	}
	excluded := make(map[string]struct{})
	legacyNeeded := false
	dueExact := make(map[string]struct{})
	for _, id := range discoverDueExactNudgeSessionIDs(state, now) {
		dueExact[id] = struct{}{}
	}
	for _, item := range state.Pending {
		due := item.DeliverAfter.IsZero() || !item.DeliverAfter.After(now)
		if due {
			if _, exact := dueExact[item.SessionID]; !exact {
				legacyNeeded = true
			}
		}
	}
	for _, item := range state.InFlight {
		due := !item.LeaseUntil.IsZero() && item.LeaseUntil.Before(now)
		if due {
			if _, exact := dueExact[item.SessionID]; !exact {
				legacyNeeded = true
			}
		}
	}
	for _, id := range discoverDueExactNudgeSessionIDs(state, now) {
		cr.nudgeKeyMu.Lock()
		_, fallback := cr.nudgeKeyFallback[id]
		if fallback {
			delete(cr.nudgeKeyFallback, id)
		}
		cr.nudgeKeyMu.Unlock()
		if fallback {
			legacyNeeded = true
			continue
		}
		outcome, err := controller.Admit(id)
		if err != nil {
			controller.RequestAudit()
			if mode == rollout.Require {
				excluded[id] = struct{}{}
			} else {
				legacyNeeded = true
			}
			continue
		}
		switch outcome {
		case sessionStartAdmissionAccepted, sessionStartAdmissionCoalesced:
			excluded[id] = struct{}{}
		case sessionStartAdmissionOverflow:
			controller.RequestAudit()
			if mode == rollout.Require {
				excluded[id] = struct{}{}
			} else {
				legacyNeeded = true
			}
		}
	}
	return excluded, legacyNeeded, state, nil
}

func (cr *CityRuntime) requestNudgeKeyAudit() {
	if cr == nil {
		return
	}
	cr.nudgeKeyMu.Lock()
	controller := cr.nudgeKeyController
	cr.nudgeKeyMu.Unlock()
	if controller != nil {
		controller.RequestAudit()
	}
}

func (cr *CityRuntime) nudgeKeyControllerActive() bool {
	if cr == nil {
		return false
	}
	cr.nudgeKeyMu.Lock()
	defer cr.nudgeKeyMu.Unlock()
	return cr.nudgeKeyController != nil
}

func (cr *CityRuntime) signalNudgeKeyWake() {
	if !cr.nudgeKeyControllerActive() {
		return
	}
	select {
	case cr.nudgeWakeCh <- struct{}{}:
	default:
	}
}
