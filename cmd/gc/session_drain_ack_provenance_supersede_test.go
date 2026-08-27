package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	session "github.com/gastownhall/gascity/internal/session"
)

// The drain-ack provenance treadmill (ga-f7v2ft.161, win3 post-6b): a
// stop-pending row carrying neither durable agent stamps nor a readable legacy
// runtime marker was refused by BOTH owners — keyed recovery parked on
// "provenance is not a confirmed legacy marker" while the legacy exclusion
// predicate barred legacy from the same row — so the obligation retried at
// reconcile cadence forever (~108/min fleet-wide on a bounded stale set).
//
// The adjudication (coexistence doctrine): the provenance marker is a MEANS of
// re-validation, not an end. A lease with no recognizable provenance is
// re-validated against CURRENT authoritative state under the per-key lock —
// a fresh, COMPLETE observation proving the runtime dead leaves no destructive
// stop to protect, so the keyed owner supersedes the lease and finalizes the
// row, stamping the recovery keyed (drain_ack_source=keyed-superseded). A live
// or unprovable runtime keeps the protection: refused, zero STOP effects.
// Marker-absence is never treated as a marker (no grandfathering).

// TestRecoverRoutedWorkPoolDrainAckLeaseUnrecognizedProvenanceIsNotAnError pins
// the recovery contract for the storm's exact shape: the provenance witness is
// readable and shows NO recognizable marker (GetMeta returns "" for a live
// runtime). That is evidence of absence, not a read failure — the caller owes
// it a liveness re-validation, not an infinite park.
func TestRecoverRoutedWorkPoolDrainAckLeaseUnrecognizedProvenanceIsNotAnError(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := clearReconcilerDrainAckMetadata(fixture.provider, fixture.info.SessionName); err != nil {
		t.Fatalf("clear runtime drain-ack metadata: %v", err)
	}

	_, agentAck, legacyMarker, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || agentAck || legacyMarker {
		t.Fatalf("unrecognized provenance recovery = (agent=%t, legacy=%t, %v), want (false, false, nil): a readable witness with no marker is evidence, not an error", agentAck, legacyMarker, err)
	}
}

// TestRecoverRoutedWorkPoolDrainAckLeaseAbsentRuntimeIsUnrecognized covers the
// sibling texture: the runtime itself is gone, so its meta read fails AND
// IsRunning is false. No marker can exist on an absent runtime; absence of the
// witness is still not treated as a marker — the caller re-validates against a
// fresh complete liveness observation before any effect.
func TestRecoverRoutedWorkPoolDrainAckLeaseAbsentRuntimeIsUnrecognized(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := fixture.provider.Stop(fixture.info.SessionName); err != nil {
		t.Fatalf("stop fixture runtime: %v", err)
	}
	snapshot := fixture.snapshot
	snapshot.Provider = poolDrainAckGetMetaErrorProvider{
		Provider: fixture.provider,
		err:      errors.New("session is gone"),
	}

	_, agentAck, legacyMarker, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(snapshot, fixture.info)
	if err != nil || agentAck || legacyMarker {
		t.Fatalf("absent-runtime recovery = (agent=%t, legacy=%t, %v), want (false, false, nil): no marker can exist on an absent runtime", agentAck, legacyMarker, err)
	}
}

// TestRecoverRoutedWorkPoolDrainAckLeaseStillConfirmsLegacyMarker is the
// control for the legacy arm: a READABLE reconciler marker still yields to
// legacy, exactly as before. The supersede arm must not widen into it.
func TestRecoverRoutedWorkPoolDrainAckLeaseStillConfirmsLegacyMarker(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := clearReconcilerDrainAckMetadata(fixture.provider, fixture.info.SessionName); err != nil {
		t.Fatalf("clear runtime drain-ack metadata: %v", err)
	}
	if err := fixture.provider.SetMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue); err != nil {
		t.Fatalf("stamp legacy marker: %v", err)
	}

	_, agentAck, legacyMarker, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || agentAck || !legacyMarker {
		t.Fatalf("legacy-marker recovery = (agent=%t, legacy=%t, %v), want a confirmed legacy marker", agentAck, legacyMarker, err)
	}
}

