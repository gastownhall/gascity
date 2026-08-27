package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

var (
	errSessionWaitDependencyStaleCertification    = errors.New("wait dependency target is no longer certified")
	errSessionWaitDependencySnapshotUnavailable   = errors.New("wait dependency session snapshot is unavailable")
	errSessionWaitDependencyTargetReadUnavailable = errors.New("wait dependency target read is unavailable")
)

// sessionWaitDependencyStartHint is an untrusted, bounded routing hint. It
// contains no authority: the runtime loop certifies the current index and the
// durable wait/session pair before admitting a keyed start.
type sessionWaitDependencyStartHint struct {
	Target sessionWaitDependencyTarget
	Cause  sessionWaitDependencyCause
}

// buildObservedSessionWaitDependencyIndex builds a private candidate from one
// observed census without changing runtime state.
func buildObservedSessionWaitDependencyIndex(store beads.SessionStore) (observedSessionWaitCensus, *sessionWaitDependencyIndex, error) {
	census, err := observeSessionWaitCensus(store)
	if err != nil {
		return census, nil, err
	}
	candidate := newSessionWaitDependencyIndex()
	if err := candidate.Rebuild(census.waits); err != nil {
		return census, nil, fmt.Errorf("building session wait dependency index: %w", err)
	}
	return census, candidate, nil
}

// publishObservedSessionWaitDependencyIndex replaces the runtime's private
// index only while the cache observation that supplied its census is current.
func (cr *CityRuntime) publishObservedSessionWaitDependencyIndex(census observedSessionWaitCensus, candidate *sessionWaitDependencyIndex) (bool, error) {
	if candidate == nil {
		return false, fmt.Errorf("publishing session wait dependency index: candidate is nil")
	}
	return census.cache.WithCurrentObservation(census.observation, func() error {
		cr.sessionWaitDependencyMu.Lock()
		defer cr.sessionWaitDependencyMu.Unlock()
		cr.sessionWaitDependencyIndexGeneration++
		cr.sessionWaitDependencyIndex = candidate
		cr.sessionWaitDependencyRejectedCensusIDs = nil
		return nil
	})
}

func (cr *CityRuntime) installObservedSessionWaitDependencyIndex(store beads.SessionStore) (bool, error) {
	census, candidate, err := buildObservedSessionWaitDependencyIndex(store)
	if err != nil {
		if !errors.Is(err, beads.ErrCacheUnavailable) {
			retained, retainErr := cr.publishRejectedSessionWaitDependencyCensus(census)
			if retainErr != nil {
				return false, retainErr
			}
			if !retained {
				return false, nil
			}
		}
		return false, err
	}
	return cr.publishObservedSessionWaitDependencyIndex(census, candidate)
}

func (cr *CityRuntime) publishRejectedSessionWaitDependencyCensus(census observedSessionWaitCensus) (bool, error) {
	if census.cache == nil {
		return false, fmt.Errorf("publishing rejected session wait census: %w", beads.ErrCacheUnavailable)
	}
	ids := make(map[string]struct{}, len(census.waits))
	for _, wait := range census.waits {
		if wait.ID != "" {
			ids[wait.ID] = struct{}{}
		}
	}
	return census.cache.WithCurrentObservation(census.observation, func() error {
		cr.sessionWaitDependencyMu.Lock()
		defer cr.sessionWaitDependencyMu.Unlock()
		cr.sessionWaitDependencyIndexGeneration++
		cr.sessionWaitDependencyRejectedCensusIDs = ids
		cr.sessionWaitDependencyStartupCensusOwed = true
		return nil
	})
}

// startSessionWaitDependencyShadow arms inert steady-state refresh before
// requesting the initial census. Cache unavailability and stale observations
// remain pending; deterministic census errors wait for a relevant wait change.
func (cr *CityRuntime) startSessionWaitDependencyShadow() {
	cr.startSessionWaitDependencyShadowWithContext(context.Background())
}

func (cr *CityRuntime) startSessionWaitDependencyShadowWithContext(ctx context.Context) {
	cr.enableSessionWaitDependencyLifecycleShadowSink(ctx)
	cr.sessionWaitDependencyMu.Lock()
	cr.sessionWaitDependencyStartupCensusOwed = true
	cr.sessionWaitDependencyMu.Unlock()
	producerStarted := cr.startSessionWaitDependencyProducer()
	armed := false
	if cr.cs != nil {
		var producerAdmission func(sessionWaitDependencyProducerRequest)
		if producerStarted {
			producerAdmission = cr.submitSessionWaitDependencyProducerRequest
		}
		if err := cr.cs.installSessionWaitDependencyShadowAdmissionWithProducer(func() sessionWaitShadowRefreshResult {
			installed, refreshErr := cr.installObservedSessionWaitDependencyIndex(cr.sessionsBeadStore())
			switch {
			case installed:
				cr.submitSessionWaitDependencyStartupCensus()
				return sessionWaitShadowConverged
			case refreshErr == nil || errors.Is(refreshErr, beads.ErrCacheUnavailable):
				return sessionWaitShadowRetry
			default:
				fmt.Fprintf(cr.stderr, "%s: session-wait shadow refresh: %v\n", cr.logPrefix, refreshErr) //nolint:errcheck
				return sessionWaitShadowAwaitRelevant
			}
		}, cr.sessionWaitDependencyContainsWait, producerAdmission); err != nil {
			if producerStarted {
				cr.stopSessionWaitDependencyProducer()
			}
			fmt.Fprintf(cr.stderr, "%s: session-wait shadow admission: %v\n", cr.logPrefix, err) //nolint:errcheck
		} else {
			if err := cr.cs.installSessionWaitDependencyPrePokeAdmission(func(evt events.Event) {
				if evt.Type != events.BeadClosed || evt.Subject == "" {
					return
				}
				for _, target := range cr.reserveSessionWaitDependencyTargets(ctx, evt.Subject) {
					if enqueueErr := cr.enqueueSessionWaitDependencyStartHint(ctx, target, sessionWaitDependencyCauseDependency); enqueueErr != nil {
						cr.handleReservedSessionWaitDependencyEnqueueFailure(target, enqueueErr)
					}
				}
			}); err != nil {
				cr.cs.stopSessionWaitDependencyShadowAdmission()
				if producerStarted {
					cr.stopSessionWaitDependencyProducer()
				}
				fmt.Fprintf(cr.stderr, "%s: session-wait pre-poke admission: %v\n", cr.logPrefix, err) //nolint:errcheck
				return
			}
			armed = true
			cr.submitSessionWaitDependencyProducerRequests(cr.cs.requestSessionWaitDependencyShadowRefreshForBead(beads.Bead{}, true))
		}
	}
	if !armed {
		installed, err := cr.installObservedSessionWaitDependencyIndex(cr.sessionsBeadStore())
		if err != nil &&
			!errors.Is(err, beads.ErrCacheUnavailable) {
			fmt.Fprintf(cr.stderr, "%s: session-wait shadow index: %v\n", cr.logPrefix, err) //nolint:errcheck // inert best-effort shadow setup
		}
		if installed {
			cr.submitSessionWaitDependencyStartupCensus()
		}
	}
}

