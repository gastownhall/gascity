package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestTraceDetailScopesIncludesDependencies(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "polecat",
				Dir:       "repo",
				DependsOn: []string{"repo/db"},
			},
			{
				Name: "db",
				Dir:  "repo",
			},
		},
	}
	scopes := buildTraceDetailScopes(cfg, []TraceArm{{
		ScopeType:  TraceArmScopeTemplate,
		ScopeValue: "repo/polecat",
		Source:     TraceArmSourceManual,
		Level:      TraceModeDetail,
	}})
	if got := scopes["repo/polecat"]; got != TraceSourceManual {
		t.Fatalf("direct scope = %q, want %q", got, TraceSourceManual)
	}
	if got := scopes["repo/db"]; got != TraceSourceDerivedDependency {
		t.Fatalf("dependency scope = %q, want %q", got, TraceSourceDerivedDependency)
	}
}

func TestConfigDriftTracePayloadIncludesDriftedFields(t *testing.T) {
	payload := configDriftTracePayload("stored", "current", []string{"CopyFiles"}, traceRecordPayload{
		"active_reason": "attached",
	})

	fields, ok := payload["drifted_fields"].([]string)
	if !ok {
		t.Fatalf("drifted_fields type = %T, want []string", payload["drifted_fields"])
	}
	if len(fields) != 1 || fields[0] != "CopyFiles" {
		t.Fatalf("drifted_fields = %v, want [CopyFiles]", fields)
	}
	if payload["stored_hash"] != "stored" || payload["current_hash"] != "current" {
		t.Fatalf("hash fields missing from payload: %#v", payload)
	}
	if payload["active_reason"] != "attached" {
		t.Fatalf("extra field not preserved: %#v", payload)
	}
}

func TestConfigDriftTracePayloadReservedFieldsOverrideExtras(t *testing.T) {
	payload := configDriftTracePayload("stored", "current", []string{"CopyFiles"}, traceRecordPayload{
		"stored_hash":    "extra-stored",
		"current_hash":   "extra-current",
		"drifted_fields": []string{"Command"},
	})

	fields, ok := payload["drifted_fields"].([]string)
	if !ok {
		t.Fatalf("drifted_fields type = %T, want []string", payload["drifted_fields"])
	}
	if len(fields) != 1 || fields[0] != "CopyFiles" {
		t.Fatalf("drifted_fields = %v, want [CopyFiles]", fields)
	}
	if payload["stored_hash"] != "stored" || payload["current_hash"] != "current" {
		t.Fatalf("reserved hash fields should win over extras: %#v", payload)
	}
}

func TestTraceArmStorePersistence(t *testing.T) {
	cityDir := t.TempDir()
	store := newSessionReconcilerTraceArmStore(cityDir)
	now := time.Now().UTC()
	state, err := store.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        now,
		ExpiresAt:      now.Add(15 * time.Minute),
		LastExtendedAt: now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("upsertArm: %v", err)
	}
	if len(state.Arms) != 1 {
		t.Fatalf("arms after upsert = %d, want 1", len(state.Arms))
	}
	loaded, err := store.list()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(loaded.Arms) != 1 {
		t.Fatalf("loaded arms = %d, want 1", len(loaded.Arms))
	}
	cleared, err := store.remove(TraceArmScopeTemplate, "repo/polecat", false)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(cleared.Arms) != 0 {
		t.Fatalf("arms after remove = %d, want 0", len(cleared.Arms))
	}
}

