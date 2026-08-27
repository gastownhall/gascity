package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
	"k8s.io/client-go/util/workqueue"
)

func TestCityRuntimeSessionStartControllerOffKeepsLegacyOwnership(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Off, true)

	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipLegacy {
		t.Fatalf("ownership = %v, want legacy", cr.sessionStartOwnershipState())
	}
	if option := cr.sessionStartLegacyExclusionOption(); option != nil {
		t.Fatal("off mode installed a legacy-start exclusion")
	}
}

func TestCityRuntimeSessionStartControllerExecutesDrainAckStopPendingOnTypedEvent(t *testing.T) {
	originalPoke := drainAckAsyncStopPokeController
	var pokes atomic.Int32
	drainAckAsyncStopPokeController = func(string) error {
		pokes.Add(1)
		return nil
	}
	t.Cleanup(func() { drainAckAsyncStopPokeController = originalPoke })

	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	bead, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}

	provider := &freshBlockingStopProvider{newBlockingStopProvider()}
	routedProvider := sessionauto.New(provider, runtime.NewFake())
	if err := routedProvider.Start(context.Background(), fixture.info.SessionName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":                   fixture.lease.SessionID,
		"GC_INSTANCE_TOKEN":               fixture.lease.InstanceToken,
		reconcilerDrainAckSourceKey:       drainAckSourceAgentValue,
		drainAckRequesterSessionIDKey:     fixture.lease.RequesterSessionID,
		drainAckRequesterInstanceTokenKey: fixture.lease.RequesterInstanceToken,
		"GC_DRAIN_ACK":                    "1",
	} {
		if err := routedProvider.SetMeta(fixture.info.SessionName, key, value); err != nil {
			t.Fatalf("set runtime metadata %s: %v", key, err)
		}
	}
	fixture.cr.sp = routedProvider
	fixture.cr.dops = newDrainOps(routedProvider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = routedProvider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	providerReleased := false
	t.Cleanup(func() {
		if !providerReleased {
			close(provider.releaseStop)
		}
	})
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}

	fixture.cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
	select {
	case got := <-provider.stopStarted:
		if got != fixture.info.SessionName {
			t.Fatalf("stopped session = %q, want %q", got, fixture.info.SessionName)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("typed session event did not enter the drain-ack stop path")
	}
	close(provider.releaseStop)
	providerReleased = true
	awaitCond(t, func() bool {
		info, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
		return getErr == nil && !isDrainAckStopPendingInfo(info)
	}, "exact drain-ack stop durable finalization")
	if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 1 {
		t.Fatalf("provider Stop calls = %d, want 1", got)
	}
	if got := pokes.Load(); got != 0 {
		t.Fatalf("exact drain-ack stop pokes = %d, want 0", got)
	}
}

func TestCityRuntimeKeyedDrainAckIncompleteCompletionReadmitsDurableMarker(t *testing.T) {
	oldTimeout := drainAckStopConfirmDeadTimeout
	drainAckStopConfirmDeadTimeout = 0
	t.Cleanup(func() { drainAckStopConfirmDeadTimeout = oldTimeout })

	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	bead, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}

	provider := &incompleteThenDeadUnattendedProvider{Fake: fixture.provider}
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}

	fixture.cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
	awaitCond(t, func() bool {
		info, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
		return getErr == nil && !isDrainAckStopPendingInfo(info)
	}, "incomplete exact stop completion to re-admit and finalize its durable marker")
	if got := provider.observations.Load(); got < 3 {
		t.Fatalf("fresh liveness observations = %d, want live, incomplete-dead, then complete-dead", got)
	}
	if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 1 {
		t.Fatalf("provider Stop calls = %d, want exactly one", got)
	}
}

type sequenceGetMetaProvider struct {
	*runtime.Fake
	results []getMetaResult
	calls   atomic.Int32
}

type gatedTokenReadProvider struct {
	*runtime.Fake
	metaCalls            atomic.Int32
	firstFailureObserved chan struct{}
	retryBlocked         chan struct{}
	releaseRetry         chan struct{}
	failureOnce          sync.Once
	blockOnce            sync.Once
}

type freshBlockingStopProvider struct{ *blockingStopProvider }

type incompleteThenDeadUnattendedProvider struct {
	*runtime.Fake
	observations atomic.Int32
}

func (p *incompleteThenDeadUnattendedProvider) ObserveFreshLiveness(runtime.LivenessTarget) runtime.Liveness {
	switch p.observations.Add(1) {
	case 1:
		return runtime.Liveness{Running: true, Alive: true, Complete: true}
	case 2:
		return runtime.Liveness{}
	default:
		return runtime.Liveness{Complete: true}
	}
}

func (p *incompleteThenDeadUnattendedProvider) StopUnattendedSession(name, expectedToken string) error {
	actualToken, err := p.GetMeta(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return err
	}
	if actualToken != expectedToken {
		return fmt.Errorf("instance token = %q, want %q", actualToken, expectedToken)
	}
	return p.Stop(name)
}

func (p *freshBlockingStopProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: !running}
}

func (p *freshBlockingStopProvider) StopUnattendedSession(name, _ string) error {
	return p.Stop(name)
}

type getMetaResult struct {
	token string
	err   error
}

func (p *sequenceGetMetaProvider) GetMeta(name, key string) (string, error) {
	index := int(p.calls.Add(1) - 1)
	if index < len(p.results) {
		return p.results[index].token, p.results[index].err
	}
	return p.Fake.GetMeta(name, key)
}

func (p *sequenceGetMetaProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: true}
}

func (p *sequenceGetMetaProvider) StopUnattendedSession(name, expectedToken string) error {
	actual, err := p.GetMeta(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return err
	}
	if strings.TrimSpace(actual) != expectedToken {
		return errors.New("instance token mismatch")
	}
	return p.Stop(name)
}

// GetMeta fails the FIRST instance-token read and blocks the second. It is
// keyed on GC_INSTANCE_TOKEN rather than on a call count because the keyed
// drain-ack path no longer re-reads the runtime once the acknowledgement is
// committed durably (council R1), so the surviving token read is the one
// StopUnattendedSession makes at the destructive boundary — which is the fence
// this test is actually about.
func (p *gatedTokenReadProvider) GetMeta(name, key string) (string, error) {
	if key != "GC_INSTANCE_TOKEN" {
		return p.Fake.GetMeta(name, key)
	}
	p.metaCalls.Add(1)
	failed := false
	p.failureOnce.Do(func() {
		failed = true
		close(p.firstFailureObserved)
	})
	if failed {
		return "", errors.New("token read failed")
	}
	p.blockOnce.Do(func() { close(p.retryBlocked) })
	<-p.releaseRetry
	return p.Fake.GetMeta(name, key)
}

func (p *gatedTokenReadProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: true}
}

func (p *gatedTokenReadProvider) StopUnattendedSession(name, expectedToken string) error {
	actual, err := p.GetMeta(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return err
	}
	if strings.TrimSpace(actual) != expectedToken {
		return errors.New("instance token mismatch")
	}
	return p.Stop(name)
}

type unattendedStopCall struct {
	name          string
	expectedToken string
}

type unattendedStopProvider struct {
	*runtime.Fake
	mu         sync.Mutex
	stopErrors []error
	stopError  func(int) error
	beforeStop func(int)
	stopCalls  []unattendedStopCall
}

func (p *unattendedStopProvider) ObserveFreshLiveness(target runtime.LivenessTarget) runtime.Liveness {
	running := p.IsRunning(target.SessionName)
	return runtime.Liveness{Running: running, Alive: running, Complete: true}
}

func (p *unattendedStopProvider) StopUnattendedSession(name, expectedToken string) error {
	p.mu.Lock()
	p.stopCalls = append(p.stopCalls, unattendedStopCall{
		name:          name,
		expectedToken: expectedToken,
	})
	index := len(p.stopCalls) - 1
	var err error
	if p.stopError != nil {
		err = p.stopError(index)
	} else if index < len(p.stopErrors) {
		err = p.stopErrors[index]
	}
	beforeStop := p.beforeStop
	p.mu.Unlock()
	if beforeStop != nil {
		beforeStop(index)
	}
	if err != nil {
		return err
	}
	return p.Stop(name)
}

func (p *unattendedStopProvider) stopSnapshot() []unattendedStopCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]unattendedStopCall(nil), p.stopCalls...)
}

const drainAckTestInstanceToken = "drain-token"

func markDrainAckStopPendingForTest(env *reconcilerTestEnv, bead *beads.Bead) {
	patch := session.DrainAckStopPendingPatch(env.clk.Now().UTC())
	patch["instance_token"] = drainAckTestInstanceToken
	env.setSessionMetadata(bead, patch)
}

func markExactDrainAckStopPendingForTest(env *reconcilerTestEnv, bead *beads.Bead) {
	patch := session.AgentDrainAckStopPendingPatch(env.clk.Now().UTC(), bead.ID, drainAckTestInstanceToken)
	patch["instance_token"] = drainAckTestInstanceToken
	env.setSessionMetadata(bead, patch)
}

func markExactDrainAckStopPendingForFixture(t *testing.T, fixture routedWorkPoolDrainAckAuthorizationFixture) {
	t.Helper()
	patch := session.AgentDrainAckStopPendingPatch(
		time.Now().UTC(), fixture.lease.RequesterSessionID, fixture.lease.RequesterInstanceToken,
	)
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, patch); err != nil {
		t.Fatalf("mark exact durable drain acknowledgement stop-pending: %v", err)
	}
}

func installRecoveredDrainAckLeaseForTest(params *exactSessionStartParams, sessionID string) routedWorkPoolDrainAckLease {
	lease := routedWorkPoolDrainAckLease{
		SessionID:              sessionID,
		InstanceToken:          drainAckTestInstanceToken,
		RequesterSessionID:     sessionID,
		RequesterInstanceToken: drainAckTestInstanceToken,
		ControllerGeneration:   1,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     1,
	}
	params.RecoverPoolDrainAck = func(info session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
		if info.ID != sessionID {
			return routedWorkPoolDrainAckLease{}, false, false, fmt.Errorf("recovering lease for %q, want %q", info.ID, sessionID)
		}
		return lease, true, false, nil
	}
	params.AuthorizePoolDrainAck = func(info session.Info, candidate routedWorkPoolDrainAckLease) (bool, drainAckRefusal, error) {
		// DurableAgentProvenance is a caller-supplied mode, not lease identity:
		// the reconciler sets it once the stamps are committed.
		candidate.DurableAgentProvenance = false
		return info.ID == sessionID && candidate == lease, drainAckRefusalNone, nil
	}
	return lease
}

func TestCityRuntimeSessionStartControllerRetriesDrainAckTokenReadError(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	provider := &gatedTokenReadProvider{
		Fake:                 fixture.provider,
		firstFailureObserved: make(chan struct{}),
		retryBlocked:         make(chan struct{}),
		releaseRetry:         make(chan struct{}),
	}
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	t.Cleanup(func() {
		select {
		case <-provider.releaseRetry:
		default:
			close(provider.releaseRetry)
		}
	})
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}

	if outcome, err := fixture.cr.sessionStartController.AdmitPoolDrainAck(fixture.lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit drain acknowledgement = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, provider.firstFailureObserved, "bound stop to observe token-read error")
	awaitClose(t, provider.retryBlocked, "keyed retry after token-read error")
	if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 0 {
		t.Fatalf("provider Stop calls after token-read error = %d, want 0", got)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}
	if !isDrainAckStopPendingInfo(info) {
		t.Fatal("token-read error cleared the durable stop marker")
	}

	close(provider.releaseRetry)
	awaitCond(t, func() bool { return provider.CountCalls("Stop", fixture.info.SessionName) == 1 }, "keyed retry after token-read error")
	awaitCond(t, func() bool {
		current, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
		return getErr == nil && !isDrainAckStopPendingInfo(current)
	}, "keyed retry durable finalization")
}

func TestReconcileExactSessionStartDrainAckStopPendingStrictBoundStopParksTokenFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		results []getMetaResult
	}{
		{
			name:    "empty",
			results: []getMetaResult{{}},
		},
		{
			name:    "error",
			results: []getMetaResult{{err: errors.New("token vanished")}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			bead := env.createSessionBead("worker", "worker")
			markDrainAckStopPendingForTest(env, &bead)
			provider := &sequenceGetMetaProvider{Fake: runtime.NewFake(), results: test.results}
			if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("start runtime: %v", err)
			}
			params := exactSessionStartTestParams(t, env)
			params.Provider = provider
			tracker := &asyncStartTracker{}
			params.AsyncStopTracker = tracker
			installRecoveredDrainAckLeaseForTest(&params, bead.ID)
			owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionInProcess}, params)
			if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
				t.Fatalf("reconcile exact marker = (%v, %v), want keyed pending", owner, err)
			}
			if !tracker.wait(testutil.GoroutineRaceTimeout) {
				t.Fatal("strict token-fenced stop did not settle")
			}
			if got := provider.CountCalls("Stop", "worker"); got != 0 {
				t.Fatalf("provider Stop calls = %d, want 0", got)
			}
			if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
				t.Fatal("strict token fence cleared the durable marker")
			}
		})
	}
}

func TestReconcileExactSessionStartDrainAckStopPendingStrictBoundStopParksWrappedSessionNotFound(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markExactDrainAckStopPendingForTest(env, &bead)
	provider := &unattendedStopProvider{
		Fake: runtime.NewFake(),
		stopErrors: []error{
			fmt.Errorf("reading certified pane: %w", runtime.ErrSessionNotFound),
		},
	}
	if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	params := exactSessionStartTestParams(t, env)
	params.Store = &drainAckAtomicCloseStore{Store: env.store}
	params.Provider = provider
	writer, ok := env.store.(beads.ConditionalWriter)
	if !ok {
		t.Fatal("test store does not implement conditional writer")
	}
	params.StatusWriter = writer
	tracker := &asyncStartTracker{}
	params.AsyncStopTracker = tracker
	installRecoveredDrainAckLeaseForTest(&params, bead.ID)

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("reconcile exact marker = (%v, %v), want keyed pending", owner, err)
	}
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("strict bound stop did not settle")
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 {
		t.Fatalf("unattended stop calls = %#v, want only the failed initial certification", calls)
	}
	if got := provider.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want no fallback or finalization kill", got)
	}
	if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("certified-pane lookup failure cleared the durable stop marker")
	}
}

func TestReconcileExactSessionStartDrainAckUpgradesRecoveredProvenanceBeforeStop(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)
	provider := &unattendedStopProvider{Fake: runtime.NewFake(), stopErrors: []error{errors.New("stop withheld for assertion")}}
	if err := provider.Start(t.Context(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	writer, ok := env.store.(beads.ConditionalWriter)
	if !ok {
		t.Fatal("test store does not implement conditional writer")
	}
	var sequence []string
	recording := &recordingExactStatusWriter{
		ConditionalWriter: writer,
		store:             env.store,
		forward:           true,
		onUpdate:          func() { sequence = append(sequence, "write") },
	}
	params := exactSessionStartTestParams(t, env)
	params.Store = &drainAckAtomicCloseStore{Store: env.store}
	params.Provider = provider
	params.StatusWriter = recording
	params.AsyncStopTracker = &asyncStartTracker{}
	lease := installRecoveredDrainAckLeaseForTest(&params, bead.ID)
	params.AuthorizePoolDrainAck = func(info session.Info, candidate routedWorkPoolDrainAckLease) (bool, drainAckRefusal, error) {
		sequence = append(sequence, "authorize")
		candidate.DurableAgentProvenance = false
		return info.ID == bead.ID && candidate == lease, drainAckRefusalNone, nil
	}

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("reconcile exact marker = (%v, %v), want keyed pending", owner, err)
	}
	if !params.AsyncStopTracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("recovered exact stop did not settle")
	}
	if got := len(recording.expected); got != 1 {
		t.Fatalf("provenance conditional writes = %d, want 1", got)
	}
	if want := []string{"authorize", "write", "authorize", "authorize"}; !reflect.DeepEqual(sequence, want) {
		t.Fatalf("recovered drain-ack authorization/write sequence = %#v, want %#v", sequence, want)
	}
	row, getErr := env.store.Get(bead.ID)
	if getErr != nil {
		t.Fatalf("read upgraded stop-pending row: %v", getErr)
	}
	if row.Status != "open" || row.Metadata["state"] != string(session.StateDraining) ||
		row.Metadata[session.DrainAckSourceMetadataKey] != session.DrainAckSourceAgentValue ||
		row.Metadata[session.DrainAckRequesterSessionIDMetadataKey] != bead.ID ||
		row.Metadata[session.DrainAckRequesterInstanceTokenMetadataKey] != drainAckTestInstanceToken {
		t.Fatalf("recovered row = %#v, want exact durable provenance before STOP", row)
	}
	if calls := provider.stopSnapshot(); len(calls) != 1 {
		t.Fatalf("unattended stop calls = %#v, want one post-upgrade attempt", calls)
	}
	if got := provider.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0 after injected pre-stop failure", got)
	}
}

