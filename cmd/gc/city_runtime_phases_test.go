package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/usage"
)

// opRecordingStore records every store call a tick makes, in order, as a
// stable one-line summary: the method, the bead it names and the shape of the
// write (field and metadata key names, never values, which carry wall times).
// onRecord, when set, sees each call as it is recorded.
type opRecordingStore struct {
	beads.Store
	mu       sync.Mutex
	ops      []string
	onRecord func(op string)
}

func (s *opRecordingStore) record(format string, args ...any) {
	op := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.ops = append(s.ops, op)
	onRecord := s.onRecord
	s.mu.Unlock()
	if onRecord != nil {
		onRecord(op)
	}
}

func (s *opRecordingStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func opStoreKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// setFields names the non-zero fields of a struct value.
func setFields(v any) string {
	rv := reflect.ValueOf(v)
	var names []string
	for i := 0; i < rv.NumField(); i++ {
		if !rv.Field(i).IsZero() {
			names = append(names, rv.Type().Field(i).Name)
		}
	}
	return strings.Join(names, ",")
}

func (s *opRecordingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.record("Create type=%s labels=%s metadata=%s", b.Type, strings.Join(b.Labels, ","), opStoreKeys(b.Metadata))
	return s.Store.Create(b)
}

func (s *opRecordingStore) Get(id string) (beads.Bead, error) {
	s.record("Get %s", id)
	return s.Store.Get(id)
}

func (s *opRecordingStore) Update(id string, opts beads.UpdateOpts) error {
	s.record("Update %s fields=%s metadata=%s", id, setFields(opts), opStoreKeys(opts.Metadata))
	return s.Store.Update(id, opts)
}

func (s *opRecordingStore) Close(id string) error {
	s.record("Close %s", id)
	return s.Store.Close(id)
}

func (s *opRecordingStore) Reopen(id string) error {
	s.record("Reopen %s", id)
	return s.Store.Reopen(id)
}

func (s *opRecordingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.record("CloseAll %s metadata=%s", strings.Join(ids, ","), opStoreKeys(metadata))
	return s.Store.CloseAll(ids, metadata)
}

func (s *opRecordingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.record("List status=%s type=%s label=%s metadata=%s include_closed=%t live=%t", q.Status, q.Type, q.Label, opStoreKeys(q.Metadata), q.IncludeClosed, q.Live)
	return s.Store.List(q)
}

func (s *opRecordingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	s.record("ListOpen %s", strings.Join(status, ","))
	return s.Store.ListOpen(status...)
}

func (s *opRecordingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.record("Ready")
	return s.Store.Ready(q...)
}

func (s *opRecordingStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.record("Children %s", parentID)
	return s.Store.Children(parentID, opts...)
}

func (s *opRecordingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.record("ListByLabel %s", label)
	return s.Store.ListByLabel(label, limit, opts...)
}

func (s *opRecordingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	s.record("ListByAssignee %s %s", assignee, status)
	return s.Store.ListByAssignee(assignee, status, limit)
}

func (s *opRecordingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.record("ListByMetadata %s", opStoreKeys(filters))
	return s.Store.ListByMetadata(filters, limit, opts...)
}

func (s *opRecordingStore) SetMetadata(id, key, value string) error {
	s.record("SetMetadata %s %s", id, key)
	return s.Store.SetMetadata(id, key, value)
}

func (s *opRecordingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.record("SetMetadataBatch %s %s", id, opStoreKeys(kvs))
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *opRecordingStore) SetLocalString(id, key, value string) error {
	s.record("SetLocalString %s %s", id, key)
	return s.Store.SetLocalString(id, key, value)
}

func (s *opRecordingStore) GetLocalString(id, key string) (string, error) {
	s.record("GetLocalString %s %s", id, key)
	return s.Store.GetLocalString(id, key)
}

func (s *opRecordingStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	s.record("Tx %s", commitMsg)
	return s.Store.Tx(commitMsg, fn)
}

func (s *opRecordingStore) Delete(id string) error {
	s.record("Delete %s", id)
	return s.Store.Delete(id)
}

func (s *opRecordingStore) DepAdd(issueID, dependsOnID, depType string) error {
	s.record("DepAdd %s %s %s", issueID, dependsOnID, depType)
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s *opRecordingStore) DepRemove(issueID, dependsOnID string) error {
	s.record("DepRemove %s %s", issueID, dependsOnID)
	return s.Store.DepRemove(issueID, dependsOnID)
}

func (s *opRecordingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.record("DepList %s %s", id, direction)
	return s.Store.DepList(id, direction)
}

// closeTrace closes the runtime's tracer and returns what it wrote.
func closeTrace(t *testing.T, cr *CityRuntime) []SessionReconcilerTraceRecord {
	t.Helper()
	if err := cr.trace.Close(); err != nil {
		t.Fatalf("closing the tracer: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	return records
}

// operationRecords returns the controller operation records, in order, as
// "site operation_name".
func operationRecords(records []SessionReconcilerTraceRecord) []string {
	var ops []string
	for _, r := range records {
		name := fmt.Sprint(r.Fields["operation_name"])
		if r.RecordType != TraceRecordOperation || r.SiteCode == TraceSiteRuntimeInventoryPass || phaseInternalRecord(name) {
			continue
		}
		ops = append(ops, fmt.Sprintf("%s %s", r.SiteCode, name))
	}
	return ops
}

// tickOperationRecords closes the runtime's tracer and returns its
// operation records.
func tickOperationRecords(t *testing.T, cr *CityRuntime) []string {
	t.Helper()
	return operationRecords(closeTrace(t, cr))
}

// passCompletion is how the last pass traced with this phase payload
// ("tick" or "startup") ended.
func passCompletion(records []SessionReconcilerTraceRecord, phase string) TraceCompletionStatus {
	var completion TraceCompletionStatus
	for _, r := range records {
		if r.RecordType == TraceRecordCycleResult && r.Fields["phase"] == phase {
			completion = r.CompletionStatus
		}
	}
	return completion
}

// phaseInternalRecord reports a record the session reconciler writes from
// inside its own body. The goldens pin the phases and beadReconcileTick's
// steps (bead_reconcile.*, where its moved maintenance helpers record), not
// the internals of demand, sync and the session reconcile.
func phaseInternalRecord(name string) bool {
	for _, prefix := range []string{"session_reconcile.", "demand_snapshot.", "sync_beads_and_update_index."} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// storeWrites keeps the recorded calls that write.
func storeWrites(ops []string) []string {
	var writes []string
	for _, op := range ops {
		method, _, _ := strings.Cut(op, " ")
		switch method {
		case "Create", "Update", "Close", "Reopen", "CloseAll", "SetMetadata", "SetMetadataBatch", "SetLocalString", "Tx", "Delete", "DepAdd", "DepRemove":
			writes = append(writes, op)
		}
	}
	return writes
}

func assertLinesEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s changed.\ngot:\n\t%s\nwant:\n\t%s", what, strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// phaseFixtureConfig is a soft-reload city whose reloaded config turns on
// every config-gated tick phase: wisp GC, the closed-bead worktree reaper
// (dry run, so it removes nothing) and chat auto-suspend.
func writePhaseFixtureConfig(t *testing.T, tomlPath string, gated bool) {
	t.Helper()
	writeCityRuntimeSoftReloadConfig(t, tomlPath, "")
	if !gated {
		return
	}
	f, err := os.OpenFile(tomlPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	if _, err := f.WriteString("\n[daemon]\nwisp_gc_interval = \"1m\"\nwisp_ttl = \"1h\"\nauto_reap_closed_bead_worktrees_dry_run = true\n\n[chat_sessions]\nidle_timeout = \"1h\"\n"); err != nil {
		t.Fatalf("append config: %v", err)
	}
}

// newPhaseFixtureRuntime builds a city runtime with a traced, recorded
// session store and one running session. withReload stages a manual soft
// reload of a config that enables every config-gated phase, and withLane
// publishes one inventory pass first.
func newPhaseFixtureRuntime(t *testing.T, withReload, withLane bool) (*CityRuntime, *opRecordingStore) {
	t.Helper()
	// Host IO pressure must not skip the fixture's ticks.
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writePhaseFixtureConfig(t, tomlPath, false)
	cfg, configRev := loadCityRuntimeControllerConfig(t, cityPath)
	if withReload {
		writePhaseFixtureConfig(t, tomlPath, true)
	}

	store := &opRecordingStore{Store: beads.NewMemStore()}
	if _, err := store.Store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"}),
			"generation":          "1",
			"state":               "active",
		},
	}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	dirty := &atomic.Bool{}
	dirty.Store(withReload)
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{
				"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
			}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	// Stop the session before the runtime's shutdown would wait out its
	// graceful stop budget on it.
	t.Cleanup(func() { _ = sp.Stop("worker") })
	cr.od = nil
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	cr.trace = newSessionReconcilerTraceManager(cityPath, "test-city", io.Discard)
	if withReload {
		cr.activeReload = &reloadRequest{soft: true, doneCh: make(chan reloadControlReply, 1)}
	}
	if withLane {
		if cr.initRuntimeInventoryLane() == nil {
			t.Fatal("no inventory lane")
		}
		runTestInventoryPass(cr)
	}
	return cr, store
}

func runFixtureTick(cr *CityRuntime, trigger string) {
	runFixtureTickCtx(context.Background(), cr, trigger)
}

func runFixtureTickCtx(ctx context.Context, cr *CityRuntime, trigger string) {
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool
	cr.tick(ctx, cr.configDirty, &lastProviderName, cr.cityPath, &prevPoolRunning, trigger)
}

// Kills: the phase-driven tick drifting from the tick it replaced. The traced
// operation records (beadReconcileTick's steps included) and the full store
// call log of two fixture ticks (a manual soft reload that reaches every
// config-gated phase with the inventory lane up, and a plain patrol with no
// lane) are pinned exactly as main's tick produced them before the split: any
// phase reordered, dropped, duplicated or changed in what it reads or writes
// fails here, and so does a moved beadReconcileTick helper
// (runDetachedHandoffOrphansDelta, runNudgeDispatchTick) no longer called, or
// a read-only phase body (the extmsg reapers) gutted under its record.
func TestCityRuntimeTickPhasesMatchLegacyTickRecordsAndStoreOps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withReload bool
		withLane   bool
		trigger    string
		wantOps    []string
		wantStore  []string
	}{
		{
			name:       "soft-reload",
			withReload: true,
			withLane:   true,
			trigger:    "reload",
			wantOps: []string{
				"controller.tick.phase managed_dolt_preflight",
				"controller.tick.phase wake_orders_lane",
				"controller.tick.phase runtime_inventory_lane",
				"controller.tick.phase recover_unrouted_work_routes",
				"session_snapshot.load load_session_snapshot.initial",
				"controller.tick.phase cleanup_dead_runtime_session_corpses",
				"controller.tick.phase reap_runtimes_bound_to_closed_beads",
				"controller.tick.phase sweep_process_table_orphans",
				"controller.tick.phase reap_stale_session_beads",
				"controller.tick.phase reap_closed_bead_worktrees",
				"controller.tick.phase finalize_drain_ack_stop_pending",
				"demand_snapshot.load load_demand_snapshot",
				"session_snapshot.load load_session_snapshot.after_demand",
				"desired_state.build refresh_desired_state.before_sync",
				"session_sync.update_index sync_beads_and_update_index",
				"session_snapshot.load load_session_snapshot.after_sync",
				"controller.tick.phase reap_stale_extmsg_bindings",
				"controller.tick.phase reap_stale_extmsg_participants",
				"desired_state.build refresh_desired_state.after_sync",
				"config.reload apply_soft_reload_acceptance",
				"session_snapshot.load load_session_snapshot.after_soft_reload",
				"controller.tick.phase bead_reconcile.release_orphaned_pool_assignments",
				"controller.tick.phase bead_reconcile.sweep_detached_handoff_orphans",
				"controller.tick.phase bead_reconcile.sweep_undesired_pool_sessions",
				"controller.tick.phase bead_reconcile.prepare_wait_wake_state",
				"controller.tick.phase bead_reconcile.record_trace_input_summary",
				"controller.tick.phase bead_reconcile.filter_assigned_work_for_wake",
				"controller.tick.phase bead_reconcile.reconcile_sessions",
				"controller.tick.phase bead_reconcile.record_trace_session_results",
				"controller.tick.phase bead_reconcile.dispatch_wait_nudges",
				"controller.tick.phase bead_reconcile.nudge_dispatch_tick",
				"controller.tick.phase bead_reconcile.nudge_stalled_pool_claims",
				"controller.tick.phase bead_reconcile_tick",
				"controller.tick.phase reconcile_execution_completions",
				"controller.tick.phase wisp_gc",
				"controller.tick.phase workspace_service_tick",
				"controller.tick.phase auto_suspend_chat_sessions",
				"controller.tick.phase process_convergence_requests",
				"controller.tick.phase convergence_tick",
			},
			wantStore: []string{
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status= type=session label= metadata= include_closed=false live=false",
				"List status= type= label=gc:session metadata= include_closed=false live=false",
				"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status= type= label=gc:extmsg-binding metadata= include_closed=false live=false",
				"List status= type= label=gc:extmsg-group-participant metadata= include_closed=false live=false",
				"SetMetadataBatch gc-1 core_hash_breakdown,started_config_hash,started_launch_hash,started_provision_hash",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
				"List status= type= label=gc:wait metadata= include_closed=false live=false",
				"Get gc-1",
				"SetMetadataBatch gc-1 state",
				"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
				"Get gc-1",
				"Get gc-1",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
				"List status= type=session label= metadata= include_closed=false live=false",
				"List status= type= label=gc:session metadata= include_closed=false live=false",
				"List status= type=spec label= metadata= include_closed=true live=true",
				"List status= type= label= metadata=gc.kind include_closed=true live=true",
				"List status=closed type=molecule label= metadata= include_closed=false live=false",
				"List status=closed type= label= metadata=gc.kind include_closed=false live=false",
				"List status=closed type= label= metadata=gc.formula_contract include_closed=false live=false",
				"List status=closed type= label= metadata=gc.kind include_closed=false live=false",
				"List status=open type=molecule label= metadata= include_closed=false live=false",
				"List status=open type= label= metadata=gc.kind include_closed=false live=false",
				"List status=open type= label= metadata=gc.formula_contract include_closed=false live=false",
				"List status=open type= label= metadata=gc.kind include_closed=false live=false",
				"List status=in_progress type=molecule label= metadata= include_closed=false live=false",
				"List status=in_progress type= label= metadata=gc.kind include_closed=false live=false",
				"List status=in_progress type= label= metadata=gc.formula_contract include_closed=false live=false",
				"List status=in_progress type= label= metadata=gc.kind include_closed=false live=false",
				"List status=closed type=molecule label= metadata= include_closed=false live=false",
				"List status=closed type= label= metadata=gc.kind include_closed=false live=false",
				"List status=closed type= label= metadata=gc.formula_contract include_closed=false live=false",
				"List status=closed type= label= metadata=gc.kind include_closed=false live=false",
				"List status=closed type= label= metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
			},
		},
		{
			name:    "patrol",
			trigger: "patrol",
			wantOps: []string{
				"controller.tick.phase managed_dolt_preflight",
				"controller.tick.phase wake_orders_lane",
				"controller.tick.phase recover_unrouted_work_routes",
				"session_snapshot.load load_session_snapshot.initial",
				"controller.tick.phase cleanup_dead_runtime_session_corpses",
				"controller.tick.phase reap_runtimes_bound_to_closed_beads",
				"controller.tick.phase sweep_process_table_orphans",
				"controller.tick.phase reap_stale_session_beads",
				"controller.tick.phase finalize_drain_ack_stop_pending",
				"demand_snapshot.load load_demand_snapshot",
				"session_snapshot.load load_session_snapshot.after_demand",
				"desired_state.build refresh_desired_state.before_sync",
				"session_sync.update_index sync_beads_and_update_index",
				"session_snapshot.load load_session_snapshot.after_sync",
				"controller.tick.phase reap_stale_extmsg_bindings",
				"controller.tick.phase reap_stale_extmsg_participants",
				"desired_state.build refresh_desired_state.after_sync",
				"controller.tick.phase bead_reconcile.release_orphaned_pool_assignments",
				"controller.tick.phase bead_reconcile.sweep_detached_handoff_orphans",
				"controller.tick.phase bead_reconcile.sweep_undesired_pool_sessions",
				"controller.tick.phase bead_reconcile.prepare_wait_wake_state",
				"controller.tick.phase bead_reconcile.record_trace_input_summary",
				"controller.tick.phase bead_reconcile.filter_assigned_work_for_wake",
				"controller.tick.phase bead_reconcile.reconcile_sessions",
				"controller.tick.phase bead_reconcile.record_trace_session_results",
				"controller.tick.phase bead_reconcile.dispatch_wait_nudges",
				"controller.tick.phase bead_reconcile.nudge_dispatch_tick",
				"controller.tick.phase bead_reconcile.nudge_stalled_pool_claims",
				"controller.tick.phase bead_reconcile_tick",
				"controller.tick.phase reconcile_execution_completions",
				"controller.tick.phase workspace_service_tick",
				"controller.tick.phase process_convergence_requests",
				"controller.tick.phase convergence_tick",
			},
			wantStore: []string{
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"Ready",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status= type=session label= metadata= include_closed=false live=false",
				"List status= type= label=gc:session metadata= include_closed=false live=false",
				"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status= type= label=gc:extmsg-binding metadata= include_closed=false live=false",
				"List status= type= label=gc:extmsg-group-participant metadata= include_closed=false live=false",
				"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
				"List status= type= label=gc:wait metadata= include_closed=false live=false",
				"Get gc-1",
				"SetMetadataBatch gc-1 state",
				"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
				"Get gc-1",
				"Get gc-1",
				"List status=open type=session label= metadata= include_closed=false live=false",
				"List status=open type= label=gc:session metadata= include_closed=false live=false",
				"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
				"List status= type=session label= metadata= include_closed=false live=false",
				"List status= type= label=gc:session metadata= include_closed=false live=false",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, store := newPhaseFixtureRuntime(t, tc.withReload, tc.withLane)
			runFixtureTick(cr, tc.trigger)
			gotStore := store.recorded()
			gotOps := tickOperationRecords(t, cr)
			assertLinesEqual(t, "tick operation records", gotOps, tc.wantOps)
			assertLinesEqual(t, "tick store calls", gotStore, tc.wantStore)
		})
	}
}

func tickPhaseNames(phases []tickPhase) []string {
	names := make([]string, 0, len(phases))
	for _, phase := range phases {
		names = append(names, phase.name)
	}
	return names
}

// Kills: a tick phase reordered, skipped or duplicated. The list is the
// tick as main ran it before the split; the session column is what the v2
// reconciler takes over.
func TestCityRuntimeTickLegacyPhaseSequenceUnchanged(t *testing.T) {
	want := []string{
		"reconcile_pool_deaths",
		"config_reload",
		"fs_pressure_gate",
		"managed_dolt_preflight",
		"wake_orders_lane",
		"runtime_inventory_lane",
		"recover_unrouted_work_routes",
		"load_session_snapshot",
		"cleanup_dead_runtime_session_corpses (session)",
		"reap_runtimes_bound_to_closed_beads",
		"sweep_process_table_orphans",
		"reap_stale_session_beads (session)",
		"reap_closed_bead_worktrees",
		"finalize_drain_ack_stop_pending (session)",
		"demand_desired_state_and_sync (session)",
		"reap_stale_extmsg_bindings",
		"reap_stale_extmsg_participants",
		"refresh_desired_state (session)",
		"apply_soft_reload_acceptance (session)",
		"bead_reconcile_tick (session)",
		"retire_suspended_rig_scopes",
		"reconcile_execution_completions",
		"wisp_gc",
		"workspace_service_tick",
		"auto_suspend_chat_sessions (session)",
		"process_convergence_requests",
		"convergence_tick",
	}
	assertLinesEqual(t, "tick phases", taggedPhaseNames(legacyTickPhases), want)
}

func taggedPhaseNames(phases []tickPhase) []string {
	names := tickPhaseNames(phases)
	for i, phase := range phases {
		if phase.session {
			names[i] += " (session)"
		}
	}
	return names
}

// Kills: a startup-step phase reordered, skipped or duplicated, or the
// startup step drifting from what main's ran: the phases and boot bead
// reconcile steps it traces, its full store call log, and its completion.
func TestCityRuntimeStartupLegacyPhaseSequenceUnchanged(t *testing.T) {
	want := []string{
		"managed_dolt_preflight",
		"load_session_snapshot",
		"cleanup_dead_runtime_session_corpses (session)",
		"reap_runtimes_bound_to_closed_beads",
		"sweep_process_table_orphans",
		"reap_stale_session_beads (session)",
		"build_desired_state_and_sync (session)",
		"bead_reconcile_tick (session)",
	}
	assertLinesEqual(t, "startup phases", taggedPhaseNames(legacyStartupPhases), want)

	cr, store := newPhaseFixtureRuntime(t, false, true)
	if !cr.startupReconcile(context.Background()) {
		t.Fatal("the startup step did not complete")
	}
	records := closeTrace(t, cr)
	assertLinesEqual(t, "startup operation records", operationRecords(records), []string{
		"controller.tick.phase cleanup_dead_runtime_session_corpses",
		"controller.tick.phase reap_runtimes_bound_to_closed_beads",
		"controller.tick.phase bead_reconcile.release_orphaned_pool_assignments",
		"controller.tick.phase bead_reconcile.sweep_detached_handoff_orphans",
		"pool_desired.compute bead_reconcile.compute_pool_desired",
		"controller.tick.phase bead_reconcile.sweep_undesired_pool_sessions.deferred_on_boot",
		"controller.tick.phase bead_reconcile.prepare_wait_wake_state",
		"controller.tick.phase bead_reconcile.record_trace_input_summary",
		"controller.tick.phase bead_reconcile.filter_assigned_work_for_wake",
		"controller.tick.phase bead_reconcile.reconcile_sessions",
		"controller.tick.phase bead_reconcile.record_trace_session_results",
		"controller.tick.phase bead_reconcile.dispatch_wait_nudges",
		"controller.tick.phase bead_reconcile.nudge_dispatch_tick",
		"controller.tick.phase bead_reconcile.nudge_stalled_pool_claims",
	})
	assertLinesEqual(t, "startup store calls", store.recorded(), []string{
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status= type=session label= metadata= include_closed=false live=false",
		"List status= type= label=gc:session metadata= include_closed=false live=false",
		"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
		"List status= type= label=gc:wait metadata= include_closed=false live=false",
		"Get gc-1",
		"SetMetadataBatch gc-1 state",
		"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
		"Get gc-1",
		"Get gc-1",
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status=open type= label=session:gc-1 metadata= include_closed=false live=false",
		"List status= type=session label= metadata= include_closed=false live=false",
		"List status= type= label=gc:session metadata= include_closed=false live=false",
	})
	if got := passCompletion(records, "startup"); got != TraceCompletionCompleted {
		t.Errorf("startup trace completion = %q, want %q", got, TraceCompletionCompleted)
	}
}

// Kills: the tick's closing p.completed = true dropped. A watch reload's tick
// that runs to its end must trace completed and leave the dirty flag it
// cleared cleared; without the flag the tick traces aborted and restores
// dirty, so every later tick reloads again.
func TestCityRuntimeTickWatchReloadCompletes(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, true, false)
	cr.activeReload = nil // dirty with no manual request: a watch reload
	runFixtureTick(cr, "patrol")
	if cr.configDirty.Load() {
		t.Error("configDirty is set after a completed watch-reload tick")
	}
	if got := passCompletion(closeTrace(t, cr), "tick"); got != TraceCompletionCompleted {
		t.Errorf("tick trace completion = %q, want %q", got, TraceCompletionCompleted)
	}
}