func TestTraceReaderFiltersAndRecoveryIgnoresTail(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	decision := SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            "cycle-a",
		TickID:             "trace-1",
		RecordType:         TraceRecordDecision,
		Template:           "repo/polecat",
		SessionName:        "polecat-1",
		SiteCode:           TraceSiteReconcilerWakeDecision,
		TraceMode:          TraceModeDetail,
		TraceSource:        TraceSourceManual,
		Ts:                 time.Now().UTC(),
	}
	op := decision
	op.RecordType = TraceRecordOperation
	op.Seq = 0
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{decision, op}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var segments []string
	root := filepath.Join(cityDir, ".gc", "runtime", sessionReconcilerTraceRootDir)
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d != nil && !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			segments = append(segments, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk segments: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	tail := SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            "cycle-b",
		TickID:             "trace-2",
		RecordType:         TraceRecordDecision,
		Template:           "repo/polecat",
		SessionName:        "polecat-2",
		SiteCode:           TraceSiteReconcilerWakeDecision,
		TraceMode:          TraceModeDetail,
		TraceSource:        TraceSourceManual,
		Ts:                 time.Now().UTC().Add(time.Second),
	}
	tailData, err := json.Marshal(tail)
	if err != nil {
		t.Fatalf("marshal tail: %v", err)
	}
	f, err := os.OpenFile(segments[0], os.O_APPEND|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if _, err := fmt.Fprintf(f, "%s\n", tailData); err != nil {
		f.Close() //nolint:errcheck
		t.Fatalf("append tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close segment: %v", err)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck

	seq, err := reopened.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	if seq != 3 {
		t.Fatalf("LatestSeq = %d, want 3", seq)
	}
	headSeq, err := traceHeadSeq(root)
	if err != nil {
		t.Fatalf("traceHeadSeq: %v", err)
	}
	if headSeq != 3 {
		t.Fatalf("traceHeadSeq = %d, want 3", headSeq)
	}

	records, err := reopened.List(TraceFilter{Template: "repo/polecat"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("List len = %d, want 2 committed data records", len(records))
	}
	if records[0].Seq >= records[1].Seq {
		t.Fatalf("records not ordered by seq: %#v", records)
	}
	if records[1].RecordType != TraceRecordOperation {
		t.Fatalf("last record type = %s, want %s", records[1].RecordType, TraceRecordOperation)
	}

	next := decision
	next.TraceID = "cycle-c"
	next.TickID = "trace-3"
	next.Ts = time.Now().UTC().Add(2 * time.Second)
	if err := reopened.AppendBatch([]SessionReconcilerTraceRecord{next}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}

	segments = segments[:0]
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d != nil && !d.IsDir() && filepath.Ext(path) == ".jsonl" {
			segments = append(segments, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk segments after reopen append: %v", err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments after reopen append = %d, want 2", len(segments))
	}
}

func TestTraceAutoArmPromotesBufferedDetail(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}

	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(
		TraceSiteDesiredStateBuild,
		TraceReasonNoDemand,
		TraceOutcomeNoChange,
		"repo/polecat",
		"polecat-1",
		map[string]any{"step": "before"},
	)
	cycle.RecordDecision(
		TraceSiteReconcilerPendingCreate,
		TraceReasonPendingCreateRollback,
		TraceOutcomeFailed,
		"repo/polecat",
		"polecat-1",
		map[string]any{"step": "trigger"},
	)
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var beforeFound, triggerFound, controlFound bool
	for _, rec := range records {
		if rec.RecordType == TraceRecordDecision && rec.Fields["step"] == "before" {
			beforeFound = true
			if rec.TraceMode != TraceModeDetail {
				t.Fatalf("before record trace_mode = %q, want detail", rec.TraceMode)
			}
			if rec.TraceSource != TraceSourceAuto {
				t.Fatalf("before record trace_source = %q, want auto", rec.TraceSource)
			}
		}
		if rec.RecordType == TraceRecordDecision && rec.Fields["step"] == "trigger" {
			triggerFound = true
		}
		if rec.RecordType == TraceRecordTraceControl && rec.Fields["action"] == "start" {
			controlFound = true
		}
	}
	if !beforeFound {
		t.Fatal("buffered pre-anomaly decision was not promoted")
	}
	if !triggerFound {
		t.Fatal("triggering anomaly decision missing")
	}
	if !controlFound {
		t.Fatal("auto-arm trace control record missing")
	}
}

func TestTraceRecoveryQuarantinesInteriorCorruption(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	base := SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		TraceID:            "cycle-a",
		TickID:             "trace-1",
		RecordType:         TraceRecordDecision,
		Template:           "repo/polecat",
		SessionName:        "polecat-1",
		SiteCode:           TraceSiteReconcilerWakeDecision,
		TraceMode:          TraceModeDetail,
		TraceSource:        TraceSourceManual,
		Ts:                 time.Now().UTC(),
	}
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{base}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch 1: %v", err)
	}
	second := base
	second.TraceID = "cycle-b"
	second.TickID = "trace-2"
	second.Ts = second.Ts.Add(time.Second)
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{second}, TraceDurabilityDurable); err != nil {
		t.Fatalf("append batch 2: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	root := filepath.Join(cityDir, ".gc", "runtime", sessionReconcilerTraceRootDir)
	segments, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(segments))
	}
	data, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	corrupted := false
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		if rec["record_type"] == string(TraceRecordBatchCommit) {
			rec["record_count"] = float64(99)
			updated, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal corrupt commit: %v", err)
			}
			lines[i] = string(updated)
			corrupted = true
			break
		}
	}
	if !corrupted {
		t.Fatal("did not find batch commit to corrupt")
	}
	if err := os.WriteFile(segments[0], []byte(strings.Join(lines, "\n")+"\n"), sessionReconcilerTraceOwnerFilePerm); err != nil {
		t.Fatalf("rewrite corrupted segment: %v", err)
	}

	reopened, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close() //nolint:errcheck

	quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantine: %v", err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined files = %d, want 1", len(quarantined))
	}
	segments, err = filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments after reopen: %v", err)
	}
	if len(segments) != 0 {
		t.Fatalf("segments after quarantine = %d, want 0", len(segments))
	}
}