// TestReconcileExactSessionStartDrainAckParksRecoveredProvenanceRace proves
// that an ambiguous provenance write cannot adopt a concurrently changed
// stop-pending row merely because it now carries plausible agent provenance.
func TestReconcileExactSessionStartDrainAckParksRecoveredProvenanceRace(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)
	provider := &unattendedStopProvider{Fake: runtime.NewFake(), stopErrors: []error{errors.New("stop must not be reached")}}
	if err := provider.Start(t.Context(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	writer, ok := env.store.(beads.ConditionalWriter)
	if !ok {
		t.Fatal("test store does not implement conditional writer")
	}
	raceDrainAt := env.clk.Now().UTC().Add(time.Second).Format(time.RFC3339)
	params := exactSessionStartTestParams(t, env)
	params.Store = &drainAckAtomicCloseStore{Store: env.store}
	params.Provider = provider
	params.StatusWriter = &provenanceRaceConditionalWriter{
		ConditionalWriter: writer,
		store:             env.store,
		mutate: func() error {
			return env.store.Update(bead.ID, beads.UpdateOpts{Metadata: beads.StringMap{
				"drain_at":                                        raceDrainAt,
				session.DrainAckSourceMetadataKey:                 session.DrainAckSourceAgentValue,
				session.DrainAckRequesterSessionIDMetadataKey:     bead.ID,
				session.DrainAckRequesterInstanceTokenMetadataKey: drainAckTestInstanceToken,
			}})
		},
	}
	params.AsyncStopTracker = &asyncStartTracker{}
	installRecoveredDrainAckLeaseForTest(&params, bead.ID)

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("reconcile raced marker = (%v, %v), want keyed pending", owner, err)
	}
	if !params.AsyncStopTracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("unexpected async stop did not settle")
	}
	if calls := provider.stopSnapshot(); len(calls) != 0 {
		t.Fatalf("provider stop calls = %#v, want none after unproven provenance race", calls)
	}
	row, getErr := env.store.Get(bead.ID)
	if getErr != nil {
		t.Fatalf("read raced stop-pending row: %v", getErr)
	}
	if got := row.Metadata["drain_at"]; got != raceDrainAt {
		t.Fatalf("raced drain_at = %q, want concurrent value %q preserved", got, raceDrainAt)
	}
}

type provenanceRaceConditionalWriter struct {
	beads.ConditionalWriter
	store  beads.Store
	mutate func() error
}

func (w *provenanceRaceConditionalWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	if err := w.mutate(); err != nil {
		return err
	}
	return errors.New("conditional write lost after concurrent update")
}

func TestCityRuntimeKeyedDrainAckStopParksAttachedThenRetriesDetached(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	bead, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}
	// A second live record deliberately makes the runtime name ambiguous. Strict
	// keyed STOP already owns fixture.info.ID, so it must not re-resolve that durable identity
	// through the reusable runtime name or fall back to a runtime-only handle.
	if _, err := fixture.store.Create(beads.Bead{
		Title:  "ambiguous sibling",
		Type:   session.BeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: beads.StringMap{
			"session_name":   fixture.info.SessionName,
			"agent_name":     "ambiguous-sibling",
			"template":       "worker",
			"generation":     "1",
			"instance_token": "replacement-token",
			"state":          string(session.StateActive),
		},
	}); err != nil {
		t.Fatalf("create ambiguous sibling: %v", err)
	}
	provider := &unattendedStopProvider{
		Fake:       fixture.provider,
		stopErrors: []error{errors.New("session has an attached client"), nil},
	}
	secondStopEntered := make(chan struct{})
	releaseSecondStop := make(chan struct{})
	secondStopReleased := false
	provider.beforeStop = func(index int) {
		if index == 1 {
			close(secondStopEntered)
			<-releaseSecondStop
		}
	}
	t.Cleanup(func() {
		if !secondStopReleased {
			close(releaseSecondStop)
		}
	})
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}

	fixture.cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
	awaitClose(t, secondStopEntered, "detached keyed retry to reach its destructive gate")
	if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 0 {
		t.Fatalf("attached provider Stop calls = %d, want 0", got)
	}
	if info, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID); getErr != nil || !isDrainAckStopPendingInfo(info) {
		t.Fatal("attached unattended-stop failure cleared the durable stop marker")
	}

	close(releaseSecondStop)
	secondStopReleased = true
	awaitCond(t, func() bool {
		info, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
		return getErr == nil && !isDrainAckStopPendingInfo(info)
	}, "detached keyed stop finalization")
	if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 1 {
		t.Fatalf("detached provider Stop calls = %d, want exactly 1", got)
	}
	calls := provider.stopSnapshot()
	if len(calls) != 2 {
		t.Fatalf("unattended stop calls = %#v, want attached attempt plus detached retry", calls)
	}
	for _, call := range calls {
		if call.name != fixture.info.SessionName || call.expectedToken != fixture.info.InstanceToken {
			t.Fatalf("unattended stop call = %#v, want exact runtime %s/%s for session %s", call, fixture.info.SessionName, fixture.info.InstanceToken, fixture.info.ID)
		}
	}
}

func TestCityRuntimeKeyedDrainAckStopParksWithoutUnattendedStopper(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)
	provider := &freshLivenessProvider{
		Fake: runtime.NewFake(),
		sequence: []runtime.Liveness{
			{Running: true, Alive: true, Complete: true},
			{Complete: true},
		},
		fresh: runtime.Liveness{Complete: true},
	}
	if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "drain-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	params.RolloutMode = rollout.Auto
	tracker := &asyncStartTracker{}
	params.AsyncStopTracker = tracker
	installRecoveredDrainAckLeaseForTest(&params, bead.ID)
	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
		t.Fatalf("owner/error = %v/%v, want keyed/pending", owner, err)
	}
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("unsupported keyed stop did not settle")
	}
	if got := provider.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0 without unattended stop support", got)
	}
	if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("missing unattended stopper cleared the durable stop marker")
	}
}

func TestCityRuntimeKeyedDrainAckStopRecertifiesBeforeEveryRekill(t *testing.T) {
	for _, test := range []struct {
		name    string
		stopErr error
	}{
		{name: "newly attached", stopErr: errors.New("session became attached")},
		{name: "replacement", stopErr: errors.New("session identity was replaced")},
		{name: "certified pane disappeared", stopErr: fmt.Errorf("reading certified pane: %w", runtime.ErrSessionNotFound)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
			markExactDrainAckStopPendingForFixture(t, fixture)
			bead, err := fixture.store.Get(fixture.info.ID)
			if err != nil {
				t.Fatalf("read durable stop-pending session: %v", err)
			}
			provider := &unattendedStopProvider{
				Fake: fixture.provider,
			}
			provider.stopError = func(index int) error {
				if index == 0 {
					return nil
				}
				return test.stopErr
			}
			thirdStopEntered := make(chan struct{})
			releaseThirdStop := make(chan struct{})
			thirdStopReleased := false
			provider.beforeStop = func(index int) {
				if index == 2 {
					close(thirdStopEntered)
					<-releaseThirdStop
				}
			}
			t.Cleanup(func() {
				if !thirdStopReleased {
					close(releaseThirdStop)
				}
			})
			provider.StopLeavesRunning[fixture.info.SessionName] = true
			fixture.cr.sp = provider
			fixture.cr.dops = newDrainOps(provider)
			fixture.cr.cs.mu.Lock()
			fixture.cr.cs.sp = provider
			fixture.cr.cs.mu.Unlock()
			t.Cleanup(fixture.cr.stopSessionStartController)
			if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
				t.Fatalf("ensure session-start controller: %v", err)
			}

			fixture.cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
			awaitClose(t, thirdStopEntered, "keyed stop retry after failed recertification")
			if got := provider.CountCalls("Stop", fixture.info.SessionName); got != 1 {
				t.Fatalf("provider Stop calls = %d, want initial kill only", got)
			}
			if info, getErr := sessionFrontDoor(fixture.store).Get(fixture.info.ID); getErr != nil || !isDrainAckStopPendingInfo(info) {
				t.Fatal("failed re-kill unattended stop cleared the durable stop marker")
			}
			fixture.cr.stopSessionStartController()
			close(releaseThirdStop)
			thirdStopReleased = true
			if !fixture.cr.asyncStops.wait(testutil.GoroutineRaceTimeout) {
				t.Fatal("blocked keyed retry did not settle after controller stop")
			}
		})
	}
}

func TestReconcileExactSessionStartAuthoritativeDrainAckDeathFinalizes(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	incarnationStartedAt := bead.CreatedAt.Add(time.Minute).UTC().Truncate(time.Second)
	markExactDrainAckStopPendingForTest(env, &bead)
	env.setSessionMetadata(&bead, map[string]string{
		"last_woke_at": incarnationStartedAt.Format(time.RFC3339),
	})
	params := exactSessionStartTestParams(t, env)
	params.Store = &drainAckAtomicCloseStore{Store: env.store}
	provider := &freshLivenessProvider{
		Fake:  env.sp,
		fresh: runtime.Liveness{Complete: true},
	}
	params.Provider = provider
	params.RolloutMode = rollout.Auto
	installRecoveredDrainAckLeaseForTest(&params, bead.ID)

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionAntiEntropy,
	}, params)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner/error = %v/%v, want keyed/nil", owner, err)
	}
	if isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("authoritative dead observation did not finalize the durable stop marker")
	}
	if got := provider.lastTarget().IncarnationStartedAt; !got.Equal(incarnationStartedAt) {
		t.Fatalf("fresh-dead incarnation boundary = %v, want %v", got, incarnationStartedAt)
	}
}

func TestDrainAckIncarnationStartedAtDoesNotUseAdoptedBeadCreation(t *testing.T) {
	createdAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	if got := drainAckIncarnationStartedAt(session.Info{CreatedAt: createdAt}); !got.IsZero() {
		t.Fatalf("adopted bead creation time = %v, want no trusted runtime boundary", got)
	}

	awakeStartedAt := createdAt.Add(time.Minute)
	got := drainAckIncarnationStartedAt(session.Info{
		CreatedAt:      createdAt,
		AwakeStartedAt: awakeStartedAt.Format(time.RFC3339),
	})
	if !got.Equal(awakeStartedAt) {
		t.Fatalf("awake-interval boundary = %v, want %v", got, awakeStartedAt)
	}

	wokeAt := awakeStartedAt.Add(time.Minute)
	got = drainAckIncarnationStartedAt(session.Info{
		CreatedAt:      createdAt,
		AwakeStartedAt: awakeStartedAt.Format(time.RFC3339),
		LastWokeAt:     wokeAt.Format(time.RFC3339),
	})
	if !got.Equal(wokeAt) {
		t.Fatalf("wake-attempt boundary = %v, want %v", got, wokeAt)
	}
}

func TestQueueExactDrainAckAsyncStopCompletionPanicDoesNotLeakTracker(t *testing.T) {
	provider := &freshLivenessProvider{
		Fake:  runtime.NewFake(),
		fresh: runtime.Liveness{Complete: true},
	}
	if err := provider.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "drain-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	tracker := &asyncStartTracker{}
	if !queueExactDrainAckAsyncStop("", beads.NewMemStore(), provider, &config.City{}, "gcs-stop", "worker", "drain-token", nil, time.Time{}, tracker, io.Discard, func() error { return nil }, func(drainAckAsyncStopCompletion) {
		panic("completion callback failure")
	}) {
		t.Fatal("queue exact drain-ack stop = false, want true")
	}
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("completion callback panic leaked the async-stop tracker")
	}
}

func TestQueueExactDrainAckAsyncStopWithoutEffectAuthorizationParks(t *testing.T) {
	provider := &freshLivenessProvider{Fake: runtime.NewFake(), fresh: runtime.Liveness{Running: true, Alive: true, Complete: true}}
	tracker := &asyncStartTracker{}
	if queueExactDrainAckAsyncStop("", beads.NewMemStore(), provider, &config.City{}, "gcs-stop", "worker", "drain-token", nil, time.Time{}, tracker, io.Discard, nil, nil) {
		t.Fatal("queue exact drain-ack stop without effect authorization = true, want parked")
	}
	if got := provider.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0", got)
	}
}

func TestQueueExactDrainAckAsyncStopRetainsTrackerThroughCompletion(t *testing.T) {
	provider := &freshLivenessProvider{Fake: runtime.NewFake(), fresh: runtime.Liveness{Running: true, Alive: true, Complete: true}}
	tracker := &asyncStartTracker{}
	completed := make(chan bool, 1)
	if !queueExactDrainAckAsyncStop("", beads.NewMemStore(), provider, &config.City{}, "gcs-stop", "worker", "drain-token", nil, time.Time{}, tracker, io.Discard, func() error {
		return errDrainAckAsyncStopYielded
	}, func(drainAckAsyncStopCompletion) {
		completed <- tracker.drainAckStopInFlight(drainAckAsyncStopKey("gcs-stop", "worker"))
	}) {
		t.Fatal("queue exact drain-ack stop = false, want true")
	}
	select {
	case held := <-completed:
		if !held {
			t.Fatal("completion observed released async-stop tracker key")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("exact drain-ack stop did not complete")
	}
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("async-stop tracker did not release after completion")
	}
}

func TestQueueExactDrainAckAsyncStopConfirmedCompletionRetainsTrackerThroughCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
		bead := env.createSessionBead("worker", "worker")
		provider := &unattendedStopProvider{Fake: env.sp}
		if err := provider.Start(t.Context(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
			t.Fatalf("start runtime: %v", err)
		}
		if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "drain-token"); err != nil {
			t.Fatalf("set runtime token: %v", err)
		}
		tracker := &asyncStartTracker{}
		releaseCompletion := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseCompletion) }) }
		t.Cleanup(release)
		type completionObservation struct {
			completion drainAckAsyncStopCompletion
			held       bool
		}
		observed := make(chan completionObservation, 1)
		if !queueExactDrainAckAsyncStop("", env.store, provider, env.cfg, bead.ID, "worker", "drain-token", nil, time.Time{}, tracker, io.Discard, func() error { return nil }, func(got drainAckAsyncStopCompletion) {
			observed <- completionObservation{
				completion: got,
				held:       tracker.drainAckStopInFlight(drainAckAsyncStopKey(bead.ID, "worker")),
			}
			<-releaseCompletion
		}) {
			t.Fatal("queue exact drain-ack stop = false, want true")
		}
		got := <-observed
		if got.completion != drainAckAsyncStopConfirmed {
			t.Fatalf("completion = %v, want confirmed", got.completion)
		}
		if !got.held {
			t.Fatal("confirmed completion released async-stop tracker key before its callback returned")
		}

		waitReturned := make(chan bool, 1)
		go func() { waitReturned <- tracker.wait(-1) }()
		synctest.Wait()
		select {
		case <-waitReturned:
			t.Fatal("confirmed completion left its callback untracked")
		default:
		}

		release()
		synctest.Wait()
		if !<-waitReturned {
			t.Fatal("async-stop tracker did not settle after confirmed completion")
		}
		if tracker.drainAckStopInFlight(drainAckAsyncStopKey(bead.ID, "worker")) {
			t.Fatal("confirmed completion retained async-stop tracker key after its callback returned")
		}
	})
}