// reserveSessionWaitDependencyTargets synchronously certifies only the narrow
// cohort owned by this slice. The reservation is installed before the event's
// generic legacy poke, but performs no durable mutation or provider effect.
func (cr *CityRuntime) reserveSessionWaitDependencyTargets(ctx context.Context, dependencyID string) []sessionWaitDependencyTarget {
	if cr == nil || ctx == nil || ctx.Err() != nil || dependencyID == "" {
		return nil
	}
	cr.sessionStartMu.Lock()
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	mode := cr.sessionStartMode
	cr.sessionStartMu.Unlock()
	if !owned || mode != rollout.Auto && mode != rollout.Require || cr.cs == nil {
		return nil
	}
	targets := cr.sessionWaitDependencyTargetsForDependency(dependencyID)
	if len(targets) == 0 {
		// ga-zo9h3 option (b), architect-ruled 2026-08-10. The cached index is
		// built from a CACHE observation (observeSessionWaitCensus), so a wait
		// registered since the last observation is invisible here and the
		// dependency's bead.closed found targets=0 — no reservation, no
		// certificate, and legacy's poke tick then advanced the wait in legacy
		// shape. Resolving the dependents live at the EVENT is what makes the
		// certificate exist before the poke runs a tick; the patrol redrive
		// below is demoted to the anti-entropy backstop it should always have
		// been.
		targets = cr.authoritativeSessionWaitDependencyTargetsForDependency(dependencyID)
	}
	if len(targets) == 0 {
		return nil
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: reserving dependency wait for %s: %v\n", cr.sessionStartLogPrefix(), dependencyID, err) //nolint:errcheck
		return nil
	}
	defer release()
	dependencies := newAuthoritativeWaitDependencyStoreSet(cr.cityBeadStore(), cr.rigBeadStores(), cr.graphBeadStore())
	now := clock.Real{}.Now()
	reserved := make([]sessionWaitDependencyTarget, 0, len(targets))
	for _, target := range targets {
		lease, owner, certifyErr := certifySessionWaitDependencyStartLease(snapshot.Store, target, dependencies, snapshot.Config, snapshot.Provider, snapshot.CityName, snapshot.Generation, cr.poolMembershipShadow, now)
		if certifyErr != nil {
			fmt.Fprintf(cr.sessionStartStderr(), "%s: reserving dependency wait %s: %v\n", cr.sessionStartLogPrefix(), target.WaitID, certifyErr) //nolint:errcheck
			continue
		}
		if owner != exactSessionStartKeyedOwner {
			continue
		}
		cr.sessionWaitDependencyMu.Lock()
		if !cr.sessionWaitDependencyTargetCertifiedLocked(target) {
			cr.sessionWaitDependencyMu.Unlock()
			continue
		}
		if cr.sessionWaitDependencyReservations == nil {
			cr.sessionWaitDependencyReservations = make(map[string]sessionWaitDependencyStartLease)
		}
		if previous, ok := cr.sessionWaitDependencyReservations[lease.SessionID]; ok && sameDurableWaitDependencyCertificate(previous, lease) {
			lease.Operation = previous.Operation
		}
		lease.DepIDs = append([]string(nil), lease.DepIDs...)
		cr.sessionWaitDependencyReservations[lease.SessionID] = lease
		cr.sessionWaitDependencyMu.Unlock()
		reserved = append(reserved, cloneSessionWaitDependencyTarget(target))
	}
	return reserved
}

// sessionWaitDependencyLiveResolveFloor throttles the live dependent lookup on a
// city that has NO pending waits at all. With none, no bead.closed can ever
// name a waiting dependent, so repeating the lookup per close buys nothing; the
// moment one pending wait exists the lookup runs on every close again, which is
// what keeps "targets at the event, not the next patrol" true where it matters.
const sessionWaitDependencyLiveResolveFloor = 250 * time.Millisecond

