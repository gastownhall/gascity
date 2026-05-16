//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

const fakePressurePath = "/fake/pressure/io"

// withFakePressureFile swaps fsPressureReadFile / fsPressurePath for the
// duration of the test. Tests MUST be hermetic and never touch real /proc.
func withFakePressureFile(t *testing.T, content []byte, readErr error) {
	t.Helper()
	withFakePressureReader(t, func(string) ([]byte, error) {
		if readErr != nil {
			return nil, readErr
		}
		return content, nil
	})
}

func withFakePressureReader(t *testing.T, read func(string) ([]byte, error)) {
	t.Helper()
	origRead := fsPressureReadFile
	origPath := fsPressurePath
	fsPressurePath = fakePressurePath
	fsPressureReadFile = func(p string) ([]byte, error) {
		if p != fakePressurePath {
			t.Fatalf("readFile called with unexpected path %q (want %q)", p, fakePressurePath)
		}
		return read(p)
	}
	t.Cleanup(func() {
		fsPressureReadFile = origRead
		fsPressurePath = origPath
	})
}

func resetFSPressureWarningsForTest(t *testing.T) {
	t.Helper()
	fsPressureInvalidThresholdWarned.Store(false)
	fsPressureReadErrorWarned.Store(false)
	t.Cleanup(func() {
		fsPressureInvalidThresholdWarned.Store(false)
		fsPressureReadErrorWarned.Store(false)
	})
}

const samplePressureLow = `some avg10=0.00 avg60=1.23 avg300=0.50 total=12345
full avg10=0.00 avg60=0.11 avg300=0.05 total=2345
`

const samplePressureHigh = `some avg10=80.12 avg60=75.45 avg300=60.10 total=999999
full avg10=40.00 avg60=30.00 avg300=20.00 total=77777
`

func TestFSPressureReadAvg60_Low(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	v, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 1.23 {
		t.Fatalf("expected 1.23, got %v", v)
	}
}

func TestFSPressureReadAvg60_High(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	v, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 75.45 {
		t.Fatalf("expected 75.45, got %v", v)
	}
}

func TestFSPressureReadAvg60_MalformedNoSomeLine(t *testing.T) {
	withFakePressureFile(t, []byte("garbage\nmore garbage\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error for malformed file, got nil")
	}
}

func TestFSPressureReadAvg60_MalformedNoAvg60(t *testing.T) {
	withFakePressureFile(t, []byte("some avg10=0.00 total=1\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error when avg60 missing, got nil")
	}
}

func TestFSPressureReadAvg60_UnparseableNumber(t *testing.T) {
	withFakePressureFile(t, []byte("some avg10=0.00 avg60=NOT_A_NUM total=1\n"), nil)
	if _, err := readFSPressureAvg60(fsPressurePath); err == nil {
		t.Fatal("expected error when avg60 unparseable, got nil")
	}
}

func TestShouldSkipTickForFSPressure_Below(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to NOT skip tick when avg60 below threshold")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log output when not skipping, got %q", buf.String())
	}
}

func TestShouldSkipTickForFSPressure_Above(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if !cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to skip tick when avg60 above threshold")
	}
	out := buf.String()
	if !strings.Contains(out, "supervisor: FS pressure high") {
		t.Fatalf("expected warning log, got %q", out)
	}
	if !strings.Contains(out, "avg60=75.45") {
		t.Fatalf("expected avg60=75.45 in log, got %q", out)
	}
	if !strings.Contains(out, "threshold=50.0") {
		t.Fatalf("expected threshold=50.0 in log, got %q", out)
	}
	if !strings.Contains(out, "skipping tick") {
		t.Fatalf("expected 'skipping tick' in log, got %q", out)
	}
}

func TestShouldSkipTickForFSPressure_MalformedProceeds(t *testing.T) {
	// Malformed file -> readFSPressureAvg60 returns error -> gate fails open.
	resetFSPressureWarningsForTest(t)
	withFakePressureFile(t, []byte("not a PSI file"), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to proceed (fail open) on malformed pressure file")
	}
	out := buf.String()
	if !strings.Contains(out, "supervisor: FS pressure unavailable") || !strings.Contains(out, fakePressurePath) {
		t.Fatalf("expected one-shot fail-open warning with pressure path, got %q", out)
	}
	buf.Reset()
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to proceed (fail open) on repeated malformed pressure file")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected read-failure warning to be one-shot, got %q", buf.String())
	}
}

func TestShouldSkipTickForFSPressure_MissingFileProceeds(t *testing.T) {
	resetFSPressureWarningsForTest(t)
	withFakePressureFile(t, nil, errFakeMissing{})
	t.Setenv(fsPressureThresholdEnv, "")
	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to proceed when pressure file cannot be read")
	}
	out := buf.String()
	if !strings.Contains(out, "fake: file not found") || !strings.Contains(out, fakePressurePath) {
		t.Fatalf("expected fail-open warning with read error and pressure path, got %q", out)
	}
}

