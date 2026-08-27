package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestMeasureReconcilerPerfComparePairsAllProductionPaths(t *testing.T) {
	report, err := measureReconcilerPerfCompare(
		t.Context(),
		3,
		1,
		t.TempDir(),
		validReconcilerPerfProvenance(),
	)
	if err != nil {
		t.Fatalf("measure paired start, stop, and nudge: %v", err)
	}

	if !strings.Contains(report.Provenance.Store, "MemStore") ||
		!strings.Contains(report.Provenance.Store, "NewAtomicCloseMemStore(stop,atomic in-memory)") ||
		!strings.Contains(report.Provenance.Store, "nudgequeue state file") ||
		!strings.Contains(report.Provenance.Runtime, "synthetic") ||
		!strings.Contains(report.Provenance.Workload, "workload=reconciler-synthetic-v2") ||
		!strings.Contains(report.Provenance.Workload, "latency=action-needed-to-provider-entry") ||
		!strings.Contains(report.Provenance.Workload, "fresh-isolated-single-session-alternating-sequential-pairs") ||
		!strings.Contains(report.Provenance.Workload, "tmux") ||
		!strings.Contains(report.Provenance.Workload, "Dolt") ||
		!strings.Contains(report.Provenance.Workload, "wake-socket/IPC") ||
		!strings.Contains(report.Provenance.Workload, "contention") {
		t.Fatalf("provenance = %+v, want explicit synthetic provenance", report.Provenance)
	}
	if report.Coverage.MeasuredActions != 3 || len(report.Coverage.MissingActions) != 0 {
		t.Fatalf("coverage = %+v, want all reconciler actions measured", report.Coverage)
	}
	if report.Warmup.PairsPerAction != 1 || !report.Warmup.Excluded {
		t.Fatalf("warmup policy = %+v, want one excluded pair", report.Warmup)
	}
	if len(report.Actions) != 3 {
		t.Fatalf("actions = %d, want 3", len(report.Actions))
	}
	start := report.Actions[0]
	if start.Action != reconcilerPerfActionStart ||
		start.PairCount != 3 ||
		start.MismatchCount != 0 {
		t.Fatalf("start comparison = %+v", start)
	}
	for name, arm := range map[string]reconcilerPerfArmSummary{
		"legacy": start.Legacy,
		"keyed":  start.Keyed,
	} {
		if arm.AttemptedCount != 3 ||
			arm.SampleCount != 3 ||
			arm.ErrorCount != 0 ||
			arm.MeasurementWindowNS <= 0 ||
			arm.ThroughputPerSecond <= 0 ||
			arm.Latency == nil {
			t.Errorf("%s summary = %+v, want three successful measured starts", name, arm)
		}
	}
	stop := report.Actions[1]
	if stop.Action != reconcilerPerfActionStop ||
		stop.PairCount != 3 ||
		stop.MismatchCount != 0 {
		t.Fatalf("stop comparison = %+v", stop)
	}
	for name, arm := range map[string]reconcilerPerfArmSummary{
		"legacy": stop.Legacy,
		"keyed":  stop.Keyed,
	} {
		if arm.AttemptedCount != 3 ||
			arm.SampleCount != 3 ||
			arm.ErrorCount != 0 ||
			arm.MeasurementWindowNS <= 0 ||
			arm.ThroughputPerSecond <= 0 ||
			arm.Latency == nil {
			t.Errorf("%s stop summary = %+v, want three successful measured stops", name, arm)
		}
	}
	nudge := report.Actions[2]
	if nudge.Action != reconcilerPerfActionNudge ||
		nudge.PairCount != 3 ||
		nudge.MismatchCount != 0 {
		t.Fatalf("nudge comparison = %+v", nudge)
	}
	for name, arm := range map[string]reconcilerPerfArmSummary{
		"legacy": nudge.Legacy,
		"keyed":  nudge.Keyed,
	} {
		if arm.AttemptedCount != 3 ||
			arm.SampleCount != 3 ||
			arm.ErrorCount != 0 ||
			arm.MeasurementWindowNS <= 0 ||
			arm.ThroughputPerSecond <= 0 ||
			arm.Latency == nil {
			t.Errorf("%s nudge summary = %+v, want three successful measured nudges", name, arm)
		}
	}
}

