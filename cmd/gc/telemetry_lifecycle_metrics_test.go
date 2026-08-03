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
	"errors"
	"os"
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
	"github.com/gastownhall/gascity/internal/session/sessiontest"
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
					info: seedSessionInfo(*session),
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
		if !commitStartResult(successResult(&session), sessionFrontDoor(store), clk, events.NewFake(), 0, ioDiscard{}, ioDiscard{}) {
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
		if commitStartResult(successResult(&session), sessionFrontDoor(store), clk, events.NewFake(), 0, ioDiscard{}, ioDiscard{}) {
			t.Fatal("commitStartResult returned true, want false when metadata batch fails")
		}
		if points := collectCounterDataPoints(t, reader, "gc.agent.starts.total"); len(points) != 0 {
			t.Fatalf("gc.agent.starts.total datapoints = %+v, want none when the durable commit failed", points)
		}
	})
}

// TestStopTargetsBounded_RecordsAgentStopMetric verifies both emission
// branches of stopTargetsBounded (the parallel wave and the unresolved
// serial fallback) increment gc.agent.stops.total with reason "stopped" and
// label the agent with the agent identity, not the sanitized runtime session
// name. The fixtures use a qualified identity that differs from the session
// name so the value-space drift cannot hide behind look-alike fixtures.
func TestStopTargetsBounded_RecordsAgentStopMetric(t *testing.T) {
	const sessionName = "gascity--gc__worker" // sanitized runtime session name
	const identity = "gascity/gc.worker"      // qualified agent identity

	t.Run("wave path", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := beads.NewMemStore()
		rec := events.NewFake()
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
		targets := []stopTarget{{
			sessionID: "sess-stop-wave",
			name:      sessionName,
			template:  identity,
			agentName: identity,
			subject:   identity,
			resolved:  true,
		}}
		var stdout, stderr bytes.Buffer
		if stopped := stopTargetsBounded(targets, nil, store, sp, rec, "gc", &stdout, &stderr); stopped != 1 {
			t.Fatalf("stopped = %d, want 1", stopped)
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": identity, "reason": "stopped", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=stopped status=ok: %+v", identity, points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": sessionName}) {
			t.Fatalf("gc.agent.stops.total must not label agent with the sanitized session name %q: %+v", sessionName, points)
		}
	})

	t.Run("serial fallback path", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := beads.NewMemStore()
		rec := events.NewFake()
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
		targets := []stopTarget{{
			name:      sessionName,
			template:  identity,
			agentName: identity,
			subject:   identity,
			resolved:  false,
		}}
		var stdout, stderr bytes.Buffer
		if stopped := stopTargetsBounded(targets, &config.City{}, store, sp, rec, "gc", &stdout, &stderr); stopped != 1 {
			t.Fatalf("stopped = %d, want 1\nstderr: %s", stopped, stderr.String())
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": identity, "reason": "stopped", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=stopped status=ok: %+v", identity, points)
		}
	})
}