func TestSessionStartControllerZeroDelayRetryDoesNotDuplicateDrainAckStop(t *testing.T) {
	env := newReconcilerTestEnv()
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)
	provider := &unattendedStopProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := provider.SetMeta("worker", "GC_INSTANCE_TOKEN", "drain-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}

	tracker := &asyncStartTracker{}
	completionEntered := make(chan struct{})
	retryObserved := make(chan struct{})
	releaseCompletion := make(chan struct{})
	var completionStarted atomic.Bool
	var completionCalls atomic.Int32
	var retryQueued atomic.Bool
	var retryOnce sync.Once
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0),
		Reconcile: func(context.Context, sessionStartAdmission) error {
			afterCompletion := completionStarted.Load()
			queued := queueExactDrainAckAsyncStop(
				"", env.store, provider, env.cfg, bead.ID, "worker", "drain-token", nil, time.Time{},
				tracker, io.Discard, func() error { return nil }, func(drainAckAsyncStopCompletion) {
					if completionCalls.Add(1) == 1 {
						completionStarted.Store(true)
						close(completionEntered)
						<-releaseCompletion
					}
				},
			)
			if afterCompletion {
				retryOnce.Do(func() {
					retryQueued.Store(queued)
					close(retryObserved)
				})
			} else if !queued {
				// Hold the zero-delay refusal count below the escalation
				// threshold (ga-f7v2ft.173) while the initial completion is
				// still pending: the retry this test observes is the one AFTER
				// the completion callback has entered, and an escalated
				// obligation would slow-park before it arrives.
				<-completionEntered
			}
			return errSessionStartPoolDrainAckPending
		},
	})
	controllerStopped := false
	completionReleased := false
	t.Cleanup(func() {
		if !controllerStopped {
			controller.Stop()
		}
		if !completionReleased {
			close(releaseCompletion)
		}
		tracker.wait(testutil.GoroutineRaceTimeout)
	})
	lease := routedWorkPoolDrainAckLease{
		SessionID:              bead.ID,
		InstanceToken:          "drain-token",
		RequesterSessionID:     bead.ID,
		RequesterInstanceToken: "drain-token",
		ControllerGeneration:   1,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     1,
	}
	if _, err := controller.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("admit drain acknowledgement: %v", err)
	}
	awaitClose(t, completionEntered, "initial drain-ack STOP completion callback")
	awaitClose(t, retryObserved, "zero-delay retry while initial STOP completion is blocked")
	if retryQueued.Load() {
		t.Fatal("zero-delay retry queued a duplicate STOP while the initial completion callback was blocked")
	}
	if got := len(provider.stopSnapshot()); got != 1 {
		t.Fatalf("unattended STOP effects = %d, want exactly 1 after zero-delay retry", got)
	}
	if !tracker.drainAckStopInFlight(drainAckAsyncStopKey(bead.ID, "worker")) {
		t.Fatal("initial STOP tracker released before its completion callback returned")
	}

	controller.Stop()
	controllerStopped = true
	close(releaseCompletion)
	completionReleased = true
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("initial drain-ack STOP did not release after completion")
	}
}

func TestReconcileExactSessionStartDrainAckRetainsUncertainOrUnsupportedObservation(t *testing.T) {
	for _, test := range []struct {
		name         string
		provider     runtime.Provider
		mode         rollout.Mode
		agentAck     bool
		legacyMarker bool
		wantOwner    exactSessionStartOwner
		wantPending  bool
	}{
		{
			name:        "incomplete fresh observation",
			provider:    &freshLivenessProvider{Fake: runtime.NewFake(), fresh: runtime.Liveness{}},
			mode:        rollout.Auto,
			agentAck:    true,
			wantOwner:   exactSessionStartKeyedOwner,
			wantPending: true,
		},
		{
			name:         "auto unsupported confirmed legacy marker falls back",
			provider:     runtime.NewFake(),
			mode:         rollout.Auto,
			legacyMarker: true,
			wantOwner:    exactSessionStartLegacyOwner,
		},
		{
			name:         "require unsupported confirmed legacy marker parks",
			provider:     runtime.NewFake(),
			mode:         rollout.Require,
			legacyMarker: true,
			wantOwner:    exactSessionStartKeyedOwner,
			wantPending:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			bead := env.createSessionBead("worker", "worker")
			markDrainAckStopPendingForTest(env, &bead)
			params := exactSessionStartTestParams(t, env)
			params.Provider = test.provider
			params.RolloutMode = test.mode
			lease := installRecoveredDrainAckLeaseForTest(&params, bead.ID)
			params.RecoverPoolDrainAck = func(session.Info) (routedWorkPoolDrainAckLease, bool, bool, error) {
				return lease, test.agentAck, test.legacyMarker, nil
			}

			owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
				SessionID: bead.ID,
				Source:    sessionStartAdmissionAntiEntropy,
			}, params)
			if owner != test.wantOwner || errors.Is(err, errSessionStartPoolDrainAckPending) != test.wantPending || (err != nil && !test.wantPending) {
				t.Fatalf("owner/error = %v/%v, want %v (pending=%t)", owner, err, test.wantOwner, test.wantPending)
			}
			if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
				t.Fatal("uncertain or unsupported observation finalized the durable marker")
			}
		})
	}
}

func TestReconcileExactSessionStartDrainAckFinalizationRetriesWithoutStop(t *testing.T) {
	env := newDrainAckAtomicCloseTestEnv()
	store := &drainAckAtomicCloseStore{Store: env.store}
	env.store = store
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markExactDrainAckStopPendingForTest(env, &bead)
	if err := env.store.SetMetadataBatch(bead.ID, map[string]string{
		poolManagedMetadataKey: boolMetadata(true),
	}); err != nil {
		t.Fatalf("mark stop-pending session pool managed: %v", err)
	}
	params := exactSessionStartTestParams(t, env)
	params.Provider = &freshLivenessProvider{Fake: env.sp, fresh: runtime.Liveness{Complete: true}}
	params.RolloutMode = rollout.Auto
	installRecoveredDrainAckLeaseForTest(&params, bead.ID)
	beforeFailure, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read stop-pending row before failed atomic close: %v", err)
	}
	store.closeErr = errors.New("atomic close failed")

	if _, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionAntiEntropy}, params); err == nil {
		t.Fatal("failed durable finalization returned nil error")
	}
	if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("failed durable finalization cleared the marker")
	}
	afterFailure, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read stop-pending row after failed atomic close: %v", err)
	}
	if !reflect.DeepEqual(afterFailure, beforeFailure) || afterFailure.Status != "open" {
		t.Fatalf("failed atomic close changed stop-pending row:\n got: %#v\nwant: %#v", afterFailure, beforeFailure)
	}
	store.closeErr = nil
	if _, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionAntiEntropy}, params); err != nil {
		t.Fatalf("retry durable finalization: %v", err)
	}
	if isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("retry did not finalize the durable marker")
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0 for durable finalization retries", got)
	}
}

func TestReconcileExactSessionStartStaleWakeYieldsToDrainAckStopPending(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	params := exactSessionStartTestParams(t, env)
	entered := make(chan struct{})
	release := make(chan struct{})
	params.ObserveLoadedSession = func(context.Context, string, beads.Store, runtime.Provider, *config.City, session.Info, []string) (worker.LiveObservation, error) {
		close(entered)
		<-release
		return worker.LiveObservation{}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- reconcileExactSessionStart(context.Background(), sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionInProcess}, params)
	}()
	awaitClose(t, entered, "exact pre-wake reread")
	markDrainAckStopPendingForTest(env, &bead)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile exact stale wake: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("stale wake reconciliation did not finish")
	}
	if got := env.sp.CountCalls("Start", "worker"); got != 0 {
		t.Fatalf("provider Start calls = %d, want 0", got)
	}
	if !isDrainAckStopPendingInfo(env.sessionInfo(bead.ID)) {
		t.Fatal("stale wake changed the durable stop-pending marker")
	}
}

func TestCityRuntimeSessionStartControllerDrainAckStopRetainsGenerationLease(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	fixture.cr.cs.sessionStartLeaseMu.Lock()
	baselineLeases := fixture.cr.cs.sessionStartLeases
	fixture.cr.cs.sessionStartLeaseMu.Unlock()
	markExactDrainAckStopPendingForFixture(t, fixture)
	blocking := newBlockingStopProvider()
	blocking.Fake = fixture.provider
	provider := &freshBlockingStopProvider{blocking}
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	providerReleased := false
	t.Cleanup(func() {
		if !providerReleased {
			close(provider.releaseStop)
		}
	})
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	if outcome, err := fixture.cr.sessionStartController.AdmitPoolDrainAck(fixture.lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit drain acknowledgement = (%q, %v), want accepted", outcome, err)
	}
	select {
	case <-provider.stopStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("keyed drain-ack stop did not block in provider")
	}
	fixture.cr.cs.sessionStartLeaseMu.Lock()
	leases := fixture.cr.cs.sessionStartLeases
	fixture.cr.cs.sessionStartLeaseMu.Unlock()
	if leases <= baselineLeases {
		t.Fatalf("generation leases while stop is blocked = %d, want more than fixture baseline %d", leases, baselineLeases)
	}
	close(provider.releaseStop)
	providerReleased = true
	if !fixture.cr.asyncStops.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("keyed drain-ack stop did not complete")
	}
	awaitCond(t, func() bool {
		fixture.cr.cs.sessionStartLeaseMu.Lock()
		defer fixture.cr.cs.sessionStartLeaseMu.Unlock()
		return fixture.cr.cs.sessionStartLeases == baselineLeases
	}, "generation leases to return to fixture baseline after drain-ack stop completion")
}

func TestCityRuntimeProviderReloadDefersForKeyedDrainAckStop(t *testing.T) {
	provider := &freshBlockingStopProvider{newBlockingStopProvider()}
	fixture := newSessionStartProviderSwapFixture(t, provider, rollout.Auto)
	cr := fixture.cr
	oldConfig := cr.cfg
	store := &drainAckAtomicCloseStore{Store: beads.NewAtomicCloseMemStore()}
	cr.cs.mu.Lock()
	cr.cs.cityBeadStore = store
	cr.cs.mu.Unlock()
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	closeReleased := false
	store.beforeCAS = func() {
		close(closeEntered)
		<-releaseClose
	}
	t.Cleanup(func() {
		if !closeReleased {
			close(releaseClose)
		}
	})
	unlimited := -1
	oldConfig.Agents = []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: &unlimited}}
	work, err := store.Create(beads.Bead{
		Title:  "completed routed work",
		Type:   "task",
		Status: "open",
		Metadata: beads.StringMap{
			"gc.routed_to": "worker",
		},
	})
	if err != nil {
		t.Fatalf("create routed work: %v", err)
	}
	if err := store.Close(work.ID); err != nil {
		t.Fatalf("close routed work: %v", err)
	}
	sourceStore := "city:test-city"
	sessionName := "worker-1"
	metadata := session.DrainAckStopPendingPatch(time.Now().UTC())
	metadata["session_name"] = sessionName
	metadata["agent_name"] = sessionName
	metadata["template"] = "worker"
	metadata["generation"] = "1"
	metadata["instance_token"] = "drain-token"
	metadata[poolManagedMetadataKey] = boolMetadata(true)
	metadata[beadmeta.TriggerBeadIDMetadataKey] = work.ID
	metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = sourceStore
	bead, err := store.Create(beads.Bead{
		Title:    "worker",
		Type:     session.BeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: beads.StringMap(metadata),
	})
	if err != nil {
		t.Fatalf("create drain-ack session: %v", err)
	}
	if err := store.SetMetadataBatch(bead.ID, map[string]string{
		session.DrainAckSourceMetadataKey:                 session.DrainAckSourceAgentValue,
		session.DrainAckRequesterSessionIDMetadataKey:     bead.ID,
		session.DrainAckRequesterInstanceTokenMetadataKey: "drain-token",
	}); err != nil {
		t.Fatalf("add exact durable drain-ack provenance: %v", err)
	}
	if err := provider.Start(context.Background(), sessionName, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":                   bead.ID,
		"GC_INSTANCE_TOKEN":               "drain-token",
		reconcilerDrainAckSourceKey:       drainAckSourceAgentValue,
		drainAckRequesterSessionIDKey:     bead.ID,
		drainAckRequesterInstanceTokenKey: "drain-token",
		"GC_DRAIN_ACK":                    "1",
	} {
		if err := provider.SetMeta(sessionName, key, value); err != nil {
			t.Fatalf("set runtime metadata %s: %v", key, err)
		}
	}
	cr.poolMembershipShadow = newPoolMembershipIndex()
	if !cr.poolMembershipShadow.publishRebuild(0, newPoolMembershipState()) {
		t.Fatal("publish empty pool membership")
	}
	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatalf("read stop-pending pool session: %v", err)
	}
	if err := cr.poolMembershipShadow.replace(oldConfig, info); err != nil {
		t.Fatalf("publish pool session membership: %v", err)
	}
	observation, occupied := cr.poolMembershipShadow.observeOccupiedMember("worker", bead.ID)
	if !occupied {
		t.Fatal("stop-pending pool session is not an occupied member")
	}
	lease := routedWorkPoolDrainAckLease{
		SessionID:              bead.ID,
		InstanceToken:          "drain-token",
		RequesterSessionID:     bead.ID,
		RequesterInstanceToken: "drain-token",
		ControllerGeneration:   cr.cs.sessionStartGeneration,
		PoolTarget:             "worker",
		WorkID:                 work.ID,
		SourceStore:            sourceStore,
		MembershipRevision:     observation.revision,
	}
	snapshot, release, err := cr.cs.acquireSessionStartSnapshot()
	if err != nil {
		t.Fatalf("acquire drain-ack authorization snapshot: %v", err)
	}
	authorized, _, authorizeErr := cr.authorizeRoutedWorkPoolDrainAck(snapshot, info, lease)
	release()
	if authorizeErr != nil || !authorized {
		t.Fatalf("baseline drain-ack authorization = (%t, %v), want true", authorized, authorizeErr)
	}
	t.Cleanup(cr.stopSessionStartController)
	providerReleased := false
	t.Cleanup(func() {
		if !providerReleased {
			close(provider.releaseStop)
		}
	})
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
	select {
	case <-provider.stopStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("keyed drain-ack stop did not block in provider")
	}

	writeCityRuntimeConfig(t, fixture.tomlPath, "fail")
	lastProviderName := "fake"
	reloadDone := make(chan reloadControlReply, 1)
	go func() {
		reloadDone <- cr.reloadConfigTraced(context.Background(), &lastProviderName, fixture.cityPath, nil, reloadSourceManual)
	}()
	var firstReply reloadControlReply
	select {
	case firstReply = <-reloadDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("provider reload did not defer while keyed drain-ack stop was blocked")
	}
	if firstReply.Outcome == reloadOutcomeApplied {
		t.Fatalf("blocked drain-ack provider reload outcome = %q, want non-applied: %+v", firstReply.Outcome, firstReply)
	}
	if got := provider.CountCalls("ListRunning", ""); got != 0 {
		t.Fatalf("old-provider ListRunning calls while stop blocked = %d, want 0", got)
	}
	if lastProviderName != "fake" || cr.sp != provider || cr.cfg != oldConfig {
		t.Fatal("deferred provider reload changed the active provider or config")
	}
	if cr.sessionStartController == nil || cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		t.Fatal("deferred provider reload did not restore the old keyed controller")
	}
	done, tracking := cr.asyncStops.startDrainAckStop("fresh-tracker")
	if !tracking {
		t.Fatal("deferred provider reload left the async-stop tracker unavailable")
	}
	done()

	close(provider.releaseStop)
	providerReleased = true
	select {
	case <-closeEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("atomic terminal close did not enter after confirmed drain-ack death")
	}
	cr.cs.mu.Lock()
	blockedGeneration := cr.cs.sessionStartGeneration
	blockedStoreGeneration := cr.cs.sessionStartStoreGeneration
	blockedStore := cr.cs.cityBeadStore
	admissionInstalled := cr.cs.sessionStartEventAdmission != nil
	cr.cs.mu.Unlock()
	if !admissionInstalled {
		t.Fatal("keyed session-start admission was not installed before barrier reload")
	}
	barrierDone := make(chan reloadControlReply, 1)
	go func() {
		barrierDone <- cr.reloadConfigTraced(context.Background(), &lastProviderName, fixture.cityPath, nil, reloadSourceManual)
	}()
	awaitCond(t, func() bool {
		cr.cs.mu.Lock()
		defer cr.cs.mu.Unlock()
		return cr.cs.sessionStartEventAdmission == nil
	}, "provider reload to stop keyed session-start admission before its terminal close")
	select {
	case reply := <-barrierDone:
		t.Fatalf("provider reload returned before atomic terminal close: %+v", reply)
	default:
	}
	cr.cs.mu.Lock()
	currentGeneration := cr.cs.sessionStartGeneration
	currentStoreGeneration := cr.cs.sessionStartStoreGeneration
	currentStore := cr.cs.cityBeadStore
	cr.cs.mu.Unlock()
	if currentGeneration != blockedGeneration || currentStoreGeneration != blockedStoreGeneration || currentStore != blockedStore {
		t.Fatalf("atomic close barrier published a generation/store change: got generation/store-generation/store %d/%d/%p, want %d/%d/%p", currentGeneration, currentStoreGeneration, currentStore, blockedGeneration, blockedStoreGeneration, blockedStore)
	}
	if lastProviderName != "fake" || cr.sp != provider || cr.cfg != oldConfig {
		t.Fatal("atomic close barrier reload changed the active provider or config")
	}
	if got := provider.CountCalls("Stop", sessionName); got != 1 {
		t.Fatalf("provider Stop calls at atomic close barrier = %d, want 1", got)
	}
	close(releaseClose)
	closeReleased = true
	awaitCond(t, func() bool { return !hasInFlightDrainAckStops(&cr.asyncStops) }, "keyed drain-ack stop completion")
	awaitCond(t, func() bool {
		row, err := store.Get(bead.ID)
		return err == nil && row.Status == "closed" && row.Metadata["state"] == "drained" &&
			row.Metadata["close_reason"] == session.CanonicalCloseReason("drained") && row.Metadata["closed_at"] != ""
	}, "durable drain-ack finalization after deferred reload")

	select {
	case barrierReply := <-barrierDone:
		if barrierReply.Outcome != reloadOutcomeApplied {
			t.Fatalf("provider reload after atomic close = %q, want applied: %+v", barrierReply.Outcome, barrierReply)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("provider reload did not complete after atomic terminal close")
	}
	cr.cs.mu.Lock()
	completedGeneration := cr.cs.sessionStartGeneration
	completedStoreGeneration := cr.cs.sessionStartStoreGeneration
	cr.cs.mu.Unlock()
	if completedGeneration <= blockedGeneration || completedStoreGeneration != completedGeneration {
		t.Fatalf("completed reload generation/store-generation = %d/%d, want coherent advance after %d", completedGeneration, completedStoreGeneration, blockedGeneration)
	}
	if got := provider.CountCalls("Stop", sessionName); got != 1 {
		t.Fatalf("provider Stop calls after reload = %d, want exactly the original stop", got)
	}
	if got := provider.CountCalls("ListRunning", ""); got != 1 {
		t.Fatalf("old-provider ListRunning calls after stop = %d, want 1", got)
	}
}