func TestReconcilerPerfNudgeFixturesAreDeterministicAcrossArms(t *testing.T) {
	legacy, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "pair-001")
	if err != nil {
		t.Fatalf("new legacy nudge fixture: %v", err)
	}
	keyed, err := newReconcilerPerfNudgeFixture(t.TempDir(), "keyed", "pair-001")
	if err != nil {
		t.Fatalf("new keyed nudge fixture: %v", err)
	}
	if legacy.info.ID != keyed.info.ID {
		t.Fatalf("session IDs = %q/%q, want identical deterministic IDs", legacy.info.ID, keyed.info.ID)
	}
	if legacy.item.ID != "reconciler-perf-nudge-pair-001" || legacy.item.ID != keyed.item.ID {
		t.Fatalf("nudge IDs = %q/%q, want deterministic pair-derived ID", legacy.item.ID, keyed.item.ID)
	}
	if !legacy.item.CreatedAt.Equal(reconcilerPerfNudgeFixtureTime) ||
		!legacy.item.DeliverAfter.Equal(reconcilerPerfNudgeFixtureTime) ||
		!legacy.item.ExpiresAt.Equal(reconcilerPerfNudgeFixtureExpiry) ||
		!legacy.item.CreatedAt.Equal(keyed.item.CreatedAt) ||
		!legacy.item.DeliverAfter.Equal(keyed.item.DeliverAfter) ||
		!legacy.item.ExpiresAt.Equal(keyed.item.ExpiresAt) {
		t.Fatalf("fixture timestamps legacy=%+v keyed=%+v, want fixed identical timestamps", legacy.item, keyed.item)
	}
	legacyActivity, err := legacy.provider.GetLastActivity(legacy.info.SessionName)
	if err != nil {
		t.Fatalf("legacy GetLastActivity: %v", err)
	}
	keyedActivity, err := keyed.provider.GetLastActivity(keyed.info.SessionName)
	if err != nil {
		t.Fatalf("keyed GetLastActivity: %v", err)
	}
	if !legacyActivity.Equal(reconcilerPerfNudgeFixtureActivity) || !legacyActivity.Equal(keyedActivity) {
		t.Fatalf("fixture activity = %s/%s, want fixed identical %s", legacyActivity, keyedActivity, reconcilerPerfNudgeFixtureActivity)
	}
}

func TestLegacyReconcilerPerfNudgeInstallsStoreBeforeNeededAt(t *testing.T) {
	fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "store-boundary")
	if err != nil {
		t.Fatalf("new nudge fixture: %v", err)
	}
	installed := false
	fixture.onStoreInstalled = func() { installed = true }
	fixture.measurementNow = func() time.Time {
		if !installed {
			t.Error("legacy neededAt was recorded before installing the nudge store")
		}
		return time.Now()
	}
	measurement, err := measureLegacyReconcilerPerfNudgeFixture(t.Context(), fixture)
	if err != nil {
		t.Fatalf("measure legacy nudge: %v", err)
	}
	if measurement.sample.Error != "" {
		t.Fatalf("legacy measurement = %+v, want success", measurement.sample)
	}
}