// authoritativeSessionWaitDependencyTargetsForDependency resolves the closed
// bead's WAITING DEPENDENTS from the durable wait rows rather than from the
// cached index.
//
// It is bounded, not a fleet scan: one label-scoped `gc:wait` listing capped at
// SessionWaitLookupLimit — the same bound the cached census already pays — and
// it is filtered down to the exact dependency before anything is certified. A
// city with no pending waits collapses to one listing per floor window.
func (cr *CityRuntime) authoritativeSessionWaitDependencyTargetsForDependency(depID string) []sessionWaitDependencyTarget {
	if cr == nil || strings.TrimSpace(depID) == "" {
		return nil
	}
	cr.sessionWaitDependencyMu.Lock()
	if cr.waitDependencyLiveResolveEmpty && !cr.waitDependencyLiveResolveAt.IsZero() &&
		time.Since(cr.waitDependencyLiveResolveAt) < sessionWaitDependencyLiveResolveFloor {
		cr.sessionWaitDependencyMu.Unlock()
		return nil
	}
	cr.sessionWaitDependencyMu.Unlock()

	store := cr.sessionsBeadStore()
	if store.Store == nil {
		return nil
	}
	waits, err := sessionFrontDoor(store.Store).ListWaits("pending", "")
	if err != nil && len(waits) == 0 {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: resolving waiting dependents of %s: %v\n", cr.sessionStartLogPrefix(), depID, err) //nolint:errcheck // the backstop redrive still covers this dependency
		return nil
	}

	cr.sessionWaitDependencyMu.Lock()
	cr.waitDependencyLiveResolveAt = time.Now()
	cr.waitDependencyLiveResolveEmpty = len(waits) == 0
	cr.sessionWaitDependencyMu.Unlock()

	var targets []sessionWaitDependencyTarget
	for _, wait := range waits {
		if !slices.Contains(wait.DepIDs, depID) {
			continue
		}
		registration, indexable, regErr := waitDependencyRegistrationFrom(wait)
		if regErr != nil || !indexable {
			continue
		}
		targets = append(targets, sessionWaitDependencyTarget{
			WaitID:        wait.ID,
			SessionID:     registration.sessionID,
			DepIDs:        append([]string(nil), registration.depIDs...),
			DepMode:       registration.depMode,
			generation:    sessionWaitDependencyDurableGeneration,
			authoritative: true,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].WaitID < targets[j].WaitID })
	return targets
}

func sameDurableWaitDependencyCertificate(a, b sessionWaitDependencyStartLease) bool {
	return a.WaitID == b.WaitID && a.SessionID == b.SessionID && a.DepMode == b.DepMode &&
		a.RegisteredEpoch == b.RegisteredEpoch && a.WaitRevision == b.WaitRevision && a.SessionRevision == b.SessionRevision &&
		a.ControllerGeneration == b.ControllerGeneration && a.PoolTarget == b.PoolTarget &&
		a.PoolMembershipRevision == b.PoolMembershipRevision && slices.Equal(a.DepIDs, b.DepIDs)
}

func (cr *CityRuntime) ownsReservedSessionWaitDependencyStart(sessionID string) bool {
	if cr == nil || sessionID == "" {
		return false
	}
	cr.sessionWaitDependencyMu.RLock()
	_, ok := cr.sessionWaitDependencyReservations[sessionID]
	cr.sessionWaitDependencyMu.RUnlock()
	return ok
}

func (cr *CityRuntime) ownsReservedSessionWaitDependencyWait(wait sessionpkg.WaitInfo) bool {
	if cr == nil || wait.SessionID == "" {
		return false
	}
	cr.sessionWaitDependencyMu.RLock()
	lease, ok := cr.sessionWaitDependencyReservations[wait.SessionID]
	cr.sessionWaitDependencyMu.RUnlock()
	return ok && wait.ID == lease.WaitID && wait.SessionID == lease.SessionID && wait.Status == "open" &&
		wait.Kind == "deps" && wait.State == waitStatePending && wait.DepMode == lease.DepMode &&
		wait.RegisteredEpoch == lease.RegisteredEpoch && slices.Equal(wait.DepIDs, lease.DepIDs)
}

