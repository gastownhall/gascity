package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/rollout"
	"k8s.io/client-go/util/workqueue"
)

func TestSessionStartControllerCoalescesQueuedHintsAndReplaysInFlightUpdates(t *testing.T) {
	t.Parallel()

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	firstTargetStarted := make(chan struct{})
	releaseFirstTarget := make(chan struct{})
	reconciled := make(chan sessionStartAdmission, 8)
	var targetAttempts atomic.Int32

	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 8,
		MaxRetries:  2,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			switch admission.SessionID {
			case "gcs-blocker1":
				close(blockerStarted)
				<-releaseBlocker
			case "gcs-target1":
				if targetAttempts.Add(1) == 1 {
					close(firstTargetStarted)
					<-releaseFirstTarget
				}
			}
			reconciled <- admission
			return nil
		},
	})

	if outcome, err := controller.Admit("gcs-blocker1", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit blocker = %q, %v", outcome, err)
	}
	awaitClose(t, blockerStarted, "blocker reconcile start")

	if outcome, err := controller.Admit("gcs-target1", sessionStartAdmissionAntiEntropy); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit target = %q, %v", outcome, err)
	}
	if outcome, err := controller.Admit("gcs-target1", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce target = %q, %v", outcome, err)
	}
	close(releaseBlocker)
	if got := receiveSessionStartAdmission(t, reconciled); got.SessionID != "gcs-blocker1" {
		t.Fatalf("first reconciliation = %+v, want blocker", got)
	}
	awaitClose(t, firstTargetStarted, "first target reconcile start")

	if outcome, err := controller.Admit("gcs-target1", sessionStartAdmissionInProcess); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("admit in-flight update = %q, %v", outcome, err)
	}
	close(releaseFirstTarget)

	first := receiveSessionStartAdmission(t, reconciled)
	second := receiveSessionStartAdmission(t, reconciled)
	if first.SessionID != "gcs-target1" || first.Source != sessionStartAdmissionExplicitWake {
		t.Fatalf("first target reconciliation = %+v, want latest queued explicit wake", first)
	}
	if second.SessionID != "gcs-target1" || second.Source != sessionStartAdmissionInProcess {
		t.Fatalf("replayed target reconciliation = %+v, want in-process update", second)
	}
	if second.Version <= first.Version {
		t.Fatalf("replayed version = %d, want newer than %d", second.Version, first.Version)
	}
}

func TestSessionStartControllerPoolDrainAckSupersedesMatchingInFlightPoolStart(t *testing.T) {
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	reconciled := make(chan sessionStartAdmission, 2)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseStart) }) }
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.PoolAllocation != nil {
				close(startEntered)
				<-releaseStart
			}
			reconciled <- admission
			return nil
		},
	})
	t.Cleanup(release)

	start := routedWorkPoolStartLease{
		SessionID: "gcs-pool-handoff", InstanceToken: "live-token", ControllerGeneration: 7,
		PoolTarget: "worker", WorkID: "ga-work", SourceStore: "city:test-city", MembershipRevision: 11,
	}
	if outcome, err := controller.AdmitPoolAllocation(start); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("AdmitPoolAllocation = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, startEntered, "pool-start reconciliation to enter")

	drain := routedWorkPoolDrainAckLease{
		SessionID: "gcs-pool-handoff", InstanceToken: "live-token",
		RequesterSessionID: "gcs-pool-handoff", RequesterInstanceToken: "live-token",
		ControllerGeneration: 7, PoolTarget: "worker", WorkID: "ga-work", SourceStore: "city:test-city", MembershipRevision: 12,
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*routedWorkPoolDrainAckLease)
	}{
		{name: "instance token", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.InstanceToken = "replacement-token" }},
		{name: "requester session", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.RequesterSessionID = "gcs-other" }},
		{name: "requester token", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.RequesterInstanceToken = "replacement-token" }},
		{name: "generation", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.ControllerGeneration++ }},
		{name: "pool target", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.PoolTarget = "other" }},
		{name: "work", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.WorkID = "ga-other-work" }},
		{name: "source store", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.SourceStore = "city:other" }},
		{name: "older membership", mutate: func(lease *routedWorkPoolDrainAckLease) { lease.MembershipRevision = 10 }},
	} {
		t.Run("rejects mismatched "+testCase.name, func(t *testing.T) {
			mismatched := drain
			testCase.mutate(&mismatched)
			if outcome, err := controller.AdmitPoolDrainAck(mismatched); err == nil || outcome != "" {
				t.Fatalf("AdmitPoolDrainAck = (%q, %v), want rejected", outcome, err)
			}
		})
	}
	if outcome, err := controller.AdmitPoolDrainAck(drain); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("AdmitPoolDrainAck = (%q, %v), want matching start superseded", outcome, err)
	}
	admission, ok := controller.readAdmission(start.SessionID)
	if !ok || admission.PoolAllocation != nil || admission.PoolDrainAck == nil || *admission.PoolDrainAck != drain {
		t.Fatalf("superseding admission = %+v, want only drain lease %+v", admission, drain)
	}

	release()
	first := receiveSessionStartAdmission(t, reconciled)
	second := receiveSessionStartAdmission(t, reconciled)
	if first.PoolAllocation == nil || *first.PoolAllocation != start || first.PoolDrainAck != nil {
		t.Fatalf("first reconciliation = %+v, want retained pool start", first)
	}
	if second.PoolAllocation != nil || second.PoolDrainAck == nil || *second.PoolDrainAck != drain {
		t.Fatalf("second reconciliation = %+v, want queued matching pool drain acknowledgement", second)
	}
	if got := controller.Pending(); got != 0 {
		t.Fatalf("pending admissions = %d, want 0 after start-to-stop handoff", got)
	}
}