// TestRecoverRoutedWorkPoolDrainAckLeaseStillRefusesUnreadableWitnessOnLiveRuntime
// is the control for the error arm: a LIVE runtime whose witness read fails is
// a transient infrastructure failure. It must remain an error (park + retry),
// never unrecognized — superseding on an unreadable witness would infer absence
// from a failed read.
func TestRecoverRoutedWorkPoolDrainAckLeaseStillRefusesUnreadableWitnessOnLiveRuntime(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	snapshot := fixture.snapshot
	snapshot.Provider = poolDrainAckGetMetaErrorProvider{
		Provider: fixture.provider,
		err:      errors.New("meta read timed out"),
	}

	_, agentAck, legacyMarker, err := fixture.cr.recoverRoutedWorkPoolDrainAckLease(snapshot, fixture.info)
	if err == nil || agentAck || legacyMarker {
		t.Fatalf("unreadable-witness recovery = (agent=%t, legacy=%t, %v), want an error: a failed read on a live runtime proves nothing", agentAck, legacyMarker, err)
	}
}

// TestReconcileExactSessionStartSupersedesUnrecognizedProvenanceOnDeadRuntime
// is the storm's resolution shape: a stop-pending row with no provenance
// anywhere and a runtime proven dead by a fresh COMPLETE observation MUST
// resolve — the keyed owner supersedes the unprovable lease, finalizes the row
// through the fenced terminal close, and stamps the recovery keyed.
func TestReconcileExactSessionStartSupersedesUnrecognizedProvenanceOnDeadRuntime(t *testing.T) {
	env := newDrainAckAtomicCloseTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	// The production storm set is pool/wisp rows; pool-managed is what arms the
	// fenced terminal close instead of the drained compat state.
	env.setSessionMetadata(&bead, map[string]string{"pool_managed": "true"})
	markDrainAckStopPendingForTest(env, &bead)

	store := &drainAckAtomicCloseStore{Store: env.store}
	params := exactSessionStartTestParams(t, env)
	params.Store = store
	params.Provider = &freshLivenessProvider{Fake: env.sp, fresh: runtime.Liveness{Complete: true}}
	params.RolloutMode = rollout.Auto
	params.RecoverPoolDrainAck = func(session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
		return routedWorkPoolDrainAckLease{}, false, false, nil
	}

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID:             bead.ID,
		Source:                sessionStartAdmissionAntiEntropy,
		PoolDrainAckUncertain: true,
	}, params)
	if owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("dead-runtime unrecognized provenance = (%v, %v), want a keyed supersede with no error", owner, err)
	}

	after, readErr := env.store.Get(bead.ID)
	if readErr != nil {
		t.Fatalf("read superseded row: %v", readErr)
	}
	if after.Status != "closed" || after.Metadata["state"] != "drained" ||
		after.Metadata["close_reason"] != session.CanonicalCloseReason("drained") || after.Metadata["closed_at"] == "" {
		t.Fatalf("superseded terminal row = %#v, want closed/drained canonical metadata", after)
	}
	// The recovery is stamped keyed going forward: a named supersede source,
	// never a forged agent acknowledgement.
	if got := after.Metadata[session.DrainAckSourceMetadataKey]; got != session.DrainAckSourceSupersededValue {
		t.Fatalf("superseded row drain_ack_source = %q, want %q", got, session.DrainAckSourceSupersededValue)
	}
	if got := after.Metadata[session.DrainAckRequesterSessionIDMetadataKey]; got != "" {
		t.Fatalf("superseded row forged requester provenance %q, want none", got)
	}
	if got := len(store.expected); got != 1 {
		t.Fatalf("atomic terminal close calls = %d, want exactly one fenced close", got)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0: a dead runtime leaves nothing to stop", got)
	}
}

