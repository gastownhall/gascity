package main

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestSessionLifecycleShadowWorkerRejectsInvalidConstruction(t *testing.T) {
	observer := func(sessionLifecycleStartShadowEvaluation) {}

	tests := []struct {
		name     string
		workers  int
		observer sessionLifecycleStartShadowEvaluationObserver
		stderr   io.Writer
	}{
		{name: "no workers", observer: observer, stderr: io.Discard},
		{name: "nil observer", workers: 1, stderr: io.Discard},
		{name: "nil stderr", workers: 1, observer: observer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newSessionLifecycleShadowWorker(tt.workers, tt.observer, tt.stderr); err == nil {
				t.Fatal("newSessionLifecycleShadowWorker() error = nil")
			}
		})
	}
}

func TestSessionLifecycleShadowWorkerReportsCompleteTimingIntervals(t *testing.T) {
	enqueuedAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	startedAt := enqueuedAt.Add(3 * time.Millisecond)
	completedAt := startedAt.Add(2 * time.Millisecond)
	moments := make(chan time.Time, 3)
	moments <- enqueuedAt
	moments <- startedAt
	moments <- completedAt

	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 1)
	worker, err := newSessionLifecycleShadowWorker(1, func(evaluation sessionLifecycleStartShadowEvaluation) {
		evaluations <- evaluation
	}, io.Discard)
	if err != nil {
		t.Fatalf("newSessionLifecycleShadowWorker: %v", err)
	}
	worker.now = func() time.Time { return <-moments }
	if err := worker.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(worker.Stop)

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-timing", "timing")); err != nil {
		t.Fatalf("EnqueueStart: %v", err)
	}
	evaluation := receiveShadowEvaluation(t, evaluations)
	if evaluation.EnqueuedAt != enqueuedAt ||
		evaluation.StartedAt != startedAt ||
		evaluation.CompletedAt != completedAt {
		t.Fatalf(
			"evaluation timestamps = enqueue:%s start:%s complete:%s",
			evaluation.EnqueuedAt,
			evaluation.StartedAt,
			evaluation.CompletedAt,
		)
	}
	if evaluation.QueueLatency != 3*time.Millisecond {
		t.Fatalf("queue latency = %s, want 3ms", evaluation.QueueLatency)
	}
	if evaluation.PlanningLatency != 2*time.Millisecond {
		t.Fatalf("planning latency = %s, want 2ms", evaluation.PlanningLatency)
	}
}

func TestSessionLifecycleShadowWorkerCoalescesDuplicateKeysToNewestImmutableObservation(t *testing.T) {
	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 8)
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	worker := mustStartSessionLifecycleShadowWorker(t, 1, func(evaluation sessionLifecycleStartShadowEvaluation) {
		if evaluation.Observation.Input.Info.ID == "session-blocker" {
			close(blockerStarted)
			<-releaseBlocker
		}
		evaluations <- evaluation
	})

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-blocker", "blocker")); err != nil {
		t.Fatalf("enqueue blocker: %v", err)
	}
	waitForSignal(t, blockerStarted, "blocker evaluation")

	for _, title := range []string{"old", "newer"} {
		if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-target", title)); err != nil {
			t.Fatalf("enqueue %q: %v", title, err)
		}
	}
	latest := testSessionLifecycleStartShadowObservation("session-target", "latest")
	latest.Input.Info.Labels = []string{"original"}
	if err := worker.EnqueueStart(latest); err != nil {
		t.Fatalf("enqueue latest: %v", err)
	}
	latest.Input.Info.Title = "mutated-after-enqueue"
	latest.Input.Info.Labels[0] = "mutated-after-enqueue"
	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-barrier", "barrier")); err != nil {
		t.Fatalf("enqueue barrier: %v", err)
	}

	close(releaseBlocker)

	var targetEvaluations int
	for {
		evaluation := receiveShadowEvaluation(t, evaluations)
		switch evaluation.Observation.Input.Info.ID {
		case "session-target":
			targetEvaluations++
			if got := evaluation.Observation.Input.Info.Title; got != "latest" {
				t.Fatalf("target title = %q, want latest", got)
			}
			if got := evaluation.Observation.Input.Info.Labels; len(got) != 1 || got[0] != "original" {
				t.Fatalf("target labels = %#v, want detached original", got)
			}
		case "session-barrier":
			if targetEvaluations != 1 {
				t.Fatalf("target evaluations = %d, want 1", targetEvaluations)
			}
			return
		}
	}
}

