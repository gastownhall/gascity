package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	session "github.com/gastownhall/gascity/internal/session"
)

// The keyed drain-ack fence used to be gated on keyed pool membership and on
// re-reading the acknowledgement out of the runtime after the stop-pending
// transition had already committed it. Both gates refuse exactly the shape the
// v59 journey's routed_work_drain_finalize leg exercises — a LEGACY-created
// member whose agent has acknowledged and is on its way out — so the keyed
// family handed the drain back, legacy applied the stop-pending mark, and the
// row-scoped purity assertion caught it as a coexistence violation.
//
// These are the unit-level shapes of that journey red (council R1). The
// assertion itself stays byte-identical; the fence is what moved.

// TestAuthorizeRoutedWorkPoolDrainAckHoldsLegacyCreatedMember proves the drain
// fence is the acknowledgement stamps plus the row binding, not the allocation
// lineage: a member the keyed pool index never held is still held end-to-end.
func TestAuthorizeRoutedWorkPoolDrainAckHoldsLegacyCreatedMember(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	fixture.cr.poolMembershipShadow.remove(fixture.info.ID)

	lease, agentAck, refusal, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, fixture.info)
	if err != nil || !agentAck || refusal != drainAckRefusalNone {
		t.Fatalf("legacy-created member lease = (%t, %q, %v), want an admitted agent acknowledgement", agentAck, refusal, err)
	}
	if lease.MembershipOccupied {
		t.Fatalf("legacy-created member lease = %+v, want MembershipOccupied=false", lease)
	}

	authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, lease)
	if err != nil || !authorized || refusal != drainAckRefusalNone {
		t.Fatalf("legacy-created member authorization = (%t, %q, %v), want the keyed owner to hold the drain", authorized, refusal, err)
	}

	// Teeth: allocation lineage is dropped as a PRECONDITION, not as a fence
	// for a row the keyed allocator does own. A lease that claims occupancy
	// still has to prove it.
	claimed := lease
	claimed.MembershipOccupied = true
	authorized, refusal, err = fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, claimed)
	if err != nil || authorized || refusal != drainAckRefusalMemberNotOccupied {
		t.Fatalf("claimed-occupancy authorization = (%t, %q, %v), want refusal for a lineage it cannot prove", authorized, refusal, err)
	}
}

// TestAuthorizeRoutedWorkPoolDrainAckHoldsCommittedAckWhenRuntimeIsGone is the
// durable-first half. An acknowledgement is the agent announcing it is
// finished, so the runtime the old fence re-read it from is the one the
// acknowledgement is about to stop — and once the stop-pending transition has
// committed the stamps under CAS, that read proves nothing the row does not
// already prove.
func TestAuthorizeRoutedWorkPoolDrainAckHoldsCommittedAckWhenRuntimeIsGone(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	snapshot := fixture.snapshot
	snapshot.Provider = poolDrainAckGetMetaErrorProvider{
		Provider: fixture.provider,
		err:      errors.New("session is gone"),
	}

	authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(snapshot, fixture.info, fixture.lease)
	if authorized || refusal != drainAckRefusalRuntimeGone || err == nil {
		t.Fatalf("pre-commit authorization with a dead runtime = (%t, %q, %v), want a typed runtime_gone refusal", authorized, refusal, err)
	}

	committed := fixture.lease
	committed.DurableAgentProvenance = true
	authorized, refusal, err = fixture.cr.authorizeRoutedWorkPoolDrainAck(snapshot, fixture.info, committed)
	if err != nil || !authorized || refusal != drainAckRefusalNone {
		t.Fatalf("committed authorization with a dead runtime = (%t, %q, %v), want the durable row to carry the drain", authorized, refusal, err)
	}
}

// TestReconcileExactSessionStartTracesUnprovableDrainAckFallback is the
// negative the ruling kept: an acknowledgement with no agent stamps anywhere —
// an older agent CLI, or a reconciler-authored marker — is genuinely
// unprovable, so auto mode still hands it to legacy. What changed is that the
// handback is no longer stderr-only. It carries a typed reason at the same
// trace site and with the same effect_owner as every other keyed drain record,
// which is what lets the divergence taxonomy classify the fallback instead of
// guessing at it.
func TestReconcileExactSessionStartTracesUnprovableDrainAckFallback(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)

	params := exactSessionStartTestParams(t, env)
	params.RolloutMode = rollout.Auto
	trace := newSessionReconcilerTraceManager(params.CityPath, params.CityName, io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	if _, err := newSessionReconcilerTraceArmStore(params.CityPath).upsertArm(TraceArm{
		ScopeType:  TraceArmScopeTemplate,
		ScopeValue: "worker",
		Source:     TraceArmSourceManual,
		Level:      TraceModeDetail,
		ExpiresAt:  time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("arm detail trace: %v", err)
	}
	params.Trace = trace
	lease := installRecoveredDrainAckLeaseForTest(&params, bead.ID)
	params.RecoverPoolDrainAck = func(session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
		return lease, false, true, nil
	}

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionAntiEntropy,
	}, params)
	if owner != exactSessionStartLegacyOwner || err != nil {
		t.Fatalf("unprovable acknowledgement = (%v, %v), want a clean legacy fallback", owner, err)
	}

	records, readErr := ReadTraceRecords(traceCityRuntimeDir(params.CityPath), TraceFilter{
		SiteCode:  TraceSiteReconcilerDrainAck,
		Template:  "worker",
		TraceMode: TraceModeDetail,
	})
	if readErr != nil {
		t.Fatalf("read drain-ack handback trace: %v", readErr)
	}
	var handbacks []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.OutcomeCode == TraceOutcomeRejected {
			handbacks = append(handbacks, record)
		}
	}
	if len(handbacks) != 1 {
		t.Fatalf("drain-ack handback traces = %#v, want exactly one", records)
	}
	handback := handbacks[0]
	if handback.ReasonCode != TraceReasonCode(drainAckRefusalNotAgentStamped) ||
		handback.SessionBeadID != bead.ID ||
		handback.Fields["effect_owner"] != detectorKeyedEffectOwner ||
		handback.Fields["effect_applied"] != false ||
		handback.Fields["handed_back"] != true {
		t.Fatalf("drain-ack handback trace = %#v, want a typed keyed no-effect handback", handback)
	}
	// The handback must never read as the effect it is refusing to apply. The
	// row-scoped drain purity scan (legacyDrainEffectRecord, in the tagged
	// sibling-isolation file) excludes a record on EITHER of these, and this
	// record carries both: an outcome outside legacyDrainEffectOutcomes and
	// effect_owner=keyed.
	for _, applied := range []TraceOutcomeCode{
		TraceOutcomeStopPending, TraceOutcomeStop, TraceOutcomeComplete, TraceOutcomeClosed,
		TraceOutcomeDrain, TraceOutcomeCancel, TraceOutcomeCancelPending,
		TraceOutcomeCancelAssignedWork, TraceOutcomeCancelReconcilerAck, TraceOutcomeClear,
	} {
		if handback.OutcomeCode == applied {
			t.Fatalf("drain-ack handback trace reads as an applied drain effect (%s): %#v", applied, handback)
		}
	}
}