// TestFinalizeDrainAckStoppedSession_RecordsAgentStopMetric verifies the
// drain-ack stop finalizer increments gc.agent.stops.total with reason
// "drain-ack" when it closes an unassigned drained session — including when
// no event recorder is wired, because the metric reflects the stop itself,
// not the event-bus wiring.
func TestFinalizeDrainAckStoppedSession_RecordsAgentStopMetric(t *testing.T) {
	const sessionName = "gascity--gc__worker" // sanitized runtime session name
	const identity = "gascity/gc.worker"      // qualified agent identity
	finalize := func(t *testing.T, rec events.Recorder) *sdkmetric.ManualReader {
		t.Helper()
		reader := installManualMetricReader(t)
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

		// createSessionBead ties agent_name to the session name; override it so
		// the agent identity differs from the sanitized session name and the
		// metric label cannot silently fall back to the session-name space.
		session := env.createSessionBead(sessionName, identity)
		env.setSessionMetadata(&session, map[string]string{"agent_name": identity})
		patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
		if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
			t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
		}
		session.Metadata = patch.Apply(session.Metadata)

		result := finalizeDrainAckStoppedSession(
			"", env.cfg, env.store, nil, sessiontest.SeedBead(t, session), identity, true,
			newFakeDrainOps(), env.dt, env.clk, rec, &env.stderr,
		)

		// W-tick dropped the raw session.Status="closed" mirror; the close is now
		// carried by the store write AND the result's MarkClosed fold (result.closed).
		if got, err := env.store.Get(session.ID); err != nil || got.Status != "closed" {
			t.Fatalf("store session status = %q (err %v), want closed (fixture must reach the recordStopped path)", got.Status, err)
		}
		if !result.applyTo(sessiontest.SeedBead(t, session)).Closed {
			t.Fatalf("drain-ack result fold not Closed; the recordStopped path must MarkClosed the snapshot")
		}
		return reader
	}
	assertStopRecorded := func(t *testing.T, reader *sdkmetric.ManualReader) {
		t.Helper()
		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": identity, "reason": "drain-ack", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=drain-ack status=ok: %+v", identity, points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": sessionName}) {
			t.Fatalf("gc.agent.stops.total must not label agent with the sanitized session name %q: %+v", sessionName, points)
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
// increments gc.agent.stops.total with reason "handoff" and labels the agent
// with the qualified agent identity resolved from the session bead, not the
// sanitized runtime session name.
func TestDoHandoffRemote_RecordsAgentStopMetric(t *testing.T) {
	const sessionName = "gascity--gc__worker" // sanitized runtime session name
	const identity = "gascity/gc.worker"      // qualified agent identity
	reader := installManualMetricReader(t)
	store := beads.NewMemStore()
	rec := events.NewFake()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "worker session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"agent_name":   identity,
			"template":     identity,
		},
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doHandoffRemote(store, store, rec, sp, sessionName, sessionName, "sender",
		[]string{"Context refresh", "body"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": identity, "reason": "handoff", "status": "ok"}) {
		t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=handoff status=ok: %+v", identity, points)
	}
	if hasDataPointWithStringAttrs(points, map[string]string{"agent": sessionName}) {
		t.Fatalf("gc.agent.stops.total must not label agent with the sanitized session name %q: %+v", sessionName, points)
	}
}

// TestGracefulStopAll_RecordsGracefulExitStopMetric verifies the pass-2
// "exited gracefully" branch of the controller's graceful stop increments
// gc.agent.stops.total with reason "graceful-exit" and labels the agent with
// the identity resolved from the session bead, not the runtime session name.
func TestGracefulStopAll_RecordsGracefulExitStopMetric(t *testing.T) {
	const sessionName = "custom-worker"  // runtime session name
	const identity = "gascity/gc.custom" // qualified agent identity
	reader := installManualMetricReader(t)
	sp := newExitedArtifactAfterInterruptProvider()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "custom session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"agent_name":   identity,
			"template":     identity,
		},
	}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	gracefulStopAll([]string{sessionName}, sp, 20*time.Millisecond, rec, nil, beads.SessionStore{Store: store}, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "Agent 'custom-worker' exited gracefully") {
		t.Fatalf("stdout = %q, want graceful exit message (fixture must reach pass 2)", stdout.String())
	}
	points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": identity, "reason": "graceful-exit", "status": "ok"}) {
		t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=graceful-exit status=ok: %+v", identity, points)
	}
	if hasDataPointWithStringAttrs(points, map[string]string{"agent": sessionName}) {
		t.Fatalf("gc.agent.stops.total must not label agent with the runtime session name %q: %+v", sessionName, points)
	}
}