func (cr *CityRuntime) ownsSessionWaitDependencyStart(sessionID string) bool {
	if cr.ownsReservedSessionWaitDependencyStart(sessionID) {
		return true
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	cr.sessionStartMu.Unlock()
	return controller != nil && controller.ownsWaitDependencyStart(sessionID)
}

// keyedWaitAdvanceExcluded is the legacy wait-advance boundary's stand-down
// predicate: the same template the legacy start family uses at its own effect
// boundary (ga-ij8mh, sixth round), applied one family over.
//
// The wait-identity arm alone is not enough (ga-zo9h3). It certifies the exact
// durable wait the keyed lease was minted for, and legacy's own advance is what
// moves that identity — MarkWaitReady clears ready_owner and ready_operation,
// and a session may hold a wait the certificate does not name at all. The
// moment the row drifts, the arm falls open while the keyed claim on the
// SESSION is still live, so legacy advances the wait and the same tick's
// forward pass starts a row the keyed family owns the start of. Consulting
// ownsSessionWaitDependencyStart — the arm the start boundary already consults
// — asks the question that actually decides the race, on current state.
//
// The stand-down is a full supersede: the boundary applies no effect for the
// wait and contributes no start demand for its session. It is live-claim
// triggered, never candidacy triggered, so a released or retired reservation
// leaves the wait advanceable on the next pass.
func (cr *CityRuntime) keyedWaitAdvanceExcluded(wait sessionpkg.WaitInfo) bool {
	return cr.ownsSessionWaitDependencyWait(wait) || cr.ownsSessionWaitDependencyStart(wait.SessionID)
}

func (cr *CityRuntime) ownsSessionWaitDependencyWait(wait sessionpkg.WaitInfo) bool {
	if cr.ownsReservedSessionWaitDependencyWait(wait) {
		return true
	}
	cr.sessionStartMu.Lock()
	controller := cr.sessionStartController
	cr.sessionStartMu.Unlock()
	return controller != nil && controller.ownsWaitDependencyWait(wait)
}

func (cr *CityRuntime) releaseSessionWaitDependencyReservation(target sessionWaitDependencyTarget) {
	if cr == nil {
		return
	}
	cr.sessionWaitDependencyMu.Lock()
	if lease, ok := cr.sessionWaitDependencyReservations[target.SessionID]; ok &&
		lease.WaitID == target.WaitID && lease.IndexGeneration == target.generation {
		delete(cr.sessionWaitDependencyReservations, target.SessionID)
	}
	cr.sessionWaitDependencyMu.Unlock()
}

// releaseAndRetireCertifiedSessionWaitDependencyTarget returns an exact
// reservation to legacy ownership and retires its certified index target. It
// reports whether this caller retired the target that authorizes legacy work.
func (cr *CityRuntime) releaseAndRetireCertifiedSessionWaitDependencyTarget(target sessionWaitDependencyTarget) bool {
	if cr == nil {
		return false
	}
	cr.sessionWaitDependencyMu.Lock()
	defer cr.sessionWaitDependencyMu.Unlock()

	certified := cr.sessionWaitDependencyTargetCertifiedLocked(target)
	if lease, ok := cr.sessionWaitDependencyReservations[target.SessionID]; ok &&
		lease.WaitID == target.WaitID && lease.IndexGeneration == target.generation {
		delete(cr.sessionWaitDependencyReservations, target.SessionID)
	}
	if certified {
		cr.sessionWaitDependencyIndex.Remove(target.WaitID)
	}
	return certified
}

func (cr *CityRuntime) handleReservedSessionWaitDependencyEnqueueFailure(target sessionWaitDependencyTarget, err error) {
	cr.sessionStartMu.Lock()
	mode := cr.sessionStartMode
	cr.sessionStartMu.Unlock()
	if mode == rollout.Auto {
		if cr.releaseAndRetireCertifiedSessionWaitDependencyTarget(target) {
			cr.sessionWaitDependencyReadyPokePending.Store(true)
			cr.requestLegacySessionStartFallback()
		}
	}
	if err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: dependency wait %s reservation enqueue: %v\n", cr.sessionStartLogPrefix(), target.WaitID, err) //nolint:errcheck
	}
}

func (cr *CityRuntime) drainSessionWaitDependencyStartHints(ctx context.Context) {
	if cr == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	for {
		select {
		case hint := <-cr.sessionWaitDependencyStartCh:
			cr.handleSessionWaitDependencyStart(ctx, hint)
		default:
			return
		}
	}
}

func (cr *CityRuntime) redriveSessionWaitDependencyReservations(ctx context.Context) {
	if cr == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	cr.sessionWaitDependencyMu.RLock()
	targets := make([]sessionWaitDependencyTarget, 0, len(cr.sessionWaitDependencyReservations))
	for _, lease := range cr.sessionWaitDependencyReservations {
		targets = append(targets, sessionWaitDependencyTarget{
			WaitID: lease.WaitID, SessionID: lease.SessionID, DepIDs: append([]string(nil), lease.DepIDs...),
			DepMode: lease.DepMode, generation: lease.IndexGeneration,
			// A durable-resolved reservation keeps its provenance across the
			// backstop redrive: the observed index still may not carry the wait.
			authoritative: lease.IndexGeneration == sessionWaitDependencyDurableGeneration,
		})
	}
	cr.sessionWaitDependencyMu.RUnlock()
	slices.SortFunc(targets, func(a, b sessionWaitDependencyTarget) int {
		return strings.Compare(a.WaitID, b.WaitID)
	})
	for _, target := range targets {
		cr.handleSessionWaitDependencyStart(ctx, sessionWaitDependencyStartHint{
			Target: target,
			Cause:  sessionWaitDependencyCauseRegistration,
		})
	}
}

// enableSessionWaitDependencyLifecycleShadowSink connects index-certified
// dependency-ready waits to the keyed controller's untrusted hint queue.
func (cr *CityRuntime) enableSessionWaitDependencyLifecycleShadowSink(ctx context.Context) {
	if cr == nil || cr.waitDependencyEnqueue != nil || cr.cs == nil {
		return
	}
	mode := cr.cs.RolloutFlags().SessionReconciler()
	if mode != rollout.Auto && mode != rollout.Require {
		return
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		return
	}
	_, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		return
	}
	release()
	cr.waitDependencyEnqueue = func(target sessionWaitDependencyTarget, cause sessionWaitDependencyCause) (bool, error) {
		if ctx == nil || ctx.Err() != nil {
			return false, nil
		}
		cr.sessionStartMu.Lock()
		owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
		mode := cr.sessionStartMode
		cr.sessionStartMu.Unlock()
		if !owned || mode != rollout.Auto && mode != rollout.Require {
			return false, nil
		}
		started := time.Now()
		queueErr := error(nil)
		trace := cr.trace
		cr.serviceStateMu.RLock()
		cfg := cr.cfg
		cr.serviceStateMu.RUnlock()
		cycle := trace.BeginCycle(TraceTickTriggerControl, string(cause), started, cfg)
		defer func() {
			if cycle == nil {
				return
			}
			outcome := TraceOutcomeStartCandidate
			if queueErr != nil {
				outcome = TraceOutcomeFailed
			}
			cycle.RecordControllerOperation(TraceSiteWaitDependencyShadow, TraceReasonRetained, outcome, "wait_dependency_shadow", time.Since(started), map[string]any{
				"wait_outcome":   string(sessionWaitDependencyEvaluationReady),
				"start_outcome":  "queued",
				"start_reason":   "index_certified",
				"cause":          string(cause),
				"wait_id":        target.WaitID,
				"session_id":     target.SessionID,
				"effect_applied": false,
			})
			if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
				fmt.Fprintf(cr.stderr, "%s: wait dependency trace: %v\\n", cr.logPrefix, err) //nolint:errcheck
			}
		}()
		queueErr = cr.enqueueSessionWaitDependencyStartHint(ctx, target, cause)
		return false, queueErr
	}
}