func TestTraceCycleResultRollupIncludesFlushedRecords(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	now := time.Now().UTC()
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        now,
		ExpiresAt:      now.Add(15 * time.Minute),
		LastExtendedAt: now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("upsertArm: %v", err)
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordDecision(
		TraceSiteDesiredStateBuild,
		TraceReasonRetained,
		TraceOutcomeApplied,
		"repo/polecat",
		"polecat-1",
		map[string]any{"step": "before-flush"},
	)
	if err := cycle.flushCurrentBatch(TraceDurabilityMetadata); err != nil {
		t.Fatalf("flushCurrentBatch: %v", err)
	}
	cycle.RecordOperation(
		TraceSiteLifecycleStartExecute,
		TraceReasonWake,
		TraceOutcomeApplied,
		"provider_start",
		"repo/polecat",
		"polecat-1",
		25*time.Millisecond,
		map[string]any{"step": "after-flush"},
	)
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var cycleResult *SessionReconcilerTraceRecord
	for i := range records {
		if records[i].RecordType == TraceRecordCycleResult {
			cycleResult = &records[i]
			break
		}
	}
	if cycleResult == nil {
		t.Fatal("cycle_result missing")
	}
	decisionCounts, _ := cycleResult.Fields["decision_counts"].(map[string]any)
	if got := decisionCounts[string(TraceSiteDesiredStateBuild)]; got != float64(1) {
		t.Fatalf("decision_counts[%q] = %#v, want 1", TraceSiteDesiredStateBuild, got)
	}
	operationCounts, _ := cycleResult.Fields["operation_counts"].(map[string]any)
	if got := operationCounts[string(TraceSiteLifecycleStartExecute)]; got != float64(1) {
		t.Fatalf("operation_counts[%q] = %#v, want 1", TraceSiteLifecycleStartExecute, got)
	}
	templatesTouched, _ := cycleResult.Fields["templates_touched"].([]any)
	if len(templatesTouched) != 1 || templatesTouched[0] != "repo/polecat" {
		t.Fatalf("templates_touched = %#v, want [repo/polecat]", templatesTouched)
	}
}