// TestReconcileExactSessionStartDrainAckFinalizesWhenObservationCompletes is
// the ga-lp5w6 self-recovery contract: a drain-ack stop parked on an
// INCOMPLETE liveness observation is a retained obligation, and the very next
// re-examination that sees a complete dead observation must finalize the row
// and free its pool seat — no manual close. This is the fleet-stall sequence:
// unreadable stranger processes made every host-wide sweep incomplete, the
// obligation escalated and parked on the 5m cadence, and once the sweep's
// completeness domain is the session's reachable scope the same re-examination
// self-heals.
func TestReconcileExactSessionStartDrainAckFinalizesWhenObservationCompletes(t *testing.T) {
	env := newDrainAckAtomicCloseTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{"pool_managed": "true"})
	markDrainAckStopPendingForTest(env, &bead)

	store := &drainAckAtomicCloseStore{Store: env.store}
	params := exactSessionStartTestParams(t, env)
	params.Store = store
	provider := &freshLivenessProvider{Fake: env.sp, fresh: runtime.Liveness{Complete: false}}
	params.Provider = provider
	params.RolloutMode = rollout.Auto
	params.RecoverPoolDrainAck = func(session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
		return routedWorkPoolDrainAckLease{}, false, false, nil
	}
	admission := sessionStartAdmission{
		SessionID:             bead.ID,
		Source:                sessionStartAdmissionAntiEntropy,
		PoolDrainAckUncertain: true,
	}

	// The stall: an incomplete observation parks the obligation with zero
	// effects. The row stays the durable stop-pending obligation.
	owner, err := reconcileExactSessionStartWithOwner(context.Background(), admission, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("incomplete observation = (%v, %v), want a keyed park on the pending sentinel", owner, err)
	}
	if err == nil || !strings.Contains(err.Error(), "drain acknowledgement liveness observation is incomplete") {
		t.Fatalf("incomplete-observation park cause = %v, want it to name the incomplete observation", err)
	}
	parked, readErr := env.store.Get(bead.ID)
	if readErr != nil {
		t.Fatalf("read parked row: %v", readErr)
	}
	if parked.Status != "open" {
		t.Fatalf("parked row status = %q, want the untouched open obligation", parked.Status)
	}
	if got := len(store.expected); got != 0 {
		t.Fatalf("atomic terminal close calls while parked = %d, want 0", got)
	}

	// The recovery: the observation regains completeness (the sweep can now
	// prove absence within the session's scope), and the SAME retained
	// obligation finalizes in one pass.
	provider.mu.Lock()
	provider.fresh = runtime.Liveness{Complete: true}
	provider.mu.Unlock()

	owner, err = reconcileExactSessionStartWithOwner(context.Background(), admission, params)
	if owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("complete re-examination = (%v, %v), want the finalizing pass to succeed", owner, err)
	}
	after, readErr := env.store.Get(bead.ID)
	if readErr != nil {
		t.Fatalf("read finalized row: %v", readErr)
	}
	if after.Status != "closed" || after.Metadata["state"] != "drained" {
		t.Fatalf("finalized row = status %q state %q, want closed/drained", after.Status, after.Metadata["state"])
	}
	if got := after.Metadata[session.DrainAckSourceMetadataKey]; got != session.DrainAckSourceSupersededValue {
		t.Fatalf("finalized row drain_ack_source = %q, want %q", got, session.DrainAckSourceSupersededValue)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0 across park and recovery", got)
	}
}

// TestReconcileExactSessionStartRefusesUnrecognizedProvenanceOnLiveRuntime is
// THE control: the case the provenance check exists for. A LIVE runtime whose
// acknowledgement cannot be proven must never be stopped or closed on absent
// evidence — the row stays parked with zero effects, under a cause that names
// the live-runtime refusal.
func TestReconcileExactSessionStartRefusesUnrecognizedProvenanceOnLiveRuntime(t *testing.T) {
	env := newDrainAckAtomicCloseTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&bead, map[string]string{"pool_managed": "true"})
	markDrainAckStopPendingForTest(env, &bead)

	store := &drainAckAtomicCloseStore{Store: env.store}
	params := exactSessionStartTestParams(t, env)
	params.Store = store
	params.Provider = &freshLivenessProvider{Fake: env.sp, fresh: runtime.Liveness{Running: true, Alive: true, Complete: true}}
	params.RolloutMode = rollout.Auto
	params.RecoverPoolDrainAck = func(session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
		return routedWorkPoolDrainAckLease{}, false, false, nil
	}

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID:             bead.ID,
		Source:                sessionStartAdmissionAntiEntropy,
		PoolDrainAckUncertain: true,
	}, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("live-runtime unrecognized provenance = (%v, %v), want a keyed park on the pending sentinel", owner, err)
	}
	if err == nil || !strings.Contains(err.Error(), "live runtime holds no recognizable drain acknowledgement provenance") {
		t.Fatalf("live-runtime park cause = %v, want it to name the live-runtime refusal", err)
	}

	after, readErr := env.store.Get(bead.ID)
	if readErr != nil {
		t.Fatalf("read refused row: %v", readErr)
	}
	info, infoErr := sessionFrontDoor(env.store).Get(bead.ID)
	if infoErr != nil {
		t.Fatalf("project refused row: %v", infoErr)
	}
	if after.Status != "open" || !isDrainAckStopPendingInfo(info) {
		t.Fatalf("refused row = status %q, stop-pending %t; want the durable obligation untouched", after.Status, isDrainAckStopPendingInfo(info))
	}
	if got := after.Metadata[session.DrainAckSourceMetadataKey]; got != "" {
		t.Fatalf("refused row drain_ack_source = %q, want no provenance invented", got)
	}
	if got := len(store.expected); got != 0 {
		t.Fatalf("atomic terminal close calls = %d, want 0 for a live holder", got)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0: never stop a live runtime on absent evidence", got)
	}
}