// enqueueSessionWaitDependencyStartHint sends an index-certified but untrusted
// exact target to the keyed controller, whose run loop remains the sole
// certifier and effect owner.
func (cr *CityRuntime) enqueueSessionWaitDependencyStartHint(ctx context.Context, target sessionWaitDependencyTarget, cause sessionWaitDependencyCause) error {
	if cr == nil || ctx == nil || ctx.Err() != nil || cr.sessionWaitDependencyStartCh == nil {
		return nil
	}
	if err := validateSessionWaitDependencyTarget(target); err != nil {
		return err
	}
	hint := sessionWaitDependencyStartHint{Target: cloneSessionWaitDependencyTarget(target), Cause: cause}
	select {
	case cr.sessionWaitDependencyStartCh <- hint:
		return nil
	default:
		return fmt.Errorf("admitting dependency wait %q: bounded runtime hint queue is full", target.WaitID)
	}
}

// handleSessionWaitDependencyStart turns one producer hint into a certified
// lease. This must run only on CityRuntime.run: producers are intentionally
// unable to mutate, classify lifecycle ownership, or admit starts directly.
func (cr *CityRuntime) handleSessionWaitDependencyStart(ctx context.Context, hint sessionWaitDependencyStartHint) {
	if cr == nil || ctx == nil || ctx.Err() != nil || validateSessionWaitDependencyTarget(hint.Target) != nil || causePrecedence(hint.Cause) == 0 {
		return
	}
	cr.sessionStartMu.Lock()
	owned := cr.sessionStartOwnership == sessionStartOwnershipKeyed
	mode := cr.sessionStartMode
	controller := cr.sessionStartController
	cr.sessionStartMu.Unlock()
	if !owned {
		return
	}
	if !cr.sessionWaitDependencyTargetCertified(hint.Target) {
		latest, ok := cr.sessionWaitDependencyTarget(hint.Target.WaitID)
		if !ok || !sameSessionWaitDependencyTarget(latest, hint.Target) {
			cr.releaseSessionWaitDependencyReservation(hint.Target)
			return
		}
		hint.Target = latest
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, err)
		return
	}
	defer release()
	lease, owner, err := certifySessionWaitDependencyStartLease(snapshot.Store, hint.Target,
		newAuthoritativeWaitDependencyStoreSet(cr.cityBeadStore(), cr.rigBeadStores(), cr.graphBeadStore()), snapshot.Config, snapshot.Provider, snapshot.CityName, snapshot.Generation, cr.poolMembershipShadow, clock.Real{}.Now())
	if err != nil {
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, err)
		return
	}
	if owner != exactSessionStartKeyedOwner {
		cr.releaseSessionWaitDependencyReservation(hint.Target)
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, errSessionWaitDependencyStaleCertification)
		return
	}
	if controller == nil {
		cr.retainSessionWaitDependencyReservation(hint.Target, lease)
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, errors.New("exact-start controller is unavailable"))
		return
	}
	// Certification is not authority by itself: the current generation and
	// exact index target must still match at the admission effect boundary.
	if lease.IndexGeneration == 0 || lease.IndexGeneration != hint.Target.generation || !cr.sessionWaitDependencyTargetCertified(hint.Target) {
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, errors.New("dependency wait index certification changed before admission"))
		return
	}
	outcome, err := cr.transferSessionWaitDependencyReservation(hint.Target, lease, controller)
	if err != nil || outcome == sessionStartAdmissionOverflow {
		if outcome == sessionStartAdmissionOverflow && err == nil {
			err = errors.New("exact-start admission is full")
		}
		cr.handleSessionWaitDependencyAdmissionFailure(hint, mode, err)
	}
}