func TestSessionStartControllerPoolDrainAckDoesNotSupersedeUnenteredPoolStart(t *testing.T) {
	blockerEntered := make(chan struct{})
	releaseBlocker := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBlocker) }) }
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 2, MaxRetries: 0,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-pool-handoff-blocker" {
				close(blockerEntered)
				<-releaseBlocker
			}
			return nil
		},
	})
	t.Cleanup(release)
	if outcome, err := controller.Admit("gcs-pool-handoff-blocker", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("Admit blocker = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, blockerEntered, "blocker reconciliation to enter")

	start := routedWorkPoolStartLease{
		SessionID: "gcs-pool-not-live", InstanceToken: "not-live-token", ControllerGeneration: 7,
		PoolTarget: "worker", WorkID: "ga-work", SourceStore: "city:test-city", MembershipRevision: 11,
	}
	drain := routedWorkPoolDrainAckLease{
		SessionID: "gcs-pool-not-live", InstanceToken: "not-live-token",
		RequesterSessionID: "gcs-pool-not-live", RequesterInstanceToken: "not-live-token",
		ControllerGeneration: 7, PoolTarget: "worker", WorkID: "ga-work", SourceStore: "city:test-city", MembershipRevision: 11,
	}
	if outcome, err := controller.AdmitPoolAllocation(start); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("AdmitPoolAllocation = (%q, %v), want accepted", outcome, err)
	}
	if outcome, err := controller.AdmitPoolDrainAck(drain); err == nil || outcome != "" {
		t.Fatalf("AdmitPoolDrainAck before start enters = (%q, %v), want rejected", outcome, err)
	}
	admission, ok := controller.readAdmission(start.SessionID)
	if !ok || admission.PoolAllocation == nil || *admission.PoolAllocation != start || admission.PoolDrainAck != nil {
		t.Fatalf("unentered admission = %+v, want retained start lease %+v", admission, start)
	}
}

func TestSessionStartControllerPreservesInProcessAdmissionAcrossAntiEntropy(t *testing.T) {
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	reconciled := make(chan sessionStartAdmission, 2)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-blocker1" {
				close(blockerStarted)
				<-releaseBlocker
			}
			reconciled <- admission
			return nil
		},
	})
	if _, err := controller.Admit("gcs-blocker1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit blocker: %v", err)
	}
	awaitClose(t, blockerStarted, "blocker reconcile start")
	if _, err := controller.Admit("gcs-target1", sessionStartAdmissionInProcess); err != nil {
		t.Fatalf("admit in-process target: %v", err)
	}
	original, ok := controller.readAdmission("gcs-target1")
	if !ok {
		t.Fatal("in-process admission missing before anti-entropy coalesce")
	}
	if outcome, err := controller.Admit("gcs-target1", sessionStartAdmissionSocket); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce socket = %q, %v", outcome, err)
	}
	if outcome, err := controller.Admit("gcs-target1", sessionStartAdmissionAntiEntropy); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce anti-entropy = %q, %v", outcome, err)
	}
	close(releaseBlocker)
	_ = receiveSessionStartAdmission(t, reconciled)
	got := receiveSessionStartAdmission(t, reconciled)
	if got.Source != sessionStartAdmissionInProcess || !got.AdmittedAt.Equal(original.AdmittedAt) || got.Version <= original.Version {
		t.Fatalf("queued reconciliation = %+v from %+v, want preserved in-process source/time with newer version", got, original)
	}
}

