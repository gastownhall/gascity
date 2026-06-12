// Tests for the agent lifecycle telemetry re-port (ga-vk4qzh): the
// reconciler/controller start, stop, quarantine, and reconcile-cycle paths
// must emit the gc.agent.starts/stops/quarantines.total and
// gc.reconcile.cycles.total counters that were lost when the legacy
// reconciler was deleted (3388c3aa1).
//
// These tests swap the global OTel MeterProvider for a manual-reader SDK
// provider (the pattern from internal/telemetry/recorder_invocation_test.go)
// and must therefore never call t.Parallel. Assertions are always "a
// datapoint matching these attributes exists with Value >= 1", never exact
// metric-wide totals.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// installManualMetricReader swaps the global MeterProvider for a
// manual-reader SDK provider and re-arms the telemetry instrument binding so
// production Record* calls land in the test provider. The cleanup restores
// the previous provider and re-arms the binding again so later tests in the
// binary do not keep recording into the dead test provider.
func installManualMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		telemetry.ResetInstrumentsForTest()
	})
	return reader
}

// collectCounterDataPoints collects from the reader and returns all int64
// sum datapoints recorded for the named metric. A metric that was registered
// but never Added produces no output, so "never emitted" surfaces as nil.
func collectCounterDataPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var points []metricdata.DataPoint[int64]
	for _, sm := range out.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q data type = %T, want Sum[int64]", name, m.Data)
			}
			points = append(points, sum.DataPoints...)
		}
	}
	return points
}