func (cr *CityRuntime) sessionWaitDependencyPoolWitnessCurrent(snapshot controllerSessionStartSnapshot, info sessionpkg.Info, lease sessionWaitDependencyStartLease) bool {
	if lease.PoolTarget == "" || lease.PoolMembershipRevision == 0 || cr == nil || cr.poolMembershipShadow == nil {
		return false
	}
	cr.serviceStateMu.RLock()
	configCurrent := cr.cfg == snapshot.Config
	cr.serviceStateMu.RUnlock()
	if !configCurrent || snapshot.Generation != lease.ControllerGeneration || info.ID != lease.SessionID ||
		!waitDependencyConfiguredTemplateEligible(info, snapshot.Config, snapshot.Provider, snapshot.CityName, snapshot.Store, time.Time{}) ||
		normalizedSessionTemplateInfo(info, snapshot.Config) != lease.PoolTarget {
		return false
	}
	agent := findAgentByTemplate(snapshot.Config, lease.PoolTarget)
	if agent == nil {
		return false
	}
	namedTemplates := make(map[string]struct{}, len(snapshot.Config.NamedSessions))
	for i := range snapshot.Config.NamedSessions {
		namedTemplates[snapshot.Config.NamedSessions[i].TemplateQualifiedName()] = struct{}{}
	}
	policy := newPoolAllocationShadowPolicy(snapshot.Config, agent, namedTemplates)
	// Eligibility is supported() at every pool-family site (the uniform
	// predicate contract, ga-f7v2ft.116 Q1). Narrowing by policy REASON encodes
	// one slice's scope into eligibility and silently makes the excluded arm
	// unreachable, which is exactly what kept this shipped resume from ever
	// engaging on an unlimited pool. The one genuine exclusion is the
	// canonical-singleton identity -- max==1 IS that identity
	// (config.UsesCanonicalSingletonPoolIdentity) and its rows ride other
	// families -- so it is named honestly, as the capacity clause it is: under
	// supported(), max==1 already implies reason==EligibleAgentCap
	// (poolAllocationShadowPolicy's type doc), so the reason half this used to
	// carry said nothing the cap did not. The min-floor check is a redundant
	// belt: min>0 yields reason=MinFloor, which supported() already rejects.
	if !policy.supported() || policy.maxActiveSessions == 1 ||
		agent.EffectiveMinActiveSessions() != 0 {
		return false
	}
	observation, memberIDs, exact := cr.poolMembershipShadow.observeMemberIDs(lease.PoolTarget)
	if !exact || !observation.certified || observation.revision != lease.PoolMembershipRevision ||
		observation.members != 1 || len(memberIDs) != 1 || memberIDs[0] != lease.SessionID {
		return false
	}
	if observation.occupied == 0 {
		return true
	}
	// Resuming an EXISTING member never adds a member, so its own occupancy must
	// not be counted against the pool (contract clause 2): the resume scenario by
	// construction has the member still holding its own open trigger. Occupancy
	// held by anything other than this member still refuses.
	occupancy, selfOccupied := cr.poolMembershipShadow.observeOccupiedMember(lease.PoolTarget, lease.SessionID)
	return selfOccupied && occupancy.revision == observation.revision && occupancy.occupied == 1
}

func (cr *CityRuntime) handleSessionWaitDependencyAdmissionFailure(hint sessionWaitDependencyStartHint, mode rollout.Mode, err error) {
	if mode == rollout.Auto {
		if cr.releaseAndRetireCertifiedSessionWaitDependencyTarget(hint.Target) {
			cr.sessionWaitDependencyReadyPokePending.Store(true)
			cr.requestLegacySessionStartFallback()
		}
		return
	}
	cr.sessionWaitDependencyMu.Lock()
	cr.sessionWaitDependencyStartupCensusOwed = true
	cr.sessionWaitDependencyMu.Unlock()
	if err != nil {
		fmt.Fprintf(cr.sessionStartStderr(), "%s: dependency wait %s parked: %v\n", cr.sessionStartLogPrefix(), hint.Target.WaitID, err) //nolint:errcheck
	}
}

func (cr *CityRuntime) retainSessionWaitDependencyReservation(target sessionWaitDependencyTarget, lease sessionWaitDependencyStartLease) bool {
	if cr == nil {
		return false
	}
	cr.sessionWaitDependencyMu.Lock()
	defer cr.sessionWaitDependencyMu.Unlock()
	if !cr.sessionWaitDependencyTargetCertifiedLocked(target) {
		return false
	}
	if cr.sessionWaitDependencyReservations == nil {
		cr.sessionWaitDependencyReservations = make(map[string]sessionWaitDependencyStartLease)
	}
	if previous, ok := cr.sessionWaitDependencyReservations[lease.SessionID]; ok && sameDurableWaitDependencyCertificate(previous, lease) {
		lease.Operation = previous.Operation
	}
	lease.DepIDs = append([]string(nil), lease.DepIDs...)
	cr.sessionWaitDependencyReservations[lease.SessionID] = lease
	return true
}

func (cr *CityRuntime) transferSessionWaitDependencyReservation(target sessionWaitDependencyTarget, lease sessionWaitDependencyStartLease, controller *sessionStartController) (sessionStartAdmissionOutcome, error) {
	if cr == nil || controller == nil {
		return "", errors.New("transferring dependency wait reservation: controller is unavailable")
	}
	cr.sessionWaitDependencyMu.Lock()
	defer cr.sessionWaitDependencyMu.Unlock()
	if !cr.sessionWaitDependencyTargetCertifiedLocked(target) {
		return "", errors.New("dependency wait index certification changed before admission")
	}
	if cr.sessionWaitDependencyReservations == nil {
		cr.sessionWaitDependencyReservations = make(map[string]sessionWaitDependencyStartLease)
	}
	if previous, ok := cr.sessionWaitDependencyReservations[lease.SessionID]; ok {
		if !sameDurableWaitDependencyCertificate(previous, lease) {
			return "", errors.New("dependency wait reservation changed before admission")
		}
		lease.Operation = previous.Operation
	}
	lease.DepIDs = append([]string(nil), lease.DepIDs...)
	cr.sessionWaitDependencyReservations[lease.SessionID] = lease
	outcome, err := controller.AdmitWaitDependency(lease)
	if err == nil && outcome != sessionStartAdmissionOverflow {
		delete(cr.sessionWaitDependencyReservations, lease.SessionID)
	}
	return outcome, err
}

func (cr *CityRuntime) sessionWaitDependencyTargetCertified(target sessionWaitDependencyTarget) bool {
	cr.sessionWaitDependencyMu.RLock()
	defer cr.sessionWaitDependencyMu.RUnlock()
	return cr.sessionWaitDependencyTargetCertifiedLocked(target) && target.generation > 0
}