// TestSessionStartAdmissionIsDemandCoversExactlyTheCoalescingPair pins the
// invariant the v59 exact-wake commit leg was respec'd onto (ga-f7v2ft.142).
// in_process and socket are the demand admissions, and they are exactly the pair
// the coalescing rule above can substitute for one another — which is why
// membership in the pair survives coalescing while the individual value does
// not. Every sweep and detector key must stay outside the set, or the leg would
// stop distinguishing "demand drove this wake" from "a background sweep found
// it".
func TestSessionStartAdmissionIsDemandCoversExactlyTheCoalescingPair(t *testing.T) {
	for _, source := range []sessionStartAdmissionSource{
		sessionStartAdmissionInProcess,
		sessionStartAdmissionSocket,
	} {
		if !sessionStartAdmissionIsDemand(source) {
			t.Errorf("sessionStartAdmissionIsDemand(%q) = false, want true", source)
		}
	}
	for _, source := range []sessionStartAdmissionSource{
		sessionStartAdmissionPendingCreate,
		sessionStartAdmissionExplicitWake,
		sessionStartAdmissionAntiEntropy,
		sessionStartAdmissionWaitDependency,
		sessionStartAdmissionDeadline,
		sessionStartAdmissionOrphanClose,
		sessionStartAdmissionOrphanDrain,
		sessionStartAdmissionStaleCreate,
		sessionStartAdmissionConfigDrift,
		sessionStartAdmissionDuplicateNamed,
		sessionStartAdmissionSleepDrain,
		sessionStartAdmissionProgressStall,
		"",
	} {
		if sessionStartAdmissionIsDemand(source) {
			t.Errorf("sessionStartAdmissionIsDemand(%q) = true, want false", source)
		}
	}
}

func TestSessionStartControllerRetainsPoolDrainAckLeaseAcrossGenericCoalescing(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new exact-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start exact-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	defer close(release)

	lease := routedWorkPoolDrainAckLease{
		SessionID:              "gcs-pool-drain1",
		InstanceToken:          "instance-token",
		RequesterSessionID:     "gcs-pool-drain1",
		RequesterInstanceToken: "instance-token",
		ControllerGeneration:   7,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     11,
	}
	if outcome, err := controller.AdmitPoolDrainAck(lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit pool drain acknowledgement = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "pool drain acknowledgement reconcile to enter")
	if outcome, err := controller.Admit(lease.SessionID, sessionStartAdmissionInProcess); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("coalesce generic event = (%q, %v), want coalesced", outcome, err)
	}

	admission, ok := controller.readAdmission(lease.SessionID)
	if !ok || admission.PoolDrainAck == nil || *admission.PoolDrainAck != lease {
		t.Fatalf("coalesced admission drain lease = %+v, want %+v", admission.PoolDrainAck, lease)
	}
	if !controller.ownsPoolDrainAckStop(lease.SessionID, lease.InstanceToken) {
		t.Fatal("matching retained drain acknowledgement lease did not exclude legacy")
	}
	if controller.ownsPoolDrainAckStop(lease.SessionID, "replacement-token") {
		t.Fatal("mismatched instance token retained drain acknowledgement ownership")
	}
}

func TestPoolDrainAckLeaseExcludesLegacyWhenControllerSnapshotIsUnavailable(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(entered)
			<-release
			return nil
		},
	})
	defer close(release)

	fixture.cr.sessionStartMu.Lock()
	fixture.cr.sessionStartController = controller
	fixture.cr.sessionStartOwnership = sessionStartOwnershipKeyed
	fixture.cr.sessionStartMode = rollout.Auto
	fixture.cr.sessionStartMu.Unlock()
	if outcome, err := controller.AdmitPoolDrainAck(fixture.lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit pool drain acknowledgement = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "pool drain acknowledgement reconcile to enter")

	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sessionStartStoreGeneration = 0
	fixture.cr.cs.mu.Unlock()
	option := fixture.cr.sessionStartLegacyExclusionOption()
	if option == nil || !legacySessionStartExcluded(option, fixture.info) {
		t.Fatal("in-flight exact drain acknowledgement did not exclude legacy during snapshot loss")
	}
	sibling := fixture.info
	sibling.ID = "gcs-sibling"
	sibling.InstanceToken = "sibling-token"
	if legacySessionStartExcluded(option, sibling) {
		t.Fatal("exact drain acknowledgement lease excluded a different occupied slot")
	}
}

func TestSessionStartControllerOverflowRequestsAudit(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-first1" {
				close(started)
				<-release
			}
			return nil
		},
	})

	if outcome, err := controller.Admit("gcs-first1", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit first = %q, %v", outcome, err)
	}
	awaitClose(t, started, "first reconcile start")
	if outcome, err := controller.Admit("gcs-overflow1", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionOverflow {
		t.Fatalf("overflow admission = %q, %v", outcome, err)
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("overflow did not request authoritative audit")
	}
	if controller.TakeAuditRequest() {
		t.Fatal("TakeAuditRequest did not clear the level-triggered request")
	}
	close(release)
}

func TestSessionStartControllerRetriesThenYieldsToAudit(t *testing.T) {
	t.Parallel()

	results := make(chan sessionStartReconcileResult, 8)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  1,
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0),
		Reconcile: func(context.Context, sessionStartAdmission) error {
			return errors.New("store unavailable")
		},
		Observer: func(result sessionStartReconcileResult) {
			results <- result
		},
	})

	if _, err := controller.Admit("gcs-retry1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	first := receiveSessionStartResult(t, results)
	second := receiveSessionStartResult(t, results)
	if first.Outcome != sessionStartReconcileRetrying {
		t.Fatalf("first outcome = %q, want retrying", first.Outcome)
	}
	if second.Outcome != sessionStartReconcileExhausted {
		t.Fatalf("second outcome = %q, want exhausted", second.Outcome)
	}
	if first.Err == nil || second.Err == nil {
		t.Fatalf("retry results lost errors: first=%v second=%v", first.Err, second.Err)
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("retry exhaustion did not yield to anti-entropy")
	}
	if got := controller.Pending(); got != 0 {
		t.Fatalf("pending admissions = %d, want 0 after exhaustion", got)
	}
}