func TestSessionLifecycleShadowWorkerReplaysLatestObservationWithoutConcurrentSameKeyEvaluation(t *testing.T) {
	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 4)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32

	worker := mustStartSessionLifecycleShadowWorker(t, 2, func(evaluation sessionLifecycleStartShadowEvaluation) {
		current := active.Add(1)
		updateAtomicMax(&maxActive, current)
		defer active.Add(-1)

		evaluations <- evaluation
		if evaluation.Observation.Input.Info.Title == "first" {
			close(firstStarted)
			<-releaseFirst
		}
	})

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-a", "first")); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	waitForSignal(t, firstStarted, "first evaluation")
	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-a", "latest")); err != nil {
		t.Fatalf("enqueue latest: %v", err)
	}

	first := receiveShadowEvaluation(t, evaluations)
	if first.Observation.Input.Info.Title != "first" {
		t.Fatalf("first title = %q, want first", first.Observation.Input.Info.Title)
	}
	select {
	case unexpected := <-evaluations:
		t.Fatalf("same key evaluated concurrently: %+v", unexpected)
	default:
	}

	close(releaseFirst)
	replay := receiveShadowEvaluation(t, evaluations)
	if replay.Observation.Input.Info.Title != "latest" {
		t.Fatalf("replay title = %q, want latest", replay.Observation.Input.Info.Title)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent evaluations for one key = %d, want 1", got)
	}
}

func TestSessionLifecycleShadowWorkerConsumesProjectionAndReplaysPostConsumeObservation(t *testing.T) {
	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 2)
	firstObserved := make(chan struct{})
	releaseFirst := make(chan struct{})
	worker := mustStartSessionLifecycleShadowWorker(t, 1, func(evaluation sessionLifecycleStartShadowEvaluation) {
		evaluations <- evaluation
		if evaluation.Observation.Input.Info.Title == "first" {
			close(firstObserved)
			<-releaseFirst
		}
	})

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-a", "first")); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	waitForSignal(t, firstObserved, "first observation")
	worker.mu.Lock()
	_, ok := worker.projection["session-a"]
	worker.mu.Unlock()
	if ok {
		t.Fatal("projection remained after evaluation began")
	}
	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-a", "replay")); err != nil {
		t.Fatalf("enqueue replay: %v", err)
	}
	close(releaseFirst)

	first := receiveShadowEvaluation(t, evaluations)
	if first.Observation.Input.Info.Title != "first" {
		t.Fatalf("first title = %q, want first", first.Observation.Input.Info.Title)
	}
	replay := receiveShadowEvaluation(t, evaluations)
	if replay.Observation.Input.Info.Title != "replay" {
		t.Fatalf("replay title = %q, want replay", replay.Observation.Input.Info.Title)
	}
	worker.mu.Lock()
	_, ok = worker.projection["session-a"]
	worker.mu.Unlock()
	if ok {
		t.Fatal("projection remained after replay evaluation")
	}
}

func TestSessionLifecycleShadowWorkerProcessesDifferentKeysConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32

	worker := mustStartSessionLifecycleShadowWorker(t, 2, func(evaluation sessionLifecycleStartShadowEvaluation) {
		current := active.Add(1)
		updateAtomicMax(&maxActive, current)
		defer active.Add(-1)
		started <- evaluation.Observation.Input.Info.ID
		<-release
	})

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-a", "a")); err != nil {
		t.Fatalf("enqueue session-a: %v", err)
	}
	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-b", "b")); err != nil {
		t.Fatalf("enqueue session-b: %v", err)
	}

	first := receiveString(t, started, "first key")
	second := receiveString(t, started, "second key")
	if first == second {
		t.Fatalf("concurrent keys = %q and %q, want distinct", first, second)
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("maximum concurrent evaluations = %d, want 2", got)
	}
	close(release)
}

func TestSessionLifecycleShadowWorkerStopRejectsAdmissionAndJoinsWorkers(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	worker := mustStartSessionLifecycleShadowWorker(t, 1, func(evaluation sessionLifecycleStartShadowEvaluation) {
		if evaluation.Observation.Input.Info.ID == "session-running" {
			close(started)
			<-release
		}
	})

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-running", "running")); err != nil {
		t.Fatalf("enqueue running: %v", err)
	}
	waitForSignal(t, started, "running evaluation")

	stopped := make(chan struct{})
	go func() {
		worker.Stop()
		close(stopped)
	}()

	deadline := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer deadline.Stop()
	for {
		err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-after-stop", "after"))
		if err != nil {
			if !strings.Contains(err.Error(), "stopped") {
				t.Fatalf("enqueue during stop error = %v, want stopped", err)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("worker did not stop admission")
		default:
		}
	}

	select {
	case <-stopped:
		t.Fatal("Stop returned before the in-flight evaluation completed")
	default:
	}
	close(release)
	waitForSignal(t, stopped, "worker stop")

	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-rejected", "rejected")); err == nil {
		t.Fatal("enqueue after Stop() error = nil")
	}
}