func TestReconcilerPerfNudgeLatencyEndsAtProviderEntry(t *testing.T) {
	fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "entry-latency")
	if err != nil {
		t.Fatalf("new nudge fixture: %v", err)
	}
	neededAt := time.Unix(100, 0)
	enteredAt := neededAt.Add(17 * time.Millisecond)
	fixture.provider.now = func() time.Time { return enteredAt }
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fixture.provider.entered = entered
	fixture.provider.block = release
	delivered := make(chan error, 1)
	go func() {
		count, dispatchErr := fixture.dispatchLegacy()
		if dispatchErr == nil && count != 1 {
			dispatchErr = errors.New("legacy nudge did not deliver exactly once")
		}
		delivered <- dispatchErr
	}()
	select {
	case <-entered:
	case <-time.After(reconcilerPerfArmTimeout):
		t.Fatal("provider Nudge was not entered")
	}
	close(release)
	if err := <-delivered; err != nil {
		t.Fatalf("dispatch nudge: %v", err)
	}
	measurement := fixture.finish(neededAt, enteredAt.Add(time.Second), nil)
	if measurement.sample.Error != "" || measurement.sample.LatencyNS == nil {
		t.Fatalf("nudge measurement = %+v, want successful latency sample", measurement.sample)
	}
	if got, want := *measurement.sample.LatencyNS, enteredAt.Sub(neededAt).Nanoseconds(); got != want {
		t.Fatalf("latency = %dns, want provider-entry timestamp %dns", got, want)
	}
	calls := fixture.provider.snapshotNudgeCalls()
	if len(calls) != 1 || calls[0].name != fixture.info.SessionName ||
		len(calls[0].content) == 0 || !strings.Contains(calls[0].content[0].Text, "reconciler performance nudge") {
		t.Fatalf("provider nudge calls = %+v, want one target/payload-correct entry", calls)
	}
}

func TestReconcilerPerfNudgeFinishRejectsDuplicateAndMissingDurableAck(t *testing.T) {
	t.Run("duplicate provider call", func(t *testing.T) {
		fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "duplicate")
		if err != nil {
			t.Fatalf("new nudge fixture: %v", err)
		}
		if _, err := fixture.dispatchLegacy(); err != nil {
			t.Fatalf("dispatch nudge: %v", err)
		}
		if err := fixture.provider.Nudge(fixture.info.SessionName, gcruntime.TextContent("duplicate")); err != nil {
			t.Fatalf("duplicate Nudge: %v", err)
		}
		measurement := fixture.finish(time.Now().Add(-time.Second), time.Now(), nil)
		if measurement.sample.Error == "" || measurement.sample.Outcome == "nudged_injected" {
			t.Fatalf("duplicate Nudge reported false success: %+v", measurement.sample)
		}
	})
	t.Run("missing durable acknowledgement", func(t *testing.T) {
		fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "missing-ack")
		if err != nil {
			t.Fatalf("new nudge fixture: %v", err)
		}
		if err := fixture.provider.Nudge(fixture.info.SessionName, gcruntime.TextContent("unacknowledged")); err != nil {
			t.Fatalf("Nudge: %v", err)
		}
		measurement := fixture.finish(time.Now().Add(-time.Second), time.Now(), nil)
		if measurement.sample.Error == "" || measurement.sample.Outcome == "nudged_injected" {
			t.Fatalf("unacknowledged Nudge reported false success: %+v", measurement.sample)
		}
	})
}

func TestReconcilerPerfNudgeFinishRejectsCorruptedPayload(t *testing.T) {
	fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "corrupt-payload")
	if err != nil {
		t.Fatalf("new nudge fixture: %v", err)
	}
	if _, err := fixture.dispatchLegacy(); err != nil {
		t.Fatalf("dispatch nudge: %v", err)
	}
	fixture.provider.mu.Lock()
	fixture.provider.calls[0].content = gcruntime.TextContent("corrupted")
	fixture.provider.mu.Unlock()
	measurement := fixture.finish(time.Now().Add(-time.Second), time.Now(), nil)
	if measurement.sample.Error == "" || measurement.sample.Outcome == "nudged_injected" {
		t.Fatalf("corrupted payload reported false success: %+v", measurement.sample)
	}
}