func TestSessionStartControllerRetriesPoolDrainAckPastRetryBudgetUntilReconciled(t *testing.T) {
	results := make(chan sessionStartReconcileResult, 2)
	var attempts atomic.Int32
	lease := routedWorkPoolDrainAckLease{
		SessionID:              "gcs-pool-drain-retry1",
		InstanceToken:          "instance-token",
		RequesterSessionID:     "gcs-pool-drain-retry1",
		RequesterInstanceToken: "instance-token",
		ControllerGeneration:   7,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     11,
	}
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0),
		Reconcile: func(context.Context, sessionStartAdmission) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary store outage")
			}
			return nil
		},
		Observer: func(result sessionStartReconcileResult) {
			results <- result
		},
	})

	if outcome, err := controller.AdmitPoolDrainAck(lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("AdmitPoolDrainAck = (%q, %v), want accepted", outcome, err)
	}
	first := receiveSessionStartResult(t, results)
	if first.Outcome != sessionStartReconcileRetrying {
		t.Fatalf("first outcome = %q, want retrying", first.Outcome)
	}
	second := receiveSessionStartResult(t, results)
	if second.Outcome != sessionStartReconcileSucceeded {
		t.Fatalf("second outcome = %q, want succeeded", second.Outcome)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("reconcile attempts = %d, want 2", got)
	}
	for _, result := range []sessionStartReconcileResult{first, second} {
		if result.Admission.PoolDrainAck == nil || *result.Admission.PoolDrainAck != lease {
			t.Fatalf("reconcile lease = %+v, want %+v", result.Admission.PoolDrainAck, lease)
		}
	}
	if got := controller.Pending(); got != 0 {
		t.Fatalf("pending admissions = %d, want 0 after successful reconciliation", got)
	}
}

func TestSessionStartControllerYieldPoolDrainAckRequiresMatchingLease(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	lease := routedWorkPoolDrainAckLease{
		SessionID:              "gcs-pool-drain-yield1",
		InstanceToken:          "instance-token",
		RequesterSessionID:     "gcs-pool-drain-yield1",
		RequesterInstanceToken: "instance-token",
		ControllerGeneration:   7,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     11,
	}
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 1, MaxRetries: 0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(entered)
			<-release
			return nil
		},
	})
	t.Cleanup(func() { close(release) })
	if outcome, err := controller.AdmitPoolDrainAck(lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("AdmitPoolDrainAck = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "drain-ack reconciliation start")

	stale := lease
	stale.InstanceToken = "replacement-token"
	if controller.YieldPoolDrainAck(stale) {
		t.Fatal("stale drain-ack completion yielded the retained lease")
	}
	if got := controller.Pending(); got != 1 {
		t.Fatalf("pending after stale yield = %d, want 1", got)
	}
	if !controller.YieldPoolDrainAck(lease) {
		t.Fatal("matching drain-ack completion did not yield retained lease")
	}
	if got := controller.Pending(); got != 0 {
		t.Fatalf("pending after matching yield = %d, want 0", got)
	}
}

func TestSessionStartControllerPreservesLegacyFallbackDiagnostic(t *testing.T) {
	t.Parallel()

	results := make(chan sessionStartReconcileResult, 1)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			return fmt.Errorf("%w: drain acknowledgement CAS conflict", errSessionStartLegacyFallbackRequired)
		},
		Observer: func(result sessionStartReconcileResult) {
			results <- result
		},
	})

	if _, err := controller.Admit("gcs-fallback1", sessionStartAdmissionSocket); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	result := receiveSessionStartResult(t, results)
	if result.Outcome != sessionStartReconcileSucceeded || !result.LegacyFallback {
		t.Fatalf("fallback result = %#v, want successful legacy handoff", result)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "CAS conflict") {
		t.Fatalf("fallback diagnostic = %v, want preserved CAS conflict", result.Err)
	}
}

func TestSessionStartControllerBoundsAndValidatesAdmissionKeys(t *testing.T) {
	t.Parallel()

	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile:   func(context.Context, sessionStartAdmission) error { return nil },
	})
	for _, id := range []string{"", " gcs-space1", "gcs-space1 ", "gcs/escape1", "gcs-\nline1"} {
		if _, err := controller.Admit(id, sessionStartAdmissionSocket); err == nil {
			t.Errorf("Admit(%q) error = nil", id)
		}
	}
	oversized := "gcs-" + string(make([]byte, sessionStartAdmissionMaxIDBytes))
	if _, err := controller.Admit(oversized, sessionStartAdmissionSocket); err == nil {
		t.Fatal("oversized session ID accepted")
	}
}