func TestCityRuntimeSessionStartControllerDrainAckStopPendingHasOneProviderEntry(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	bead, err := fixture.store.Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable drain acknowledgement stop-pending: %v", err)
	}
	blocking := newBlockingStopProvider()
	blocking.Fake = fixture.provider
	provider := &freshBlockingStopProvider{blocking}
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	providerReleased := false
	t.Cleanup(func() {
		if !providerReleased {
			close(provider.releaseStop)
		}
	})
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}

	if outcome, err := fixture.cr.sessionStartController.AdmitPoolDrainAck(fixture.lease); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit drain acknowledgement = (%q, %v), want accepted", outcome, err)
	}
	select {
	case <-provider.stopStarted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("keyed drain-ack stop did not enter provider")
	}
	fixture.cr.cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))
	stopInfo, err := sessionFrontDoor(fixture.store).Get(bead.ID)
	if err != nil {
		t.Fatalf("read blocked drain acknowledgement stop: %v", err)
	}
	finalizeDrainAckStopPendingSessions(
		fixture.cr.cityPath, fixture.cr.cfg, provider, beads.SessionStore{Store: fixture.store}, nil,
		[]session.Info{stopInfo}, newFakeDrainOps(), newDrainTracker(),
		&fixture.cr.asyncStops, clock.Real{}, events.Discard, io.Discard,
		fixture.cr.sessionStartLegacyExclusionPredicate(),
	)
	select {
	case got := <-provider.stopStarted:
		t.Fatalf("duplicate event or legacy prepass entered provider again for %q", got)
	default:
	}
	close(provider.releaseStop)
	providerReleased = true
}

func TestReconcileExactSessionStartDrainAckStopPendingParksInvalidIdentity(t *testing.T) {
	for _, test := range []struct {
		name                   string
		metadata               map[string]string
		runtimeToken           string
		expectedImmediateCause string
		expectedUnattended     int
	}{
		{
			name: "missing durable token",
			metadata: map[string]string{
				"state": string(session.StateDraining), "state_reason": session.DrainAckStopPendingReason, "instance_token": " ",
			},
			runtimeToken:           "drain-token",
			expectedImmediateCause: "drain acknowledgement stop lacks exact session identity",
		},
		{
			name: "missing durable name",
			metadata: map[string]string{
				"state": string(session.StateDraining), "state_reason": session.DrainAckStopPendingReason, "session_name": " ", "instance_token": "drain-token",
			},
			runtimeToken:           "drain-token",
			expectedImmediateCause: "drain acknowledgement stop lacks exact session identity",
		},
		{
			name: "runtime token mismatch",
			metadata: map[string]string{
				"state": string(session.StateDraining), "state_reason": session.DrainAckStopPendingReason, "instance_token": "drain-token",
			},
			runtimeToken:       "replacement-token",
			expectedUnattended: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			bead := env.createSessionBead("worker", "worker")
			metadata := session.DrainAckStopPendingPatch(env.clk.Now().UTC())
			for key, value := range test.metadata {
				metadata[key] = value
			}
			env.setSessionMetadata(&bead, metadata)
			if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("start runtime: %v", err)
			}
			if err := env.sp.SetMeta("worker", "GC_INSTANCE_TOKEN", test.runtimeToken); err != nil {
				t.Fatalf("set runtime token: %v", err)
			}

			params := exactSessionStartTestParams(t, env)
			writer, ok := env.store.(beads.ConditionalWriter)
			if !ok {
				t.Fatal("test store does not implement conditional writer")
			}
			provider := &unattendedStopProvider{
				Fake: env.sp,
				stopError: func(int) error {
					actual, getErr := env.sp.GetMeta("worker", "GC_INSTANCE_TOKEN")
					if getErr != nil {
						return getErr
					}
					if actual != drainAckTestInstanceToken {
						return fmt.Errorf("runtime instance token = %q, want %q", actual, drainAckTestInstanceToken)
					}
					return nil
				},
			}
			params.Store = &drainAckAtomicCloseStore{Store: env.store}
			params.Provider = provider
			params.StatusWriter = writer
			tracker := &asyncStartTracker{}
			params.AsyncStopTracker = tracker
			installRecoveredDrainAckLeaseForTest(&params, bead.ID)
			owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{SessionID: bead.ID, Source: sessionStartAdmissionInProcess}, params)
			if owner != exactSessionStartKeyedOwner || !errors.Is(err, errSessionStartPoolDrainAckPending) {
				t.Fatalf("owner/error = %v/%v, want keyed/pending", owner, err)
			}
			if test.expectedImmediateCause != "" && !strings.Contains(err.Error(), test.expectedImmediateCause) {
				t.Fatalf("invalid identity error = %v, want cause %q", err, test.expectedImmediateCause)
			}
			if !tracker.wait(testutil.GoroutineRaceTimeout) {
				t.Fatal("invalid drain-ack identity reconciliation did not settle")
			}
			if calls := provider.stopSnapshot(); len(calls) != test.expectedUnattended {
				t.Fatalf("unattended stop calls = %#v, want %d", calls, test.expectedUnattended)
			} else if test.expectedUnattended == 1 && calls[0].expectedToken != drainAckTestInstanceToken {
				t.Fatalf("unattended stop token = %q, want durable token %q", calls[0].expectedToken, drainAckTestInstanceToken)
			}
			if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
				t.Fatalf("provider Stop calls = %d, want 0", got)
			}
			info := env.sessionInfo(bead.ID)
			if !isDrainAckStopPendingInfo(info) {
				t.Fatalf("invalid marker was not retained: %#v", info)
			}
		})
	}
}

func TestCityRuntimeSessionStartControllerRetriesDrainAckStopPendingFromAntiEntropy(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	markExactDrainAckStopPendingForFixture(t, fixture)
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}
	provider := &sequenceGetMetaProvider{Fake: fixture.provider}
	fixture.cr.sp = provider
	fixture.cr.dops = newDrainOps(provider)
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.mu.Unlock()
	fixture.provider.StopErrors[info.SessionName] = errors.New("kill failed")
	t.Cleanup(fixture.cr.stopSessionStartController)

	// The fresh controller starts with no in-memory drain lease. Its
	// authoritative seed must reconstruct one from exact runtime metadata.
	if err := fixture.cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos([]session.Info{info})); err != nil {
		t.Fatalf("ensure session-start controller: %v", err)
	}
	awaitCond(t, func() bool { return fixture.provider.CountCalls("Stop", info.SessionName) >= 1 }, "recovered keyed drain-ack stop")
	retained, ok := fixture.cr.sessionStartController.readAdmission(info.ID)
	if !ok || retained.PoolDrainAck == nil || *retained.PoolDrainAck != fixture.lease {
		t.Fatalf("recovered admission = %+v, want lease %+v", retained, fixture.lease)
	}
	current, err := sessionFrontDoor(fixture.store).Get(info.ID)
	if err != nil {
		t.Fatalf("read retained stop-pending session: %v", err)
	}
	if !isDrainAckStopPendingInfo(current) {
		t.Fatal("failed kill cleared the durable stop-pending marker")
	}
	delete(fixture.provider.StopErrors, info.SessionName)
	awaitCond(t, func() bool {
		current, getErr := sessionFrontDoor(fixture.store).Get(info.ID)
		return getErr == nil && !isDrainAckStopPendingInfo(current)
	}, "recovered drain acknowledgement to retry and finalize")
	if got := fixture.provider.CountCalls("Stop", info.SessionName); got < 2 {
		t.Fatalf("provider Stop calls = %d, want failed attempt plus recovered retry", got)
	}
}

func TestCityRuntimeSessionStartControllerParksUncertainDrainAckAfterRestart(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, session.DrainAckStopPendingPatch(time.Now().UTC())); err != nil {
		t.Fatalf("mark durable drain acknowledgement stop-pending: %v", err)
	}
	if err := fixture.provider.RemoveMeta(fixture.info.SessionName, reconcilerDrainAckSourceKey); err != nil {
		t.Fatalf("remove drain acknowledgement provenance: %v", err)
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read durable stop-pending session: %v", err)
	}
	provider := &sequenceGetMetaProvider{Fake: fixture.provider}
	fixture.cr.sp = provider
	fixture.cr.cs.mu.Lock()
	fixture.cr.cs.sp = provider
	fixture.cr.cs.rolloutFlags = rollout.ForTest(rollout.WithSessionReconciler(rollout.Require))
	fixture.cr.cs.mu.Unlock()
	t.Cleanup(fixture.cr.stopSessionStartController)
	if err := fixture.cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos([]session.Info{info})); err != nil {
		t.Fatalf("ensure required session-start controller: %v", err)
	}
	awaitCond(t, func() bool {
		admission, ok := fixture.cr.sessionStartController.readAdmission(info.ID)
		return ok && admission.PoolDrainAck == nil && admission.PoolDrainAckUncertain &&
			fixture.cr.sessionStartController.queue.NumRequeues(info.ID) > 0
	}, "uncertain restart admission to remain retryable")
	if got := fixture.provider.CountCalls("Stop", info.SessionName); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0 for uncertain restart provenance", got)
	}
	excluded := fixture.cr.sessionStartLegacyExclusionPredicate()
	if excluded == nil || !excluded(info) {
		t.Fatal("uncertain restart provenance yielded the stop-pending row to legacy")
	}
}

func TestDrainAckStopPendingOffModeKeepsLegacyProviderEntry(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	bead := env.createSessionBead("worker", "worker")
	markDrainAckStopPendingForTest(env, &bead)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := env.sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "drain-token"); err != nil {
		t.Fatalf("set runtime token: %v", err)
	}
	cr := &CityRuntime{sessionStartOwnership: sessionStartOwnershipLegacy}
	tracker := &asyncStartTracker{}
	finalizeDrainAckStopPendingSessions(
		"", env.cfg, env.sp, beads.SessionStore{Store: env.store}, nil, []session.Info{env.sessionInfo(bead.ID)},
		newFakeDrainOps(), env.dt, tracker, env.clk, events.Discard, io.Discard,
		cr.sessionStartLegacyExclusionPredicate(),
	)
	if !tracker.wait(testutil.GoroutineRaceTimeout) {
		t.Fatal("legacy drain-ack provider entry did not complete")
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 1 {
		t.Fatalf("legacy provider Stop calls = %d, want 1", got)
	}
}

func TestCityRuntimeSessionStartControllerAutoFallsBackLoudly(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, false)
	var stderr bytes.Buffer
	cr.stderr = &stderr

	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("auto ensureSessionStartController: %v", err)
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipLegacy {
		t.Fatalf("ownership = %v, want loud legacy fallback", cr.sessionStartOwnershipState())
	}
	if !strings.Contains(stderr.String(), "falling back to legacy") || !strings.Contains(stderr.String(), "session store") {
		t.Fatalf("auto fallback diagnostic = %q, want legacy fallback and store reason", stderr.String())
	}
}

func TestCityRuntimeSessionStartControllerAutoRefusesAmbiguousAdmissionOwner(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, true)
	existingAdmission := func(string) {}
	if err := cr.cs.installSessionStartEventAdmission(existingAdmission); err != nil {
		t.Fatalf("install existing admission: %v", err)
	}
	t.Cleanup(cr.cs.stopSessionStartEventAdmission)

	err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil))
	if err == nil || !strings.Contains(err.Error(), "callback is already installed") {
		t.Fatalf("ensureSessionStartController error = %v, want ambiguous-admission refusal", err)
	}
	if cr.sessionStartController != nil {
		t.Fatal("ambiguous admission owner left a second keyed controller installed")
	}
	cr.cs.mu.RLock()
	gotAdmission := cr.cs.sessionStartEventAdmission
	cr.cs.mu.RUnlock()
	if gotAdmission == nil {
		t.Fatal("ambiguous admission refusal removed the existing owner's callback")
	}
}

func TestCityRuntimeSessionStartControllerRequireFailsClosed(t *testing.T) {
	cr := newSessionStartCityRuntimeForTest(t, rollout.Require, false)

	err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil))
	if err == nil {
		t.Fatal("require mode started without a coherent session store")
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipRequiredBlocked {
		t.Fatalf("ownership = %v, want required-blocked", cr.sessionStartOwnershipState())
	}
	option := cr.sessionStartLegacyExclusionOption()
	if option == nil {
		t.Fatal("require failure left legacy starts enabled")
	}
	opts := startExecutionOptions{}
	option(&opts)
	info := session.Info{
		ID:          "gcs-require1",
		Type:        session.BeadType,
		Template:    "worker",
		WakeRequest: string(session.WakeCauseExplicit),
	}
	if opts.legacyStartExcluded == nil || !opts.legacyStartExcluded(info) {
		t.Fatal("require failure did not fail closed for a keyed-owned start")
	}
}