// TestRecordWakeFailure_QuarantineRecordsMetric verifies the wake-failure
// accrual path increments gc.agent.quarantines.total only when the
// quarantine batch is actually applied, labeled with the agent identity and
// never the sanitized session name.
func TestRecordWakeFailure_QuarantineRecordsMetric(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	t.Run("quarantine threshold records the metric", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4", // one below threshold (defaultMaxWakeAttempts=5)
			"agent_name":    "gascity/gc.worker",
			"session_name":  "gascity--gc__worker",
		})

		recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max attempts")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/gc.worker"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=gascity/gc.worker: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity--gc__worker"}) {
			t.Fatalf("gc.agent.quarantines.total must not label agent with the sanitized session name: %+v", points)
		}
	})

	t.Run("below threshold records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "1",
			"session_name":  "worker-1",
		})

		recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] != "" {
			t.Fatal("fixture must not quarantine below threshold")
		}
		if points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total"); len(points) != 0 {
			t.Fatalf("gc.agent.quarantines.total datapoints = %+v, want none below threshold", points)
		}
	})

	t.Run("labels with agent identity, never the session name", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4",
			"agent_name":    "dog-1",
			"session_name":  "gc-city-dog-1",
		})

		recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
		syncBeadFromStore(&session, store)

		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog-1"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=dog-1: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gc-city-dog-1"}) {
			t.Fatalf("gc.agent.quarantines.total must not label agent with the session name: %+v", points)
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
		"agent_name":   "gascity/gc.worker",
		"session_name": "gascity--gc__worker",
	})

	recordChurn(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
	syncBeadFromStore(&session, store)

	if session.Metadata["quarantined_until"] == "" {
		t.Fatal("fixture must quarantine at max churn cycles")
	}
	points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
	if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/gc.worker"}) {
		t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=gascity/gc.worker: %+v", points)
	}
	if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity--gc__worker"}) {
		t.Fatalf("gc.agent.quarantines.total must not label agent with the sanitized session name: %+v", points)
	}
}

// TestCmdSessionKill_RecordsAgentStopMetric verifies a successful
// `gc session kill` increments gc.agent.stops.total exactly once with the
// agent identity from the session bead, reason "killed" (matching the adjacent
// SessionStopped event payload), and status "ok" (ga-rjk4or). The session name
// differs from the agent identity so the metric cannot silently use the
// session name.
func TestCmdSessionKill_RecordsAgentStopMetric(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-kill-stop-metric-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)

	fakeProvider := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fakeProvider, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const identity = "session-a"
	const sessionName = "s-gc-kill-stop-metric"
	const agentIdentity = "gascity/gc.worker"
	bead, err := store.Create(beads.Bead{
		Title:  "named session",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":                      identity,
			"template":                   "worker",
			"agent_name":                 agentIdentity,
			"session_name":               sessionName,
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}
	if err := fakeProvider.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("fakeProvider.Start: %v", err)
	}
	if err := fakeProvider.SetMeta(sessionName, "GC_SESSION_ID", bead.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	lis, err := startControllerSocket(
		cityDir,
		func() {},
		nil,
		nil,
		make(chan reloadRequest),
		make(chan convergenceRequest, 1),
		make(chan struct{}, 1),
		make(chan struct{}, 1),
	)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	defer lis.Close()                              //nolint:errcheck
	defer os.Remove(controllerSocketPath(cityDir)) //nolint:errcheck

	reader := installManualMetricReader(t)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionKill = %d, want 0; stderr=%s", code, stderr.String())
	}

	points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
	want := map[string]string{"agent": agentIdentity, "reason": "killed", "status": "ok"}
	if !hasDataPointWithStringAttrs(points, want) {
		t.Fatalf("gc.agent.stops.total has no datapoint with agent=%s reason=killed status=ok: %+v", agentIdentity, points)
	}
	if hasDataPointWithStringAttrs(points, map[string]string{"agent": sessionName}) {
		t.Fatalf("gc.agent.stops.total must not label agent with the session name %q: %+v", sessionName, points)
	}
	for _, dp := range points {
		matched := true
		for key, wantValue := range want {
			val, ok := dp.Attributes.Value(attribute.Key(key))
			if !ok || val.AsString() != wantValue {
				matched = false
				break
			}
		}
		if matched && dp.Value != 1 {
			t.Fatalf("gc.agent.stops.total datapoint value = %d, want exactly 1 per kill: %+v", dp.Value, dp.Attributes)
		}
	}
}

