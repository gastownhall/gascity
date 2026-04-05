package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionReconcilerTraceLifecycleRecordsTick(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "trace-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{
			{
				Name:              "polecat",
				Dir:               "repo",
				MinActiveSessions: intPtr(1),
				MaxActiveSessions: intPtr(1),
				ScaleCheck:        "",
				WorkQuery:         "",
				SlingQuery:        "",
			},
		},
	}

	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title: "polecat",
		Metadata: map[string]string{
			"session_name":       "polecat-1",
			"template":           "repo/polecat",
			"agent_name":         "polecat",
			"state":              "active",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), bead.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("seed provider session: %v", err)
	}

	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	armNow := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        armNow,
		ExpiresAt:      armNow.Add(15 * time.Minute),
		LastExtendedAt: armNow,
		UpdatedAt:      armNow,
	}); err != nil {
		t.Fatalf("upsert arm: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            cityDir,
		cityName:            "trace-town",
		cfg:                 cfg,
		sp:                  sp,
		trace:               tracer,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", armNow, cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.configRevision = "rev-trace-1"
	cycle.syncArms(armNow, cfg)

	result := DesiredStateResult{
		State: map[string]TemplateParams{
			"polecat-1": {
				TemplateName: "repo/polecat",
				SessionName:  "polecat-1",
				InstanceName: "polecat-1",
			},
		},
		ScaleCheckCounts: map[string]int{"repo/polecat": 1},
	}
	cr.beadReconcileTick(context.Background(), result, sessionBeads, cycle)
	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected trace records, got none")
	}

	var (
		haveCycleStart        bool
		haveCycleResult       bool
		haveTraceControlStart bool
		haveInputSnapshot     bool
		haveTemplateConfig    bool
		haveTemplateSummary   bool
		haveSessionBaseline   bool
		haveSessionResult     bool
		cycleResult           SessionReconcilerTraceRecord
	)
	for _, rec := range records {
		if rec.TraceID != cycle.traceID {
			t.Fatalf("trace_id = %q, want %q", rec.TraceID, cycle.traceID)
		}
		if rec.TickID != cycle.tickID {
			t.Fatalf("tick_id = %q, want %q", rec.TickID, cycle.tickID)
		}
		if rec.TraceSchemaVersion != sessionReconcilerTraceSchemaVersion {
			t.Fatalf("trace_schema_version = %d, want %d", rec.TraceSchemaVersion, sessionReconcilerTraceSchemaVersion)
		}
		switch rec.RecordType {
		case TraceRecordCycleStart:
			haveCycleStart = true
		case TraceRecordTraceControl:
			if rec.Fields["action"] == "start" && rec.Fields["scope_value"] == "repo/polecat" {
				haveTraceControlStart = true
				if rec.TraceMode != TraceModeBaseline {
					t.Fatalf("trace_control trace_mode = %q, want baseline", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceAlwaysOn {
					t.Fatalf("trace_control trace_source = %q, want always_on", rec.TraceSource)
				}
			}
		case TraceRecordCycleInputSnapshot:
			haveInputSnapshot = true
			if got := traceFieldInt(rec.Fields["desired_session_count"]); got != 1 {
				t.Fatalf("desired_session_count = %#v, want 1", rec.Fields["desired_session_count"])
			}
			if got := traceFieldInt(rec.Fields["open_session_count"]); got != 1 {
				t.Fatalf("open_session_count = %#v, want 1", rec.Fields["open_session_count"])
			}
		case TraceRecordTemplateConfig:
			if rec.Template == "repo/polecat" {
				haveTemplateConfig = true
				if rec.TraceMode != TraceModeDetail {
					t.Fatalf("template config trace_mode = %q, want detail", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceManual {
					t.Fatalf("template config trace_source = %q, want manual", rec.TraceSource)
				}
			}
		case TraceRecordTemplateTickSummary:
			if rec.Template == "repo/polecat" {
				haveTemplateSummary = true
				if rec.TraceMode != TraceModeBaseline {
					t.Fatalf("template summary trace_mode = %q, want baseline", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceAlwaysOn {
					t.Fatalf("template summary trace_source = %q, want always_on", rec.TraceSource)
				}
				if rec.EvaluationStatus != TraceEvaluationEligible {
					t.Fatalf("template summary evaluation_status = %q, want eligible", rec.EvaluationStatus)
				}
			}
		case TraceRecordSessionBaseline:
			if rec.Template == "repo/polecat" {
				haveSessionBaseline = true
				if rec.TraceMode != TraceModeBaseline {
					t.Fatalf("session baseline trace_mode = %q, want baseline", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceAlwaysOn {
					t.Fatalf("session baseline trace_source = %q, want always_on", rec.TraceSource)
				}
			}
		case TraceRecordSessionResult:
			if rec.Template == "repo/polecat" {
				haveSessionResult = true
				if rec.CompletenessStatus != TraceCompletenessComplete {
					t.Fatalf("session result completeness_status = %q, want complete", rec.CompletenessStatus)
				}
			}
		case TraceRecordCycleResult:
			haveCycleResult = true
			cycleResult = rec
		}
	}

	if !haveCycleStart {
		t.Fatal("missing cycle_start record")
	}
	if !haveCycleResult {
		t.Fatal("missing cycle_result record")
	}
	if !haveTraceControlStart {
		t.Fatal("missing trace_control start record")
	}
	if !haveInputSnapshot {
		t.Fatal("missing cycle_input_snapshot record")
	}
	if !haveTemplateConfig {
		t.Fatal("missing template_config_snapshot record")
	}
	if !haveTemplateSummary {
		t.Fatal("missing template_tick_summary record")
	}
	if !haveSessionBaseline {
		t.Fatal("missing session_baseline record")
	}
	if !haveSessionResult {
		t.Fatal("missing session_result record")
	}
	if cycleResult.CompletionStatus != TraceCompletionCompleted {
		t.Fatalf("cycle_result completion_status = %q, want completed", cycleResult.CompletionStatus)
	}
	if got, want := cycleResult.RecordCount, len(records)-1; got != want {
		t.Fatalf("cycle_result record_count = %d, want %d", got, want)
	}
	if got := cycleResult.ConfigRevision; got != "rev-trace-1" {
		t.Fatalf("cycle_result config_revision = %q, want rev-trace-1", got)
	}
}

func TestSessionReconcilerTraceStartAndDrainSubOps(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "trace-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "repo", MaxActiveSessions: intPtr(1)},
			{Name: "db", Dir: "repo", MaxActiveSessions: intPtr(1)},
		},
	}

	store := beads.NewMemStore()
	startBead, err := store.Create(beads.Bead{
		Title: "worker",
		Metadata: map[string]string{
			"session_name":       "worker-1",
			"template":           "repo/worker",
			"agent_name":         "worker",
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("Create start bead: %v", err)
	}
	drainBead, err := store.Create(beads.Bead{
		Title: "db",
		Metadata: map[string]string{
			"session_name":       "db-1",
			"template":           "repo/db",
			"agent_name":         "db",
			"state":              "active",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("Create drain bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), drainBead.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("seed drain session: %v", err)
	}

	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	armNow := time.Date(2026, 3, 8, 12, 10, 0, 0, time.UTC)
	for _, template := range []string{"repo/worker", "repo/db"} {
		if _, err := tracer.armStore.upsertArm(TraceArm{
			ScopeType:      TraceArmScopeTemplate,
			ScopeValue:     template,
			Source:         TraceArmSourceManual,
			Level:          TraceModeDetail,
			ArmedAt:        armNow,
			ExpiresAt:      armNow.Add(15 * time.Minute),
			LastExtendedAt: armNow,
			UpdatedAt:      armNow,
		}); err != nil {
			t.Fatalf("upsert arm %s: %v", template, err)
		}
	}

	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", armNow, cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.configRevision = "rev-trace-2"
	cycle.syncArms(armNow, cfg)

	startCand := startCandidate{
		session: &startBead,
		tp: TemplateParams{
			TemplateName: "repo/worker",
			SessionName:  "worker-1",
			InstanceName: "worker-1",
		},
	}
	wakeCount := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{startCand},
		cfg,
		map[string]TemplateParams{
			"worker-1": startCand.tp,
		},
		sp,
		store,
		"trace-town",
		clock.Real{},
		events.NewFake(),
		5*time.Second,
		io.Discard,
		io.Discard,
		cycle,
	)
	if wakeCount != 1 {
		t.Fatalf("wakeCount = %d, want 1", wakeCount)
	}

	drainTracker := newDrainTracker()
	drainTracker.set(drainBead.ID, &drainState{
		startedAt:  armNow.Add(-time.Minute),
		deadline:   armNow.Add(time.Minute),
		reason:     "idle",
		generation: 1,
	})
	drainLookup := func(id string) *beads.Bead {
		switch id {
		case drainBead.ID:
			clone := drainBead
			return &clone
		case startBead.ID:
			clone := startBead
			return &clone
		default:
			return nil
		}
	}
	wakeEvals := map[string]wakeEvaluation{
		drainBead.ID: {Reasons: nil},
	}
	advanceSessionDrainsWithSessionsTraced(
		drainTracker,
		sp,
		store,
		drainLookup,
		[]beads.Bead{drainBead},
		wakeEvals,
		cfg,
		map[string]int{},
		nil,
		nil,
		clock.Real{},
		cycle,
	)

	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "start-drain"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected trace records, got none")
	}

	var haveStartOp, haveStartMutation, haveDrainMutation, haveCycleResult bool
	for _, rec := range records {
		switch rec.RecordType {
		case TraceRecordOperation:
			if rec.SiteCode == TraceSiteLifecycleStartRun {
				haveStartOp = true
				if rec.TraceMode != TraceModeDetail {
					t.Fatalf("start operation trace_mode = %q, want detail", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceManual {
					t.Fatalf("start operation trace_source = %q, want manual", rec.TraceSource)
				}
				if rec.OutcomeCode != TraceOutcomeSuccess {
					t.Fatalf("start operation outcome = %q, want success", rec.OutcomeCode)
				}
			}
		case TraceRecordMutation:
			if rec.SiteCode == TraceSiteMutationBeadMetadata && rec.Fields["template"] == "repo/worker" {
				haveStartMutation = true
			}
			if rec.SiteCode == TraceSiteMutationRuntimeMeta && rec.Fields["template"] == "repo/db" && rec.Fields["field"] == "GC_DRAIN_ACK" {
				haveDrainMutation = true
				if rec.TraceMode != TraceModeDetail {
					t.Fatalf("drain mutation trace_mode = %q, want detail", rec.TraceMode)
				}
				if rec.TraceSource != TraceSourceManual {
					t.Fatalf("drain mutation trace_source = %q, want manual", rec.TraceSource)
				}
				if rec.OutcomeCode != TraceOutcomeSuccess {
					t.Fatalf("drain mutation outcome = %q, want success", rec.OutcomeCode)
				}
			}
		case TraceRecordCycleResult:
			haveCycleResult = true
			if rec.CompletionStatus != TraceCompletionCompleted {
				t.Fatalf("cycle_result completion_status = %q, want completed", rec.CompletionStatus)
			}
		}
	}

	if !haveStartOp {
		t.Fatal("missing start execute operation record")
	}
	if !haveStartMutation {
		t.Fatal("missing start mutation record")
	}
	if !haveDrainMutation {
		t.Fatal("missing drain GC_DRAIN_ACK mutation record")
	}
	if !haveCycleResult {
		t.Fatal("missing cycle_result record")
	}
}

func traceFieldInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case uint:
		return int(n)
	case uint8:
		return int(n)
	case uint16:
		return int(n)
	case uint32:
		return int(n)
	case uint64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