func TestCityRuntimeSessionStartControllerStartsAndCommitsSeededWake(t *testing.T) {
	env := newReconcilerTestEnv()
	openConditionalStore := func() beads.Store {
		opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
			Provider: "file", ConditionalWrites: gate.Auto,
			OpenFileStore: func() (beads.Store, error) { return beads.NewMemStore(), nil },
		})
		if err != nil {
			t.Fatalf("open conditional-write store: %v", err)
		}
		return opened.Store
	}
	env.store = openConditionalStore()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	exactStatusCh := make(chan exactSessionLifecycleStatusResult, 2)
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				exactStatusCh <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)

	seed := newSessionBeadSnapshotFromInfos([]session.Info{env.sessionInfo(bead.ID)})
	if err := cr.ensureSessionStartController(context.Background(), seed); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	awaitCond(t, func() bool {
		return env.sessionInfo(bead.ID).MetadataState == string(session.StateActive)
	}, "seeded exact wake to commit active")
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("provider Start calls = %d, want 1", got)
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		t.Fatalf("ownership = %v, want keyed", cr.sessionStartOwnershipState())
	}
	if cr.sessionStartLegacyExclusionOption() == nil {
		t.Fatal("keyed controller did not install legacy exclusion")
	}
	var exactStatus exactSessionLifecycleStatusResult
	select {
	case exactStatus = <-exactStatusCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("exact status observer did not receive the seeded reconciliation result")
	}
	if exactStatus.ControllerGeneration != 1 || exactStatus.AdmissionVersion == 0 || exactStatus.Context != exactSessionLifecycleStatusContextDesired {
		t.Fatalf("exact status composition result = %#v, want generation-1 result", exactStatus)
	}

	controller := cr.sessionStartController
	env.store = openConditionalStore()
	swapBead := env.createSessionBead("worker-swap", "worker")
	swapPatch := session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())
	swapPatch["session_key"] = "resume"
	if err := env.store.SetMetadataBatch(swapBead.ID, swapPatch); err != nil {
		t.Fatalf("request replacement-store heal: %v", err)
	}
	if err := env.sp.Start(context.Background(), "worker-swap", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("seed replacement-store runtime: %v", err)
	}
	swapInitial, err := env.store.Get(swapBead.ID)
	if err != nil {
		t.Fatalf("read replacement-store session: %v", err)
	}
	releaseSwap := cs.beginSessionStartGenerationSwap()
	cs.mu.Lock()
	cs.advanceSessionStartGenerationLocked()
	cs.cityBeadStore = env.store
	cs.sessionStartStoreGeneration = cs.sessionStartGeneration
	cs.mu.Unlock()
	releaseSwap()
	if cr.sessionStartController != controller {
		t.Fatal("store-generation swap restarted the keyed controller")
	}
	if _, err := controller.Admit(swapBead.ID, sessionStartAdmissionInProcess); err != nil {
		t.Fatalf("admit replacement-store heal: %v", err)
	}
	var swapStatus exactSessionLifecycleStatusResult
	select {
	case swapStatus = <-exactStatusCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement-store heal did not reconcile")
	}
	swapFinal, err := env.store.Get(swapBead.ID)
	if err != nil {
		t.Fatalf("read healed replacement-store session: %v", err)
	}
	if swapStatus.LoadedRevision != swapInitial.Revision || swapFinal.Revision != swapInitial.Revision+1 ||
		swapFinal.Metadata["state"] != string(session.StateAwake) {
		t.Fatalf("replacement-store heal revision/state = %d/%q from %d, want one fenced awake heal", swapFinal.Revision, swapFinal.Metadata["state"], swapInitial.Revision)
	}

	env.addDesired("worker-guard", "worker", true)
	guardBead := env.createSessionBead("worker-guard", "worker")
	if err := env.store.SetMetadataBatch(guardBead.ID, swapPatch); err != nil {
		t.Fatalf("request legacy-guard wake: %v", err)
	}
	guardBefore, err := env.store.Get(guardBead.ID)
	if err != nil {
		t.Fatalf("read legacy-guard session: %v", err)
	}
	env.startOptions = append(env.startOptions, cr.sessionStartLegacyExclusionOption())
	if woken := env.reconcile([]beads.Bead{guardBefore}); woken != 0 {
		t.Fatalf("legacy guard wake attempts = %d, want 0", woken)
	}
	guardAfter, err := env.store.Get(guardBead.ID)
	if err != nil {
		t.Fatalf("read guarded session: %v", err)
	}
	if guardAfter.Metadata["state"] != string(session.StateAsleep) {
		t.Fatalf("legacy desired heal was not excluded: state = %q, want asleep", guardAfter.Metadata["state"])
	}

	t.Run("concurrent update fences exact heal before effects", func(t *testing.T) {
		fenceEnv := newReconcilerTestEnv()
		fenceEnv.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
		fenceBead := fenceEnv.createSessionBead("worker", "worker")
		fenceEnv.setSessionMetadata(&fenceBead, map[string]string{
			"wake_request": string(session.WakeCauseExplicit),
			"state":        string(session.StateAsleep),
			"session_key":  "resume",
		})
		initial, err := fenceEnv.store.Get(fenceBead.ID)
		if err != nil {
			t.Fatalf("read initial fenced session: %v", err)
		}
		counting := newExactStatusCountingStore(t, fenceEnv.store)
		params := exactSessionStartTestParams(t, fenceEnv)
		params.Generation, params.Store, params.StatusWriter = 1, counting, counting
		params.ObserveLoadedSession = func(context.Context, string, beads.Store, runtime.Provider, *config.City, session.Info, []string) (worker.LiveObservation, error) {
			if err := fenceEnv.store.Update(fenceBead.ID, beads.UpdateOpts{Metadata: map[string]string{"state": string(session.StateQuarantined)}}); err != nil {
				t.Fatalf("commit concurrent update: %v", err)
			}
			return worker.LiveObservation{Running: true, Alive: true}, nil
		}
		owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
			SessionID: fenceBead.ID, Source: sessionStartAdmissionExplicitWake, Version: 1,
		}, params)
		if owner != exactSessionStartKeyedOwner || !beads.IsPreconditionFailed(err) {
			t.Fatalf("owner/error = %d/%v, want keyed/wrapped precondition", owner, err)
		}
		if counting.gets != 1 || counting.lists != 0 || counting.extraWrites != 1 || len(counting.Calls()) != 0 {
			t.Fatalf("get/list/CAS/ordinary = %d/%d/%d/%d, want 1/0/1/0", counting.gets, counting.lists, counting.extraWrites, len(counting.Calls()))
		}
		final, err := fenceEnv.store.Get(fenceBead.ID)
		if err != nil {
			t.Fatalf("read final fenced session: %v", err)
		}
		if final.Revision != initial.Revision+1 || final.Metadata["state"] != string(session.StateQuarantined) ||
			len(fenceEnv.sp.SnapshotCalls()) != 0 {
			t.Fatalf("fenced final revision/state/provider = %d/%q/%#v, want concurrent row preserved and no effects", final.Revision, final.Metadata["state"], fenceEnv.sp.SnapshotCalls())
		}
	})

	cr.stopSessionStartController()
	select {
	case extra := <-exactStatusCh:
		t.Fatalf("exact status observer received an extra result: %#v", extra)
	default:
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipLegacy {
		t.Fatalf("ownership after stop = %v, want legacy for auto mode", cr.sessionStartOwnershipState())
	}
	cs.mu.RLock()
	admission := cs.sessionStartEventAdmission
	cs.mu.RUnlock()
	if admission != nil {
		t.Fatal("session-event admission remained installed after child stop")
	}
}

func TestCityRuntimeSessionStartEventStartsWithoutFleetTick(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	status := make(chan exactSessionLifecycleStatusResult, 1)
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		trace:    trace,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}

	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	bead, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("get event bead: %v", err)
	}
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))

	awaitCond(t, func() bool {
		return env.sessionInfo(bead.ID).MetadataState == string(session.StateActive)
	}, "event-admitted exact wake to commit active")
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("provider Start calls = %d, want 1 without a fleet tick", got)
	}
	var exactStatus exactSessionLifecycleStatusResult
	select {
	case exactStatus = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("event-admitted exact start did not report status")
	}
	if exactStatus.Plan == nil || exactStatus.Plan.Outcome != sessionLifecycleStatusNoop ||
		exactStatus.Plan.Reason != sessionLifecycleStatusReasonConverged {
		t.Fatalf("missing-runtime exact status = %#v, want converged no-op before provider start", exactStatus)
	}
	if exactStatus.RuntimeLive {
		t.Fatalf("missing-runtime exact status = %#v, want runtime_live=false", exactStatus)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read start trace: %v", err)
	}
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			t.Fatalf("start event emitted false no-effect status witness: %#v", record)
		}
	}
}

func TestCityRuntimeSessionStartEventRecordsConvergedStatusShadowWithoutEffects(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, map[string]string{
		"state":        string(session.StateAwake),
		"wake_request": string(session.WakeCauseExplicit),
	}); err != nil {
		t.Fatalf("configure active wake: %v", err)
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("seed live runtime: %v", err)
	}
	before := exactStatusStoreState(t, env.store)
	store := newExactStatusCountingStore(t, env.store)
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, store, rollout.Auto)
	status := make(chan exactSessionLifecycleStatusResult, 1)
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		trace:    trace,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	beforeCalls := len(env.sp.SnapshotCalls())
	eventBead, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read post-commit event bead: %v", err)
	}
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, eventBead))

	var got exactSessionLifecycleStatusResult
	select {
	case got = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("event-admitted converged status did not report")
	}
	if got.Admission.Source != sessionStartAdmissionInProcess || got.AdmissionVersion == 0 || got.ControllerGeneration != 1 ||
		!got.RuntimeLive || got.Disposition != exactSessionLifecycleStatusDispositionCandidate || got.Plan == nil ||
		got.Plan.Outcome != sessionLifecycleStatusNoop || got.Plan.Reason != sessionLifecycleStatusReasonConverged {
		t.Fatalf("status result = %#v, want event-admitted converged no-op candidate", got)
	}
	if store.lists != 0 {
		t.Fatalf("store List calls = %d, want 0", store.lists)
	}
	requireExactStatusStoreUnchanged(t, before, store)
	readOnlyProviderCalls := map[string]bool{
		"GetLastActivity": true,
		"GetMeta":         true,
		"IsAttached":      true,
		"IsRunning":       true,
	}
	for _, call := range env.sp.SnapshotCalls()[beforeCalls:] {
		if !readOnlyProviderCalls[call.Method] {
			t.Fatalf("provider call after event = %#v, want only read-only observation", call)
		}
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read detached shadow trace: %v", err)
	}
	var witnesses []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			witnesses = append(witnesses, record)
		}
	}
	if len(witnesses) != 1 {
		t.Fatalf("status-shadow witnesses = %#v, want one", witnesses)
	}
	witness := witnesses[0]
	if witness.OutcomeCode != TraceOutcomeNoChange || witness.Fields["session_id"] != bead.ID ||
		witness.Fields["admission"] != string(sessionStartAdmissionInProcess) ||
		witness.Fields["admission_version"] != float64(got.AdmissionVersion) ||
		witness.Fields["generation"] != float64(got.ControllerGeneration) ||
		witness.Fields["status_outcome"] != "noop" ||
		witness.Fields["status_reason"] != string(sessionLifecycleStatusReasonConverged) ||
		witness.Fields["effect_applied"] != false {
		t.Fatalf("status-shadow witness = %#v, want converged detached event witness", witness)
	}
}

func TestCityRuntimeSessionStartSocketRecordsConvergedStatusShadowWithoutEffects(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, map[string]string{
		"state":        string(session.StateAwake),
		"wake_request": string(session.WakeCauseExplicit),
	}); err != nil {
		t.Fatalf("configure active wake: %v", err)
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("seed live runtime: %v", err)
	}
	before := exactStatusStoreState(t, env.store)
	store := newExactStatusCountingStore(t, env.store)
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, store, rollout.Auto)
	eventProv := cs.eventProv.(*events.Fake)
	status := make(chan exactSessionLifecycleStatusResult, 1)
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		trace:    trace,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	beforeCalls := len(env.sp.SnapshotCalls())
	if reply := cr.admitSessionStartSocketKey(bead.ID); reply != sessionStartSocketReplyOK {
		t.Fatalf("socket admission reply = %q, want %q", reply, sessionStartSocketReplyOK)
	}

	var got exactSessionLifecycleStatusResult
	select {
	case got = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("socket-admitted converged status did not report")
	}
	if got.Admission.Source != sessionStartAdmissionSocket || got.AdmissionVersion == 0 || got.ControllerGeneration != 1 ||
		!got.RuntimeLive || got.Disposition != exactSessionLifecycleStatusDispositionCandidate ||
		got.Reason != exactSessionLifecycleStatusReasonCandidate || got.Plan == nil ||
		got.Plan.Outcome != sessionLifecycleStatusNoop || got.Plan.Reason != sessionLifecycleStatusReasonConverged || got.EffectApplied {
		t.Fatalf("status result = %#v, want socket-admitted converged no-op candidate", got)
	}
	if store.gets != 2 {
		t.Fatalf("store Get calls = %d, want one admission read and one effect-time read", store.gets)
	}
	if store.lists != 0 {
		t.Fatalf("store List calls = %d, want 0", store.lists)
	}
	requireExactStatusStoreUnchanged(t, before, store)
	readOnlyProviderCalls := map[string]bool{
		"GetLastActivity": true,
		"GetMeta":         true,
		"IsAttached":      true,
		"IsRunning":       true,
	}
	for _, call := range env.sp.SnapshotCalls()[beforeCalls:] {
		if !readOnlyProviderCalls[call.Method] {
			t.Fatalf("provider call after socket admission = %#v, want only read-only observation", call)
		}
	}
	recordedEvents, err := eventProv.List(events.Filter{})
	if err != nil {
		t.Fatalf("list recorded events: %v", err)
	}
	if len(recordedEvents) != 0 {
		t.Fatalf("socket shadow recorded events = %#v, want none", recordedEvents)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read socket shadow trace: %v", err)
	}
	var witnesses []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			witnesses = append(witnesses, record)
		}
	}
	if len(witnesses) != 1 {
		t.Fatalf("socket status-shadow witnesses = %#v, want one", witnesses)
	}
	witness := witnesses[0]
	if witness.OutcomeCode != TraceOutcomeNoChange || witness.Fields["session_id"] != bead.ID ||
		witness.Fields["admission"] != string(sessionStartAdmissionSocket) ||
		witness.Fields["admission_version"] != float64(got.AdmissionVersion) ||
		witness.Fields["generation"] != float64(got.ControllerGeneration) ||
		witness.Fields["status_outcome"] != "noop" ||
		witness.Fields["status_reason"] != string(sessionLifecycleStatusReasonConverged) ||
		witness.Fields["effect_applied"] != false {
		t.Fatalf("socket status-shadow witness = %#v, want converged detached socket witness", witness)
	}
}

func TestCityRuntimeSessionStartSocketMissingRuntimeStartsOnceWithoutFalseShadow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	status := make(chan exactSessionLifecycleStatusResult, 1)
	terminal := make(chan sessionStartReconcileResult, 1)
	originalControllerConstructor := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		originalObserver := opts.Observer
		opts.Observer = func(result sessionStartReconcileResult) {
			if originalObserver != nil {
				originalObserver(result)
			}
			terminal <- result
		}
		return originalControllerConstructor(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = originalControllerConstructor })

	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		trace:    trace,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	if reply := cr.admitSessionStartSocketKey(bead.ID); reply != sessionStartSocketReplyOK {
		t.Fatalf("socket admission reply = %q, want %q", reply, sessionStartSocketReplyOK)
	}

	var terminalResult sessionStartReconcileResult
	select {
	case terminalResult = <-terminal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("socket-admitted missing runtime did not reach a terminal result")
	}
	if terminalResult.Outcome != sessionStartReconcileSucceeded {
		t.Fatalf("terminal result = %#v, want succeeded", terminalResult)
	}
	var exactStatus exactSessionLifecycleStatusResult
	select {
	case exactStatus = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("socket-admitted missing runtime did not report exact status")
	}
	if exactStatus.Admission.Source != sessionStartAdmissionSocket || exactStatus.RuntimeLive ||
		exactStatus.Plan == nil || exactStatus.Plan.Outcome != sessionLifecycleStatusNoop ||
		exactStatus.Plan.Reason != sessionLifecycleStatusReasonConverged {
		t.Fatalf("missing-runtime exact status = %#v, want socket converged no-op before provider start", exactStatus)
	}
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("provider Start calls = %d, want exactly 1", got)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("provider Stop calls = %d, want 0", got)
	}
	if got := env.sp.CountCalls("Nudge", "worker"); got != 0 {
		t.Fatalf("provider Nudge calls = %d, want 0", got)
	}
	if got := env.sessionInfo(bead.ID).MetadataState; got != string(session.StateActive) {
		t.Fatalf("durable session state = %q, want active", got)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read missing-runtime socket trace: %v", err)
	}
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			t.Fatalf("missing-runtime socket start emitted false no-effect status witness: %#v", record)
		}
	}
}