func (cr *CityRuntime) submitSessionWaitDependencyStartupCensus() {
	cr.sessionWaitDependencyMu.Lock()
	if !cr.sessionWaitDependencyStartupCensusOwed ||
		cr.sessionWaitDependencyIndex == nil ||
		cr.sessionWaitDependencyRejectedCensusIDs != nil {
		cr.sessionWaitDependencyMu.Unlock()
		return
	}
	targets := cr.sessionWaitDependencyIndex.AllTargets()
	for n := range targets {
		targets[n].generation = cr.sessionWaitDependencyIndexGeneration
	}
	cr.sessionWaitDependencyStartupCensusOwed = false
	cr.sessionWaitDependencyMu.Unlock()
	cr.submitSessionWaitDependencyTargets(targets, sessionWaitDependencyCauseRegistration)
}

func (cr *CityRuntime) startSessionWaitDependencyProducer() bool {
	if cr == nil || cr.waitDependencyEnqueue == nil || cr.waitDependencyProducer != nil {
		return false
	}
	producer, err := newSessionWaitDependencyProducer(sessionWaitDependencyProducerOptions{
		MaxDistinct: sessionpkg.SessionWaitLookupLimit,
		TargetForWait: func(waitID string) (sessionWaitDependencyTarget, bool) {
			return cr.sessionWaitDependencyTarget(waitID)
		},
		Dependencies: func() waitDependencyReader {
			return newWaitDependencyStoreSet(cr.cityBeadStore(), cr.rigBeadStores(), cr.graphBeadStore())
		},
		EnqueueSession: func(plan sessionWaitDependencyPlan, cause sessionWaitDependencyCause) error {
			cr.sessionWaitDependencyMu.RLock()
			if !cr.sessionWaitDependencyTargetCertifiedLocked(plan.Target) {
				cr.sessionWaitDependencyMu.RUnlock()
				return fmt.Errorf("%w: %s", errSessionWaitDependencyStaleCertification, plan.Target.WaitID)
			}
			enqueue := cr.waitDependencyEnqueue
			retire, err := enqueue(plan.Target, cause)
			cr.sessionWaitDependencyMu.RUnlock()
			if errors.Is(err, errSessionWaitDependencySnapshotUnavailable) ||
				errors.Is(err, errSessionWaitDependencyTargetReadUnavailable) {
				cr.sessionWaitDependencyMu.Lock()
				cr.sessionWaitDependencyStartupCensusOwed = true
				cr.sessionWaitDependencyMu.Unlock()
				if errors.Is(err, errSessionWaitDependencySnapshotUnavailable) {
					cr.requestLegacySessionStartFallback()
					return nil
				}
				return err
			}
			if err != nil {
				cr.sessionStartMu.Lock()
				mode := cr.sessionStartMode
				cr.sessionStartMu.Unlock()
				cr.handleSessionWaitDependencyAdmissionFailure(sessionWaitDependencyStartHint{Target: plan.Target, Cause: cause}, mode, err)
				return err
			}
			if retire {
				cr.retireCertifiedSessionWaitDependencyTarget(plan.Target)
			}
			return nil
		},
		ReportError: func(err error) {
			fmt.Fprintf(cr.stderr, "%s: wait dependency producer: %v\n", cr.logPrefix, err) //nolint:errcheck
		},
	})
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: wait dependency producer disabled: %v\n", cr.logPrefix, err) //nolint:errcheck
		return false
	}
	if err := producer.Start(); err != nil {
		fmt.Fprintf(cr.stderr, "%s: wait dependency producer disabled: %v\n", cr.logPrefix, err) //nolint:errcheck
		return false
	}
	cr.sessionWaitDependencyMu.Lock()
	cr.waitDependencyProducer = producer
	cr.sessionWaitDependencyMu.Unlock()
	return true
}

func (cr *CityRuntime) retireCertifiedSessionWaitDependencyTarget(target sessionWaitDependencyTarget) {
	cr.sessionWaitDependencyMu.Lock()
	defer cr.sessionWaitDependencyMu.Unlock()
	if cr.sessionWaitDependencyTargetCertifiedLocked(target) {
		cr.sessionWaitDependencyIndex.Remove(target.WaitID)
	}
}

// newAuthoritativeWaitDependencyStoreSet is newWaitDependencyStoreSet read
// through the authoritative live handle. graphStore carries #5488's split-city
// leg: a graph-resident dependency is invisible to a city+rig-only set. A zero
// GraphStore is the single-store default and is skipped by the constructor.
func newAuthoritativeWaitDependencyStoreSet(cityStore beads.Store, rigStores map[string]beads.Store, graphStore beads.GraphStore) waitDependencyStoreSet {
	stores := newWaitDependencyStoreSet(cityStore, rigStores, graphStore)
	for n, store := range stores {
		stores[n] = authoritativeSessionStartReadStore{Store: store, live: beads.HandlesFor(store).Live}
	}
	return stores
}

func (cr *CityRuntime) stopSessionWaitDependencyProducer() {
	if cr == nil {
		return
	}
	cr.sessionWaitDependencyMu.Lock()
	producer := cr.waitDependencyProducer
	cr.waitDependencyProducer = nil
	cr.sessionWaitDependencyMu.Unlock()
	if producer != nil {
		producer.Stop()
	}
}

func (cr *CityRuntime) sessionWaitDependencyTarget(waitID string) (sessionWaitDependencyTarget, bool) {
	cr.sessionWaitDependencyMu.RLock()
	index := cr.sessionWaitDependencyIndex
	generation := cr.sessionWaitDependencyIndexGeneration
	rejected := cr.sessionWaitDependencyRejectedCensusIDs != nil
	cr.sessionWaitDependencyMu.RUnlock()
	if rejected || index == nil {
		return sessionWaitDependencyTarget{}, false
	}
	target, ok := index.TargetForWait(waitID)
	target.generation = generation
	return target, ok
}