func TestReconcilerPerfNudgeFinishRejectsResidualOtherAgentItem(t *testing.T) {
	fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "legacy", "residual-other-agent")
	if err != nil {
		t.Fatalf("new nudge fixture: %v", err)
	}
	if _, err := fixture.dispatchLegacy(); err != nil {
		t.Fatalf("dispatch nudge: %v", err)
	}
	residual := newQueuedNudgeWithOptions("other-agent", "residual", "session", reconcilerPerfNudgeFixtureTime, queuedNudgeOptions{})
	residual.ID = "reconciler-perf-residual-other-agent"
	residual.CreatedAt = reconcilerPerfNudgeFixtureTime
	residual.DeliverAfter = reconcilerPerfNudgeFixtureTime
	residual.ExpiresAt = reconcilerPerfNudgeFixtureExpiry
	if err := enqueueQueuedNudgeWithStore(fixture.cityPath, beads.NudgesStore{Store: fixture.store}, residual); err != nil {
		t.Fatalf("enqueue residual nudge: %v", err)
	}
	measurement := fixture.finish(time.Now().Add(-time.Second), time.Now(), nil)
	if measurement.sample.Error == "" || measurement.sample.Outcome == "nudged_injected" {
		t.Fatalf("residual other-agent nudge reported false success: %+v", measurement.sample)
	}
}

func TestKeyedReconcilerPerfNudgeStopsBeforeRestoringStore(t *testing.T) {
	fixture, err := newReconcilerPerfNudgeFixture(t.TempDir(), "keyed", "cancel-store-order")
	if err != nil {
		t.Fatalf("new nudge fixture: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fixture.provider.entered = entered
	fixture.provider.block = release
	stopping := make(chan struct{})
	fixture.beforeControllerStop = func() { close(stopping) }
	previous := openNudgeBeadStore
	restoredOpenerReached := make(chan struct{}, 1)
	openNudgeBeadStore = func(string) beads.NudgesStore {
		select {
		case restoredOpenerReached <- struct{}{}:
		default:
		}
		return beads.NudgesStore{Store: fixture.store}
	}
	t.Cleanup(func() { openNudgeBeadStore = previous })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan reconcilerPerfNudgeMeasurement, 1)
	go func() {
		measurement, measureErr := measureKeyedReconcilerPerfNudgeFixture(ctx, fixture)
		if measureErr != nil {
			t.Errorf("measure keyed nudge: %v", measureErr)
		}
		done <- measurement
	}()
	select {
	case <-entered:
	case <-time.After(reconcilerPerfArmTimeout):
		t.Fatal("keyed provider Nudge was not entered")
	}
	cancel()
	select {
	case <-stopping:
	case <-time.After(reconcilerPerfArmTimeout):
		t.Fatal("keyed controller Stop did not begin")
	}
	close(release)
	<-done
	select {
	case <-restoredOpenerReached:
		t.Fatal("keyed worker reached the restored nudge-store opener")
	default:
	}
}

func TestMeasureReconcilerPerfCompareRejectsInvalidCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		iter   int
		warmup int
	}{
		{name: "zero iterations", iter: 0},
		{name: "negative iterations", iter: -1},
		{name: "negative warmup", iter: 1, warmup: -1},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := measureReconcilerPerfCompare(
				context.Background(),
				tt.iter,
				tt.warmup,
				t.TempDir(),
				validReconcilerPerfProvenance(),
			)
			if err == nil {
				t.Fatal("measureReconcilerPerfStart error = nil")
			}
		})
	}
}