type errFakeMissing struct{}

func (errFakeMissing) Error() string { return "fake: file not found" }

func TestShouldSkipTickForFSPressure_EnvThresholdOverride(t *testing.T) {
	// With low pressure but a very low threshold, we should now skip.
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "1.0") // 1.23 > 1.0 -> skip
	var buf bytes.Buffer
	cr := &CityRuntime{stderr: &buf}
	if !cr.shouldSkipTickForFSPressure(nil, "patrol") {
		t.Fatal("expected to skip when env threshold overridden below measured value")
	}
	if !strings.Contains(buf.String(), "threshold=1.0") {
		t.Fatalf("expected threshold=1.0 in log, got %q", buf.String())
	}
}

func TestCityRuntimeTickSkipsBeforeManagedDoltAndDemandUnderFSPressure(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")

	var buildCalls atomic.Int32
	var managedDoltCalls atomic.Int32
	var stderr bytes.Buffer
	rec := events.NewFake()
	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			buildCalls.Add(1)
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		dops:          newDrainOps(sp),
		rec:           rec,
		sessionDrains: newDrainTracker(),
		logPrefix:     "gc test",
		stdout:        io.Discard,
		stderr:        &stderr,
		managedDoltOwned: func(string) (bool, error) {
			managedDoltCalls.Add(1)
			return true, nil
		},
		managedDoltPort: func(string) string {
			return ""
		},
		managedDoltHealth: func(string) error {
			managedDoltCalls.Add(1)
			return nil
		},
	}

	dirty := &atomic.Bool{}
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")

	if got := managedDoltCalls.Load(); got != 0 {
		t.Fatalf("managed dolt calls = %d, want 0 before pressure-skip gate", got)
	}
	if got := buildCalls.Load(); got != 0 {
		t.Fatalf("build desired calls = %d, want 0 before pressure-skip gate", got)
	}
	if !strings.Contains(stderr.String(), "FS pressure high") {
		t.Fatalf("stderr = %q, want FS pressure skip warning", stderr.String())
	}

	evts, err := rec.List(events.Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("events = %#v, want one FS pressure event", evts)
	}
	if evts[0].Type != events.SupervisorFSPressureSkippedTick {
		t.Fatalf("event type = %q, want %q", evts[0].Type, events.SupervisorFSPressureSkippedTick)
	}
	var payload events.SupervisorFSPressureSkippedTickPayload
	if err := json.Unmarshal(evts[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Avg60 != 75.45 || payload.Threshold != defaultFSPressureThreshold {
		t.Fatalf("payload = %+v, want avg60 and default threshold", payload)
	}
	if payload.ConsecutiveSkips != 1 || payload.MaxConsecutiveSkips != maxConsecutiveFSPressureSkips {
		t.Fatalf("payload skip counts = %+v, want first skip of bounded policy", payload)
	}
	if payload.Outcome != fsPressureOutcomeSkipped {
		t.Fatalf("payload outcome = %q, want %q", payload.Outcome, fsPressureOutcomeSkipped)
	}
}

func TestCityRuntimeTickSkipsDueOrderDispatchUnderFSPressure(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")

	store := beads.NewMemStore()
	releaseExec := make(chan struct{})
	execStarted := make(chan struct{}, 1)
	fakeExec := func(ctx context.Context, _, _ string, _ []string) ([]byte, error) {
		select {
		case execStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseExec:
			return []byte("ok\n"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ad := buildOrderDispatcherFromListExec(
		[]orders.Order{{Name: "pressure-due", Trigger: "cooldown", Interval: "1s", Exec: "scripts/noop.sh"}},
		store, nil, fakeExec, nil,
	)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	if mad, ok := ad.(*memoryOrderDispatcher); ok {
		t.Cleanup(mad.cancel)
	}
	t.Cleanup(func() { close(releaseExec) })

	var buildCalls atomic.Int32
	var stderr bytes.Buffer
	rec := events.NewFake()
	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		sp:                  sp,
		standaloneCityStore: store,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			buildCalls.Add(1)
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		dops:          newDrainOps(sp),
		od:            ad,
		rec:           rec,
		sessionDrains: newDrainTracker(),
		logPrefix:     "gc test",
		stdout:        io.Discard,
		stderr:        &stderr,
		managedDoltOwned: func(string) (bool, error) {
			t.Fatal("managed dolt preflight should not run before pressure-skip gate")
			return false, nil
		},
	}

	dirty := &atomic.Bool{}
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")

	if got := buildCalls.Load(); got != 0 {
		t.Fatalf("build desired calls = %d, want 0 before pressure-skip gate", got)
	}
	tracking, err := store.ListByLabel("order-run:pressure-due", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("list order tracking beads: %v", err)
	}
	if len(tracking) != 0 {
		t.Fatalf("order tracking beads = %#v, want none while FS pressure skips tick", tracking)
	}
	select {
	case <-execStarted:
		t.Fatal("order exec started during pressure-skipped tick")
	default:
	}
	if !strings.Contains(stderr.String(), "FS pressure high") {
		t.Fatalf("stderr = %q, want FS pressure skip warning", stderr.String())
	}
	evts, err := rec.List(events.Filter{Type: events.SupervisorFSPressureSkippedTick})
	if err != nil {
		t.Fatalf("list FS pressure events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("FS pressure events = %#v, want one skip event", evts)
	}
	var payload events.SupervisorFSPressureSkippedTickPayload
	if err := json.Unmarshal(evts[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Outcome != fsPressureOutcomeSkipped {
		t.Fatalf("payload outcome = %q, want %q", payload.Outcome, fsPressureOutcomeSkipped)
	}
}

func TestCityRuntimeTickForcesRunAfterMaxConsecutiveFSPressureSkips(t *testing.T) {
	pressure := []byte(samplePressureHigh)
	withFakePressureReader(t, func(string) ([]byte, error) {
		return pressure, nil
	})
	t.Setenv(fsPressureThresholdEnv, "")

	var buildCalls atomic.Int32
	var stderr bytes.Buffer
	sp := runtime.NewFake()
	rec := events.NewFake()
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  sp,
		standaloneCityStore: beads.NewMemStore(),
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			buildCalls.Add(1)
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		dops:          newDrainOps(sp),
		trace:         newSessionReconcilerTraceManager(cityPath, "test-city", &stderr),
		rec:           rec,
		sessionDrains: newDrainTracker(),
		logPrefix:     "gc test",
		stdout:        io.Discard,
		stderr:        &stderr,
		managedDoltOwned: func(string) (bool, error) {
			return false, nil
		},
	}

	dirty := &atomic.Bool{}
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	pressure = []byte(samplePressureLow)
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	pressure = []byte(samplePressureHigh)
	for i := 0; i < maxConsecutiveFSPressureSkips+1; i++ {
		cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	}
	pressure = []byte(samplePressureLow)
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	pressure = []byte(samplePressureHigh)
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")

	if got := buildCalls.Load(); got != 3 {
		t.Fatalf("build desired calls = %d, want low-pressure ticks plus one forced tick", got)
	}
	stderrText := stderr.String()
	if got := strings.Count(stderrText, "skipping tick"); got != 3 {
		t.Fatalf("skip warnings = %d, want one per pressure episode; stderr=%q", got, stderrText)
	}
	if got := strings.Count(stderrText, "forcing tick after"); got != 1 {
		t.Fatalf("forced-run warnings = %d, want one; stderr=%q", got, stderrText)
	}

	wantConsecutive := []int{1, 1, 2, 3, 4, 5, 1}
	evts, err := rec.List(events.Filter{Type: events.SupervisorFSPressureSkippedTick})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != len(wantConsecutive)+1 {
		t.Fatalf("FS pressure events = %d, want %d skip events plus one force event: %#v", len(evts), len(wantConsecutive)+1, evts)
	}
	var skippedPayloads []events.SupervisorFSPressureSkippedTickPayload
	var forcedPayloads []events.SupervisorFSPressureSkippedTickPayload
	for i, evt := range evts {
		var payload events.SupervisorFSPressureSkippedTickPayload
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload %d: %v", i, err)
		}
		switch payload.Outcome {
		case fsPressureOutcomeSkipped:
			skippedPayloads = append(skippedPayloads, payload)
		case fsPressureOutcomeForced:
			forcedPayloads = append(forcedPayloads, payload)
		default:
			t.Fatalf("event %d outcome = %q, want %q or %q", i, payload.Outcome, fsPressureOutcomeSkipped, fsPressureOutcomeForced)
		}
	}
	if len(skippedPayloads) != len(wantConsecutive) {
		t.Fatalf("skip payloads = %d, want %d: %#v", len(skippedPayloads), len(wantConsecutive), skippedPayloads)
	}
	for i, payload := range skippedPayloads {
		if payload.ConsecutiveSkips != wantConsecutive[i] {
			t.Fatalf("event %d consecutive_skips = %d, want %d", i, payload.ConsecutiveSkips, wantConsecutive[i])
		}
		if payload.MaxConsecutiveSkips != maxConsecutiveFSPressureSkips {
			t.Fatalf("event %d max_consecutive_skips = %d, want %d", i, payload.MaxConsecutiveSkips, maxConsecutiveFSPressureSkips)
		}
	}
	if len(forcedPayloads) != 1 {
		t.Fatalf("forced payloads = %#v, want exactly one force event", forcedPayloads)
	}
	if forcedPayloads[0].ConsecutiveSkips != maxConsecutiveFSPressureSkips {
		t.Fatalf("forced consecutive_skips = %d, want %d", forcedPayloads[0].ConsecutiveSkips, maxConsecutiveFSPressureSkips)
	}

	if err := cr.trace.Close(); err != nil {
		t.Fatalf("close trace: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType:  TraceRecordDecision,
		SiteCode:    TraceSiteSupervisorFSPressure,
		ReasonCode:  TraceReasonFSPressure,
		OutcomeCode: TraceOutcomeSkipped,
	})
	if err != nil {
		t.Fatalf("read trace records: %v", err)
	}
	if len(records) != len(wantConsecutive) {
		t.Fatalf("FS pressure trace decisions = %d, want %d: %#v", len(records), len(wantConsecutive), records)
	}
	for i, record := range records {
		if record.TraceMode != TraceModeBaseline {
			t.Fatalf("trace decision %d mode = %q, want %q", i, record.TraceMode, TraceModeBaseline)
		}
		if record.TraceSource != TraceSourceAlwaysOn {
			t.Fatalf("trace decision %d source = %q, want %q", i, record.TraceSource, TraceSourceAlwaysOn)
		}
		if got := traceFieldInt(record.Fields["consecutive_skips"]); got != wantConsecutive[i] {
			t.Fatalf("trace decision %d consecutive_skips = %d, want %d", i, got, wantConsecutive[i])
		}
		if got := traceFieldInt(record.Fields["max_consecutive_skips"]); got != maxConsecutiveFSPressureSkips {
			t.Fatalf("trace decision %d max_consecutive_skips = %d, want %d", i, got, maxConsecutiveFSPressureSkips)
		}
		if got := record.Fields["trigger"]; got != "patrol" {
			t.Fatalf("trace decision %d trigger = %#v, want patrol", i, got)
		}
	}
	forcedRecords, err := ReadTraceRecords(traceCityRuntimeDir(cityPath), TraceFilter{
		RecordType:  TraceRecordDecision,
		SiteCode:    TraceSiteSupervisorFSPressure,
		ReasonCode:  TraceReasonFSPressure,
		OutcomeCode: TraceOutcomeApplied,
	})
	if err != nil {
		t.Fatalf("read forced trace records: %v", err)
	}
	if len(forcedRecords) != 1 {
		t.Fatalf("forced FS pressure trace decisions = %d, want 1: %#v", len(forcedRecords), forcedRecords)
	}
	if got := traceFieldInt(forcedRecords[0].Fields["consecutive_skips"]); got != maxConsecutiveFSPressureSkips {
		t.Fatalf("forced trace consecutive_skips = %d, want %d", got, maxConsecutiveFSPressureSkips)
	}
	if got := forcedRecords[0].Fields["outcome"]; got != fsPressureOutcomeForced {
		t.Fatalf("forced trace outcome field = %#v, want %q", got, fsPressureOutcomeForced)
	}
}

func TestCityRuntimeManualReloadBypassesFSPressureSkipUntilDemandRefresh(t *testing.T) {
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)

	doneCh := make(chan reloadControlReply, 1)
	dirty := &atomic.Bool{}
	dirty.Store(true)
	sp := runtime.NewFake()
	var buildCalls atomic.Int32
	var stdout bytes.Buffer
	rec := events.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			buildCalls.Add(1)
			select {
			case reply := <-doneCh:
				t.Fatalf("manual reload replied before desired-state rebuild under FS pressure: %+v", reply)
			default:
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    rec,
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	cr.od = nil
	cr.activeReload = &reloadRequest{doneCh: doneCh}
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool

	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "poke")

	if got := buildCalls.Load(); got != 1 {
		select {
		case reply := <-doneCh:
			t.Fatalf("manual reload replied without desired-state rebuild under FS pressure: %+v", reply)
		default:
			t.Fatalf("build desired calls = %d, want reload tick to bypass FS pressure skip", got)
		}
	}
	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeNoChange {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeNoChange)
		}
	default:
		t.Fatal("manual reload did not reply after demand refresh")
	}
	evts, err := rec.List(events.Filter{Type: events.SupervisorFSPressureSkippedTick})
	if err != nil {
		t.Fatalf("list FS pressure events: %v", err)
	}
	if len(evts) != 0 {
		t.Fatalf("FS pressure skip events = %#v, want none for manual reload refresh tick", evts)
	}
}