func (cr *CityRuntime) sessionWaitDependencyAllTargets() []sessionWaitDependencyTarget {
	cr.sessionWaitDependencyMu.RLock()
	index, generation := cr.sessionWaitDependencyIndex, cr.sessionWaitDependencyIndexGeneration
	rejected := cr.sessionWaitDependencyRejectedCensusIDs != nil
	cr.sessionWaitDependencyMu.RUnlock()
	if index == nil || rejected {
		return nil
	}
	targets := index.AllTargets()
	for n := range targets {
		targets[n].generation = generation
	}
	return targets
}

func (cr *CityRuntime) sessionWaitDependencyTargetsForDependency(depID string) []sessionWaitDependencyTarget {
	cr.sessionWaitDependencyMu.RLock()
	index, generation := cr.sessionWaitDependencyIndex, cr.sessionWaitDependencyIndexGeneration
	rejected := cr.sessionWaitDependencyRejectedCensusIDs != nil
	cr.sessionWaitDependencyMu.RUnlock()
	if index == nil || rejected {
		return nil
	}
	targets := index.TargetsForDependency(depID)
	for n := range targets {
		targets[n].generation = generation
	}
	return targets
}

func (cr *CityRuntime) submitSessionWaitDependencyTargets(targets []sessionWaitDependencyTarget, cause sessionWaitDependencyCause) {
	cr.sessionWaitDependencyMu.RLock()
	producer := cr.waitDependencyProducer
	cr.sessionWaitDependencyMu.RUnlock()
	if producer == nil {
		return
	}
	for _, target := range targets {
		if err := producer.Admit(target, cause); err != nil {
			fmt.Fprintf(cr.stderr, "%s: wait dependency producer admission: %v\n", cr.logPrefix, err) //nolint:errcheck
		}
	}
}

func (cr *CityRuntime) submitSessionWaitDependencyProducerRequest(request sessionWaitDependencyProducerRequest) {
	type targetAdmission struct {
		target sessionWaitDependencyTarget
		cause  sessionWaitDependencyCause
	}
	byWaitID := make(map[string]targetAdmission)
	add := func(targets []sessionWaitDependencyTarget, cause sessionWaitDependencyCause) {
		for _, target := range targets {
			current, exists := byWaitID[target.WaitID]
			if !exists || target.generation > current.target.generation {
				byWaitID[target.WaitID] = targetAdmission{target: target, cause: cause}
				continue
			}
			if target.generation == current.target.generation {
				current.cause = mergeSessionWaitDependencyCause(current.cause, cause)
				byWaitID[target.WaitID] = current
			}
		}
	}
	if request.fullCensus {
		add(cr.sessionWaitDependencyAllTargets(), sessionWaitDependencyCauseRegistration)
	}
	if request.waitHint {
		if target, ok := cr.sessionWaitDependencyTarget(request.beadID); ok {
			add([]sessionWaitDependencyTarget{target}, sessionWaitDependencyCauseWaitCommit)
		}
	}
	if request.beadID != "" {
		add(cr.sessionWaitDependencyTargetsForDependency(request.beadID), sessionWaitDependencyCauseDependency)
	}
	waitIDs := make([]string, 0, len(byWaitID))
	for waitID := range byWaitID {
		waitIDs = append(waitIDs, waitID)
	}
	slices.Sort(waitIDs)
	for _, waitID := range waitIDs {
		admission := byWaitID[waitID]
		cr.submitSessionWaitDependencyTargets([]sessionWaitDependencyTarget{admission.target}, admission.cause)
	}
}

func (cr *CityRuntime) submitSessionWaitDependencyProducerRequests(requests []sessionWaitDependencyProducerRequest) {
	for _, request := range requests {
		cr.submitSessionWaitDependencyProducerRequest(request)
	}
}

func (cr *CityRuntime) sessionWaitDependencyTargetCertifiedLocked(target sessionWaitDependencyTarget) bool {
	if target.authoritative {
		// A live-resolved target carries its own authority: the durable wait row
		// was read live at the event, and certifySessionWaitDependencyStartLease
		// re-read BOTH durable rows before this point. Asking the observed index
		// to certify it would re-impose the staleness this path exists to route
		// around.
		return true
	}
	index, generation := cr.sessionWaitDependencyIndex, cr.sessionWaitDependencyIndexGeneration
	rejected := cr.sessionWaitDependencyRejectedCensusIDs != nil
	if rejected || index == nil || generation != target.generation {
		return false
	}
	current, ok := index.TargetForWait(target.WaitID)
	return ok && current.SessionID == target.SessionID && current.DepMode == target.DepMode && slices.Equal(current.DepIDs, target.DepIDs)
}

func (cr *CityRuntime) sessionWaitDependencyContainsWait(id string) bool {
	cr.sessionWaitDependencyMu.RLock()
	index := cr.sessionWaitDependencyIndex
	_, rejected := cr.sessionWaitDependencyRejectedCensusIDs[id]
	cr.sessionWaitDependencyMu.RUnlock()
	return rejected || index != nil && index.containsWait(id)
}

// sessionWaitDependencySessions returns detached session IDs from the current
// private shadow index, if one was installed.
func (cr *CityRuntime) sessionWaitDependencySessions(depID string) []string {
	cr.sessionWaitDependencyMu.RLock()
	index := cr.sessionWaitDependencyIndex
	rejected := cr.sessionWaitDependencyRejectedCensusIDs != nil
	cr.sessionWaitDependencyMu.RUnlock()
	if index == nil || rejected {
		return nil
	}
	return index.SessionsForDependency(depID)
}
