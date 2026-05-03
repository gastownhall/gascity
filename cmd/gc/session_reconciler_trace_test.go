package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
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

func TestNormalizeTraceOutcomeCodeAcceptsDeferredActive(t *testing.T) {
	got, raw := normalizeTraceOutcomeCode(string(TraceOutcomeDeferredActive))
	if got != TraceOutcomeDeferredActive {
		t.Fatalf("outcome = %q, want %q", got, TraceOutcomeDeferredActive)
	}
	if raw != "" {
		t.Fatalf("raw outcome = %q, want empty", raw)
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