func TestReconcilerPerfStopLatencyEndsAtProviderEntry(t *testing.T) {
	fixture, err := newReconcilerPerfStopFixture(t.TempDir(), "entry-latency")
	if err != nil {
		t.Fatalf("new stop fixture: %v", err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	fixture.provider.entered = entered
	fixture.provider.block = release
	neededAt := time.Unix(100, 0).UTC()
	stopEnteredAt := neededAt.Add(17 * time.Millisecond)
	fixture.provider.now = func() time.Time { return stopEnteredAt }
	tracker := &asyncStartTracker{}
	go finalizeDrainAckStopPendingSessions(
		fixture.cityPath, fixture.cfg, fixture.provider, beads.SessionStore{Store: fixture.store}, nil,
		[]sessionpkg.Info{fixture.info}, newFakeDrainOps(), newDrainTracker(), tracker, clock.Real{}, events.Discard, io.Discard,
	)
	select {
	case <-entered:
	case <-time.After(reconcilerPerfArmTimeout):
		t.Fatal("provider Stop was not entered")
	}
	close(release)
	if !tracker.wait(reconcilerPerfArmTimeout) {
		t.Fatal("blocked stop did not drain")
	}
	measurement := fixture.finish(neededAt, stopEnteredAt.Add(time.Second), nil)
	if measurement.sample.Error != "" || measurement.sample.LatencyNS == nil {
		t.Fatalf("stop measurement = %+v, want successful latency sample", measurement.sample)
	}
	if got, want := *measurement.sample.LatencyNS, stopEnteredAt.Sub(neededAt).Nanoseconds(); got != want {
		t.Fatalf("latency = %dns, want provider-entry timestamp %dns", got, want)
	}
}

func TestReconcilerPerfStopProviderSuppliesCompleteFreshDeath(t *testing.T) {
	fixture, err := newReconcilerPerfStopFixture(t.TempDir(), "fresh-death")
	if err != nil {
		t.Fatalf("new stop fixture: %v", err)
	}
	target := gcruntime.LivenessTarget{
		SessionID:            fixture.info.ID,
		SessionName:          fixture.sessionName,
		ProcessNames:         drainAckStopPendingProcessNames(fixture.cfg, fixture.info),
		IncarnationStartedAt: drainAckIncarnationStartedAt(fixture.info),
	}

	before := gcruntime.ObserveFreshLiveness(fixture.provider, target)
	if !before.Running || !before.Alive || !before.Complete {
		t.Errorf("fresh liveness before stop = %+v, want live complete observation", before)
	}
	if err := fixture.provider.StopUnattendedSession(fixture.sessionName, fixture.info.InstanceToken); err != nil {
		t.Fatalf("stop fixture runtime: %v", err)
	}
	after := gcruntime.ObserveFreshLiveness(fixture.provider, target)
	if after.Running || after.Alive || !after.Complete {
		t.Errorf("fresh liveness after stop = %+v, want absent complete observation", after)
	}

	oldTimeout := drainAckStopConfirmDeadTimeout
	drainAckStopConfirmDeadTimeout = 0
	defer func() { drainAckStopConfirmDeadTimeout = oldTimeout }()
	if !confirmDrainAckRuntimeDead(
		fixture.cityPath,
		fixture.store,
		fixture.provider,
		fixture.cfg,
		target.SessionID,
		target.SessionName,
		fixture.info.InstanceToken,
		target.ProcessNames,
		io.Discard,
		target.IncarnationStartedAt,
		true,
	) {
		t.Error("strict confirm-dead rejected complete fresh absence")
	}
	if got := fixture.provider.CountCalls("Stop", fixture.sessionName); got != 1 {
		t.Fatalf("provider Stop calls = %d, want exactly 1", got)
	}
}

func TestReconcilerPerfStopMismatchAndFailureAreNotSuccess(t *testing.T) {
	tests := []struct {
		name   string
		pairID string
		setup  func(*reconcilerPerfStopFixture)
	}{
		{
			name:   "token mismatch",
			pairID: "mismatch",
			setup: func(fixture *reconcilerPerfStopFixture) {
				if err := fixture.provider.SetMeta(fixture.sessionName, "GC_INSTANCE_TOKEN", "replacement-token"); err != nil {
					t.Fatalf("set mismatch token: %v", err)
				}
			},
		},
		{
			name:   "stop failure",
			pairID: "failure",
			setup: func(fixture *reconcilerPerfStopFixture) {
				fixture.provider.StopErrors[fixture.sessionName] = errors.New("stop failed")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture, err := newReconcilerPerfStopFixture(t.TempDir(), test.pairID)
			if err != nil {
				t.Fatalf("new stop fixture: %v", err)
			}
			test.setup(fixture)
			tracker := &asyncStartTracker{}
			neededAt := time.Now().UTC()
			lease := fixture.lease
			if _, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{SessionID: fixture.info.ID, Source: sessionStartAdmissionInProcess, PoolDrainAck: &lease}, exactSessionStartParams{
				CityPath: fixture.cityPath, Config: fixture.cfg, Provider: fixture.provider, Store: fixture.store,
				Clock: clock.Real{}, Recorder: events.Discard, Stdout: io.Discard, Stderr: io.Discard, AsyncStopTracker: tracker,
				AuthorizePoolDrainAck: fixture.authorizePoolDrainAck,
			}); !errors.Is(err, errSessionStartPoolDrainAckPending) {
				t.Fatalf("keyed stop reconciliation: %v", err)
			}
			if !tracker.wait(reconcilerPerfArmTimeout) {
				t.Fatal("keyed failed stop did not drain")
			}
			measurement := fixture.finish(neededAt, time.Now().UTC(), nil)
			if measurement.sample.Error == "" || measurement.sample.Outcome == "stopped_runtime_dead_pending_finalize" {
				t.Fatalf("failed stop reported false success: %+v", measurement.sample)
			}
		})
	}
}

func TestWaitReconcilerPerfStopTrackerDistinguishesCancellationAndTimeout(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		tracker := &asyncStartTracker{}
		done, ok := tracker.start()
		if !ok {
			t.Fatal("start tracker = false")
		}
		defer done()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitReconcilerPerfStopTracker(ctx, tracker, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		tracker := &asyncStartTracker{}
		done, ok := tracker.start()
		if !ok {
			t.Fatal("start tracker = false")
		}
		defer done()
		err := waitReconcilerPerfStopTracker(context.Background(), tracker, 0)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("wait error = %v, want timeout", err)
		}
	})
}