// TestRecordSessionKillStop_SkipOnUnknown pins the skip-on-unknown contract
// of recordSessionKillStop: when the session bead failed to load, or carries
// no bounded session name, nothing is recorded — an unknown identity must
// not become a garbage metric label. The beadErr path is deterministically
// unreachable through cmdSessionKill end-to-end (handle resolution on the
// same store fails first), so this helper-level test is the only way to pin
// it.
func TestRecordSessionKillStop_SkipOnUnknown(t *testing.T) {
	// The fixtures below are deliberately degraded (empty ID / no session type),
	// so they cannot round-trip through a store double — the front door would
	// reject them. They are built as session.Info struct literals directly.
	// recordSessionKillStop reads only SessionNameMetadata and the agent-identity
	// fields (AgentName / "agent:" Labels / Template / PoolSlot), never
	// Info.SessionName, so leaving SessionName zero is intentional and
	// outcome-identical to the former InfoFromPersistedBead projection.
	t.Run("bead load failure records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)

		recordSessionKillStop(sessionpkg.Info{}, errors.New("store unavailable"), nil)

		if points := collectCounterDataPoints(t, reader, "gc.agent.stops.total"); len(points) != 0 {
			t.Fatalf("gc.agent.stops.total datapoints = %+v, want none when the bead failed to load", points)
		}
	})

	t.Run("empty session name records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)

		recordSessionKillStop(sessionpkg.Info{SessionNameMetadata: "  "}, nil, nil)

		if points := collectCounterDataPoints(t, reader, "gc.agent.stops.total"); len(points) != 0 {
			t.Fatalf("gc.agent.stops.total datapoints = %+v, want none for a blank session name", points)
		}
	})

	t.Run("loaded bead records the stop", func(t *testing.T) {
		reader := installManualMetricReader(t)

		recordSessionKillStop(sessionpkg.Info{
			SessionNameMetadata: "gascity--gc__worker",
			AgentName:           "gascity/gc.worker",
		}, nil, nil)

		points := collectCounterDataPoints(t, reader, "gc.agent.stops.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/gc.worker", "reason": "killed", "status": "ok"}) {
			t.Fatalf("gc.agent.stops.total has no datapoint with agent=gascity/gc.worker reason=killed status=ok: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity--gc__worker"}) {
			t.Fatalf("gc.agent.stops.total must not label agent with the session name: %+v", points)
		}
	})

	t.Run("bounded session name but unresolved identity records nothing", func(t *testing.T) {
		reader := installManualMetricReader(t)

		// A bead with a bounded session_name but no agent_name, agent: label,
		// or template passes the session-name guard yet resolves to an empty
		// agent identity. The RecordAgentStop backstop must drop it so the
		// counter is never polluted with a blank, unjoinable agent series.
		recordSessionKillStop(sessionpkg.Info{
			SessionNameMetadata: "gascity--gc__worker",
		}, nil, nil)

		if points := collectCounterDataPoints(t, reader, "gc.agent.stops.total"); len(points) != 0 {
			t.Fatalf("gc.agent.stops.total datapoints = %+v, want none when the agent identity resolves empty", points)
		}
	})
}