func TestSessionStartControllerStopDoesNotStartQueuedReconciliations(t *testing.T) {
	t.Parallel()

	firstStarted := make(chan struct{})
	reconciled := make(chan string, 2)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(ctx context.Context, admission sessionStartAdmission) error {
			reconciled <- admission.SessionID
			if admission.SessionID == "gcs-running1" {
				close(firstStarted)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	})
	if _, err := controller.Admit("gcs-running1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit running key: %v", err)
	}
	awaitClose(t, firstStarted, "running reconciliation")
	if _, err := controller.Admit("gcs-queued1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit queued key: %v", err)
	}

	controller.Stop()

	if got := <-reconciled; got != "gcs-running1" {
		t.Fatalf("first reconciliation = %q, want gcs-running1", got)
	}
	select {
	case got := <-reconciled:
		t.Fatalf("Stop started queued reconciliation %q", got)
	default:
	}
}

func TestSessionStartControllerStopClosesAdmissionAndJoinsWorkers(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReconcile := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseReconcile()
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(started)
			<-release
			return nil
		},
	})
	if _, err := controller.Admit("gcs-stop1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	awaitClose(t, started, "reconcile start")

	stopped := make(chan struct{})
	go func() {
		controller.Stop()
		close(stopped)
	}()
	awaitClose(t, controller.ctx.Done(), "controller cancellation")
	select {
	case <-stopped:
		t.Fatal("Stop returned before the active worker joined")
	default:
	}
	if _, err := controller.Admit("gcs-after1", sessionStartAdmissionExplicitWake); err == nil {
		t.Fatal("admission remained open during Stop")
	}
	releaseReconcile()
	awaitClose(t, stopped, "controller stop")
}

func TestSessionStartControllerStopJoinsAuthoritativeSeedBeforeQueueDrain(t *testing.T) {
	firstStarted := make(chan struct{})
	reconciled := make(chan string, 2)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(ctx context.Context, admission sessionStartAdmission) error {
			reconciled <- admission.SessionID
			if admission.SessionID == "gcs-seed-stop-first" {
				close(firstStarted)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	})
	first := true
	producerBlocked := make(chan struct{})
	if err := controller.StartAuthoritativeSeed(func(ctx context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-seed-stop-first"}
		}
		close(producerBlocked)
		<-ctx.Done()
		return sessionStartAuthoritativeSeedResult{Err: ctx.Err()}
	}); err != nil {
		t.Fatalf("StartAuthoritativeSeed: %v", err)
	}
	awaitClose(t, firstStarted, "first authoritative seed reconciliation")
	awaitClose(t, producerBlocked, "authoritative seed producer waiting for cancellation")

	stopped := make(chan struct{})
	go func() {
		controller.Stop()
		close(stopped)
	}()
	awaitClose(t, controller.ctx.Done(), "controller cancellation")
	awaitClose(t, stopped, "controller stop")
	if got := <-reconciled; got != "gcs-seed-stop-first" {
		t.Fatalf("reconciliation before stop = %q, want first seed", got)
	}
	select {
	case got := <-reconciled:
		t.Fatalf("Stop admitted a seed after cancellation: %q", got)
	default:
	}
}

func TestSessionStartControllerCompleteAuthoritativeCensusDoesNotCullInFlightAdmission(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var releaseOldOnce sync.Once
	release := func() { releaseOldOnce.Do(func() { close(releaseOld) }) }
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-old" {
				close(oldStarted)
				<-releaseOld
			}
			return nil
		},
	})
	t.Cleanup(release)

	first := true
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-old"}
		}
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start first census: %v", err)
	}
	awaitClose(t, oldStarted, "old authoritative reconciliation")
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return !controller.seedActive
	}, "first complete authoritative census")
	if got := controller.Pending(); got != 1 {
		t.Fatalf("pending after first census = %d, want 1 in-flight old admission", got)
	}

	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start second census: %v", err)
	}
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return !controller.seedActive
	}, "second complete authoritative census")
	if got := controller.Pending(); got != 1 {
		t.Fatalf("pending after absent key in complete later census = %d, want 1 while effect is in flight", got)
	}
	release()
	awaitCond(t, func() bool { return controller.Pending() == 0 }, "in-flight admission completion")
}

func TestSessionStartControllerCompleteAuthoritativeCensusCullsQueuedAdmission(t *testing.T) {
	controller := newSessionStartControllerWithQueuedAuthoritativeAdmission(context.Background(), t)
	if _, ok := controller.readAdmission("gcs-old"); !ok {
		t.Fatal("old anti-entropy admission missing before later census")
	}

	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start empty later census: %v", err)
	}
	awaitSessionStartSeedInactive(t, controller, "later complete authoritative census")
	if admission, ok := controller.readAdmission("gcs-old"); !ok || !admission.Culled {
		t.Fatalf("absent queued admission = (%+v, %t), want retained cull marker", admission, ok)
	}
}