func TestReconcilerPerfStopResultErrorSynthesizesTerminalFailure(t *testing.T) {
	result := sessionStartReconcileResult{Outcome: sessionStartReconcileExhausted}
	if err := reconcilerPerfStopResultError(result); err == nil ||
		!strings.Contains(err.Error(), string(sessionStartReconcileExhausted)) {
		t.Fatalf("result error = %v, want synthesized exhausted error", err)
	}
}

func TestRunPerfReconcilerCompareEmitsVersionedJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"perf", "reconciler-compare", "--iter", "2", "--warmup", "0", "--json"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf(
			"gc perf reconciler-compare --json exit = %d, stderr=%q stdout=%q",
			code,
			stderr.String(),
			stdout.String(),
		)
	}

	var report reconcilerPerfReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.SchemaVersion != reconcilerPerfSchemaV1 ||
		!report.OK ||
		report.Provenance.Workload != "workload=reconciler-synthetic-v2; latency=action-needed-to-provider-entry; fresh-isolated-single-session-alternating-sequential-pairs; excludes=tmux,Dolt,wake-socket/IPC,contention" ||
		len(report.Actions) != 3 ||
		report.Actions[0].Action != reconcilerPerfActionStart ||
		report.Actions[1].Action != reconcilerPerfActionStop ||
		report.Actions[2].Action != reconcilerPerfActionNudge ||
		report.Actions[0].PairCount != 2 ||
		report.Actions[1].PairCount != 2 ||
		report.Actions[2].PairCount != 2 {
		t.Fatalf("JSON report = %+v", report)
	}
}

func TestPerfReconcilerCompareFlagDefaults(t *testing.T) {
	t.Parallel()

	cmd := newPerfReconcilerCompareCmd(nil)
	iter, _ := cmd.Flags().GetInt("iter")
	warmup, _ := cmd.Flags().GetInt("warmup")
	jsonOut, _ := cmd.Flags().GetBool("json")
	if iter != 20 || warmup != 2 || jsonOut {
		t.Fatalf("defaults = iter:%d warmup:%d json:%t, want 20/2/false", iter, warmup, jsonOut)
	}
}