// Kills: a stopped pass counted as complete (runTickPhases reporting the end
// after a phase stopped it, or tick marking the pass completed anyway), and
// reap_closed_bead_worktrees's trailing ctx check dropped. The tick is
// canceled at its first store call, the initial session snapshot: the next
// phase with a ctx check (the worktree reaper's, run even when the reaper is
// off) stops it, nothing after it records, and the trace ends aborted.
func TestCityRuntimeTickCancelledMidPassAborts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withReload bool
		trigger    string
		lastRecord string
	}{
		{name: "soft-reload", withReload: true, trigger: "reload", lastRecord: "controller.tick.phase reap_closed_bead_worktrees"},
		// The reaper is off on patrol and records nothing, but its check
		// still stops the tick.
		{name: "patrol", trigger: "patrol", lastRecord: "controller.tick.phase reap_stale_session_beads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, store := newPhaseFixtureRuntime(t, tc.withReload, tc.withReload)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store.onRecord = func(string) { cancel() }
			runFixtureTickCtx(ctx, cr, tc.trigger)
			records := closeTrace(t, cr)
			ops := operationRecords(records)
			if len(ops) == 0 || ops[len(ops)-1] != tc.lastRecord {
				t.Errorf("a canceled tick recorded past the phase that stopped it, want last %q:\n\t%s", tc.lastRecord, strings.Join(ops, "\n\t"))
			}
			if got := passCompletion(records, "tick"); got != TraceCompletionAborted {
				t.Errorf("canceled tick trace completion = %q, want %q", got, TraceCompletionAborted)
			}
		})
	}
}