func TestCityRuntimeLifecycleShadowWorkerIsOptInAndLegacyKeepsEffectOwnership(t *testing.T) {
	if option := (&CityRuntime{}).sessionLifecycleShadowStartOption(nil); option != nil {
		t.Fatal("default CityRuntime shadow option is enabled")
	}

	evaluations := make(chan sessionLifecycleStartShadowEvaluation, 1)
	worker, err := newSessionLifecycleShadowWorker(1, func(evaluation sessionLifecycleStartShadowEvaluation) {
		evaluations <- evaluation
	}, io.Discard)
	if err != nil {
		t.Fatalf("newSessionLifecycleShadowWorker: %v", err)
	}
	cr := &CityRuntime{
		lifecycleShadowWorker: worker,
		stderr:                &bytes.Buffer{},
	}
	cr.startSessionLifecycleShadowWorker()
	t.Cleanup(cr.stopSessionLifecycleShadowWorker)
	cycle := &SessionReconcilerTraceCycle{directDetailArms: map[string]sessionLifecycleStartShadowAdmission{
		"worker": {
			Template:  "worker",
			Source:    TraceSourceManual,
			ExpiresAt: time.Now().UTC().Add(time.Minute),
		},
	}}

	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	sessionBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&sessionBead)
	env.startOptions = append(env.startOptions, cr.sessionLifecycleShadowStartOption(cycle))

	if woken := env.reconcile([]beads.Bead{sessionBead}); woken != 1 {
		t.Fatalf("legacy woken count = %d, want 1", woken)
	}
	evaluation := receiveShadowEvaluation(t, evaluations)
	if evaluation.Comparison.Plan.SessionID != sessionBead.ID ||
		evaluation.Comparison.Plan.Outcome != sessionLifecycleStartSelectionPrepare ||
		evaluation.Comparison.Outcome != sessionLifecycleStartSelectionComparisonMatched {
		t.Fatalf("shadow evaluation = %+v, want matching ready selection", evaluation)
	}
	if got := env.sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("runtime Start calls = %d, want exactly one legacy-owned effect", got)
	}

	cr.stopSessionLifecycleShadowWorker()
	if err := worker.EnqueueStart(testSessionLifecycleStartShadowObservation("session-after-runtime-stop", "stopped")); err == nil {
		t.Fatal("CityRuntime stop left shadow worker admission open")
	}
}

func TestRecordSessionLifecycleStartShadowEvaluationPersistsNoEffectEvidence(t *testing.T) {
	cityPath := t.TempDir()
	trace := newSessionReconcilerTraceManager(cityPath, "shadow-city", io.Discard)
	t.Cleanup(func() { _ = trace.Close() })
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "shadow-city",
		cfg:      &config.City{},
		trace:    trace,
		stderr:   io.Discard,
	}
	observedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	expiresAt := observedAt.Add(2 * time.Millisecond)
	cr.recordSessionLifecycleStartShadowEvaluation(sessionLifecycleStartShadowEvaluation{
		Observation: newAdmittedSessionLifecycleStartShadowObservation(
			sessionLifecycleStartShadowInput{
				Info:       session.Info{ID: "gcs-shadow"},
				ObservedAt: observedAt,
			},
			true,
			sessionLifecycleStartShadowAdmission{
				Template:  "worker",
				Source:    TraceSourceManual,
				ExpiresAt: expiresAt,
			},
		),
		Comparison: sessionLifecycleStartSelectionComparison{
			Plan: sessionLifecycleStartSelectionPlan{
				SessionID: "gcs-shadow",
				Outcome:   sessionLifecycleStartSelectionPrepare,
				Reason:    sessionLifecycleStartSelectionReasonReady,
			},
			LegacySelected: true,
			Outcome:        sessionLifecycleStartSelectionComparisonMatched,
			Reason:         sessionLifecycleStartSelectionComparisonReasonEquivalent,
		},
		EnqueuedAt:      observedAt.Add(time.Millisecond),
		StartedAt:       observedAt.Add(2 * time.Millisecond),
		CompletedAt:     observedAt.Add(3 * time.Millisecond),
		QueueLatency:    time.Millisecond,
		PlanningLatency: time.Millisecond,
	})

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		SiteCode:    TraceSiteLifecycleStartSelectionShadow,
		Template:    "worker",
		TraceMode:   TraceModeDetail,
		TraceSource: TraceSourceManual,
	})
	if err != nil {
		t.Fatalf("read start-selection shadow trace: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("start-selection shadow records = %#v, want one", records)
	}
	record := records[0]
	if record.RecordType != TraceRecordOperation ||
		record.Template != "worker" ||
		record.SessionBeadID != "gcs-shadow" ||
		record.TraceMode != TraceModeDetail ||
		record.TraceSource != TraceSourceManual ||
		record.OutcomeCode != TraceOutcomeNoChange ||
		record.Fields["session_id"] != "gcs-shadow" ||
		record.Fields["admitted_template"] != "worker" ||
		record.Fields["admitted_source"] != string(TraceSourceManual) ||
		record.Fields["candidate_outcome"] != "prepare" ||
		record.Fields["candidate_reason"] != string(sessionLifecycleStartSelectionReasonReady) ||
		record.Fields["legacy_selected"] != true ||
		record.Fields["comparison_outcome"] != string(sessionLifecycleStartSelectionComparisonMatched) ||
		record.Fields["comparison_reason"] != string(sessionLifecycleStartSelectionComparisonReasonEquivalent) ||
		record.Fields["effect_applied"] != false {
		t.Fatalf("start-selection shadow record = %#v, want matched no-effect evidence", record)
	}
}