func TestSessionStartControllerCensusCullRetainsQueuedSlotUntilDequeue(t *testing.T) {
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBlocker) }) }
	culledReconciled := make(chan struct{}, 1)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			switch admission.SessionID {
			case "gcs-blocker":
				close(blockerStarted)
				<-releaseBlocker
			case "gcs-old":
				culledReconciled <- struct{}{}
			}
			return nil
		},
	})
	t.Cleanup(release)

	if _, err := controller.Admit("gcs-blocker", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit blocker: %v", err)
	}
	awaitClose(t, blockerStarted, "blocker reconciliation")
	first := true
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-old"}
		}
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start initial census: %v", err)
	}
	awaitSessionStartSeedInactive(t, controller, "initial complete census")
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start empty census: %v", err)
	}
	awaitSessionStartSeedInactive(t, controller, "empty complete census")

	if got := controller.Pending(); got != 2 {
		t.Fatalf("retained state after cull = %d, want blocker plus queued tombstone", got)
	}
	if got := controller.queue.Len(); got != 1 {
		t.Fatalf("queue length after cull = %d, want one retained queued key", got)
	}

	release()
	awaitCond(t, func() bool { return controller.Pending() == 0 && controller.queue.Len() == 0 }, "culled key dequeue")
	select {
	case <-culledReconciled:
		t.Fatal("culled queued key reached reconcile callback")
	default:
	}
}

func TestSessionStartControllerCensusCullBetweenReadAndMarkSkipsEffect(t *testing.T) {
	markEntered := make(chan struct{})
	releaseMark := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMark) }) }
	reconciled := make(chan struct{}, 1)
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			reconciled <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new session-start controller: %v", err)
	}
	controller.beforeMarkInFlightForTest = func() {
		close(markEntered)
		<-releaseMark
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	t.Cleanup(release)

	first := true
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-raced-cull"}
		}
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start initial census: %v", err)
	}
	awaitClose(t, markEntered, "worker read before in-flight mark")
	awaitSessionStartSeedInactive(t, controller, "initial complete census")
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start empty census: %v", err)
	}
	awaitSessionStartSeedInactive(t, controller, "empty complete census")
	if admission, ok := controller.readAdmission("gcs-raced-cull"); !ok || !admission.Culled {
		t.Fatalf("admission before mark release = (%+v, %t), want retained cull marker", admission, ok)
	}

	release()
	awaitCond(t, func() bool { return controller.Pending() == 0 }, "raced cull dequeue")
	select {
	case <-reconciled:
		t.Fatal("cull between worker read and in-flight mark reached reconcile callback")
	default:
	}
}

func TestSessionStartControllerBlockedWorkersKeepDistinctCensusQueueBounded(t *testing.T) {
	const (
		retainedLimit = 16
		attempts      = 128
	)
	releaseWorkers := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWorkers) }) }
	var blockersStarted atomic.Int32
	var unexpectedEffects atomic.Int32
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     retainedLimit,
		MaxDistinct: retainedLimit * 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if strings.HasPrefix(admission.SessionID, "gcs-blocker-") {
				blockersStarted.Add(1)
				<-releaseWorkers
				return nil
			}
			unexpectedEffects.Add(1)
			return nil
		},
	})
	t.Cleanup(release)

	for i := range retainedLimit {
		if _, err := controller.Admit(fmt.Sprintf("gcs-blocker-%02d", i), sessionStartAdmissionExplicitWake); err != nil {
			t.Fatalf("admit blocker %d: %v", i, err)
		}
	}
	awaitCond(t, func() bool { return blockersStarted.Load() == retainedLimit }, "all workers blocked")

	accepted := 0
	overflowed := 0
	for i := range attempts {
		presentGeneration := advanceSessionStartSeedGenerationForTest(controller)
		outcome, _, err := controller.admitAuthoritative(
			fmt.Sprintf("gcs-census-%03d", i),
			presentGeneration,
			nil,
			false,
			"",
		)
		if err != nil {
			t.Fatalf("admit census key %d: %v", i, err)
		}
		switch outcome {
		case sessionStartAdmissionAccepted:
			accepted++
			controller.publishCompleteAuthoritativeCensus(presentGeneration)
			emptyGeneration := advanceSessionStartSeedGenerationForTest(controller)
			controller.publishCompleteAuthoritativeCensus(emptyGeneration)
		case sessionStartAdmissionOverflow:
			overflowed++
		default:
			t.Fatalf("admit census key %d outcome = %q, want accepted or overflow", i, outcome)
		}
	}

	if accepted != retainedLimit || overflowed != attempts-retainedLimit {
		t.Fatalf("census outcomes = accepted %d overflow %d, want %d and %d", accepted, overflowed, retainedLimit, attempts-retainedLimit)
	}
	if got := controller.queue.Len(); got != retainedLimit {
		t.Fatalf("queued census keys = %d, want bounded %d", got, retainedLimit)
	}
	if got := controller.Pending(); got != retainedLimit*2 {
		t.Fatalf("retained controller keys = %d, want bounded %d", got, retainedLimit*2)
	}
	controller.mu.Lock()
	retainedSeeds := len(controller.seedOutstanding)
	controller.mu.Unlock()
	if retainedSeeds != retainedLimit {
		t.Fatalf("retained authoritative slots = %d, want %d", retainedSeeds, retainedLimit)
	}
	if got := unexpectedEffects.Load(); got != 0 {
		t.Fatalf("census effects while workers blocked = %d, want 0", got)
	}
}