// Kills: the startup step reported complete when a phase stopped it (forced
// completion, or build_desired_state_and_sync's trailing ctx check dropped).
// Canceled while it builds desired state, it returns false, so run() retries
// it, never reaches the bead reconcile, and traces aborted.
func TestCityRuntimeStartupCancelledReturnsFalse(t *testing.T) {
	cr, _ := newPhaseFixtureRuntime(t, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	build := cr.buildFn
	cr.buildFn = func(cfg *config.City, sp runtime.Provider, store beads.Store) DesiredStateResult {
		cancel()
		return build(cfg, sp, store)
	}
	if cr.startupReconcile(ctx) {
		t.Fatal("a canceled startup step reported completion")
	}
	records := closeTrace(t, cr)
	for _, op := range operationRecords(records) {
		if strings.Contains(op, "bead_reconcile") {
			t.Errorf("a canceled startup step ran the bead reconcile: %s", op)
		}
	}
	if got := passCompletion(records, "startup"); got != TraceCompletionAborted {
		t.Errorf("canceled startup trace completion = %q, want %q", got, TraceCompletionAborted)
	}
}

// armMaintenanceLanes makes the fixture's maintenance sub-steps observable:
// a usage sink and an awake session, so the usage lane's live (non-boot)
// sweep reads the session bead, and the supervisor's historical transcript
// pass enabled.
func armMaintenanceLanes(t *testing.T, cr *CityRuntime, store *opRecordingStore) {
	t.Helper()
	cr.cs.usageSink = usage.NewLocalSink(filepath.Join(t.TempDir(), "usage.jsonl"))
	if err := store.Store.SetMetadata("gc-1", "awake_started_at", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetMetadata(awake_started_at): %v", err)
	}
	cr.transcriptMetaEnabled = true
	t.Cleanup(func() {
		cr.transcriptMetaMu.Lock()
		done := cr.transcriptMetaDone
		cr.transcriptMetaMu.Unlock()
		if done != nil {
			<-done
		}
	})
}

func transcriptMetaStarted(cr *CityRuntime) bool {
	cr.transcriptMetaMu.Lock()
	defer cr.transcriptMetaMu.Unlock()
	return cr.transcriptMetaStarted
}

// Kills: a session phase (chat auto-suspend, corpse cleanup, demand and
// sync, bead reconcile, ...) left in what v2 leaves the tick, a maintenance
// phase dropped from it, or a beadReconcileTick maintenance sub-step gutted:
// the detached-orphan delta or the nudge fallback (their records), the
// historical transcript pass (not started), or the usage facts flipped to
// boot mode (no live-lane read of the awake session). Running that list
// writes nothing to the session row and traces no session phase.
func TestCityRuntimeTickV2RunsMaintenancePhasesOnly(t *testing.T) {
	assertLinesEqual(t, "v2 tick phases", taggedPhaseNames(maintenancePhases(legacyTickPhases)), []string{
		"reconcile_pool_deaths",
		"config_reload",
		"fs_pressure_gate",
		"managed_dolt_preflight",
		"wake_orders_lane",
		"runtime_inventory_lane",
		"recover_unrouted_work_routes",
		"load_session_snapshot",
		"reap_runtimes_bound_to_closed_beads",
		"sweep_process_table_orphans",
		"reap_closed_bead_worktrees",
		"reap_stale_extmsg_bindings",
		"reap_stale_extmsg_participants",
		"emit_due_compute_facts",
		"start_historical_transcript_meta_reconcile",
		"sweep_detached_handoff_orphans",
		"nudge_dispatch_tick",
		"retire_suspended_rig_scopes",
		"reconcile_execution_completions",
		"wisp_gc",
		"workspace_service_tick",
		"process_convergence_requests",
		"convergence_tick",
	})

	cr, store := newPhaseFixtureRuntime(t, false, true)
	armMaintenanceLanes(t, cr, store)
	p := &tickPass{ctx: context.Background(), dirty: cr.configDirty, trigger: "patrol", prevPoolRunning: new(map[string]bool)}
	p.trace = cr.beginTraceCycle("patrol", "controller_tick", nil)
	if !cr.runTickPhases(p, maintenancePhases(legacyTickPhases)) {
		t.Fatal("the v2 tick phases stopped early")
	}
	p.trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"})
	assertLinesEqual(t, "v2 tick operation records", tickOperationRecords(t, cr), []string{
		"controller.tick.phase managed_dolt_preflight",
		"controller.tick.phase wake_orders_lane",
		"controller.tick.phase runtime_inventory_lane",
		"controller.tick.phase recover_unrouted_work_routes",
		"session_snapshot.load load_session_snapshot.initial",
		"controller.tick.phase reap_runtimes_bound_to_closed_beads",
		"controller.tick.phase sweep_process_table_orphans",
		"controller.tick.phase reap_stale_extmsg_bindings",
		"controller.tick.phase reap_stale_extmsg_participants",
		"controller.tick.phase bead_reconcile.sweep_detached_handoff_orphans",
		"controller.tick.phase bead_reconcile.nudge_dispatch_tick",
		"controller.tick.phase reconcile_execution_completions",
		"controller.tick.phase workspace_service_tick",
		"controller.tick.phase process_convergence_requests",
		"controller.tick.phase convergence_tick",
	})
	assertLinesEqual(t, "v2 tick store calls", store.recorded(), []string{
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
		"List status= type= label=gc:extmsg-binding metadata= include_closed=false live=false",
		"List status= type= label=gc:extmsg-group-participant metadata= include_closed=false live=false",
		"Get gc-1",
	})
	if writes := storeWrites(store.recorded()); len(writes) != 0 {
		t.Errorf("the v2 tick phases wrote to the store: %v", writes)
	}
	if !transcriptMetaStarted(cr) {
		t.Error("the v2 tick phases did not start the historical transcript pass")
	}
}

// Kills: a session phase left in what v2 leaves the startup step, or its
// maintenance phases dropped or changed from the legacy boot reconcile's:
// boot-mode usage facts (no live-lane read), no historical transcript pass,
// then the detached-orphan delta and the nudge fallback.
func TestCityRuntimeStartupV2RunsMaintenancePhasesOnly(t *testing.T) {
	assertLinesEqual(t, "v2 startup phases", taggedPhaseNames(maintenancePhases(legacyStartupPhases)), []string{
		"managed_dolt_preflight",
		"load_session_snapshot",
		"reap_runtimes_bound_to_closed_beads",
		"sweep_process_table_orphans",
		"emit_due_compute_facts",
		"sweep_detached_handoff_orphans",
		"nudge_dispatch_tick",
	})

	cr, store := newPhaseFixtureRuntime(t, false, true)
	armMaintenanceLanes(t, cr, store)
	p := &tickPass{ctx: context.Background()}
	if !cr.runTickPhases(p, maintenancePhases(legacyStartupPhases)) {
		t.Fatal("the v2 startup phases stopped early")
	}
	p.trace.end(TraceCompletionCompleted, traceRecordPayload{"phase": "startup"})
	assertLinesEqual(t, "v2 startup operation records", tickOperationRecords(t, cr), []string{
		"controller.tick.phase reap_runtimes_bound_to_closed_beads",
		"controller.tick.phase bead_reconcile.sweep_detached_handoff_orphans",
		"controller.tick.phase bead_reconcile.nudge_dispatch_tick",
	})
	assertLinesEqual(t, "v2 startup store calls", store.recorded(), []string{
		"List status=open type=session label= metadata= include_closed=false live=false",
		"List status=open type= label=gc:session metadata= include_closed=false live=false",
	})
	if transcriptMetaStarted(cr) {
		t.Error("the v2 startup phases started the historical transcript pass")
	}
}

// Kills: the guard removed (a legacy session path runs under v2), or the
// guard active under legacy. Under v2 every way into the legacy session
// reconciler is refused and counted, and each site is reported once; under
// legacy the same calls run and count nothing.
func TestLegacySessionEntryGuardBlocksAndCountsUnderV2(t *testing.T) {
	enterEverySite := func(cr *CityRuntime) {
		p := &tickPass{ctx: context.Background(), dirty: cr.configDirty, trigger: "patrol", prevPoolRunning: new(map[string]bool)}
		cr.runTickPhases(p, legacyTickPhases)
		cr.beadReconcileTick(context.Background(), DesiredStateResult{}, nil, nil, false)
		cr.controlDispatcherTick(context.Background())
	}

	t.Run("v2", func(t *testing.T) {
		cr, store := newPhaseFixtureRuntime(t, false, true)
		cr.reconcilerDrift = reconcilerModeDrift{running: reconcilerV2}
		var stderr strings.Builder
		cr.stderr = &stderr
		enterEverySite(cr)
		enterEverySite(cr)
		// Eight session tick phases plus the two direct entries, twice.
		if got := cr.legacySessionEntries.Load(); got != 20 {
			t.Errorf("legacySessionEntries = %d, want 20", got)
		}
		if writes := storeWrites(store.recorded()); len(writes) != 0 {
			t.Errorf("a refused legacy session path wrote to the store: %v", writes)
		}
		for _, site := range []string{"cleanup_dead_runtime_session_corpses", "demand_desired_state_and_sync", "bead_reconcile_tick", "auto_suspend_chat_sessions", "control_dispatcher_tick"} {
			if n := strings.Count(stderr.String(), fmt.Sprintf("entry %q", site)); n != 1 {
				t.Errorf("site %s reported %d times, want once:\n%s", site, n, stderr.String())
			}
		}
	})

	t.Run("legacy", func(t *testing.T) {
		cr, store := newPhaseFixtureRuntime(t, false, true)
		enterEverySite(cr)
		if got := cr.legacySessionEntries.Load(); got != 0 {
			t.Errorf("legacySessionEntries = %d under legacy, want 0", got)
		}
		if writes := storeWrites(store.recorded()); len(writes) == 0 {
			t.Error("the legacy session phases did not run under legacy")
		}
	})
}