func TestCityRuntimeLifecycleShadowStartOptionRequiresDirectCycleArm(t *testing.T) {
	worker := mustStartSessionLifecycleShadowWorker(t, 1, func(sessionLifecycleStartShadowEvaluation) {})
	cr := &CityRuntime{lifecycleShadowWorker: worker, stderr: io.Discard}

	if option := cr.sessionLifecycleShadowStartOption(&SessionReconcilerTraceCycle{}); option != nil {
		t.Fatal("unarmed trace cycle published a shadow option")
	}
	expiresAt := time.Date(2030, 7, 29, 12, 0, 0, 0, time.UTC)
	cycle := &SessionReconcilerTraceCycle{directDetailArms: map[string]sessionLifecycleStartShadowAdmission{
		"worker": {Template: "worker", Source: TraceSourceManual, ExpiresAt: expiresAt},
	}}
	option := cr.sessionLifecycleShadowStartOption(cycle)
	if option == nil {
		t.Fatal("direct detail arm did not publish a shadow option")
	}
	var options startExecutionOptions
	option(&options)
	if options.startSelectionShadowAdmission == nil {
		t.Fatal("shadow option did not install admission predicate")
	}
	admission, ok := options.startSelectionShadowAdmission("worker")
	if !ok || admission.Template != "worker" || admission.Source != TraceSourceManual || !admission.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("admission = %+v, %t; want direct arm token", admission, ok)
	}
	cr.sessionStartOwnership = sessionStartOwnershipKeyed
	if option := cr.sessionLifecycleShadowStartOption(cycle); option != nil {
		t.Fatal("keyed ownership published a shadow option")
	}
}

func mustStartSessionLifecycleShadowWorker(
	t *testing.T,
	workers int,
	observer sessionLifecycleStartShadowEvaluationObserver,
) *sessionLifecycleShadowWorker {
	t.Helper()
	worker, err := newSessionLifecycleShadowWorker(workers, observer, io.Discard)
	if err != nil {
		t.Fatalf("newSessionLifecycleShadowWorker: %v", err)
	}
	if err := worker.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(worker.Stop)
	return worker
}

func testSessionLifecycleStartShadowObservation(id, title string) sessionLifecycleStartShadowObservation {
	return newSessionLifecycleStartShadowObservation(
		sessionLifecycleStartShadowInput{
			Info: session.Info{
				ID:    id,
				Title: title,
			},
			WakeDecisionObserved: true,
			ShouldWake:           true,
			RuntimeObserved:      true,
			ObservedAt:           time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
		},
		true,
	)
}

func receiveShadowEvaluation(
	t *testing.T,
	evaluations <-chan sessionLifecycleStartShadowEvaluation,
) sessionLifecycleStartShadowEvaluation {
	t.Helper()
	select {
	case evaluation := <-evaluations:
		return evaluation
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for shadow evaluation")
		return sessionLifecycleStartShadowEvaluation{}
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func receiveString(t *testing.T, values <-chan string, name string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", name)
		return ""
	}
}

func updateAtomicMax(maximum *atomic.Int32, candidate int32) {
	for {
		current := maximum.Load()
		if candidate <= current || maximum.CompareAndSwap(current, candidate) {
			return
		}
	}
}