func TestRecordControllerOperationIsAlwaysOnBaseline(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordControllerOperation(
		TraceSiteDesiredStateBuild,
		TraceReasonRetained,
		TraceOutcomeApplied,
		"load_demand_snapshot",
		42*time.Millisecond,
		map[string]any{"phase": "demand"},
	)
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var op *SessionReconcilerTraceRecord
	for i := range records {
		if records[i].RecordType == TraceRecordOperation && records[i].SiteCode == TraceSiteDesiredStateBuild {
			op = &records[i]
			break
		}
	}
	if op == nil {
		t.Fatal("controller operation missing")
	}
	if op.TraceMode != TraceModeBaseline {
		t.Fatalf("trace_mode = %q, want baseline", op.TraceMode)
	}
	if op.TraceSource != TraceSourceAlwaysOn {
		t.Fatalf("trace_source = %q, want always_on", op.TraceSource)
	}
	if op.OperationID == "" {
		t.Fatal("operation_id should be populated")
	}
	if op.DurationMS != 42 {
		t.Fatalf("duration_ms = %d, want 42", op.DurationMS)
	}
	if op.Fields["phase"] != "demand" {
		t.Fatalf("phase field = %#v, want demand", op.Fields["phase"])
	}
}

// TestReconcileTraceResultsObservePostTickValues pins that the RESULTS trace
// recorder observes POST-tick values after the row reshape (Blocker 3 drift):
// the reconciler's WriteBackReconcileInfos folds its post-tick Info snapshot onto
// the carrier, and recordReconcileTraceResults reads that carrier. A dedup-retired
// loser must be traced under its retired session_name="" (RetireNamedSessionPatch
// clears session_name), not its pre-retire name. If the writeback is dropped or the
// recorder reads the pre-tick input, the loser is traced under its pre-retire name
// and this pin fails.
func TestReconcileTraceResultsObservePostTickValues(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	cityName := config.EffectiveCityName(cfg, "")
	spec, ok := session.FindNamedSessionSpec(cfg, cityName, "mayor")
	if !ok {
		t.Fatal("named spec for mayor not resolvable; fixture cfg no longer resolves it")
	}
	store := beads.NewMemStore()
	mk := func(gen, sessName string) string {
		b, err := store.Create(beads.Bead{
			Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "mayor", "configured_named_session": "true",
				"configured_named_identity": "mayor", "generation": gen, "session_name": sessName,
			},
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return b.ID
	}
	_ = mk("5", spec.SessionName) // winner (canonical name + higher gen)
	loserName := spec.SessionName + "-stale"
	loserID := mk("3", loserName)

	all, err := session.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	snap := newSessionBeadSnapshot(all)

	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, cityName, io.Discard)
	defer tracer.Close() //nolint:errcheck
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), cfg)

	reconcileSessionBeadsTracedWithNamedDemand(
		context.Background(), cityDir, snap.OpenForReconcile(), snap, nil, map[string]bool{},
		cfg, runtime.NewFake(), beads.SessionStore{Store: store}, nil, nil, nil, nil,
		newDrainTracker(), nil, nil, nil, false, nil, cityName, nil, clock.Real{},
		events.Discard, 0, 0, io.Discard, io.Discard, cycle,
	)

	// Production flow: after the reconciler's writeback, the RESULTS recorder reads
	// the post-tick carrier (sessionBeads.OpenInfos()).
	cr := &CityRuntime{cfg: cfg}
	cr.recordReconcileTraceResults(cycle, snap.OpenInfos(), func(TraceSiteCode, string, time.Time, map[string]any) {})

	// Find the RESULTS record for the loser bead (by id-derived post-tick lookup:
	// the retired loser's session_name is now "", so assert NO result record still
	// carries the pre-retire loser name).
	sawPreRetireName := false
	for _, rec := range cycle.records {
		if rec.RecordType != TraceRecordSessionResult {
			continue
		}
		if rec.SessionName == loserName {
			sawPreRetireName = true
		}
	}
	if sawPreRetireName {
		t.Fatalf("RESULTS trace recorded the retired loser under its PRE-retire name %q — the post-tick writeback/observation regressed (loser id %s)", loserName, loserID)
	}
	// And the store confirms the retire actually happened (so the pin isn't vacuous).
	got, err := store.Get(loserID)
	if err != nil {
		t.Fatalf("get loser: %v", err)
	}
	if got.Metadata["session_name"] != "" || got.Metadata["state"] != "archived" {
		t.Fatalf("loser was not retired (session_name=%q state=%q); fixture no longer exercises the dedup retire", got.Metadata["session_name"], got.Metadata["state"])
	}
}