// TestReconcileSessionBeads_RecordsReconcileCycleMetric verifies every
// reconciler tick increments gc.reconcile.cycles.total at the chokepoint all
// reconcile wrappers funnel into — including ticks aborted by context
// cancellation, so the counter means "cycles", not "cycles that ran to
// completion". Stops and skips are not aggregated at the tick boundary, so
// the metric intentionally carries only the started attribute.
func TestReconcileSessionBeads_RecordsReconcileCycleMetric(t *testing.T) {
	assertCycleRecorded := func(t *testing.T, reader *sdkmetric.ManualReader) {
		t.Helper()
		points := collectCounterDataPoints(t, reader, "gc.reconcile.cycles.total")
		if !hasDataPointWithIntAttrs(points, map[string]int64{"started": 0}) {
			t.Fatalf("gc.reconcile.cycles.total has no datapoint with started=0: %+v", points)
		}
		for _, dp := range points {
			if _, ok := dp.Attributes.Value(attribute.Key("stopped")); ok {
				t.Fatalf("gc.reconcile.cycles.total datapoint carries dropped attribute %q: %+v", "stopped", dp.Attributes)
			}
			if _, ok := dp.Attributes.Value(attribute.Key("skipped")); ok {
				t.Fatalf("gc.reconcile.cycles.total datapoint carries dropped attribute %q: %+v", "skipped", dp.Attributes)
			}
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

// TestSessionAgentMetricIdentity_PooledFallback pins the gc.agent.* identity
// resolver against the start path's tp.DisplayName() value space. The riskiest
// cases are legacy aliasless pooled session beads (template + pool_slot, no
// agent_name): a non-themed pool must record agent="<template>-<slot>", and a
// namepool-themed pool must record its themed instance identity (reusing the
// start path's poolInstanceIdentity), never the numeric "<template>-<slot>"
// form, so the stop/quarantine series join the start counter.
func TestSessionAgentMetricIdentity_PooledFallback(t *testing.T) {
	cases := []struct {
		name string
		bead beads.Bead
		cfg  *config.City
		want string
	}{
		{
			name: "agent_name wins over template and pool_slot",
			bead: beads.Bead{Metadata: map[string]string{
				"agent_name": "dog-3", "template": "dog", "pool_slot": "3",
			}},
			want: "dog-3",
		},
		{
			name: "agent label fallback when agent_name empty",
			bead: beads.Bead{
				Labels:   []string{"agent:dog-7"},
				Metadata: map[string]string{"template": "dog", "pool_slot": "7"},
			},
			want: "dog-7",
		},
		{
			name: "legacy aliasless pooled bead synthesizes template-pool_slot",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-dog-3-abc", "template": "dog", "pool_slot": "3",
			}},
			want: "dog-3",
		},
		{
			name: "rig-qualified legacy pooled bead keeps the qualified base",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-myrig--dog-3", "template": "myrig/dog", "pool_slot": "3",
			}},
			want: "myrig/dog-3",
		},
		{
			name: "non-pool bead falls back to the bare template",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-worker", "template": "gascity/gc.worker",
			}},
			want: "gascity/gc.worker",
		},
		{
			name: "no identity metadata resolves empty",
			bead: beads.Bead{Metadata: map[string]string{"session_name": "s-orphan"}},
			want: "",
		},
		{
			name: "namepool-themed legacy pooled bead records the themed start identity",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-fenrir", "template": "gascity/dog", "pool_slot": "1",
			}},
			cfg: &config.City{Agents: []config.Agent{{
				Dir: "gascity", Name: "dog", NamepoolNames: []string{"fenrir", "wolf"},
			}}},
			// Start records QualifiedInstanceName(poolInstanceName("dog",1,agent))
			// = "gascity/fenrir"; the numeric "gascity/dog-1" would not join.
			want: "gascity/fenrir",
		},
		{
			name: "namepool-themed legacy pooled bead resolves the second slot",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-wolf", "template": "gascity/dog", "pool_slot": "2",
			}},
			cfg: &config.City{Agents: []config.Agent{{
				Dir: "gascity", Name: "dog", NamepoolNames: []string{"fenrir", "wolf"},
			}}},
			want: "gascity/wolf",
		},
		{
			name: "non-themed pooled bead with cfg keeps the qualified numbered instance",
			bead: beads.Bead{Metadata: map[string]string{
				"session_name": "s-dog-3", "template": "gascity/dog", "pool_slot": "3",
			}},
			cfg:  &config.City{Agents: []config.Agent{{Dir: "gascity", Name: "dog"}}},
			want: "gascity/dog-3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionAgentMetricIdentity(tc.bead, tc.cfg); got != tc.want {
				t.Fatalf("sessionAgentMetricIdentity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStopTargetsForNames_LegacyPooledIdentityJoinsStartIdentity drives the
// metric identity through the real stop-target builder for a legacy aliasless
// pooled session bead. The resulting stopTarget.agentName feeds
// gc.agent.stops.total, so it must equal the start path's "<template>-<slot>"
// instance identity rather than the bare base template.
func TestStopTargetsForNames_LegacyPooledIdentityJoinsStartIdentity(t *testing.T) {
	const sessionName = "s-dog-3-legacy"
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "legacy pooled session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "dog",
			"pool_slot":    "3",
			// No agent_name and no agent: label: the legacy aliasless pooled
			// shape the migration must keep joinable.
		},
	}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	var stderr bytes.Buffer
	targets := stopTargetsForNames([]string{sessionName}, &config.City{}, store, &stderr)
	if len(targets) != 1 {
		t.Fatalf("stopTargetsForNames returned %d targets, want 1", len(targets))
	}
	if got := targets[0].agentName; got != "dog-3" {
		t.Fatalf("stopTarget.agentName = %q, want dog-3 (must join the start identity, not the bare template)", got)
	}
}

