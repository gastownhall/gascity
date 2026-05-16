package main

import (
	"context"
	"io"
	"path/filepath"
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
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "worker-1",
			"template":           "repo/worker",
			"agent_name":         "worker",
			"provider":           "claude",
			"work_dir":           filepath.Join(cityDir, "repos", "worker"),
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("Create start bead: %v", err)
	}
	drainBead, err := store.Create(beads.Bead{
		Title:  "db",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "db-1",
			"template":           "repo/db",
			"agent_name":         "db",
			"provider":           "claude",
			"work_dir":           filepath.Join(cityDir, "repos", "db"),
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
			Command:      "trace-worker --resume",
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
		"",
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

func TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 20, 0, 0, time.UTC)

	tests := []struct {
		name                string
		template            string
		wantStartCandidates int
		setup               func(t *testing.T, cityDir string, store beads.Store, sp runtime.Provider) (*config.City, DesiredStateResult, *sessionBeadSnapshot)
	}{
		{
			name:                "named session post-kill",
			template:            "dispatcher",
			wantStartCandidates: 1,
			setup: func(t *testing.T, cityDir string, store beads.Store, sp runtime.Provider) (*config.City, DesiredStateResult, *sessionBeadSnapshot) {
				t.Helper()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "trace-town"},
					Session:   config.SessionConfig{Provider: "fake"},
					Agents: []config.Agent{{
						Name:              "dispatcher",
						StartCommand:      "true",
						MaxActiveSessions: intPtr(1),
					}},
					NamedSessions: []config.NamedSession{{
						Template: "dispatcher",
						Mode:     "on_demand",
					}},
				}
				if _, err := store.Create(beads.Bead{
					Title:  "queued dispatcher work",
					Type:   "task",
					Status: "open",
					Metadata: map[string]string{
						"gc.routed_to": "dispatcher",
					},
				}); err != nil {
					t.Fatalf("Create named work: %v", err)
				}
				sessionName := config.NamedSessionRuntimeName("trace-town", cfg.Workspace, "dispatcher")
				if _, err := store.Create(beads.Bead{
					Title:  sessionName,
					Type:   sessionBeadType,
					Labels: []string{sessionBeadLabel},
					Metadata: map[string]string{
						"session_name":               sessionName,
						"alias":                      "dispatcher",
						"template":                   "dispatcher",
						"state":                      "asleep",
						"generation":                 "1",
						"continuation_epoch":         "1",
						"instance_token":             "named-token",
						namedSessionMetadataKey:      boolMetadata(true),
						namedSessionIdentityMetadata: "dispatcher",
						namedSessionModeMetadata:     "on_demand",
					},
				}); err != nil {
					t.Fatalf("Create named session: %v", err)
				}
				dsResult := buildDesiredState("trace-town", cityDir, now, cfg, sp, store, io.Discard)
				if !dsResult.NamedSessionDemand["dispatcher"] {
					t.Fatal("NamedSessionDemand[dispatcher] = false, want true")
				}
				snapshot, err := loadSessionBeadSnapshot(store)
				if err != nil {
					t.Fatalf("load session snapshot: %v", err)
				}
				return cfg, dsResult, snapshot
			},
		},
		{
			name:                "pool respawn after drain",
			template:            "repo/worker",
			wantStartCandidates: 1,
			setup: func(t *testing.T, cityDir string, store beads.Store, sp runtime.Provider) (*config.City, DesiredStateResult, *sessionBeadSnapshot) {
				t.Helper()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "trace-town"},
					Session:   config.SessionConfig{Provider: "fake"},
					Agents: []config.Agent{{
						Name:              "worker",
						Dir:               "repo",
						StartCommand:      "true",
						MinActiveSessions: intPtr(0),
						MaxActiveSessions: intPtr(5),
					}},
				}
				createRoutedReadyWork(t, store, "repo/worker", 1)
				dsResult := buildDesiredState("trace-town", cityDir, now, cfg, sp, store, io.Discard)
				if got := dsResult.ScaleCheckCounts["repo/worker"]; got != 1 {
					t.Fatalf("ScaleCheckCounts[repo/worker] = %d, want 1", got)
				}
				snapshot, err := loadSessionBeadSnapshot(store)
				if err != nil {
					t.Fatalf("load session snapshot: %v", err)
				}
				return cfg, dsResult, snapshot
			},
		},
		{
			name:                "pool grows past min active sessions",
			template:            "repo/worker",
			wantStartCandidates: 3,
			setup: func(t *testing.T, cityDir string, store beads.Store, sp runtime.Provider) (*config.City, DesiredStateResult, *sessionBeadSnapshot) {
				t.Helper()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "trace-town"},
					Session:   config.SessionConfig{Provider: "fake"},
					Agents: []config.Agent{{
						Name:              "worker",
						Dir:               "repo",
						StartCommand:      "true",
						MinActiveSessions: intPtr(3),
						MaxActiveSessions: intPtr(100),
					}},
				}
				createRoutedReadyWork(t, store, "repo/worker", 6)
				for slot := 1; slot <= 3; slot++ {
					session := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, slot)
					if err := store.SetMetadata(session.ID, "state", "active"); err != nil {
						t.Fatalf("set active state: %v", err)
					}
					if err := store.SetMetadata(session.ID, "pending_create_claim", ""); err != nil {
						t.Fatalf("clear pending create claim: %v", err)
					}
					if err := store.SetMetadata(session.ID, "pending_create_started_at", ""); err != nil {
						t.Fatalf("clear pending create timestamp: %v", err)
					}
					if err := sp.Start(context.Background(), session.Metadata["session_name"], runtime.Config{}); err != nil {
						t.Fatalf("seed active runtime session: %v", err)
					}
				}
				dsResult := buildDesiredState("trace-town", cityDir, now, cfg, sp, store, io.Discard)
				if got := dsResult.ScaleCheckCounts["repo/worker"]; got != 6 {
					t.Fatalf("ScaleCheckCounts[repo/worker] = %d, want 6", got)
				}
				snapshot, err := loadSessionBeadSnapshot(store)
				if err != nil {
					t.Fatalf("load session snapshot: %v", err)
				}
				return cfg, dsResult, snapshot
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			writeCityTOML(t, cityDir, "trace-town", "worker")
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			cfg, dsResult, sessionBeads := tc.setup(t, cityDir, store, sp)

			tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
			if !tracer.Enabled() {
				t.Fatal("tracer should be enabled")
			}
			if _, err := tracer.armStore.upsertArm(TraceArm{
				ScopeType:      TraceArmScopeTemplate,
				ScopeValue:     tc.template,
				Source:         TraceArmSourceManual,
				Level:          TraceModeDetail,
				ArmedAt:        now,
				ExpiresAt:      now.Add(15 * time.Minute),
				LastExtendedAt: now,
				UpdatedAt:      now,
			}); err != nil {
				t.Fatalf("upsert arm: %v", err)
			}
			cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", now, cfg)
			if cycle == nil {
				t.Fatal("BeginCycle returned nil")
			}
			cycle.syncArms(now, cfg)
			cr := &CityRuntime{
				cityPath:            cityDir,
				cityName:            "trace-town",
				cfg:                 cfg,
				sp:                  sp,
				trace:               tracer,
				standaloneCityStore: store,
				sessionDrains:       newDrainTracker(),
				rec:                 events.NewFake(),
				pokeCh:              make(chan struct{}, 1),
				stdout:              io.Discard,
				stderr:              io.Discard,
			}
			cr.beadReconcileTick(context.Background(), dsResult, sessionBeads, cycle)
			if !cr.waitForAsyncStarts() {
				t.Fatal("async starts did not finish")
			}
			if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "gh1654"}); err != nil {
				t.Fatalf("cycle.End: %v", err)
			}
			if err := tracer.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: cycle.traceID})
			if err != nil {
				t.Fatalf("ReadTraceRecords: %v", err)
			}
			var startCandidates int
			haveWorkRequestedSummary := false
			for _, rec := range records {
				if rec.RecordType == TraceRecordTemplateTickSummary && rec.Template == tc.template {
					if rec.EvaluationStatus != TraceEvaluationEligible {
						t.Fatalf("template summary evaluation_status = %q, want eligible", rec.EvaluationStatus)
					}
					if !traceFieldBool(rec.Fields["work_requested"]) {
						t.Fatalf("template summary work_requested = %#v, want true", rec.Fields["work_requested"])
					}
					haveWorkRequestedSummary = true
				}
				if rec.RecordType == TraceRecordDecision &&
					rec.SiteCode == TraceSiteReconcilerWakeDecision &&
					rec.Template == tc.template &&
					rec.OutcomeCode == TraceOutcomeStartCandidate {
					startCandidates++
				}
			}
			if !haveWorkRequestedSummary {
				t.Fatalf("missing work-requested template summary for %s", tc.template)
			}
			if startCandidates != tc.wantStartCandidates {
				t.Fatalf("start_candidate decisions = %d, want %d", startCandidates, tc.wantStartCandidates)
			}
		})
	}
}

func createRoutedReadyWork(t *testing.T, store beads.Store, template string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := store.Create(beads.Bead{
			Title:  "queued work",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": template,
			},
		}); err != nil {
			t.Fatalf("Create routed work: %v", err)
		}
	}
}

func createCanonicalPoolSession(t *testing.T, store beads.Store, cfgAgent *config.Agent, now time.Time, slot int) beads.Bead {
	t.Helper()
	_, qualifiedInstance := poolInstanceIdentity(cfgAgent, slot, io.Discard)
	session, err := createPoolSessionBead(store, cfgAgent.QualifiedName(), nil, now, poolSessionCreateIdentity{
		AgentName: qualifiedInstance,
		Alias:     qualifiedInstance,
		Slot:      slot,
	})
	if err != nil {
		t.Fatalf("create pool session: %v", err)
	}
	return session
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

func traceFieldBool(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