func TestCityRuntimeSessionStartEventAppliesOneFencedStatusHeal(t *testing.T) {
	env := newReconcilerTestEnv()
	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		Provider:          "file",
		ConditionalWrites: gate.Auto,
		OpenFileStore: func() (beads.Store, error) {
			return beads.NewMemStore(), nil
		},
	})
	if err != nil {
		t.Fatalf("open conditional-write store: %v", err)
	}
	env.store = opened.Store
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadata(bead.ID, "wake_request", string(session.WakeCauseExplicit)); err != nil {
		t.Fatalf("configure exact start ownership: %v", err)
	}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("seed live runtime: %v", err)
	}
	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read stale session: %v", err)
	}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	status := make(chan exactSessionLifecycleStatusResult, 1)
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       cs,
		trace:    trace,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
		sessionStartOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			withExactSessionLifecycleStatusObserver(func(result exactSessionLifecycleStatusResult) {
				status <- result
			}),
		},
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	beforeCalls := len(env.sp.SnapshotCalls())
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, bead))

	var got exactSessionLifecycleStatusResult
	select {
	case got = <-status:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("event-admitted stale status did not report")
	}
	if got.Plan == nil || got.Plan.Outcome != sessionLifecycleStatusHeal || got.Plan.Reason != sessionLifecycleStatusReasonHeal {
		t.Fatalf("status result = %#v, want live stale heal candidate", got)
	}
	if !got.EffectApplied {
		t.Fatalf("status result = %#v, want successful fenced heal to retain EffectApplied", got)
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("read healed session: %v", err)
	}
	if after.Revision != before.Revision+1 || after.Metadata["state"] != string(session.StateAwake) {
		t.Fatalf("healed revision/state = %d/%q from %d, want one awake heal", after.Revision, after.Metadata["state"], before.Revision)
	}
	readOnlyProviderCalls := map[string]bool{
		"GetLastActivity": true,
		"IsAttached":      true,
		"IsRunning":       true,
	}
	for _, call := range env.sp.SnapshotCalls()[beforeCalls:] {
		if !readOnlyProviderCalls[call.Method] {
			t.Fatalf("provider call after status heal = %#v, want only read-only observation", call)
		}
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read applied status trace: %v", err)
	}
	var witnesses []SessionReconcilerTraceRecord
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			t.Fatalf("applied status heal emitted shadow witness: %#v", record)
		}
		if record.RecordType == TraceRecordMutation && record.SiteCode == TraceSiteMutationBeadMetadata &&
			record.Fields["session_id"] == bead.ID && record.Fields["effect_applied"] == true {
			witnesses = append(witnesses, record)
		}
	}
	if len(witnesses) != 1 {
		t.Fatalf("applied status witnesses = %#v, want exactly one", witnesses)
	}
	witness := witnesses[0]
	if witness.OutcomeCode != TraceOutcomeApplied || witness.Fields["admission"] != string(sessionStartAdmissionInProcess) ||
		witness.Fields["admission_version"] != float64(got.AdmissionVersion) || witness.Fields["generation"] != float64(got.ControllerGeneration) ||
		witness.Fields["status_outcome"] != "heal" || witness.Fields["status_reason"] != string(sessionLifecycleStatusReasonHeal) {
		t.Fatalf("applied status witness = %#v, want fenced applied event metadata mutation", witness)
	}
	// The status heal is a keyed effect like any other, so it carries the keyed
	// ownership stamp. Without it the record claims an applied effect that no
	// engine owns, and every keyed-effect filter — the parity join's role
	// classifier, the soak census — steps over it even as it fires
	// (ga-f7v2ft.161).
	if witness.Fields["effect_owner"] != detectorKeyedEffectOwner {
		t.Fatalf("applied status witness effect_owner = %#v, want %q", witness.Fields["effect_owner"], detectorKeyedEffectOwner)
	}
}

func TestRecordExactSessionLifecycleStatusShadowUsesAdmissionToObservationLatency(t *testing.T) {
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		trace:    trace,
		stderr:   io.Discard,
	}
	admittedAt := time.Now().UTC().Add(-time.Second)
	observedAt := admittedAt.Add(137 * time.Millisecond)
	result := exactSessionLifecycleStatusResult{
		Admission: sessionStartAdmission{
			SessionID:  "gcs-latency",
			Source:     sessionStartAdmissionInProcess,
			Version:    3,
			AdmittedAt: admittedAt,
		},
		AdmissionVersion:     3,
		ControllerGeneration: 7,
		RequestedID:          "gcs-latency",
		LoadedID:             "gcs-latency",
		Context:              exactSessionLifecycleStatusContextDesired,
		ObservedAt:           observedAt,
		RuntimeLive:          true,
		Disposition:          exactSessionLifecycleStatusDispositionCandidate,
		Reason:               exactSessionLifecycleStatusReasonCandidate,
		Plan: &sessionLifecycleStatusPlan{
			SessionID: "gcs-latency",
			Outcome:   sessionLifecycleStatusNoop,
			Reason:    sessionLifecycleStatusReasonConverged,
		},
	}
	cr.recordExactSessionLifecycleStatusShadow(&config.City{}, result)

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read status latency trace: %v", err)
	}
	var cycleStart, witness *SessionReconcilerTraceRecord
	for i := range records {
		switch {
		case records[i].RecordType == TraceRecordCycleStart:
			cycleStart = &records[i]
		case records[i].RecordType == TraceRecordOperation && records[i].SiteCode == TraceSiteLifecycleStatusShadow:
			witness = &records[i]
		}
	}
	if cycleStart == nil || witness == nil {
		t.Fatalf("latency trace records = %#v, want cycle start and status witness", records)
	}
	if !cycleStart.Ts.Equal(admittedAt) {
		t.Fatalf("status-shadow cycle start = %s, want admission %s", cycleStart.Ts, admittedAt)
	}
	if witness.DurationMS != 137 {
		t.Fatalf("status-shadow duration_ms = %d, want admission-to-observation 137", witness.DurationMS)
	}

	for _, timing := range []struct {
		admittedAt time.Time
		observedAt time.Time
	}{
		{observedAt: observedAt},
		{admittedAt: admittedAt},
		{admittedAt: observedAt, observedAt: admittedAt},
	} {
		result.Admission.AdmittedAt = timing.admittedAt
		result.ObservedAt = timing.observedAt
		cr.recordExactSessionLifecycleStatusShadow(&config.City{}, result)
	}
	records, err = ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("read status trace after invalid timing: %v", err)
	}
	witnesses := 0
	for _, record := range records {
		if record.RecordType == TraceRecordOperation && record.SiteCode == TraceSiteLifecycleStatusShadow {
			witnesses++
		}
	}
	if witnesses != 1 {
		t.Fatalf("status-shadow witnesses after invalid timing = %d, want original valid witness only", witnesses)
	}
}

func TestCityRuntimeSessionStartEventOverflowRequestsLegacyFallback(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	pokeCh := make(chan struct{}, 1)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstID := ""
	original := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.MaxDistinct = 1
		opts.Reconcile = func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == firstID {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		}
		return original(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = original })
	cr := &CityRuntime{
		cfg:    env.cfg,
		sp:     env.sp,
		cs:     cs,
		pokeCh: pokeCh,
		stdout: io.Discard,
		stderr: io.Discard,
	}
	t.Cleanup(cr.stopSessionStartController)
	t.Cleanup(func() { close(releaseFirst) })
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	first := env.createSessionBead("gcs-event-overflow-first", "worker")
	second := env.createSessionBead("gcs-event-overflow-second", "worker")
	firstID = first.ID
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, first))
	awaitClose(t, firstEntered, "first event reconciliation")
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, second))

	select {
	case <-pokeCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("overflowed session event did not request immediate legacy fallback")
	}
	if !cr.sessionStartController.TakeAuditRequest() {
		t.Fatal("overflowed session event did not preserve the authoritative audit request")
	}
}

func TestCityRuntimeSessionStartEventOverflowDoesNotBlockOnFullFallback(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	pokeCh := make(chan struct{}, 1)
	pokeCh <- struct{}{}
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	release := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
	firstID := ""
	original := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.MaxDistinct = 1
		opts.Reconcile = func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == firstID {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		}
		return original(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = original })
	cr := &CityRuntime{cfg: env.cfg, sp: env.sp, cs: cs, pokeCh: pokeCh, stdout: io.Discard, stderr: io.Discard}
	t.Cleanup(cr.stopSessionStartController)
	t.Cleanup(release)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	first := env.createSessionBead("gcs-event-full-first", "worker")
	second := env.createSessionBead("gcs-event-full-second", "worker")
	firstID = first.ID
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, first))
	awaitClose(t, firstEntered, "first event reconciliation")
	eventDone := make(chan struct{})
	go func() {
		cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, second))
		close(eventDone)
	}()
	awaitClose(t, eventDone, "overflow event with full fallback channel")
	if !cr.sessionStartController.TakeAuditRequest() {
		t.Fatal("full fallback channel cleared the authoritative audit request")
	}
}

type sessionStartBlockingListStore struct {
	beads.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *sessionStartBlockingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return s.Store.List(query)
}

func TestCityRuntimeSessionStartOverflowDoesNotDeadlockControllerStop(t *testing.T) {
	baseStore := beads.NewMemStore()
	store := &sessionStartBlockingListStore{
		Store:   baseStore,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	sp := runtime.NewFake()
	cs := coherentSessionStartControllerStateForTest(cfg, sp, store, rollout.Auto)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	releaseWorker := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(releaseWorker)

	firstID := ""
	original := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.MaxDistinct = 1
		opts.Reconcile = func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == firstID {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		}
		return original(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = original })

	cr := &CityRuntime{cfg: cfg, sp: sp, cs: cs, pokeCh: make(chan struct{}, 1), stdout: io.Discard, stderr: io.Discard}
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	first, err := baseStore.Create(beads.Bead{ID: "gcs-stop-overflow-first", Type: session.BeadType})
	if err != nil {
		t.Fatalf("create first session: %v", err)
	}
	second, err := baseStore.Create(beads.Bead{ID: "gcs-stop-overflow-second", Type: session.BeadType})
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	firstID = first.ID
	cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, first))
	awaitClose(t, firstEntered, "first event reconciliation")

	eventDone := make(chan struct{})
	go func() {
		cs.admitSessionStartEvent(beadEventForSessionStartTest(t, events.BeadUpdated, second))
		close(eventDone)
	}()
	awaitClose(t, store.entered, "overflow snapshot read")

	stopDone := make(chan struct{})
	go func() {
		cr.stopSessionStartController()
		close(stopDone)
	}()
	// "Stop is underway" is now the LIFECYCLE lock, which stop holds for its
	// whole body. It used to be sessionStartMu, which stop no longer holds
	// across its drain — ga-f7v2ft.143 moved the drain out from under the very
	// lock its in-flight workers need, which is the inversion this test's own
	// name is about.
	awaitCond(t, func() bool {
		if cr.sessionStartLifecycleMu.TryLock() {
			cr.sessionStartLifecycleMu.Unlock()
			return false
		}
		return true
	}, "controller stop to take the lifecycle lock")

	close(store.release)
	awaitClose(t, eventDone, "overflow event during controller stop")
	releaseWorker()
	awaitClose(t, stopDone, "controller stop after overflow event")
}

func TestCityRuntimeSessionStartSeedOverflowDoesNotRequestLegacyFallback(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(cfg, runtime.NewFake(), beads.NewMemStore(), rollout.Auto)
	pokeCh := make(chan struct{}, 1)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	original := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.MaxDistinct = 1
		opts.Reconcile = func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == "gcs-seed-overflow-first" {
				close(firstEntered)
				<-releaseFirst
			}
			return nil
		}
		return original(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = original })
	cr := &CityRuntime{cfg: cfg, sp: cs.sp, cs: cs, pokeCh: pokeCh, stdout: io.Discard, stderr: io.Discard}
	t.Cleanup(cr.stopSessionStartController)
	t.Cleanup(func() { close(releaseFirst) })
	seed := newSessionBeadSnapshotFromInfos([]session.Info{
		{ID: "gcs-seed-overflow-first", Type: session.BeadType, Template: "worker", WakeRequest: string(session.WakeCauseExplicit)},
		{ID: "gcs-seed-overflow-second", Type: session.BeadType, Template: "worker", WakeRequest: string(session.WakeCauseExplicit)},
	})
	if err := cr.ensureSessionStartController(context.Background(), seed); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	awaitClose(t, firstEntered, "first seed reconciliation")
	select {
	case <-pokeCh:
		t.Fatal("seed-time overflow requested legacy fallback")
	default:
	}
}

func TestCityRuntimeSessionStartSeedLeavesHeadroomForExactWakeBeyondAdmissionLimit(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(cfg, runtime.NewFake(), beads.NewMemStore(), rollout.Auto)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	release := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
	reconciled := make(chan string, sessionStartControllerMaxDistinct+2)
	firstID := "gcs-seed-0000"
	original := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.Workers = 1
		opts.Reconcile = func(_ context.Context, admission sessionStartAdmission) error {
			if admission.SessionID == firstID {
				close(firstEntered)
				<-releaseFirst
			}
			reconciled <- admission.SessionID
			return nil
		}
		return original(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = original })
	cr := &CityRuntime{cfg: cfg, sp: cs.sp, cs: cs, stdout: io.Discard, stderr: io.Discard}
	t.Cleanup(cr.stopSessionStartController)
	t.Cleanup(release)

	infos := make([]session.Info, 0, sessionStartControllerMaxDistinct+1)
	for i := 0; i <= sessionStartControllerMaxDistinct; i++ {
		infos = append(infos, session.Info{
			ID:          fmt.Sprintf("gcs-seed-%04d", i),
			Type:        session.BeadType,
			Template:    "worker",
			WakeRequest: string(session.WakeCauseExplicit),
		})
	}
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(infos)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	awaitClose(t, firstEntered, "first authoritative seed reconciliation")
	controller := cr.sessionStartController
	if got := controller.Pending(); got != 1 {
		t.Fatalf("pending authoritative keys while first reconciliation is blocked = %d, want 1", got)
	}
	if outcome, err := controller.Admit("gcs-exact-wake", sessionStartAdmissionExplicitWake); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit exact wake = %q, %v, want accepted", outcome, err)
	}
	if got := controller.Pending(); got != 2 {
		t.Fatalf("pending keys after exact wake = %d, want 2", got)
	}

	release()
	first := receiveSessionStartID(t, reconciled)
	if first != firstID {
		t.Fatalf("first reconciliation = %q, want %q", first, firstID)
	}
	if second := receiveSessionStartID(t, reconciled); second != "gcs-exact-wake" {
		t.Fatalf("second reconciliation = %q, want immediate exact wake", second)
	}

	seen := map[string]bool{firstID: true, "gcs-exact-wake": true}
	for len(seen) < len(infos)+1 {
		id := receiveSessionStartID(t, reconciled)
		if seen[id] {
			t.Fatalf("session %q reconciled more than once", id)
		}
		seen[id] = true
	}
	for _, info := range infos {
		if !seen[info.ID] {
			t.Fatalf("authoritative seed session %q was not reconciled", info.ID)
		}
	}
}

func receiveSessionStartID(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(hangBudget):
		t.Fatal("timed out waiting for session-start reconciliation")
		return ""
	}
}

func TestCityRuntimeSessionStartWorkerImmediatelyDelegatesLegacyOwnedKey(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{
		{Name: "database", StartCommand: "true"},
		{Name: "worker", StartCommand: "true", DependsOn: []string{"database"}},
	}}
	bead := env.createSessionBead("worker", "worker")
	if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
	pokeCh := make(chan struct{}, 1)
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cfg:      env.cfg,
		sp:       env.sp,
		cs:       coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
		pokeCh:   pokeCh,
		stdout:   io.Discard,
		stderr:   io.Discard,
	}
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	if _, err := cr.sessionStartController.Admit(bead.ID, sessionStartAdmissionInProcess); err != nil {
		t.Fatalf("admit legacy-owned key: %v", err)
	}

	select {
	case <-pokeCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("legacy-owned exact key did not request an immediate fleet reconcile")
	}
}