func advanceSessionStartSeedGenerationForTest(controller *sessionStartController) uint64 {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.seedGeneration++
	return controller.seedGeneration
}

func TestSessionStartControllerCompleteAuthoritativeCensusKeepsNewerExactAdmission(t *testing.T) {
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-old" {
				close(oldStarted)
				<-releaseOld
			}
			return nil
		},
	})
	t.Cleanup(func() { close(releaseOld) })

	first := true
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-old"}
		}
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start first census: %v", err)
	}
	awaitClose(t, oldStarted, "old authoritative reconciliation")
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return !controller.seedActive
	}, "first complete authoritative census")
	beforeExact, ok := controller.readAdmission("gcs-old")
	if !ok {
		t.Fatal("old admission missing before newer exact admission")
	}

	secondEntered := make(chan struct{})
	releaseSecond := make(chan struct{})
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		close(secondEntered)
		<-releaseSecond
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start second census: %v", err)
	}
	awaitClose(t, secondEntered, "second census before completion")
	if outcome, err := controller.Admit("gcs-old", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionCoalesced {
		t.Fatalf("admit newer exact key = %q, %v, want coalesced", outcome, err)
	}
	close(releaseSecond)
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return !controller.seedActive
	}, "second complete authoritative census")
	if admission, ok := controller.readAdmission("gcs-old"); !ok || admission.Source != sessionStartAdmissionExplicitWake || admission.Version <= beforeExact.Version {
		t.Fatalf("newer exact admission = (%+v, %t), want retained", admission, ok)
	}
}

func TestSessionStartControllerIncompleteAuthoritativeCensusesDoNotCullQueuedAdmission(t *testing.T) {
	t.Run("producer error after partial page", func(t *testing.T) {
		releaseBlockers := make(chan struct{})
		var blockersStarted atomic.Int32
		controller := mustStartSessionStartController(t, sessionStartControllerOptions{
			Workers:     2,
			MaxDistinct: 4,
			MaxRetries:  1,
			Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
				if strings.HasPrefix(admission.SessionID, "gcs-blocker-") {
					blockersStarted.Add(1)
					<-releaseBlockers
				}
				return nil
			},
		})
		t.Cleanup(func() { close(releaseBlockers) })
		for i := range 2 {
			if _, err := controller.Admit(fmt.Sprintf("gcs-blocker-%d", i), sessionStartAdmissionExplicitWake); err != nil {
				t.Fatalf("admit blocker %d: %v", i, err)
			}
		}
		awaitCond(t, func() bool { return blockersStarted.Load() == 2 }, "both workers blocked")

		first := true
		if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
			if first {
				first = false
				return sessionStartAuthoritativeSeedResult{SessionID: "gcs-old"}
			}
			return sessionStartAuthoritativeSeedResult{Complete: true}
		}); err != nil {
			t.Fatalf("start initial authoritative census: %v", err)
		}
		awaitSessionStartSeedInactive(t, controller, "initial complete authoritative census")
		oldBefore, ok := controller.readAdmission("gcs-old")
		if !ok {
			t.Fatal("old anti-entropy admission missing before failed census")
		}

		var producerCalls atomic.Int32
		if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
			if producerCalls.Add(1) == 1 {
				return sessionStartAuthoritativeSeedResult{SessionID: "gcs-seen"}
			}
			return sessionStartAuthoritativeSeedResult{Err: errors.New("pagination failed")}
		}); err != nil {
			t.Fatalf("start failed census: %v", err)
		}
		awaitSessionStartSeedInactive(t, controller, "failed authoritative census")
		if got := producerCalls.Load(); got != 2 {
			t.Fatalf("failed census producer calls = %d, want yielded key then error", got)
		}
		if _, ok := controller.readAdmission("gcs-seen"); !ok {
			t.Fatal("failed census did not admit its yielded key")
		}
		if oldAfter, ok := controller.readAdmission("gcs-old"); !ok || oldAfter.Culled || oldAfter.Version != oldBefore.Version {
			t.Fatalf("unseen queued admission after failed census = (%+v, %t), want unchanged %+v", oldAfter, ok, oldBefore)
		}
		if !controller.TakeAuditRequest() {
			t.Fatal("failed census did not request a follow-up audit")
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		controller := newSessionStartControllerWithQueuedAuthoritativeAdmission(parent, t)
		if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
			cancel()
			return sessionStartAuthoritativeSeedResult{Complete: true}
		}); err != nil {
			t.Fatalf("start canceled census: %v", err)
		}
		awaitSessionStartSeedInactive(t, controller, "canceled authoritative census")
		if _, ok := controller.readAdmission("gcs-old"); !ok {
			t.Fatal("canceled census culled queued old admission")
		}
	})

	t.Run("supersession", func(t *testing.T) {
		controller := newSessionStartControllerWithQueuedAuthoritativeAdmission(context.Background(), t)
		entered := make(chan struct{})
		release := make(chan struct{})
		if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
			close(entered)
			<-release
			return sessionStartAuthoritativeSeedResult{Complete: true}
		}); err != nil {
			t.Fatalf("start superseded census: %v", err)
		}
		awaitClose(t, entered, "superseded authoritative census")
		if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
			return sessionStartAuthoritativeSeedResult{Complete: true}
		}); err != nil {
			t.Fatalf("supersede authoritative census: %v", err)
		}
		close(release)
		awaitSessionStartSeedInactive(t, controller, "superseded authoritative census exit")
		if _, ok := controller.readAdmission("gcs-old"); !ok {
			t.Fatal("superseded census culled queued old admission")
		}
		if !controller.TakeAuditRequest() {
			t.Fatal("superseded census did not retain a follow-up audit")
		}
	})
}