// TestFinalizeDrainAckStoppedSession_WitnessBranchDoesNotRecordMetric pins the
// drain-ack idempotency fix: when this observer only witnesses that another
// actor already closed the drained bead, it must NOT increment
// gc.agent.stops.total — otherwise every redundant NDI observer inflates the
// monotonic stop counter for a single drain-ack.
func TestFinalizeDrainAckStoppedSession_WitnessBranchDoesNotRecordMetric(t *testing.T) {
	const sessionName = "gascity--gc__worker"
	const identity = "gascity/gc.worker"
	reader := installManualMetricReader(t)
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	session := env.createSessionBead(sessionName, identity)
	env.setSessionMetadata(&session, map[string]string{"agent_name": identity})
	patch := sessionpkg.DrainAckStopPendingPatch(env.clk.Now().UTC())
	if err := env.store.SetMetadataBatch(session.ID, patch); err != nil {
		t.Fatalf("SetMetadataBatch(stop-pending): %v", err)
	}
	session.Metadata = patch.Apply(session.Metadata)

	// Another observer already closed the drained bead. This finalize call is
	// then a witness: it must observe the close (line 341 branch) without
	// re-recording the stop metric.
	if err := env.store.Close(session.ID); err != nil {
		t.Fatalf("pre-closing the bead to force the witness branch: %v", err)
	}

	result := finalizeDrainAckStoppedSession(
		"", env.cfg, env.store, nil, sessiontest.SeedBead(t, session), identity, true,
		newFakeDrainOps(), env.dt, env.clk, events.NewFake(), &env.stderr,
	)

	// W-tick dropped the raw session.Status="closed" mirror; the witness close is
	// carried by result.witnessInfo (Closed), which applyTo folds onto the snapshot.
	if !result.applyTo(sessiontest.SeedBead(t, session)).Closed {
		t.Fatalf("witness-branch result fold not Closed; the witness path must fold the authoritative closed Info")
	}
	if points := collectCounterDataPoints(t, reader, "gc.agent.stops.total"); len(points) != 0 {
		t.Fatalf("gc.agent.stops.total datapoints = %+v, want none on the witness branch", points)
	}
}

// TestRecordWakeFailure_QuarantineLegacyPooledIdentity pins the attempt-3 review
// fix: a legacy aliasless pooled session bead (template + pool_slot, no
// agent_name and no agent: label) that quarantines via repeated wake failures
// must record gc.agent.quarantines.total with the start-path-joinable instance
// identity — "<template>-<slot>" for a non-themed pool and the themed instance
// for a namepool pool — never the bare template. The identity is resolved at the
// call site exactly as checkStability does, through sessionAgentMetricIdentity.
func TestRecordWakeFailure_QuarantineLegacyPooledIdentity(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	t.Run("non-themed pool joins template-slot, never the bare template", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4", // one below threshold (defaultMaxWakeAttempts=5)
			"template":      "dog",
			"pool_slot":     "3",
			"session_name":  "s-dog-3-legacy",
		})

		recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max attempts")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog-3"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=dog-3: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog"}) {
			t.Fatalf("gc.agent.quarantines.total must not label agent with the bare template: %+v", points)
		}
	})

	t.Run("namepool-themed pool joins the themed start identity", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		cfg := &config.City{Agents: []config.Agent{{
			Dir: "gascity", Name: "dog", NamepoolNames: []string{"fenrir", "wolf"},
		}}}
		session := makeBead("b1", map[string]string{
			"wake_attempts": "4",
			"template":      "gascity/dog",
			"pool_slot":     "1",
			"session_name":  "s-fenrir-legacy",
		})

		recordWakeFailure(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, cfg))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max attempts")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/fenrir"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=gascity/fenrir: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/dog-1"}) {
			t.Fatalf("gc.agent.quarantines.total must not label the themed pool with the numeric fallback: %+v", points)
		}
	})
}