func TestSessionReconcilePhaseTraceUsesDistinctSites(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	defer tracer.Close() //nolint:errcheck
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}

	reconcileSessionBeadsTracedWithNamedDemand(
		context.Background(),
		cityDir,
		nil, // rows []session.ReconcileSession
		nil, // snapshot *sessionBeadSnapshot
		nil,
		nil,
		&config.City{},
		nil,
		beads.SessionStore{},
		nil,
		nil,
		nil,
		nil,
		newDrainTracker(),
		nil, // gate (*providerHealthGate) — ADR-0013 A1 M3a
		nil,
		nil,
		false,
		nil,
		"trace-town",
		nil,
		clock.Real{},
		events.Discard,
		0,
		0,
		io.Discard,
		io.Discard,
		cycle,
	)

	got := make(map[string]TraceSiteCode)
	for _, rec := range cycle.records {
		if rec.RecordType != TraceRecordOperation {
			continue
		}
		name, _ := rec.Fields["operation_name"].(string)
		if strings.HasPrefix(name, "session_reconcile.") {
			got[name] = rec.SiteCode
		}
	}
	want := map[string]TraceSiteCode{
		"session_reconcile.build_deps":                        TraceSiteSessionReconcileBuildDeps,
		"session_reconcile.heal_and_retire_duplicates":        TraceSiteSessionReconcileHealRetire,
		"session_reconcile.topo_order":                        TraceSiteSessionReconcileTopoOrder,
		"session_reconcile.circuit_breaker_restore":           TraceSiteSessionReconcileCircuitBreaker,
		"session_reconcile.forward_pass":                      TraceSiteSessionReconcileForwardPass,
		"session_reconcile.compute_awake_set_and_idle_probes": TraceSiteSessionReconcileAwakeSet,
		"session_reconcile.apply_wake_sleep_decisions":        TraceSiteSessionReconcileWakeSleep,
		"session_reconcile.execute_planned_starts":            TraceSiteSessionReconcileStartExecution,
		"session_reconcile.advance_drains":                    TraceSiteSessionReconcileDrainAdvance,
	}
	if len(got) != len(want) {
		t.Fatalf("session_reconcile operation count = %d, want %d; got %#v, want %#v", len(got), len(want), got, want)
	}
	for name, site := range want {
		if got[name] != site {
			t.Fatalf("%s site = %q, want %q; all sites: %#v", name, got[name], site, got)
		}
	}
}

// livenessGetErrStore forces Get(target) to fail so the reconciler's runtime
// liveness probe returns an observation error (livenessErr != nil) for that one
// session, without disturbing any other store access. The reconcile loop body
// never re-reads the session bead through the store (it works off the passed-in
// slice and the mid-tick snapshot), so this affects only the liveness probe.
type livenessGetErrStore struct {
	beads.Store
	target string
	err    error
}