func TestSessionStartControllerSupersessionFencesAdmissionAfterProducerReturns(t *testing.T) {
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	var releaseBlockerOnce sync.Once
	release := func() { releaseBlockerOnce.Do(func() { close(releaseBlocker) }) }
	staleReconciled := make(chan struct{}, 1)
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 3,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			switch admission.SessionID {
			case "gcs-blocker":
				close(blockerStarted)
				<-releaseBlocker
			case "gcs-stale":
				staleReconciled <- struct{}{}
			}
			return nil
		},
	})
	t.Cleanup(release)

	if _, err := controller.Admit("gcs-blocker", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit blocker: %v", err)
	}
	awaitClose(t, blockerStarted, "blocker reconciliation")

	nextEntered := make(chan struct{})
	releaseNext := make(chan struct{})
	var releaseNextOnce sync.Once
	releaseProducer := func() { releaseNextOnce.Do(func() { close(releaseNext) }) }
	t.Cleanup(releaseProducer)
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		close(nextEntered)
		<-releaseNext
		return sessionStartAuthoritativeSeedResult{SessionID: "gcs-stale"}
	}); err != nil {
		t.Fatalf("start stale producer: %v", err)
	}
	awaitClose(t, nextEntered, "stale producer callback")
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("supersede stale producer: %v", err)
	}
	releaseProducer()
	awaitSessionStartSeedInactive(t, controller, "superseded producer exit")

	if admission, ok := controller.readAdmission("gcs-stale"); ok {
		t.Errorf("superseded producer admitted stale key: %+v", admission)
	}
	if got := controller.queue.Len(); got != 0 {
		t.Errorf("queue length after superseded producer = %d, want 0", got)
	}

	release()
	controller.Stop()
	select {
	case <-staleReconciled:
		t.Fatal("superseded producer reached reconcile callback")
	default:
	}
}

func newSessionStartControllerWithQueuedAuthoritativeAdmission(ctx context.Context, t *testing.T) *sessionStartController {
	t.Helper()
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 2,
		MaxRetries:  1,
		Reconcile: func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-blocker" {
				close(blockerStarted)
				<-releaseBlocker
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new session-start controller: %v", err)
	}
	if err := controller.Start(ctx); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)
	t.Cleanup(func() { close(releaseBlocker) })

	if _, err := controller.Admit("gcs-blocker", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit blocker: %v", err)
	}
	awaitClose(t, blockerStarted, "blocker reconciliation")
	first := true
	if err := controller.StartAuthoritativeSeed(func(context.Context) sessionStartAuthoritativeSeedResult {
		if first {
			first = false
			return sessionStartAuthoritativeSeedResult{SessionID: "gcs-old"}
		}
		return sessionStartAuthoritativeSeedResult{Complete: true}
	}); err != nil {
		t.Fatalf("start initial authoritative census: %v", err)
	}
	awaitSessionStartSeedInactive(t, controller, "initial complete authoritative census")
	return controller
}

func awaitSessionStartSeedInactive(t *testing.T, controller *sessionStartController, what string) {
	t.Helper()
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return !controller.seedActive
	}, what)
}

func mustStartSessionStartController(t *testing.T, opts sessionStartControllerOptions) *sessionStartController {
	t.Helper()
	controller, err := newSessionStartController(opts)
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(controller.Stop)
	return controller
}

func receiveSessionStartAdmission(t *testing.T, ch <-chan sessionStartAdmission) sessionStartAdmission {
	t.Helper()
	select {
	case admission := <-ch:
		return admission
	case <-time.After(hangBudget):
		t.Fatal("timed out waiting for session-start reconciliation")
		return sessionStartAdmission{}
	}
}

func receiveSessionStartResult(t *testing.T, ch <-chan sessionStartReconcileResult) sessionStartReconcileResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(hangBudget):
		t.Fatal("timed out waiting for session-start result")
		return sessionStartReconcileResult{}
	}
}