// TestRecordChurn_QuarantineLegacyPooledIdentity mirrors
// TestRecordWakeFailure_QuarantineLegacyPooledIdentity for the context-churn
// quarantine producer.
func TestRecordChurn_QuarantineLegacyPooledIdentity(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	t.Run("non-themed pool joins template-slot, never the bare template", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		session := makeBead("b1", map[string]string{
			"churn_count":  "2", // one below threshold (defaultMaxChurnCycles=3)
			"template":     "dog",
			"pool_slot":    "3",
			"session_name": "s-dog-3-legacy",
		})

		recordChurn(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, nil))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max churn cycles")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog-3"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=dog-3: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "dog"}) {
			t.Fatalf("gc.agent.quarantines.total must not label agent with the bare template: %+v", points)
		}
	})

	t.Run("namepool-themed pool joins the themed start identity", func(t *testing.T) {
		reader := installManualMetricReader(t)
		store := newTestStore()
		cfg := &config.City{Agents: []config.Agent{{
			Dir: "gascity", Name: "dog", NamepoolNames: []string{"fenrir", "wolf"},
		}}}
		session := makeBead("b1", map[string]string{
			"churn_count":  "2",
			"template":     "gascity/dog",
			"pool_slot":    "2",
			"session_name": "s-wolf-legacy",
		})

		recordChurn(seedSessionInfo(session), sessionFrontDoor(store), clk, sessionAgentMetricIdentity(session, cfg))
		syncBeadFromStore(&session, store)

		if session.Metadata["quarantined_until"] == "" {
			t.Fatal("fixture must quarantine at max churn cycles")
		}
		points := collectCounterDataPoints(t, reader, "gc.agent.quarantines.total")
		if !hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/wolf"}) {
			t.Fatalf("gc.agent.quarantines.total has no datapoint with agent=gascity/wolf: %+v", points)
		}
		if hasDataPointWithStringAttrs(points, map[string]string{"agent": "gascity/dog-2"}) {
			t.Fatalf("gc.agent.quarantines.total must not label the themed pool with the numeric fallback: %+v", points)
		}
	})
}

// TestStopTargetsForNames_NamepoolThemedPooledIdentityJoinsStart drives the stop
// metric identity through the real stop-target builder for a namepool-themed
// legacy aliasless pooled bead. stopTarget.agentName feeds gc.agent.stops.total
// and must equal the themed start identity (poolInstanceIdentity), not the
// numeric "<template>-<slot>" form. It also asserts the stop event subject shares
// that single resolved identity, guarding the deduplicated fallback path.
func TestStopTargetsForNames_NamepoolThemedPooledIdentityJoinsStart(t *testing.T) {
	const sessionName = "s-fenrir-legacy"
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "namepool-themed legacy pooled session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": sessionName,
			"template":     "gascity/dog",
			"pool_slot":    "1",
			// No agent_name and no agent: label: the legacy aliasless pooled
			// shape whose themed identity the migration must keep joinable.
		},
	}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{
		Dir: "gascity", Name: "dog", NamepoolNames: []string{"fenrir", "wolf"},
	}}}
	var stderr bytes.Buffer
	targets := stopTargetsForNames([]string{sessionName}, cfg, store, &stderr)
	if len(targets) != 1 {
		t.Fatalf("stopTargetsForNames returned %d targets, want 1", len(targets))
	}
	if got := targets[0].agentName; got != "gascity/fenrir" {
		t.Fatalf("stopTarget.agentName = %q, want gascity/fenrir (themed start identity, not gascity/dog-1)", got)
	}
	if got := targets[0].subject; got != "gascity/fenrir" {
		t.Fatalf("stopTarget.subject = %q, want gascity/fenrir (subject must share the resolved metric identity)", got)
	}
}