func TestCityRuntimeSessionStartReportsDiagnosticLegacyFallback(t *testing.T) {
	originalControllerConstructor := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.Reconcile = func(context.Context, sessionStartAdmission) error {
			return fmt.Errorf("%w: drain acknowledgement revision conflict", errSessionStartLegacyFallbackRequired)
		}
		return originalControllerConstructor(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = originalControllerConstructor })

	cr := newSessionStartCityRuntimeForTest(t, rollout.Auto, true)
	var stderr synchronizedBuffer
	cr.stderr = &stderr
	cr.pokeCh = make(chan struct{}, 1)
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	if _, err := cr.sessionStartController.Admit("gcs-fallback-visible", sessionStartAdmissionSocket); err != nil {
		t.Fatalf("admit diagnostic fallback: %v", err)
	}

	awaitCond(t, func() bool {
		return strings.Contains(stderr.String(), "revision conflict") && len(cr.pokeCh) == 1
	}, "diagnostic priority legacy fallback")
}

func TestCityRuntimeSessionStartRequireRetriesWithoutLegacyFallback(t *testing.T) {
	originalControllerConstructor := newCitySessionStartController
	var attempts atomic.Int32
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		opts.MaxRetries = 0
		opts.Reconcile = func(context.Context, sessionStartAdmission) error {
			attempts.Add(1)
			return errors.New("required drain acknowledgement refused")
		}
		return originalControllerConstructor(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = originalControllerConstructor })

	cr := newSessionStartCityRuntimeForTest(t, rollout.Require, true)
	var stderr synchronizedBuffer
	cr.stderr = &stderr
	cr.pokeCh = make(chan struct{}, 1)
	t.Cleanup(cr.stopSessionStartController)
	if err := cr.ensureSessionStartController(t.Context(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("ensureSessionStartController: %v", err)
	}
	lease := routedWorkPoolDrainAckLease{
		SessionID:              "gcs-required-drain",
		InstanceToken:          "required-token",
		RequesterSessionID:     "gcs-required-drain",
		RequesterInstanceToken: "required-token",
		ControllerGeneration:   1,
		PoolTarget:             "worker",
		WorkID:                 "ga-work",
		SourceStore:            "city:test-city",
		MembershipRevision:     1,
	}
	if _, err := cr.sessionStartController.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("admit required drain acknowledgement: %v", err)
	}

	awaitCond(t, func() bool {
		return attempts.Load() >= 2 && strings.Contains(stderr.String(), "required drain acknowledgement refused")
	}, "required drain acknowledgement retry with visible cause past exhaustion budget")
	retained, ok := cr.sessionStartController.readAdmission(lease.SessionID)
	if !ok || retained.PoolDrainAck == nil || *retained.PoolDrainAck != lease {
		t.Fatalf("retained admission = %+v, want exact drain acknowledgement lease %+v", retained, lease)
	}
	if len(cr.pokeCh) != 0 {
		t.Fatal("require-mode drain acknowledgement retry requested legacy fallback")
	}
}

func TestCityRuntimeSessionStartAntiEntropySeedsWithoutQueueAlarm(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(cfg, runtime.NewFake(), beads.NewMemStore(), rollout.Auto)
	release := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  1,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			<-release
			return nil
		},
	})
	t.Cleanup(func() {
		close(release)
		controller.Stop()
	})
	cr := &CityRuntime{
		cfg:                    cfg,
		cs:                     cs,
		stdout:                 io.Discard,
		stderr:                 io.Discard,
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
	}
	snapshot := newSessionBeadSnapshotFromInfos([]session.Info{{
		ID:          "gcs-audit1",
		Type:        session.BeadType,
		Template:    "worker",
		WakeRequest: string(session.WakeCauseExplicit),
	}})

	cr.seedActiveSessionStartController(snapshot)

	awaitCond(t, func() bool { return controller.Pending() == 1 }, "periodic authoritative seed admission")
	if got := controller.Pending(); got != 1 {
		t.Fatalf("pending keys = %d, want 1 from periodic authoritative seed without a queue alarm", got)
	}
}

func TestCityRuntimeSessionStartRestartUsesFreshSnapshotAfterPartialSeed(t *testing.T) {
	const oldFleetSize = sessionStartSeedPageSize + 1
	maxWakes := 1
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &maxWakes},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	oldIDs := make([]string, 0, oldFleetSize)
	for i := range oldFleetSize {
		name := fmt.Sprintf("old-%03d", i)
		bead := env.createSessionBead(name, "worker")
		if err := env.store.SetMetadataBatch(bead.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
			t.Fatalf("request old wake %d: %v", i, err)
		}
		oldIDs = append(oldIDs, bead.ID)
	}

	cityPath := t.TempDir()
	cs := coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto)
	cs.cityPath = cityPath
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 env.cfg,
		sp:                  env.sp,
		cs:                  cs,
		rec:                 events.Discard,
		sessionStartOptions: env.startOptions,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	t.Cleanup(cr.stopSessionStartController)

	oldAdmission := make(chan string, 1)
	oldReconcileStarted := make(chan struct{})
	var factoryCalls atomic.Int32
	previousFactory := newCitySessionStartController
	newCitySessionStartController = func(opts sessionStartControllerOptions) (*sessionStartController, error) {
		if factoryCalls.Add(1) == 1 {
			opts.Reconcile = func(ctx context.Context, admission sessionStartAdmission) error {
				oldAdmission <- admission.SessionID
				close(oldReconcileStarted)
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return previousFactory(opts)
	}
	t.Cleanup(func() { newCitySessionStartController = previousFactory })

	oldSnapshot := cr.loadSessionBeadSnapshot()
	if got := len(oldSnapshot.OpenInfos()); got != oldFleetSize {
		t.Fatalf("old authoritative snapshot rows = %d, want %d", got, oldFleetSize)
	}
	if err := cr.ensureSessionStartController(context.Background(), oldSnapshot); err != nil {
		t.Fatalf("start old keyed child: %v", err)
	}
	oldController := cr.sessionStartController
	awaitClose(t, oldReconcileStarted, "old partial-snapshot reconciliation")
	if got := <-oldAdmission; got != oldIDs[0] {
		t.Fatalf("old partial snapshot reconciled %q, want first row %q", got, oldIDs[0])
	}

	cr.stopSessionStartController()
	if got := oldController.Pending(); got != 0 {
		t.Fatalf("stopped old controller pending admissions = %d, want 0", got)
	}

	if closed, err := env.store.CloseAll(oldIDs, nil); err != nil || closed != len(oldIDs) {
		t.Fatalf("close old durable rows = %d, %v; want %d", closed, err, len(oldIDs))
	}
	if err := env.store.Reopen(oldIDs[0]); err != nil {
		t.Fatalf("restore first durable row: %v", err)
	}
	current := env.createSessionBead("current-after-restore", "worker")
	if err := env.store.SetMetadataBatch(current.ID, session.RequestExplicitWakePatch(string(session.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request current wake: %v", err)
	}

	if err := cr.restartSessionStartController(context.Background()); err != nil {
		t.Fatalf("restart keyed child from durable authority: %v", err)
	}
	currentController := cr.sessionStartController
	if currentController == nil || currentController == oldController {
		t.Fatal("restart did not install a fresh keyed child")
	}
	awaitSessionStartSeedInactive(t, currentController, "fresh authoritative snapshot")
	awaitCond(t, func() bool { return currentController.Pending() == 0 }, "fresh snapshot reconciliations")

	if got := factoryCalls.Load(); got != 2 {
		t.Fatalf("keyed controller constructions = %d, want old and fresh", got)
	}
	if got := env.sp.CountCalls("Start", "old-000"); got != 1 {
		t.Fatalf("restored row start calls = %d, want 1 from fresh snapshot", got)
	}
	if got := env.sp.CountCalls("Start", "current-after-restore"); got != 1 {
		t.Fatalf("current row start calls = %d, want 1 from fresh snapshot", got)
	}
	for i := 1; i < oldFleetSize; i++ {
		name := fmt.Sprintf("old-%03d", i)
		if got := env.sp.CountCalls("Start", name); got != 0 {
			t.Fatalf("closed old row %q start calls = %d, want 0", name, got)
		}
	}
}

func TestCityRuntimeSessionStartConfigMutationKeepsOneOwner(t *testing.T) {
	stubSessionStartCityStoreOpen(t)
	tests := []struct {
		name       string
		oldDepends []string
		newDepends []string
	}{
		{name: "dependency added", newDepends: []string{"database"}},
		{name: "dependency removed", oldDepends: []string{"database"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheCtx, cancelCache := context.WithCancel(context.Background())
			t.Cleanup(cancelCache)
			oldCfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", DependsOn: test.oldDepends}}}
			newCfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", DependsOn: test.newDepends}}}
			cs := coherentSessionStartControllerStateForTest(oldCfg, runtime.NewFake(), beads.NewMemStore(), rollout.Auto)
			cs.cacheCtx = cacheCtx
			cr := &CityRuntime{
				cfg:                   oldCfg,
				cs:                    cs,
				sessionStartMode:      rollout.Auto,
				sessionStartOwnership: sessionStartOwnershipKeyed,
				stdout:                io.Discard,
				stderr:                io.Discard,
			}
			info := session.Info{
				ID:          "gcs-config-transition1",
				Type:        session.BeadType,
				Template:    "worker",
				WakeRequest: string(session.WakeCauseExplicit),
			}

			assertSingleSessionStartOwner(t, cr, info, oldCfg)
			cs.updateWithPendingConfigMutation(newCfg, cs.sp, "next-revision")

			if _, _, err := cs.acquireSessionStartSnapshot(); err == nil {
				t.Fatal("keyed owner remained available while runtime config application was pending")
			}
			// The option itself is no longer empty during the handoff: WD.2's
			// D-DEADLINE yield rides on it and is installed whenever a
			// controller exists, because an admitted deadline key outlives this
			// window. What must stand down is the START exclusion.
			var opts startExecutionOptions
			if option := cr.sessionStartLegacyExclusionOption(); option != nil {
				option(&opts)
			}
			if opts.legacyStartExcluded != nil {
				t.Fatal("auto mode did not temporarily return pending config ownership to legacy")
			}

			cr.cfg = newCfg
			cs.clearConfigMutationPending()
			assertSingleSessionStartOwner(t, cr, info, newCfg)
		})
	}
}

func TestCityRuntimeSessionStartRequireBlocksBothOwnersDuringConfigMutation(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
	cs := coherentSessionStartControllerStateForTest(cfg, runtime.NewFake(), beads.NewMemStore(), rollout.Require)
	cs.markConfigMutationPending("next-revision")
	cr := &CityRuntime{
		cfg:                   cfg,
		cs:                    cs,
		sessionStartMode:      rollout.Require,
		sessionStartOwnership: sessionStartOwnershipKeyed,
	}
	info := session.Info{
		ID:          "gcs-required-config1",
		Type:        session.BeadType,
		Template:    "worker",
		WakeRequest: string(session.WakeCauseExplicit),
	}

	if _, _, err := cs.acquireSessionStartSnapshot(); err == nil {
		t.Fatal("required keyed owner acquired config while runtime application was pending")
	}
	option := cr.sessionStartLegacyExclusionOption()
	if option == nil || !legacySessionStartExcluded(option, info) {
		t.Fatal("require mode allowed legacy to enter while keyed config was unavailable")
	}
	drainAck := info
	drainAck.WakeRequest = ""
	drainAck.MetadataState = string(session.StateDraining)
	drainAck.StateReason = session.DrainAckStopPendingReason
	if !legacySessionStartExcluded(option, drainAck) {
		t.Fatal("require mode allowed legacy drain-ack provider entry while keyed config was unavailable")
	}
}

func TestCityRuntimeProviderSwapDrainsKeyedStartBeforeListingOldProvider(t *testing.T) {
	oldProvider := runtime.NewFake()
	fixture := newSessionStartProviderSwapFixture(t, oldProvider, rollout.Auto)
	cr := fixture.cr

	entered := make(chan struct{})
	release := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(entered)
			<-release
			return nil
		},
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	cr.sessionStartController = controller
	cr.sessionStartOwnership = sessionStartOwnershipKeyed
	cr.sessionStartMode = rollout.Auto
	if _, err := controller.Admit("gcs-provider-swap1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit pending keyed start: %v", err)
	}
	awaitClose(t, entered, "pending keyed start")

	writeCityRuntimeConfig(t, fixture.tomlPath, "fail")
	lastProviderName := "fake"
	var reply reloadControlReply
	reloadDone := make(chan struct{})
	go func() {
		reply = cr.reloadConfigTraced(context.Background(), &lastProviderName, fixture.cityPath, nil, reloadSourceManual)
		close(reloadDone)
	}()
	awaitCond(t, func() bool {
		controller.mu.Lock()
		stopped := controller.stopped
		controller.mu.Unlock()
		return stopped || oldProvider.CountCalls("ListRunning", "") > 0 || channelClosed(reloadDone)
	}, "provider reload to reach keyed drain or old-provider listing")

	controller.mu.Lock()
	stopped := controller.stopped
	controller.mu.Unlock()
	if !stopped {
		t.Fatal("provider reload listed or swapped the old provider before stopping the keyed child")
	}
	if got := oldProvider.CountCalls("ListRunning", ""); got != 0 {
		t.Fatalf("old-provider ListRunning calls before keyed drain = %d, want 0", got)
	}

	close(release)
	awaitClose(t, reloadDone, "provider reload after keyed drain")
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload outcome = %q, want applied: %+v", reply.Outcome, reply)
	}
	if got := oldProvider.CountCalls("ListRunning", ""); got != 1 {
		t.Fatalf("old-provider ListRunning calls = %d, want 1 after keyed drain", got)
	}
	if cr.sessionStartController == nil || cr.sessionStartController == controller {
		t.Fatal("provider reload did not restart a fresh keyed child")
	}
	if cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		t.Fatalf("ownership after provider reload = %v, want keyed", cr.sessionStartOwnershipState())
	}
}

func TestCityRuntimeProviderSwapListingFailureRestoresKeyedChild(t *testing.T) {
	oldProvider := &partialListPoolProvider{
		Fake:    runtime.NewFake(),
		listErr: errors.New("old provider unavailable"),
	}
	fixture := newSessionStartProviderSwapFixture(t, oldProvider, rollout.Auto)
	if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatalf("start old keyed child: %v", err)
	}
	oldChild := fixture.cr.sessionStartController

	writeCityRuntimeConfig(t, fixture.tomlPath, "fail")
	lastProviderName := "fake"
	reply := fixture.cr.reloadConfigTraced(context.Background(), &lastProviderName, fixture.cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reload outcome = %q, want failed: %+v", reply.Outcome, reply)
	}
	if lastProviderName != "fake" || fixture.cr.sp != oldProvider {
		t.Fatal("aborted provider swap changed the active provider")
	}
	if fixture.cr.sessionStartController == nil || fixture.cr.sessionStartController == oldChild {
		t.Fatal("aborted provider swap did not restore a fresh keyed child")
	}
	if fixture.cr.sessionStartOwnershipState() != sessionStartOwnershipKeyed {
		t.Fatalf("ownership after aborted provider swap = %v, want keyed", fixture.cr.sessionStartOwnershipState())
	}
}

func TestCityRuntimeProviderSwapRestartFailureHonorsRolloutMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          rollout.Mode
		wantOwnership sessionStartOwnership
		wantText      string
	}{
		{name: "auto degrades loudly", mode: rollout.Auto, wantOwnership: sessionStartOwnershipLegacy, wantText: "falling back to legacy"},
		{name: "require remains blocked", mode: rollout.Require, wantOwnership: sessionStartOwnershipRequiredBlocked, wantText: "keyed starts remain blocked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldProvider := runtime.NewFake()
			fixture := newSessionStartProviderSwapFixture(t, oldProvider, test.mode)
			var stderr bytes.Buffer
			fixture.cr.stderr = &stderr
			if err := fixture.cr.ensureSessionStartController(context.Background(), newSessionBeadSnapshotFromInfos(nil)); err != nil {
				t.Fatalf("start old keyed child: %v", err)
			}
			previousFactory := newCitySessionStartController
			newCitySessionStartController = func(sessionStartControllerOptions) (*sessionStartController, error) {
				return nil, errors.New("injected child restart failure")
			}
			t.Cleanup(func() {
				newCitySessionStartController = previousFactory
			})

			writeCityRuntimeConfig(t, fixture.tomlPath, "fail")
			lastProviderName := "fake"
			reply := fixture.cr.reloadConfigTraced(context.Background(), &lastProviderName, fixture.cityPath, nil, reloadSourceManual)

			if reply.Outcome != reloadOutcomeApplied {
				t.Fatalf("reload outcome = %q, want applied after committed provider swap: %+v", reply.Outcome, reply)
			}
			if lastProviderName != "fail" {
				t.Fatalf("last provider = %q, want fail", lastProviderName)
			}
			if fixture.cr.sessionStartOwnershipState() != test.wantOwnership {
				t.Fatalf("ownership = %v, want %v", fixture.cr.sessionStartOwnershipState(), test.wantOwnership)
			}
			combinedDiagnostics := stderr.String() + strings.Join(reply.Warnings, "\n")
			if !strings.Contains(combinedDiagnostics, test.wantText) || !strings.Contains(combinedDiagnostics, "injected child restart failure") {
				t.Fatalf("diagnostics = %q, want %q and injected failure", combinedDiagnostics, test.wantText)
			}
		})
	}
}

