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
)

// opRecordingStore records every store call a tick makes, in order, as a
// stable one-line summary: the method, the bead it names and the shape of the
// write (field and metadata key names, never values, which carry wall times).
type opRecordingStore struct {
	beads.Store
	mu  sync.Mutex
	ops []string
}

func (s *opRecordingStore) record(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, fmt.Sprintf(format, args...))
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

// tickOperationRecords returns the controller operation records the runtime
// traced, in order, as "site operation_name".
func tickOperationRecords(t *testing.T, cr *CityRuntime) []string {
	t.Helper()
	if err := cr.trace.Close(); err != nil {
		t.Fatalf("closing the tracer: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cr.cityPath), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
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

// phaseInternalRecord reports a record a phase writes from inside its own
// body. The golden pins the phases, not the internals of the reconcile.
func phaseInternalRecord(name string) bool {
	for _, prefix := range []string{"bead_reconcile.", "session_reconcile.", "demand_snapshot.", "sync_beads_and_update_index."} {
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
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), cr.configDirty, &lastProviderName, cr.cityPath, &prevPoolRunning, trigger)
}

// Kills: the phase-driven tick drifting from the tick it replaced. The traced
// operation records and the store calls of two fixture ticks (a manual soft
// reload that reaches every config-gated phase with the inventory lane up,
// and a plain patrol with no lane) are pinned exactly as main's tick
// produced them before the split: any phase reordered, dropped, duplicated or
// changed in what it reads or writes fails here.
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
				"controller.tick.phase bead_reconcile_tick",
				"controller.tick.phase reconcile_execution_completions",
				"controller.tick.phase wisp_gc",
				"controller.tick.phase workspace_service_tick",
				"controller.tick.phase auto_suspend_chat_sessions",
				"controller.tick.phase process_convergence_requests",
				"controller.tick.phase convergence_tick",
			},
			wantStore: []string{
				"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
				"SetMetadataBatch gc-1 core_hash_breakdown,started_config_hash,started_launch_hash,started_provision_hash",
				"SetMetadataBatch gc-1 state",
				"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
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
				"controller.tick.phase bead_reconcile_tick",
				"controller.tick.phase reconcile_execution_completions",
				"controller.tick.phase workspace_service_tick",
				"controller.tick.phase process_convergence_requests",
				"controller.tick.phase convergence_tick",
			},
			wantStore: []string{
				"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
				"SetMetadataBatch gc-1 state",
				"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, store := newPhaseFixtureRuntime(t, tc.withReload, tc.withLane)
			runFixtureTick(cr, tc.trigger)
			gotStore := storeWrites(store.recorded())
			gotOps := tickOperationRecords(t, cr)
			assertLinesEqual(t, "tick operation records", gotOps, tc.wantOps)
			assertLinesEqual(t, "tick store writes", gotStore, tc.wantStore)
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
// startup step drifting from what main's ran: the phases it traces and the
// store writes it makes.
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
	assertLinesEqual(t, "startup operation records", tickOperationRecords(t, cr), []string{
		"controller.tick.phase cleanup_dead_runtime_session_corpses",
		"controller.tick.phase reap_runtimes_bound_to_closed_beads",
	})
	assertLinesEqual(t, "startup store writes", storeWrites(store.recorded()), []string{
		"SetMetadataBatch gc-1 agent_name,command,continuation_epoch,pool_managed,session_origin,synced_at",
		"SetMetadataBatch gc-1 state",
		"SetMetadataBatch gc-1 effective_sleep_after_idle,requested_sleep_after_idle,sleep_capability,sleep_policy_source",
	})
}

// Kills: a session phase (chat auto-suspend, corpse cleanup, demand and
// sync, bead reconcile, ...) left in what v2 leaves the tick, or a
// maintenance phase dropped from it. Running that list writes nothing to the
// session row and traces no session phase.
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
		"reconcile_execution_completions",
		"wisp_gc",
		"workspace_service_tick",
		"process_convergence_requests",
		"convergence_tick",
	})

	cr, store := newPhaseFixtureRuntime(t, false, true)
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
		"controller.tick.phase reconcile_execution_completions",
		"controller.tick.phase workspace_service_tick",
		"controller.tick.phase process_convergence_requests",
		"controller.tick.phase convergence_tick",
	})
	if writes := storeWrites(store.recorded()); len(writes) != 0 {
		t.Errorf("the v2 tick phases wrote to the store: %v", writes)
	}
}

// Kills: a session phase left in what v2 leaves the startup step, or its
// maintenance phases dropped.
func TestCityRuntimeStartupV2RunsMaintenancePhasesOnly(t *testing.T) {
	assertLinesEqual(t, "v2 startup phases", taggedPhaseNames(maintenancePhases(legacyStartupPhases)), []string{
		"managed_dolt_preflight",
		"load_session_snapshot",
		"reap_runtimes_bound_to_closed_beads",
		"sweep_process_table_orphans",
		"emit_due_compute_facts",
	})
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