func (s livenessGetErrStore) Get(id string) (beads.Bead, error) {
	if id == s.target {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

// TestReconcileOrphanCloseFailsClosedOnLivenessError proves the S16 fail-closed
// gate fires end to end in the running reconciler (F1). A healthy liveness
// observation closes the undesired, dead orphan (baseline). But when the
// liveness probe errors — providerAlive=false then means "observation
// unavailable", not "confirmed dead" — the destructive orphan CLOSE is skipped
// this tick and the bead is kept open for re-observation, rather than orphaning
// a session that may still be alive on a transient blip (#3872-family). The
// only variable between the two runs is the liveness observation error, so the
// close→keep-open flip (plus the guard's stderr line) isolates the guard. The
// three sibling !providerAlive destructive paths (pending-create rollback,
// failed-create close, drain-ack finalize) carry the identical guard added in
// this PR.
func TestReconcileOrphanCloseFailsClosedOnLivenessError(t *testing.T) {
	run := func(t *testing.T, injectLivenessErr bool) (status, stderr string) {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{}
		// An asleep, undesired session with a dead runtime is the plain
		// orphan-close case: createSessionBead defaults state=asleep and an empty
		// desiredState makes it undesired.
		session := env.createSessionBead("worker", "worker")

		store := env.store
		if injectLivenessErr {
			// Fail the liveness probe's read of just this session. With sp=nil,
			// handle construction surfaces the failure as an observation error —
			// the same class a transient tmux/store blip produces at runtime.
			store = livenessGetErrStore{
				Store:  env.store,
				target: session.ID,
				err:    errors.New("boom: transient store failure"),
			}
		}

		var stderrBuf bytes.Buffer
		reconcileSessionBeads(
			context.Background(),
			[]beads.Bead{session},
			nil, // desiredState — empty ⇒ orphan
			nil, // configuredNames
			env.cfg,
			nil,   // sp — nil ⇒ dead runtime; with the wrapped store the probe errors
			store, // store
			nil,   // dops
			nil,   // assignedWorkBeads
			nil,   // readyWaitSet
			newDrainTracker(),
			nil,   // poolDesired
			false, // storeQueryPartial
			nil,   // workSet
			"",    // cityName
			nil,   // idleTracker
			env.clk,
			events.Discard,
			0, 0,
			io.Discard, &stderrBuf,
		)

		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		return got.Status, stderrBuf.String()
	}

	t.Run("healthy liveness closes orphan (baseline)", func(t *testing.T) {
		status, _ := run(t, false)
		if status != "closed" {
			t.Fatalf("baseline orphan close: status = %q, want closed (the close path must be reachable for the guard to matter)", status)
		}
	})

	t.Run("liveness error skips the close (fail closed)", func(t *testing.T) {
		status, stderr := run(t, true)
		if status == "closed" {
			t.Fatalf("orphan bead was closed despite a liveness observation error; want kept open (fail closed)")
		}
		if !strings.Contains(stderr, "skipping close of 'worker': liveness observation failed") {
			t.Fatalf("expected the fail-closed guard's stderr line, got %q", stderr)
		}
	})
}

func TestTraceFlushAfterEndOnlyPersistsPostEndRecords(t *testing.T) {
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	now := time.Now().UTC()
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "worker",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        now,
		ExpiresAt:      now.Add(15 * time.Minute),
		LastExtendedAt: now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("upsertArm: %v", err)
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordOperation(
		TraceSiteLifecycleStartExecute,
		TraceReasonWake,
		TraceOutcomeApplied,
		"provider_start",
		"worker",
		"worker",
		10*time.Millisecond,
		map[string]any{"step": "before-end"},
	)
	if err := cycle.End(TraceCompletionCompleted, map[string]any{}); err != nil {
		t.Fatalf("End: %v", err)
	}
	cycle.RecordOperation(
		TraceSiteLifecycleStartExecute,
		TraceReasonWake,
		TraceOutcomeApplied,
		"provider_start",
		"worker",
		"worker",
		20*time.Millisecond,
		map[string]any{"step": "after-end"},
	)
	if err := cycle.flushCurrentBatch(TraceDurabilityDurable); err != nil {
		t.Fatalf("flushCurrentBatch: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var beforeEnd, afterEnd int
	var cycleResult *SessionReconcilerTraceRecord
	for _, rec := range records {
		if rec.RecordType == TraceRecordCycleResult {
			recCopy := rec
			cycleResult = &recCopy
			continue
		}
		if rec.RecordType != TraceRecordOperation {
			continue
		}
		switch rec.Fields["step"] {
		case "before-end":
			beforeEnd++
		case "after-end":
			if got := rec.Fields["post_cycle_result"]; got != true {
				t.Fatalf("post_cycle_result = %#v, want true", got)
			}
			if got := rec.Fields["rollup_excluded"]; got != true {
				t.Fatalf("rollup_excluded = %#v, want true", got)
			}
			afterEnd++
		}
	}
	if cycleResult == nil {
		t.Fatal("cycle_result missing")
	}
	if cycleResult.RecordCount >= len(records) {
		t.Fatalf("cycle_result record_count = %d, want less than persisted records %d because post-End records are rollup-excluded", cycleResult.RecordCount, len(records))
	}
	if beforeEnd != 1 || afterEnd != 1 {
		t.Fatalf("operation counts before-end=%d after-end=%d, want 1 each", beforeEnd, afterEnd)
	}
}

func TestTraceFlushCurrentBatchQueueFullDegrades(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	var stderr bytes.Buffer
	tracer := &SessionReconcilerTracer{
		cityPath: cityDir,
		cityName: "trace-town",
		enabled:  true,
		stderr:   &stderr,
		store:    store,
		armStore: newSessionReconcilerTraceArmStore(cityDir),
		flushCh:  make(chan sessionReconcilerTraceFlushRequest),
		closeCh:  make(chan struct{}),
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordSessionBaseline("repo/polecat", "polecat-1", nil)
	if err := cycle.flushCurrentBatch(TraceDurabilityMetadata); err != nil {
		t.Fatalf("flushCurrentBatch: %v", err)
	}
	if cycle.droppedBatches != 1 {
		t.Fatalf("droppedBatches = %d, want 1", cycle.droppedBatches)
	}
	if got := cycle.dropReasons["flush_queue_full"]; got == 0 {
		t.Fatalf("dropReasons[flush_queue_full] = %d, want > 0", got)
	}
	if !strings.Contains(stderr.String(), "flush_queue_full") {
		t.Fatalf("stderr = %q, want flush_queue_full", stderr.String())
	}
}

func TestTraceCloseDoesNotDependOnMutableFlushChannelField(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	flushCh := make(chan sessionReconcilerTraceFlushRequest)
	tracer := &SessionReconcilerTracer{
		store:     store,
		flushDone: make(chan struct{}),
		flushCh:   nil,
	}
	go tracer.runFlushLoop(flushCh)
	close(flushCh)

	select {
	case <-tracer.flushDone:
	case <-time.After(time.Second):
		t.Fatal("flush loop did not exit after the original channel closed")
	}
}

func TestTraceFlushCurrentBatchWaitBudgetDegrades(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close() //nolint:errcheck

	var stderr bytes.Buffer
	tracer := &SessionReconcilerTracer{
		cityPath: cityDir,
		cityName: "trace-town",
		enabled:  true,
		stderr:   &stderr,
		store:    store,
		armStore: newSessionReconcilerTraceArmStore(cityDir),
		flushCh:  make(chan sessionReconcilerTraceFlushRequest),
		closeCh:  make(chan struct{}),
	}
	release := make(chan struct{})
	go func() {
		req := <-tracer.flushCh
		<-release
		req.result <- nil
	}()
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", time.Now().UTC(), &config.City{})
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.RecordSessionBaseline("repo/polecat", "polecat-1", nil)
	if err := cycle.flushCurrentBatch(TraceDurabilityDurable); err != nil {
		t.Fatalf("flushCurrentBatch: %v", err)
	}
	close(release)
	if cycle.droppedBatches != 0 {
		t.Fatalf("droppedBatches = %d, want 0", cycle.droppedBatches)
	}
	if got := cycle.dropReasons["flush_queue_full"]; got != 0 {
		t.Fatalf("dropReasons[flush_queue_full] = %d, want 0", got)
	}
	if !strings.Contains(stderr.String(), "slow_storage_degraded") {
		t.Fatalf("stderr = %q, want slow_storage_degraded", stderr.String())
	}
}