func TestCityRuntimeRunCoordinatesSessionStartRolloutBeforeReadiness(t *testing.T) {
	tests := []struct {
		name           string
		mode           rollout.Mode
		factoryFails   bool
		wantStarted    bool
		wantBuild      bool
		wantOwnership  sessionStartOwnership
		wantChild      bool
		wantDiagnostic string
	}{
		{
			name:          "keyed child precedes legacy startup and readiness",
			mode:          rollout.Auto,
			wantStarted:   true,
			wantBuild:     true,
			wantOwnership: sessionStartOwnershipKeyed,
			wantChild:     true,
		},
		{
			name:           "auto degradation runs legacy startup",
			mode:           rollout.Auto,
			factoryFails:   true,
			wantStarted:    true,
			wantBuild:      true,
			wantOwnership:  sessionStartOwnershipLegacy,
			wantDiagnostic: "falling back to legacy",
		},
		{
			name:           "require failure prevents readiness",
			mode:           rollout.Require,
			factoryFails:   true,
			wantOwnership:  sessionStartOwnershipRequiredBlocked,
			wantDiagnostic: "session-start controller",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stubManagedDoltStoreOpeners(t)
			cityPath := t.TempDir()
			tomlPath := filepath.Join(cityPath, "city.toml")
			writeCityRuntimeConfig(t, tomlPath, "fake")
			cfg, err := config.Load(osFS{}, tomlPath)
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			cfg.Daemon.SessionReconciler = string(test.mode)
			provider := runtime.NewFake()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			var stderr bytes.Buffer
			var started atomic.Bool
			var buildCalled atomic.Bool
			var buildOwnership sessionStartOwnership
			var buildHadChild bool
			var cr *CityRuntime
			cr = newTestCityRuntime(t, CityRuntimeParams{
				CityPath: cityPath,
				CityName: "test-city",
				TomlPath: tomlPath,
				Cfg:      cfg,
				SP:       provider,
				BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
					buildCalled.Store(true)
					buildOwnership = cr.sessionStartOwnershipState()
					cr.sessionStartMu.Lock()
					buildHadChild = cr.sessionStartController != nil
					cr.sessionStartMu.Unlock()
					return DesiredStateResult{State: map[string]TemplateParams{}}
				},
				Dops: newDrainOps(provider),
				Rec:  events.Discard,
				OnStarted: func() {
					started.Store(true)
					cancel()
				},
				Stdout: io.Discard,
				Stderr: &stderr,
			})
			cs := newControllerState(ctx, cfg, provider, events.NewFake(), "test-city", cityPath)
			cs.cityBeadStore = beads.NewMemStore()
			cr.setControllerState(cs)

			if test.factoryFails {
				previousFactory := newCitySessionStartController
				newCitySessionStartController = func(sessionStartControllerOptions) (*sessionStartController, error) {
					if test.mode == rollout.Require {
						cancel()
					}
					return nil, errors.New("injected startup child failure")
				}
				t.Cleanup(func() {
					newCitySessionStartController = previousFactory
				})
			}

			cr.run(ctx)

			if got := started.Load(); got != test.wantStarted {
				t.Fatalf("OnStarted called = %t, want %t", got, test.wantStarted)
			}
			if got := buildCalled.Load(); got != test.wantBuild {
				t.Fatalf("legacy startup build called = %t, want %t", got, test.wantBuild)
			}
			if test.wantBuild {
				if buildOwnership != test.wantOwnership || buildHadChild != test.wantChild {
					t.Fatalf("legacy startup observed ownership=%v child=%t, want ownership=%v child=%t", buildOwnership, buildHadChild, test.wantOwnership, test.wantChild)
				}
			} else if got := cr.sessionStartOwnershipState(); got != test.wantOwnership {
				t.Fatalf("ownership after refused startup = %v, want %v", got, test.wantOwnership)
			}
			if test.wantDiagnostic != "" && !strings.Contains(stderr.String(), test.wantDiagnostic) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.wantDiagnostic)
			}
		})
	}
}

func TestCityRuntimeShutdownDrainsSessionStartBeforeSessionTeardown(t *testing.T) {
	provider := runtime.NewFake()
	store := beads.NewMemStore()
	cs := coherentSessionStartControllerStateForTest(&config.City{}, provider, store, rollout.Auto)

	admissionEntered := make(chan struct{})
	releaseAdmission := make(chan struct{})
	if err := cs.installSessionStartEventAdmission(func(string) {
		close(admissionEntered)
		<-releaseAdmission
	}); err != nil {
		t.Fatalf("install event admission: %v", err)
	}
	t.Cleanup(cs.stopSessionStartEventAdmission)
	defer func() {
		select {
		case <-releaseAdmission:
		default:
			close(releaseAdmission)
		}
	}()
	eventDone := make(chan struct{})
	evt := beadEventForSessionStartTest(t, events.BeadUpdated, beads.Bead{
		ID:   "gcs-shutdown-admission1",
		Type: session.BeadType,
	})
	go func() {
		cs.admitSessionStartEvent(evt)
		close(eventDone)
	}()
	awaitClose(t, admissionEntered, "shutdown event admission")

	workerEntered := make(chan struct{})
	releaseWorker := make(chan struct{})
	controller := mustStartSessionStartController(t, sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(workerEntered)
			<-releaseWorker
			return nil
		},
	})
	t.Cleanup(controller.Stop)
	defer func() {
		select {
		case <-releaseWorker:
		default:
			close(releaseWorker)
		}
	}()
	if _, err := controller.Admit("gcs-shutdown-worker1", sessionStartAdmissionExplicitWake); err != nil {
		t.Fatalf("admit keyed shutdown work: %v", err)
	}
	awaitClose(t, workerEntered, "shutdown keyed worker")

	cr := &CityRuntime{
		cfg:                    &config.City{},
		sp:                     provider,
		cs:                     cs,
		rec:                    events.Discard,
		stdout:                 io.Discard,
		stderr:                 io.Discard,
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Auto,
	}
	shutdownDone := make(chan struct{})
	go func() {
		cr.shutdown()
		close(shutdownDone)
	}()
	awaitCond(t, func() bool {
		cs.mu.RLock()
		stopping := cs.sessionStartEventAdmissionStopping
		cs.mu.RUnlock()
		return stopping
	}, "shutdown to begin event-admission drain")

	controller.mu.Lock()
	workerStopped := controller.stopped
	controller.mu.Unlock()
	if workerStopped || provider.CountCalls("ListRunning", "") != 0 {
		t.Fatal("shutdown advanced past event admission before its callback drained")
	}

	close(releaseAdmission)
	awaitClose(t, eventDone, "shutdown event callback")
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.stopped
	}, "shutdown to begin keyed-worker join")
	if provider.CountCalls("ListRunning", "") != 0 {
		t.Fatal("session teardown began before the keyed worker joined")
	}

	close(releaseWorker)
	awaitClose(t, shutdownDone, "shutdown after keyed-worker join")
	if got := provider.CountCalls("ListRunning", ""); got != 1 {
		t.Fatalf("session teardown ListRunning calls = %d, want 1 after child join", got)
	}
}

type sessionStartProviderSwapFixture struct {
	cr       *CityRuntime
	cityPath string
	tomlPath string
}

func newSessionStartProviderSwapFixture(
	t *testing.T,
	oldProvider runtime.Provider,
	mode rollout.Mode,
) sessionStartProviderSwapFixture {
	t.Helper()
	stubManagedDoltStoreOpeners(t)
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       oldProvider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(oldProvider),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, oldProvider, events.NewFake(), "test-city", cityPath)
	cs.rolloutFlags = rollout.ForTest(rollout.WithSessionReconciler(mode))
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	return sessionStartProviderSwapFixture{cr: cr, cityPath: cityPath, tomlPath: tomlPath}
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func assertSingleSessionStartOwner(
	t *testing.T,
	cr *CityRuntime,
	info session.Info,
	cfg *config.City,
) {
	t.Helper()
	keyedOwns := resolveExactSessionStartOwnership(info, cfg, time.Now().UTC())
	option := cr.sessionStartLegacyExclusionOption()
	legacyOwns := option == nil || !legacySessionStartExcluded(option, info)
	if keyedOwns == legacyOwns {
		t.Fatalf("session start owner = keyed:%t legacy:%t, want exactly one", keyedOwns, legacyOwns)
	}
}

func legacySessionStartExcluded(option startExecutionOption, info session.Info) bool {
	opts := startExecutionOptions{}
	option(&opts)
	return opts.legacyStartExcluded != nil && opts.legacyStartExcluded(info)
}

func newSessionStartCityRuntimeForTest(t *testing.T, mode rollout.Mode, coherent bool) *CityRuntime {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	provider := runtime.NewFake()
	store := beads.NewMemStore()
	cs := coherentSessionStartControllerStateForTest(cfg, provider, store, mode)
	if !coherent {
		cs.sessionStartStoreGeneration = 0
	}
	return &CityRuntime{
		cityPath: "test-city",
		cityName: "test-city",
		cfg:      cfg,
		sp:       provider,
		cs:       cs,
		rec:      events.Discard,
		stdout:   io.Discard,
		stderr:   io.Discard,
	}
}

func coherentSessionStartControllerStateForTest(
	cfg *config.City,
	provider runtime.Provider,
	store beads.Store,
	mode rollout.Mode,
) *controllerState {
	return &controllerState{
		cfg:                         cfg,
		sp:                          provider,
		cityBeadStore:               store,
		cityName:                    "test-city",
		cityPath:                    "test-city",
		eventProv:                   events.NewFake(),
		rolloutFlags:                rollout.ForTest(rollout.WithSessionReconciler(mode)),
		sessionStartGeneration:      1,
		sessionStartStoreGeneration: 1,
	}
}

type indexedExactSessionStartBenchmarkStore struct {
	beads.Store
	byID map[string]beads.Bead
}

func (s *indexedExactSessionStartBenchmarkStore) Get(id string) (beads.Bead, error) {
	bead, ok := s.byID[id]
	if !ok {
		return beads.Bead{}, fmt.Errorf("getting benchmark bead %q: %w", id, beads.ErrNotFound)
	}
	return bead, nil
}

// exactSessionStartBenchmarkProbeStore counts the one setup reconciliation
// that proves the benchmark enters the exact-key path. Timed iterations use
// the indexed store directly.
type exactSessionStartBenchmarkProbeStore struct {
	beads.Store
	gets int
}

func (s *exactSessionStartBenchmarkProbeStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

func BenchmarkExactSessionStartPerKeyFleetSize(b *testing.B) {
	for _, fleetSize := range []int{1, 1000, 10000} {
		b.Run(fmt.Sprint(fleetSize), func(b *testing.B) {
			b.StopTimer()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
			}
			rows := make([]beads.Bead, fleetSize)
			byID := make(map[string]beads.Bead, fleetSize)
			for i := range fleetSize {
				name := fmt.Sprintf("worker-%05d", i)
				row := beads.Bead{
					ID:       fmt.Sprintf("gcs-benchmark-%05d", i),
					Title:    name,
					Type:     session.BeadType,
					Status:   "open",
					Labels:   []string{session.LabelSession},
					Revision: 1,
					Metadata: map[string]string{
						"session_name":         name,
						"agent_name":           name,
						"template":             "worker",
						"session_origin":       "manual",
						"state":                string(session.StateCreating),
						"pending_create_claim": "true",
						"generation":           "1",
						"instance_token":       "benchmark-token",
					},
				}
				rows[i] = row
				byID[row.ID] = row
			}
			target := rows[len(rows)-1]
			store := &indexedExactSessionStartBenchmarkStore{
				Store: beads.NewMemStoreFrom(fleetSize, rows, nil),
				byID:  byID,
			}
			provider := runtime.NewFake()
			if err := provider.Start(context.Background(), target.Metadata["session_name"], runtime.Config{Command: "true"}); err != nil {
				b.Fatalf("start converged target: %v", err)
			}
			params := exactSessionStartParams{
				CityPath: b.TempDir(),
				CityName: "test-city",
				Config:   cfg,
				Provider: provider,
				Store:    store,
				Recorder: events.Discard,
				Stdout:   io.Discard,
				Stderr:   io.Discard,
			}
			admission := sessionStartAdmission{
				SessionID: target.ID,
				Source:    sessionStartAdmissionAntiEntropy,
			}
			probeStore := &exactSessionStartBenchmarkProbeStore{Store: store}
			params.Store = probeStore
			owner, err := reconcileExactSessionStartWithOwner(context.Background(), admission, params)
			if err != nil {
				b.Fatalf("pre-benchmark exact reconciliation: %v", err)
			}
			if owner != exactSessionStartKeyedOwner {
				b.Fatalf("pre-benchmark owner = %v, want keyed", owner)
			}
			if got := probeStore.gets; got != 1 {
				b.Fatalf("pre-benchmark indexed Get calls = %d, want 1", got)
			}
			if got := provider.CountCalls("IsRunning", target.Metadata["session_name"]); got == 0 {
				b.Fatal("pre-benchmark exact reconciliation did not observe the target runtime")
			}
			params.Store = store

			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if err := reconcileExactSessionStart(context.Background(), admission, params); err != nil {
					b.Fatalf("reconcile converged exact key: %v", err)
				}
			}
		})
	}
}

// recordingListStore captures every ListQuery the session front door issues, so
// a test can assert on the cache-bypass flag itself rather than on some cache's
// observable behavior.
type recordingListStore struct {
	beads.Store
	mu      sync.Mutex
	queries []beads.ListQuery
}

func (s *recordingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	return s.Store.List(query)
}

func (s *recordingListStore) liveFlags() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	flags := make([]bool, 0, len(s.queries))
	for _, q := range s.queries {
		flags = append(flags, q.Live)
	}
	return flags
}

func (s *recordingListStore) reset() {
	s.mu.Lock()
	s.queries = nil
	s.mu.Unlock()
}

// `gc session new` writes the session bead and then pokes the controller, so the
// tick that services that poke has to observe a write made by another process —
// which a CachingStore-served read cannot, until its own reconcile cycle catches
// up. ga-0khp1: with the keyed reconciler off, the deferred ad-hoc start sat in
// start-pending for a full cache cadence, and on a city whose patrol interval
// outlives that wait, no tick ever followed the reconcile to start it.
func TestPokedTickReadsSessionSnapshotLive(t *testing.T) {
	store := &recordingListStore{Store: beads.NewMemStore()}
	cr := &CityRuntime{
		cityName:            "test-city",
		cfg:                 &config.City{},
		standaloneCityStore: store,
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	if snap := cr.loadSessionBeadSnapshot(); snap == nil {
		t.Fatal("cached load returned no snapshot")
	}
	for _, live := range store.liveFlags() {
		if live {
			t.Fatalf("an unpoked load bypassed the cache: %v", store.liveFlags())
		}
	}

	store.reset()
	cr.requireLiveSessionSnapshot()
	if snap := cr.loadSessionBeadSnapshot(); snap == nil {
		t.Fatal("live load returned no snapshot")
	}
	flags := store.liveFlags()
	if len(flags) == 0 {
		t.Fatal("live load issued no list at all")
	}
	for _, live := range flags {
		if !live {
			t.Fatalf("poked load served a leg from cache: %v", flags)
		}
	}

	// One-shot: a live list absorbs its rows back into the cache, so the later
	// loads in the same tick must not each pay another store round-trip.
	store.reset()
	cr.loadSessionBeadSnapshot()
	for _, live := range store.liveFlags() {
		if live {
			t.Fatalf("the live request latched past its snapshot: %v", store.liveFlags())
		}
	}
}