// hasDataPointWithStringAttrs reports whether any datapoint with Value >= 1
// carries every given string attribute.
func hasDataPointWithStringAttrs(points []metricdata.DataPoint[int64], want map[string]string) bool {
	for _, dp := range points {
		if dp.Value < 1 {
			continue
		}
		matched := true
		for key, wantValue := range want {
			val, ok := dp.Attributes.Value(attribute.Key(key))
			if !ok || val.AsString() != wantValue {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// hasDataPointWithIntAttrs reports whether any datapoint with Value >= 1
// carries every given int attribute.
func hasDataPointWithIntAttrs(points []metricdata.DataPoint[int64], want map[string]int64) bool {
	for _, dp := range points {
		if dp.Value < 1 {
			continue
		}
		matched := true
		for key, wantValue := range want {
			val, ok := dp.Attributes.Value(attribute.Key(key))
			if !ok || val.AsInt64() != wantValue {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// TestCommitStartResult_RecordsAgentStartMetric verifies the successful
// start-commit path increments gc.agent.starts.total with the display name
// and ok status, and that a failed durable commit records nothing — start
// telemetry shares the session.woke durable-commit contract (ga-kmoj9c).
func TestCommitStartResult_RecordsAgentStartMetric(t *testing.T) {
	successResult := func(session *beads.Bead) startResult {
		return startResult{
			prepared: preparedStart{
				candidate: startCandidate{
					session: session,
					tp: TemplateParams{
						SessionName:  "sky",
						TemplateName: "helper",
					},
				},
				coreHash: "core",
				liveHash: "live",
			},
			outcome:  "success",
			started:  time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC),
			finished: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC),
		}
	}
	sessionMeta := func() map[string]string {
		return map[string]string{
			"session_name": "sky",
			"state":        "creating",
		}
	}
	clk := &clock.Fake{Time: time.Date(2026, 3, 18, 12, 0, 1, 0, time.UTC)}

	t.Run("successful commit records the start", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := beads.NewMemStore()
		session, err := store.Create(beads.Bead{
			Title:    "helper",
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: sessionMeta(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !commitStartResult(successResult(&session), store, clk, events.NewFake(), 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned false for successful start")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.starts.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "helper", "status": "ok"}) {
			t.Fatalf("gc.agent.starts.total has no datapoint with agent=helper status=ok: %+v", points)
		}
	})

	t.Run("metadata batch failure records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := &failingMetadataBatchStore{MemStore: beads.NewMemStore(), failBatch: true}
		session, err := store.Create(beads.Bead{
			Title:    "helper",
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: sessionMeta(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if commitStartResult(successResult(&session), store, clk, events.NewFake(), 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned true, want false when metadata batch fails")
		}
		if points := collectCounterDataPoints(t, reader, "gc.agent.starts.total"); len(points) != 0 {
			t.Fatalf("gc.agent.starts.total datapoints = %+v, want none when the durable commit failed", points)
		}
	})
}

// TestStopTargetsBounded_RecordsAgentStopMetric verifies both emission
// branches of stopTargetsBounded (the parallel wave and the unresolved
// serial fallback) increment gc.agent.stops.total with reason "stopped".
func TestStopTargetsBounded_RecordsAgentStopMetric(t *testing.T) {
	t.Run("wave path", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := beads.NewMemStore()
		rec := events.NewFake()
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
		targets := []stopTarget{{
			sessionID: "sess-worker-1",
			name:      "worker-1",
			template:  "worker",
			subject:   "worker-1",
			resolved:  true,
		}}
		var stdout, stderr bytes.Buffer
		if stopped := stopTargetsBounded(targets, nil, store, sp, rec, "gc", &stdout, &stderr); stopped != 1 {
			t.Fatalf("stopped = %d, want 1", stopped)
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker-1", "reason": "stopped", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=worker-1 reason=stopped status=ok: %+v", points)
		}
	})

	t.Run("serial fallback path", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := beads.NewMemStore()
		rec := events.NewFake()
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
		targets := []stopTarget{{
			name:     "worker-1",
			template: "worker",
			subject:  "worker-1",
			resolved: false,
		}}
		var stdout, stderr bytes.Buffer
		if stopped := stopTargetsBounded(targets, &config.City{}, store, sp, rec, "gc", &stdout, &stderr); stopped != 1 {
			t.Fatalf("stopped = %d, want 1\nstderr: %s", stopped, stderr.String())
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker-1", "reason": "stopped", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=worker-1 reason=stopped status=ok: %+v", points)
		}
	})
}

// TestFinalizeDrainAckStoppedSession_RecordsAgentStopMetric verifies the
// drain-ack stop finalizer increments gc.agent.stops.total with reason
// "drain-ack" when it closes an unassigned drained session — including when
// no event recorder is wired, because the metric reflects the stop itself,
// not the event-bus wiring.
func TestFinalizeDrainAckStoppedSession_RecordsAgentStopMetric(t *testing.T) {
	finalize := func(t *testing.T, rec events.Recorder) *sdkmetric.ManualReader {
		t.Helper()
		reader := installManualMetricReader(t)
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

		session := env.createSessionBead("worker", "worker")
		patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
		if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
			t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
		}
		session.Metadata = patch.Apply(session.Metadata)

		finalizeDrainAckStoppedSession(
			"", env.cfg, env.store, nil, &session, "worker", true,
			newFakeDrainOps(), env.dt, env.clk, rec, &env.stderr,
		)

		if session.Status != "closed" {
			t.Fatalf("session status = %q, want closed (fixture must reach the recordStopped path)", session.Status)
		}
		return reader
	}
	assertStopRecorded := func(t *testing.T, reader *sdkmetric.ManualReader) {
		t.Helper()
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker", "reason": "drain-ack", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=worker reason=drain-ack status=ok: %+v", points)
		}
	}

	t.Run("with event recorder", func(t *testing.T) {
		assertStopRecorded(t, finalize(t, events.NewFake()))
	})

	t.Run("nil event recorder still records the metric", func(t *testing.T) {
		assertStopRecorded(t, finalize(t, nil))
	})
}

// TestDoHandoffRemote_RecordsAgentStopMetric verifies the handoff kill path
// increments gc.agent.stops.total with the bounded runtime session name and
// reason "handoff", matching its SessionStopped emission.
func TestDoHandoffRemote_RecordsAgentStopMetric(t *testing.T) {
	reader := installManualMetricReader(t)
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-7", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "worker-7 session",
		Type:     "session",
		Assignee: "worker-7",
		Metadata: map[string]string{"session_name": "worker-7"},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, rec, sp, "worker-7", "worker-7", "sender",
		[]string{"Context refresh", "body"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker-7", "reason": "handoff", "status": "ok"}) {
		t.Fatalf("gc.agent.stops.total has no datapoint with agent=worker-7 reason=handoff status=ok: %+v", points)
	}
}

// TestGracefulStopAll_RecordsGracefulExitStopMetric verifies the pass-2
// "exited gracefully" branch of the controller's graceful stop increments
// gc.agent.stops.total with reason "graceful-exit".
func TestGracefulStopAll_RecordsGracefulExitStopMetric(t *testing.T) {
	reader := installManualMetricReader(t)
	sp := newExitedArtifactAfterInterruptProvider()
	if err := sp.Start(context.Background(), "custom-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	gracefulStopAll([]string{"custom-worker"}, sp, 20*time.Millisecond, rec, nil, nil, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "Agent 'custom-worker' exited gracefully") {
		t.Fatalf("stdout = %q, want graceful exit message (fixture must reach pass 2)", stdout.String())
	}
	points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "custom-worker", "reason": "graceful-exit", "status": "ok"}) {
		t.Fatalf("gc.agent.stops.total has no datapoint with agent=custom-worker reason=graceful-exit status=ok: %+v", points)
	}
}

// TestRecordWakeFailure_QuarantineRecordsMetric verifies the wake-failure
// accrual path increments gc.agent.quarantines.total only when the
// quarantine batch is actually applied, labeled with the agent identity.
func TestRecordWakeFailure_QuarantineRecordsMetric(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	t.Run("quarantine threshold records the metric", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4", // one below threshold (defaultMaxWakeAttempts=5)
			"session_name":  "worker-1",
		})

		recordWakeFailure(&session, store, clk)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max attempts")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker-1"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=worker-1: %+v", points)
		}
	})

	t.Run("below threshold records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "1",
			"session_name":  "worker-1",
		})

		recordWakeFailure(&session, store, clk)

		if session.Metadata["quarantined_until"] != "" {
			t.Fatal("fixture must not quarantine below threshold")
		}
		if points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total"); len(points) != 0 {
			t.Fatalf("gc.agent.quarantines.total datapoints = %+v, want none below threshold", points)
		}
	})

	t.Run("agent_name takes precedence over session_name", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4",
			"agent_name":    "dog-1",
			"session_name":  "gc-city-dog-1",
		})

		recordWakeFailure(&session, store, clk)

		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog-1"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=dog-1: %+v", points)
		}
	})
}

// TestRecordChurn_QuarantineRecordsMetric verifies the context-churn accrual
// path increments gc.agent.quarantines.total when the churn quarantine batch
// is applied.
func TestRecordChurn_QuarantineRecordsMetric(t *testing.T) {
	reader := installManualMetricReader(t)
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	store := newTestStore()
	session := makeBead("b1", map[string]string{
		"churn_count":  "2", // one below threshold (defaultMaxChurnCycles=3)
		"session_name": "worker-1",
	})

	recordChurn(&session, store, clk)

	if session.Metadata["quarantined_until"] == "" {
		t.Fatal("fixture must quarantine at max churn cycles")
	}
	points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "worker-1"}) {
		t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=worker-1: %+v", points)
	}
}

// TestReconcileSessionBeads_RecordsReconcileCycleMetric verifies every
// reconciler tick increments gc.reconcile.cycles.total at the chokepoint all
// reconcile wrappers funnel into — including ticks aborted by context
// cancellation, so the counter means "cycles", not "cycles that ran to
// completion". Stops and skips are not aggregated at the tick boundary, so
// they are honestly reported as 0.
func TestReconcileSessionBeads_RecordsReconcileCycleMetric(t *testing.T) {
	assertCycleRecorded := func(t *testing.T, reader *sdkmetric.ManualReader) {
		t.Helper()
		points := collectCounterDataPoints(t, reader, "gc.reconcile.cycles.total")
		if !hasDataPointWithIntAttrs(points, map[string]int64{"started": 0, "stopped": 0, "skipped": 0}) {
			t.Fatalf("gc.reconcile.cycles.total has no datapoint with started=0 stopped=0 skipped=0: %+v", points)
		}
	}

	t.Run("completed tick records the cycle", func(t *testing.T) {
		reader := installManualMetricReader(t)
		env := newReconcilerTestEnv()

		env.reconcile(nil)

		assertCycleRecorded(t, reader)
	})

	t.Run("canceled context still records the cycle", func(t *testing.T) {
		reader := installManualMetricReader(t)
		env := newReconcilerTestEnv()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		reconcileSessionBeads(
			ctx, nil, env.desiredState, configuredSessionNames(env.cfg, "", env.store),
			env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{},
			false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		)

		assertCycleRecorded(t, reader)
	})
}
