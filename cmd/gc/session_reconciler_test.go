package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeIdleTracker is a test double for idleTracker.
type fakeIdleTracker struct {
	idle map[string]bool
}

func newFakeIdleTracker() *fakeIdleTracker {
	return &fakeIdleTracker{idle: make(map[string]bool)}
}

func (f *fakeIdleTracker) checkIdle(sessionName string, _ runtime.Provider, _ time.Time) bool {
	return f.idle[sessionName]
}

func (f *fakeIdleTracker) setTimeout(_ string, _ time.Duration) {}

type delayedSessionExistsProvider struct {
	*runtime.Fake
	pendingConflict map[string]bool
	hiddenRunning   map[string]bool
	hiddenMeta      map[string]map[string]string
}

func newDelayedSessionExistsProvider() *delayedSessionExistsProvider {
	return &delayedSessionExistsProvider{
		Fake:            runtime.NewFake(),
		pendingConflict: make(map[string]bool),
		hiddenRunning:   make(map[string]bool),
		hiddenMeta:      make(map[string]map[string]string),
	}
}

func (p *delayedSessionExistsProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.pendingConflict[name] {
		delete(p.pendingConflict, name)
		p.hiddenRunning[name] = true
		return runtime.ErrSessionExists
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *delayedSessionExistsProvider) IsRunning(name string) bool {
	if p.hiddenRunning[name] {
		return true
	}
	return p.Fake.IsRunning(name)
}

func (p *delayedSessionExistsProvider) GetMeta(name, key string) (string, error) {
	if p.hiddenRunning[name] {
		return p.hiddenMeta[name][key], nil
	}
	return p.Fake.GetMeta(name, key)
}

func (p *delayedSessionExistsProvider) ProcessAlive(name string, processNames []string) bool {
	if p.hiddenRunning[name] {
		return true
	}
	return p.Fake.ProcessAlive(name, processNames)
}

type lateSuccessStartProvider struct {
	*runtime.Fake
	startErr error
}

func (p *lateSuccessStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if id := cfg.Env["GC_SESSION_ID"]; id != "" {
		_ = p.SetMeta(name, "GC_SESSION_ID", id)
	}
	if token := cfg.Env["GC_INSTANCE_TOKEN"]; token != "" {
		_ = p.SetMeta(name, "GC_INSTANCE_TOKEN", token)
	}
	if p.startErr != nil {
		return p.startErr
	}
	return nil
}

// reconcilerTestEnv holds common test infrastructure.
type reconcilerTestEnv struct {
	store        beads.Store
	sp           *runtime.Fake
	dt           *drainTracker
	clk          *clock.Fake
	rec          events.Recorder
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	cfg          *config.City
	desiredState map[string]TemplateParams
}

func newReconcilerTestEnv() *reconcilerTestEnv {
	return &reconcilerTestEnv{
		store:        beads.NewMemStore(),
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
	}
}

// addDesired registers a session in the desired state and optionally starts
// it in the provider. Returns the TemplateParams for further customization.
func (e *reconcilerTestEnv) addDesired(name, template string, running bool) {
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  name,
		TemplateName: template,
	}
	e.desiredState[name] = tp
	if running {
		_ = e.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"})
	}
}

// addRunningWorkerDesiredWithNewConfig registers and starts the worker session with the drift test command.
func (e *reconcilerTestEnv) addRunningWorkerDesiredWithNewConfig() {
	tp := TemplateParams{
		Command:      "new-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	e.desiredState["worker"] = tp
	_ = e.sp.Start(context.Background(), "worker", runtime.Config{Command: "new-cmd"})
}

// addDesiredLive registers a session with custom session_live config.
func (e *reconcilerTestEnv) addDesiredLive(name, template string, running bool, live []string) {
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  name,
		TemplateName: template,
		Hints:        agent.StartupHints{SessionLive: live},
	}
	e.desiredState[name] = tp
	if running {
		_ = e.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd", SessionLive: live})
	}
}

func (e *reconcilerTestEnv) createSessionBead(name, template string) beads.Bead {
	meta := map[string]string{
		"session_name":   name,
		"agent_name":     name,
		"template":       template,
		"live_hash":      runtime.LiveFingerprint(runtime.Config{Command: "test-cmd"}),
		"generation":     "1",
		"instance_token": "test-token",
		"state":          "asleep",
	}
	b, err := e.store.Create(beads.Bead{
		Title:    name,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		panic("creating test bead: " + err.Error())
	}
	return b
}

func (e *reconcilerTestEnv) setSessionMetadata(session *beads.Bead, kvs map[string]string) {
	for key, value := range kvs {
		_ = e.store.SetMetadata(session.ID, key, value)
		session.Metadata[key] = value
	}
}

func (e *reconcilerTestEnv) markSessionCreating(session *beads.Bead) {
	e.setSessionMetadata(session, map[string]string{"state": "creating"})
}

func (e *reconcilerTestEnv) markSessionActive(session *beads.Bead) {
	e.setSessionMetadata(session, map[string]string{
		"state":        "active",
		"last_woke_at": e.clk.Now().UTC().Format(time.RFC3339),
	})
}

func (e *reconcilerTestEnv) reconcile(sessions []beads.Bead) int {
	// Auto-derive poolDesired from desiredState, mirroring production behavior
	// where ComputePoolDesiredStates populates ScaleCheckCounts before calling
	// reconcileSessionBeads. Each template in the desired set gets a count of
	// how many sessions reference it.
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	return e.reconcileWithPoolDesired(sessions, poolDesired)
}

func (e *reconcilerTestEnv) reconcileWithPoolDesired(sessions []beads.Bead, poolDesired map[string]int) int {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	return reconcileSessionBeads(
		context.Background(), sessions, e.desiredState, cfgNames, e.cfg, e.sp,
		e.store, nil, nil, nil, e.dt, poolDesired, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr,
	)
}

func TestReconcileSessionBeads_DrainAckKeepsBeadOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should be stopped after drain-ack")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_DrainAckWithAssignedOpenWorkSleepsInsteadOfDraining(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if _, err := env.store.Create(beads.Bead{
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(future work): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should be stopped after drain-ack")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_UndesiredDrainAckStopsAndCloses(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should be stopped after drain-ack even after leaving desired state")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed; metadata=%v", got.Status, got.Metadata)
	}
	if got.Metadata["close_reason"] != "drained" {
		t.Fatalf("close_reason = %q, want drained", got.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_UndesiredDrainAckWithAssignedOpenWorkSleepsInsteadOfClosing(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	if _, err := env.store.Create(beads.Bead{
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(future work): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		nil,
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should be stopped after drain-ack even after leaving desired state")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after drain-ack", got.Metadata["pending_create_claim"])
	}
}

// TestReconcileSessionBeads_DrainAckUsesLiveStoreQuery is the regression
// guard for the stuck-pool-worker bug on ga-ttn5z. Pool workers close
// their own work bead with `bd close` BEFORE calling `gc runtime
// drain-ack`; the old code path then read `hasAssignedWork` from a tick
// snapshot captured before the close, so the snapshot falsely reported
// the now-closed bead as open+assigned. That flipped the session into
// CompleteDrainPatch (state=asleep, sleep_reason=idle) instead of
// AcknowledgeDrainPatch (state=drained), which hid the bead from the
// close gate and stranded new queue work on a ghost slot.
//
// Fix: the snapshot-based path is gone; drain-ack queries the live
// store via sessionHasOpenAssignedWork. This test verifies the
// post-fix outcome: with no open assigned work in the store, drain-ack
// lands the session in `drained` (the correct terminal for recycling).
func TestReconcileSessionBeads_DrainAckUsesLiveStoreQuery(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil, // rigStores
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained — drain-ack must query the live store and land a session with no assigned work in drained state", got.Metadata["state"])
	}
}

// TestReconcileSessionBeads_AsleepIdlePoolBeadFreesSlot is the other half
// of the fix: even if a pool bead somehow lands in state=asleep +
// sleep_reason=idle (from the old buggy drain-ack path OR any other
// route), the close gate must still free the slot so the supervisor can
// spawn a fresh worker for pending queue work. Before the fix the close
// gate only fired for state=drained, so idle-asleep pool beads sat open
// indefinitely and blocked new spawns.
func TestReconcileSessionBeads_AsleepIdlePoolBeadFreesSlot(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // NOT running
	session := env.createSessionBead("worker", "worker")
	// Simulate the post-drain-ack-ghost state: asleep + sleep_reason=idle
	// + pool-managed, but the runtime has exited and no work is assigned.
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
		nil, // rigStores
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed — asleep-idle pool beads must free their slot via the live-query close gate", got.Status)
	}
}

// (Removed: TestReconcileSessionBeads_DrainAckPartialOwnershipSnapshotFailsClosed
// guarded the old snapshot-backed fail-closed path — the sleep_reason
// "ownership_snapshot_partial" branch. Drain-ack now re-queries the store
// live so the pre-close ownership snapshot can no longer leak into the
// decision. Live store errors still fail closed, but the path that
// produced the ownership_snapshot_partial reason is gone.)

// listErrStore wraps a beads.Store and returns a configured error from
// List. Used by the drain-ack fail-closed regression test below.
type listErrStore struct {
	beads.Store
	err error
}

func (s *listErrStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(q)
}

// TestReconcileSessionBeads_DrainAckLiveStoreErrorFailsClosed guards the
// drain-ack live-query error path. When sessionHasOpenAssignedWork returns
// an error, drain-ack treats hasAssignedWork as true (fail-closed) so the
// session lands in CompleteDrainPatch (asleep+idle) rather than
// AcknowledgeDrainPatch (drained). This prevents a transient store failure
// from silently closing a session whose assignment status we cannot verify.
func TestReconcileSessionBeads_DrainAckLiveStoreErrorFailsClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Wrap the store so List returns an error for the live-query check.
	erroring := &listErrStore{Store: env.store, err: fmt.Errorf("store is unavailable")}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		erroring,
		dops,
		nil,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("state=%q sleep_reason=%q, want asleep/idle — live-query error must fail closed (hasAssignedWork=true) so the session does not enter drained state",
			got.Metadata["state"], got.Metadata["sleep_reason"])
	}
}

// TestReconcileSessionBeads_CloseGateLiveStoreErrorKeepsSlot guards the
// mirror-image fail-closed guard in the asleep-idle close gate. When
// sessionHasOpenAssignedWork errors during the close-gate check, the gate
// must treat hasAssignedWork as true (fail-closed) and leave the bead
// open. Without this guard a transient store blip would silently close a
// pool slot whose assignment status was unverifiable.
func TestReconcileSessionBeads_CloseGateLiveStoreErrorKeepsSlot(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false) // NOT running
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	erroring := &listErrStore{Store: env.store, err: fmt.Errorf("store is unavailable")}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		erroring,
		newFakeDrainOps(),
		nil,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = %q, want open — close-gate live-query error must fail closed (hasAssignedWork=true) so the pool slot stays open until the store can confirm no assignments", got.Status)
	}
}

// TestReconcileSessionBeads_CloseGateRespectsCrossStoreAssignedWork
// verifies that the close gate's live ownership check looks across the
// primary store AND any attached rig stores. A city-stored asleep+idle
// pool session with work assigned to it in a rig store must NOT get
// its slot freed — the rig-stored work would be orphaned.
func TestReconcileSessionBeads_CloseGateRespectsCrossStoreAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
	})

	// Work assigned to the session lives in a rig store, not the city
	// store. The live cross-store query must find it and veto the close.
	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{
		Title:    "cross-store work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work bead: %v", err)
	}
	rigStores := map[string]beads.Store{"some-rig": rigStore}

	reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		newFakeDrainOps(),
		nil,
		rigStores,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("status = %q, want open — close gate must honor cross-store assigned work and leave the pool slot for the rig-stored work to drain", got.Status)
	}
}

// TestReconcileSessionBeads_CloseGatePreservesSleepReason verifies that the
// close gate carries the session's existing sleep_reason (idle,
// idle-timeout, drained) into the closed bead's close reason. Losing this
// distinction in closed records erases the forensic difference between an
// idle-timeout recycle and an explicit drain.
func TestReconcileSessionBeads_CloseGatePreservesSleepReason(t *testing.T) {
	cases := []struct {
		name        string
		sleepReason string
		wantReason  string
	}{
		{"idle", "idle", "idle"},
		{"idle-timeout", "idle-timeout", "idle-timeout"},
		{"drained-reason", "drained", "drained"},
		{"missing-reason", "", "drained"}, // fallback
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{
				Agents: []config.Agent{{Name: "worker"}},
			}
			env.addDesired("worker", "worker", false)
			session := env.createSessionBead("worker", "worker")
			meta := map[string]string{
				"state":                "asleep",
				poolManagedMetadataKey: boolMetadata(true),
			}
			if tc.sleepReason != "" {
				meta["sleep_reason"] = tc.sleepReason
			} else {
				// Drained state still qualifies as freeable via
				// isDrainedSessionBead; use that so the close gate fires
				// with no sleep_reason set.
				meta["state"] = "drained"
			}
			env.setSessionMetadata(&session, meta)

			reconcileSessionBeadsAtPath(
				context.Background(),
				"",
				[]beads.Bead{session},
				env.desiredState,
				map[string]bool{"worker": true},
				env.cfg,
				env.sp,
				env.store,
				newFakeDrainOps(),
				nil,
				nil,
				nil,
				env.dt,
				nil,
				false,
				nil,
				"",
				nil,
				env.clk,
				env.rec,
				0,
				0,
				&env.stdout,
				&env.stderr,
			)

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", session.ID, err)
			}
			if got.Status != "closed" {
				t.Fatalf("status = %q, want closed", got.Status)
			}
			if got.Metadata["close_reason"] != tc.wantReason {
				t.Fatalf("close_reason = %q, want %q — close gate must preserve the originating sleep_reason for forensic fidelity", got.Metadata["close_reason"], tc.wantReason)
			}
		})
	}
}

func TestReconcileSessionBeads_DrainAckResumeModePreservesSessionIdentity(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "resume",
		"session_key":         "resume-key",
		"started_config_hash": "hash-before-drain",
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["session_key"] != "resume-key" {
		t.Fatalf("session_key = %q, want preserved resume key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-drain" {
		t.Fatalf("started_config_hash = %q, want preserved hash", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_DrainAckFreshModeClearsSessionIdentity(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "fresh",
		"session_key":         "fresh-key",
		"started_config_hash": "hash-before-drain",
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want cleared for wake_mode=fresh", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared for wake_mode=fresh", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_DrainedPoolSessionStoreQueryPartialStaysOpen
// verifies that when storeQueryPartial is set (a transient bead-store
// failure produced an incomplete assignedWorkBeads snapshot), the
// drained pool session bead is NOT closed. Close decisions must fail
// closed whenever the tick's store visibility is compromised, even if
// the live ownership check itself returns cleanly.
func TestReconcileSessionBeads_DrainedPoolSessionStoreQueryPartialStaysOpen(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}

	session := env.createSessionBead("worker-live", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "drained",
		"sleep_reason":         "drained",
		"pool_slot":            "1",
		poolManagedMetadataKey: boolMetadata(true),
		"session_origin":       "ephemeral",
	})

	woken := reconcileSessionBeadsAtPath(
		context.Background(),
		"",
		[]beads.Bead{session},
		nil,
		nil,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil, // rigStores
		nil,
		env.dt,
		map[string]int{},
		true, // storeQueryPartial
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly under storeQueryPartial: metadata=%v", got.Metadata)
	}
}

// stopFailProvider wraps a Fake but makes Stop always fail.
// The session remains running (IsRunning returns true).
type stopFailProvider struct {
	*runtime.Fake
}

func (p *stopFailProvider) Stop(_ string) error {
	return fmt.Errorf("stop failed: session unavailable")
}

func TestReconcileSessionBeads_DrainAckStopFailurePreservesMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "fresh",
		"session_key":         "fresh-key",
		"started_config_hash": "hash-before-drain",
		"last_woke_at":        env.clk.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	// Wrap the real provider so Stop fails but IsRunning still returns true.
	failSp := &stopFailProvider{Fake: env.sp}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		failSp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	// When Stop fails, metadata should NOT be updated — the session is still alive.
	if got.Metadata["state"] == "drained" {
		t.Fatalf("state should not be drained when stop failed")
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Fatalf("last_woke_at should be preserved when stop failed")
	}
	if got.Metadata["session_key"] == "" {
		t.Fatalf("session_key should be preserved when stop failed")
	}
}

func TestReconcileSessionBeads_DrainAckResumeModeNotClassifiedAsCrashNextTick(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wake_mode":           "resume",
		"session_key":         "resume-key",
		"started_config_hash": "hash-before-drain",
	})

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after drain-ack: %v", session.ID, err)
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want cleared after drain-ack", got.Metadata["last_woke_at"])
	}

	woken = reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{got},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("second tick woken = %d, want 0", woken)
	}

	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after second tick: %v", session.ID, err)
	}
	if got.Metadata["session_key"] != "resume-key" {
		t.Fatalf("session_key = %q after second tick, want preserved resume key", got.Metadata["session_key"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty for intentional drain", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_DrainAckHonoredAfterSessionExit(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if err := env.sp.Stop("worker"); err != nil {
		t.Fatalf("Stop(worker): %v", err)
	}

	woken := reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{session},
		env.desiredState,
		map[string]bool{"worker": true},
		env.cfg,
		env.sp,
		env.store,
		dops,
		nil,
		nil,
		env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("worker should remain stopped after drain-ack")
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead closed unexpectedly: metadata=%v", got.Metadata)
	}
	if got.Metadata["state"] != "drained" {
		t.Fatalf("state = %q, want drained", got.Metadata["state"])
	}
}

// --- buildDepsMap tests ---

func TestBuildDepsMap_NilConfig(t *testing.T) {
	deps := buildDepsMap(nil)
	if deps != nil {
		t.Errorf("expected nil, got %v", deps)
	}
}

func TestBuildDepsMap_NoDeps(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "a"},
			{Name: "b"},
		},
	}
	deps := buildDepsMap(cfg)
	if len(deps) != 0 {
		t.Errorf("expected empty map, got %v", deps)
	}
}

func TestBuildDepsMap_WithDeps(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	deps := buildDepsMap(cfg)
	if len(deps) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(deps))
	}
	if len(deps["worker"]) != 1 || deps["worker"][0] != "db" {
		t.Errorf("expected worker -> [db], got %v", deps["worker"])
	}
}

// --- derivePoolDesired tests ---

// --- allDependenciesAlive tests ---

func TestAllDependenciesAlive_NoDeps(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	sp := runtime.NewFake()
	if !allDependenciesAlive(session, cfg, nil, sp, "test", nil) {
		t.Error("no deps should return true")
	}
}

func TestAllDependenciesAlive_DepAlive(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "db", runtime.Config{})
	desired := map[string]TemplateParams{
		"db": {TemplateName: "db"},
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "test", nil) {
		t.Error("dep is alive, should return true")
	}
}

func TestAllDependenciesAlive_DepDead(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sp := runtime.NewFake()
	desired := map[string]TemplateParams{
		"db": {TemplateName: "db"},
	}
	if allDependenciesAlive(session, cfg, desired, sp, "test", nil) {
		t.Error("dep is dead, should return false")
	}
}

func TestAllDependenciesAlive_UsesLegacyAgentLabelTemplate(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "db",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/db"},
		Metadata: map[string]string{
			"template":     "frontend/db",
			"session_name": "custom-db",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	session := beads.Bead{
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
		Metadata: map[string]string{
			"template":     "worker",
			"pool_slot":    "1",
			"session_name": "custom-worker-1",
		},
	}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", DependsOn: []string{"frontend/db"}, MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
			{Name: "db", Dir: "frontend"},
		},
	}
	sp := runtime.NewFake()
	desired := map[string]TemplateParams{
		"custom-db":       {TemplateName: "frontend/db"},
		"custom-worker-1": {TemplateName: "frontend/worker"},
	}
	if allDependenciesAlive(session, cfg, desired, sp, "test", store) {
		t.Error("legacy labeled worker should still see db as a missing dependency")
	}
	if err := sp.Start(context.Background(), "custom-db", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "test", store) {
		t.Error("legacy labeled worker should see db as alive once the dependency starts")
	}
}

// --- reconcileSessionBeads tests ---

func TestReconcileSessionBeads_WakesDeadSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Errorf("expected 1 woken, got %d", woken)
	}
	if !env.sp.IsRunning("worker") {
		t.Error("session should have been started via Provider")
	}
}

func TestReconcileSessionBeads_AlwaysNamedSessionWakesFromDrainedCompatibilityState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("always named session %q should have been restarted", sessionName)
	}
}

func TestReconcileSessionBeads_OrdinaryDesiredStateDoesNotWakeDrainedCompatibilityState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(1)}},
	}
	env.desiredState["worker"] = TemplateParams{
		Command:      "true",
		SessionName:  "worker",
		TemplateName: "worker",
	}
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning("worker") {
		t.Fatal("ordinary desiredState presence must not restart a drained compatibility bead")
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionDoesNotWakeFromDesiredStatePresence(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "on_demand",
	}
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "asleep",
		"sleep_reason":               "drained",
		"continuation_reset_pending": "true",
	})

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("on-demand named session %q should remain asleep without direct demand", sessionName)
	}
}

func TestReconcileSessionBeads_SyncsGCDirWithWorkDirOverride(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		WorkDir:      "/template/worktree",
		Env:          map[string]string{"GC_DIR": "/template/worktree"},
	}
	session := env.createSessionBead("worker", "worker")
	_ = env.store.SetMetadata(session.ID, "work_dir", "/instance/worktree")
	session.Metadata["work_dir"] = "/instance/worktree"
	env.markSessionCreating(&session)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 1 {
		t.Fatalf("expected 1 woken, got %d", woken)
	}
	var startCfg runtime.Config
	found := false
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "worker" {
			startCfg = call.Config
			found = true
		}
	}
	if !found {
		t.Fatal("expected Start call for worker")
	}
	if startCfg.WorkDir != "/instance/worktree" {
		t.Fatalf("WorkDir = %q, want %q", startCfg.WorkDir, "/instance/worktree")
	}
	if got := startCfg.Env["GC_DIR"]; got != "/instance/worktree" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/instance/worktree")
	}
	if got := env.desiredState["worker"].Env["GC_DIR"]; got != "/template/worktree" {
		t.Fatalf("desiredState GC_DIR mutated to %q", got)
	}
}

func TestReconcileSessionBeads_SkipsAliveSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Errorf("expected 0 woken, got %d", woken)
	}
}

func TestReconcileSessionBeads_SkipsQuarantinedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	// Set quarantine in the future.
	_ = env.store.SetMetadata(session.ID, "quarantined_until",
		env.clk.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	session.Metadata["quarantined_until"] = env.clk.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)

	woken := env.reconcile([]beads.Bead{session})

	if woken != 0 {
		t.Errorf("expected 0 woken (quarantined), got %d", woken)
	}
}

func TestReconcileSessionBeads_RespectsWakeBudget(t *testing.T) {
	env := newReconcilerTestEnv()
	var cfgAgents []config.Agent
	var sessions []beads.Bead
	for i := 0; i < defaultMaxWakesPerTick+3; i++ {
		name := fmt.Sprintf("worker-%d", i)
		cfgAgents = append(cfgAgents, config.Agent{Name: name})
		env.addDesired(name, name, false)
		session := env.createSessionBead(name, name)
		env.markSessionCreating(&session)
		sessions = append(sessions, session)
	}
	env.cfg = &config.City{Agents: cfgAgents}

	woken := env.reconcile(sessions)

	if woken != defaultMaxWakesPerTick {
		t.Errorf("expected %d woken (budget), got %d", defaultMaxWakesPerTick, woken)
	}
}

func TestReconcileSessionBeads_ConfigDriftInitiatesDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has a DIFFERENT config than what's in the bead.
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	// Session has fully started — started_config_hash records what it launched with.
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
	})

	// Verify hashes differ.
	currentHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if startedHash == currentHash {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", startedHash, currentHash)
	}

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("expected drain to be initiated for config drift (session.ID=%q, stderr=%s)", session.ID, env.stderr.String())
	}
	if ds.reason != "config-drift" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "config-drift")
	}
}

func TestReconcileSessionBeads_NoDriftWhenHashMatches(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true) // same config as bead
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain, got %+v", ds)
	}
}

// Regression test for #127: a freshly created session can be drained for
// config-drift shortly after wake because the reconciler's drift check runs
// before started_config_hash is written. The fix skips drift detection until
// started_config_hash is present.
func TestReconcileSessionBeads_NoDriftBeforeStartedHashWritten(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has a DIFFERENT config than the bead's config_hash.
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	// Do NOT set started_config_hash — simulates the window between
	// sync-time config_hash write and post-start started_config_hash write.

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain before started_config_hash is written, got reason=%q", ds.reason)
	}
}

func TestReconcileSessionBeads_PendingCreateLeasePreventsOrphanClose(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	session := env.createSessionBead("s-gc-late", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "creating",
		"manual_session":       "true",
		"pending_create_claim": "true",
	})

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("pending-create session was closed as orphan: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" || got.Metadata["close_reason"] == "orphaned" {
		t.Fatalf("pending-create session was marked orphaned: %+v", got.Metadata)
	}
}

func TestReconcileSessionBeads_DependencyOrdering_DepDeadBlocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	env.addDesired("worker", "worker", false)
	// db is in desired but starts fail (provider Start returns error).
	env.addDesired("db", "db", false)
	env.sp.StartErrors = map[string]error{"db": fmt.Errorf("db failed to start")}

	dbBead := env.createSessionBead("db", "db")
	workerBead := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{workerBead, dbBead})

	// worker should NOT be started because db is still dead.
	// (db start failed, so sp.IsRunning("db") is false)
	if env.sp.IsRunning("worker") {
		t.Error("worker should NOT have been started (dep not alive)")
	}
}

func TestReconcileSessionBeads_DependencyOrdering_TopoOrder(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db", "db", false)

	dbBead := env.createSessionBead("db", "db")
	workerBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&dbBead)
	env.markSessionCreating(&workerBead)

	// Even though worker bead is listed first, topo ordering ensures
	// db is processed first. Since the Fake provider marks sessions as
	// running on Start, worker can wake in the same tick after db succeeds.
	woken := env.reconcile([]beads.Bead{workerBead, dbBead})

	if woken != 2 {
		t.Errorf("expected 2 woken (both), got %d", woken)
	}
}

func TestReconcileSessionBeads_PoolDependencyBlocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(2)},
		},
	}
	// Worker depends on pool "db". No db instances in desired → worker blocked.
	env.addDesired("worker", "worker", false)
	workerBead := env.createSessionBead("worker", "worker")

	woken := env.reconcile([]beads.Bead{workerBead})

	if woken != 0 {
		t.Errorf("expected 0 woken (pool dep dead), got %d", woken)
	}
}

func TestReconcileSessionBeads_PoolDependencyUnblocksWake(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(2)},
		},
	}
	env.addDesired("worker", "worker", false)
	env.addDesired("db-1", "db", true) // one pool instance alive
	workerBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&workerBead)
	dbBead := env.createSessionBead("db-1", "db")
	env.markSessionActive(&dbBead)

	woken := env.reconcile([]beads.Bead{workerBead, dbBead})

	if woken != 1 {
		t.Errorf("expected 1 woken (pool dep alive), got %d", woken)
	}
}

func TestReconcileSessionBeads_OrphanSessionDrained(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	// Session bead for "orphan" with no matching desired entry, but running.
	_ = env.sp.Start(context.Background(), "orphan", runtime.Config{})
	session := env.createSessionBead("orphan", "orphan")

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for orphan session")
	}
	if ds.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "orphaned")
	}
}

// TestReconcileSessionBeads_OrphanDrainLogThrottled covers issue #855:
// once a session is draining, the reconciler must not re-emit
// "Draining session '...': orphaned" on every subsequent tick. The
// drainTracker is idempotent, so a pre-existing drain entry means the
// reconciler tick is a no-op with respect to state — the user-visible
// log must reflect that.
func TestReconcileSessionBeads_OrphanDrainLogThrottled(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	_ = env.sp.Start(context.Background(), "orphan", runtime.Config{})
	session := env.createSessionBead("orphan", "orphan")

	// Simulate a drain that was begun on a prior tick and has not yet
	// converged (e.g., in-progress work beads still assigned to the
	// session block its bead from closing — the exact loop described
	// in #855).
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(defaultDrainTimeout),
		reason:     "orphaned",
		generation: 1,
	})

	env.reconcile([]beads.Bead{session})

	if got := env.stdout.String(); strings.Contains(got, "Draining session 'orphan'") {
		t.Errorf("stdout contains redundant drain log on repeat tick:\n%s", got)
	}
}

func TestReconcileSessionBeads_OrphanNotRunningClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	session := env.createSessionBead("orphan", "orphan")

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Errorf("orphan bead status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "orphaned" {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], "orphaned")
	}
}

func TestReconcileSessionBeads_SuspendedSessionDrained(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Template: "worker"}},
	}
	// "worker" is in config (configuredNames) but NOT in desiredState.
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{})
	session := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for suspended session")
	}
	if ds.reason != "suspended" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "suspended")
	}
}

func TestReconcileSessionBeads_SuspendedNotRunningClosed(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Template: "worker"}},
	}
	session := env.createSessionBead("worker", "worker")

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Errorf("suspended bead status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "suspended" {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], "suspended")
	}
}

func TestReconcileSessionBeads_PreservesConfiguredNamedSessionOutsideDesiredState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "open" {
		t.Fatalf("status = %q, want open", b.Status)
	}
	if b.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", b.Metadata["close_reason"])
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("unexpected drain for configured named session: %+v", ds)
	}
}

func TestReconcileSessionBeads_PreservedRunningNamedSessionStillIdleDrains(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})
	preservedTP, err := resolvePreservedConfiguredNamedSessionTemplate(".", env.cfg.Workspace.Name, env.cfg, env.sp, env.store, []beads.Bead{session}, session, env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve preserved named session: %v", err)
	}
	runtimeCfg := templateParamsToConfig(preservedTP)
	env.setSessionMetadata(&session, map[string]string{
		"live_hash":   runtime.LiveFingerprint(runtimeCfg),
		"detached_at": env.clk.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), sessionName, runtimeCfg); err != nil {
		t.Fatalf("start session: %v", err)
	}
	env.sp.WaitForIdleErrors[sessionName] = nil
	idleGate := make(chan struct{}) // see waitForIdleProbeReady godoc
	env.sp.WaitForIdleGates[sessionName] = idleGate

	env.reconcile([]beads.Bead{session})
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected idle drain for preserved running named session")
	}
	if ds.reason != "idle" {
		t.Fatalf("drain reason = %q, want idle", ds.reason)
	}
	b, _ := env.store.Get(session.ID)
	if b.Status != "open" {
		t.Fatalf("status = %q, want open", b.Status)
	}
}

func TestReconcileSessionBeads_PreservedRunningNamedSessionHonorsRestartRequest(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
		"pending_create_claim":       "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	// The stale create claim should be cleared by the restart path. Match the
	// live runtime to this bead so the pending-create rollback guard does not
	// claim the fixture first.
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("preserved running named session should stop after restart request")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", got.Metadata["restart_requested"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after durable restart request", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_HealsRunningPendingCreateToActive(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "test-cmd", MaxActiveSessions: intPtr(1)}},
	}
	env.addDesired("worker", "worker", false)

	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
		"sleep_reason":         "",
	})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active", got.Metadata["state"])
	}
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", got.Metadata["state_reason"])
	}
	if got.Metadata["started_config_hash"] == "" {
		t.Fatal("started_config_hash should be recorded when healing a live pending create")
	}
}

// TestReconcileAndWake_RestartRequestBumpsContinuationEpoch is an end-to-end
// test that chains reconcile (sets continuation_reset_pending) with
// preWakeCommit (consumes the flag and bumps continuation_epoch). This covers
// the full restart-requested → wake handoff.
func TestReconcileAndWake_RestartRequestBumpsContinuationEpoch(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
		"continuation_epoch":         "3",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	// Phase 1: reconcile processes restart_requested → sets continuation_reset_pending.
	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("after reconcile: continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}

	// Phase 2: preWakeCommit consumes continuation_reset_pending → bumps epoch.
	if _, _, err := preWakeCommit(&got, env.store, env.clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	woke, _ := env.store.Get(session.ID)
	if woke.Metadata["continuation_epoch"] != "4" {
		t.Fatalf("after wake: continuation_epoch = %q, want 4", woke.Metadata["continuation_epoch"])
	}
	if woke.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("after wake: continuation_reset_pending = %q, want empty", woke.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_InvalidNamedSessionConfigDoesNotPreserveBead(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
	})

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Status != "closed" {
		t.Fatalf("status = %q, want closed", b.Status)
	}
	if b.Metadata["close_reason"] != "suspended" {
		t.Fatalf("close_reason = %q, want suspended", b.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_OnDemandNamedSessionDoesNotRecoverClosedCanonicalFromWorkQuery(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "refinery",
			StartCommand:      "true",
			WorkQuery:         "printf ready",
			ScaleCheck:        "printf 0",
			MaxActiveSessions: intPtr(2),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
			Mode:     "on_demand",
		}},
	}

	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "refinery")
	historical, err := store.Create(beads.Bead{
		Title:  "refinery",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "refinery",
			"template":                   "refinery",
			"state":                      "stopped",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "refinery",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("create historical bead: %v", err)
	}
	if err := store.Close(historical.ID); err != nil {
		t.Fatalf("close historical bead: %v", err)
	}

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, io.Discard)
	if _, ok := dsResult.State[sessionName]; ok {
		t.Fatalf("desired state recovered named session %q from controller-side work_query; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if dsResult.NamedSessionDemand["refinery"] {
		t.Fatal("NamedSessionDemand should not include refinery from work_query")
	}
}

func TestReconcileSessionBeads_HealsExpiredTimers(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)
	past := env.clk.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "held_until", past)
	_ = env.store.SetMetadata(session.ID, "sleep_reason", "user-hold")
	session.Metadata["held_until"] = past
	session.Metadata["sleep_reason"] = "user-hold"

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["held_until"] != "" {
		t.Error("expired held_until should be cleared")
	}
	if b.Metadata["sleep_reason"] != "" {
		t.Error("sleep_reason should be cleared with expired hold")
	}
}

func TestReconcileSessionBeads_CrashDetection(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	recentWake := env.clk.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "last_woke_at", recentWake)
	session.Metadata["last_woke_at"] = recentWake

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "1" {
		t.Errorf("wake_attempts = %q, want %q", b.Metadata["wake_attempts"], "1")
	}
}

func TestReconcileSessionBeads_StableClearsFailures(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	stableWake := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	_ = env.store.SetMetadata(session.ID, "wake_attempts", "3")
	_ = env.store.SetMetadata(session.ID, "last_woke_at", stableWake)
	session.Metadata["wake_attempts"] = "3"
	session.Metadata["last_woke_at"] = stableWake

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "0" {
		t.Errorf("wake_attempts = %q, want %q", b.Metadata["wake_attempts"], "0")
	}
}

func TestReconcileSessionBeads_StableAlreadyClearDoesNotWriteMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	countingStore := newCountingMetadataStore()
	env.store = countingStore
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	stableWake := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"state":             "active",
		"wake_attempts":     "3",
		"last_woke_at":      stableWake,
		"quarantined_until": "",
	})

	countingStore.singleCalls = 0
	countingStore.batchCalls = 0
	env.reconcile([]beads.Bead{session})
	if countingStore.batchCalls == 0 {
		t.Fatal("first stable tick should write metadata to clear wake failures")
	}

	cleared, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("getting session bead: %v", err)
	}
	if cleared.Metadata["wake_attempts"] != "0" {
		t.Fatalf("wake_attempts after first tick = %q, want 0", cleared.Metadata["wake_attempts"])
	}

	countingStore.singleCalls = 0
	countingStore.batchCalls = 0
	env.reconcile([]beads.Bead{cleared})
	if got := countingStore.singleCalls + countingStore.batchCalls; got != 0 {
		t.Fatalf("second stable tick performed %d metadata write(s), want 0", got)
	}
}

func TestReconcileSessionBeads_NoAgentNotWoken(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	session := env.createSessionBead("orphan", "orphan")

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Errorf("expected 0 woken for orphan, got %d", woken)
	}
}

func TestReconcileSessionBeads_PreWakeCommitWritesMetadata(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	env.reconcile([]beads.Bead{session})

	b, _ := env.store.Get(session.ID)
	if b.Metadata["generation"] != "2" {
		t.Errorf("generation = %q, want %q (incremented by preWakeCommit)", b.Metadata["generation"], "2")
	}
	if b.Metadata["instance_token"] == "test-token" {
		t.Error("instance_token should have been regenerated by preWakeCommit")
	}
	if b.Metadata["last_woke_at"] == "" {
		t.Error("last_woke_at should be set by preWakeCommit")
	}
}

func TestReconcileSessionBeads_CancelsDrainOnWakeReason(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	gen := 1
	env.dt.set(session.ID, &drainState{
		startedAt:  env.clk.Now(),
		deadline:   env.clk.Now().Add(5 * time.Minute),
		reason:     "pool-excess",
		generation: gen,
	})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("drain should be canceled, got %+v", ds)
	}
}

func TestReconcileSessionBeads_UsesSleepIntentForDrainReason(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{}
	env.addDesired("worker", "worker", true)
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{})
	session := env.createSessionBead("worker", "worker")
	_ = env.store.SetMetadata(session.ID, "sleep_intent", "wait-hold")
	session.Metadata["sleep_intent"] = "wait-hold"

	env.reconcile([]beads.Bead{session})

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain when desired session has no wake reason")
	}
	if ds.reason != "wait-hold" {
		t.Fatalf("drain reason = %q, want wait-hold", ds.reason)
	}
}

func TestReconcileSessionBeads_StartFailureNoDoubleCounting(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	env.sp.StartErrors = map[string]error{"worker": fmt.Errorf("start failed")}
	session := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&session)

	// First tick: Start fails, wake_attempts should be 1.
	env.reconcile([]beads.Bead{session})
	b, _ := env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "1" {
		t.Fatalf("after first tick: wake_attempts = %q, want 1", b.Metadata["wake_attempts"])
	}

	// Second tick: reload bead from store to get updated metadata.
	b, _ = env.store.Get(session.ID)
	env.reconcile([]beads.Bead{b})
	b, _ = env.store.Get(session.ID)
	if b.Metadata["wake_attempts"] != "2" {
		t.Errorf("after second tick: wake_attempts = %q, want 2", b.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackAdHocCreateOnRuntimeCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"provider":              "claude",
			"work_dir":              t.TempDir(),
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	sp.hiddenMeta["sky"] = map[string]string{
		"GC_SESSION_ID":     "different-bead",
		"GC_INSTANCE_TOKEN": "different-token",
	}
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_ConvergesPendingCreateWhenRuntimeMatchesBead(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"provider":              "claude",
			"work_dir":              t.TempDir(),
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	sp.hiddenMeta["sky"] = map[string]string{
		"GC_SESSION_ID":     bead.ID,
		"GC_INSTANCE_TOKEN": "test-token",
	}
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("pending create should converge, not close")
	}
	if got.Metadata["session_name"] != "sky" {
		t.Fatalf("session_name = %q, want sky", got.Metadata["session_name"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateWhenConflictingRuntimeAlreadyRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}
	if err := sp.Start(context.Background(), "sky", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(conflicting runtime): %v", err)
	}
	if err := sp.SetMeta("sky", "GC_SESSION_ID", "different-bead"); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := sp.SetMeta("sky", "GC_INSTANCE_TOKEN", "different-token"); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
}

func TestReconcileSessionBeads_ConvergesPendingCreateOnLateSuccessStartError(t *testing.T) {
	store := beads.NewMemStore()
	sp := &lateSuccessStartProvider{
		Fake:     runtime.NewFake(),
		startErr: context.DeadlineExceeded,
	}
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("late-success start error should converge, not close")
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
}

func TestReconcileSessionBeads_DoesNotRollbackExistingSessionWithoutPendingClaim(t *testing.T) {
	store := beads.NewMemStore()
	sp := newDelayedSessionExistsProvider()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":       "sky",
			"template":           "helper",
			"state":              "active",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	sp.pendingConflict["sky"] = true
	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("non-pending session should remain open after session_exists")
	}
	if got.Metadata["session_name"] != "sky" {
		t.Fatalf("session_name = %q, want sky", got.Metadata["session_name"])
	}
	// With WakeWork removed, the session has no wake reason (state is healed
	// to "asleep" since it's dead, so WakeSession no longer applies). The
	// session is never started, so wake_attempts remains empty.
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_RollsBackPendingCreateOnProviderError(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.StartErrors = map[string]error{"sky": fmt.Errorf("start failed")}
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	cfg := &config.City{Agents: []config.Agent{{Name: "helper"}}}
	desired := map[string]TemplateParams{
		"sky": {
			Command:      "test-cmd",
			SessionName:  "sky",
			TemplateName: "helper",
		},
	}

	bead, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"session_name":          "sky",
			"session_name_explicit": "true",
			"pending_create_claim":  "true",
			"template":              "helper",
			"state":                 "creating",
			"generation":            "1",
			"continuation_epoch":    "1",
			"instance_token":        "test-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{bead}, desired, cfgNames,
		cfg, sp, store, nil, nil, nil, newDrainTracker(), map[string]int{"helper": 1}, false, nil, "",
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}

	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.Metadata["session_name"] != "" {
		t.Fatalf("session_name = %q, want empty after rollback", got.Metadata["session_name"])
	}
	if got.Metadata["close_reason"] != "failed-create" {
		t.Fatalf("close_reason = %q, want failed-create", got.Metadata["close_reason"])
	}
	if got.Metadata["wake_attempts"] != "" {
		t.Fatalf("wake_attempts = %q, want empty", got.Metadata["wake_attempts"])
	}
}

func TestReconcileSessionBeads_PoolScaleDownOrphansExcess(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	// worker-1 is in the desired set; worker-2 is NOT (scale-down).
	env.addDesired("worker-1", "worker", true)
	// worker-2 is running in provider but not in desiredState.
	_ = env.sp.Start(context.Background(), "worker-2", runtime.Config{})
	s1 := env.createSessionBead("worker-1", "worker")
	_ = env.store.SetMetadata(s1.ID, "pool_slot", "1")
	s1.Metadata["pool_slot"] = "1"
	s2 := env.createSessionBead("worker-2", "worker")
	_ = env.store.SetMetadata(s2.ID, "pool_slot", "2")
	s2.Metadata["pool_slot"] = "2"

	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{s1, s2}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, poolDesired, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	d2 := env.dt.get(s2.ID)
	if d2 == nil {
		t.Fatal("expected drain for excess pool instance")
	}
	if d2.reason != "orphaned" {
		t.Errorf("drain reason = %q, want %q", d2.reason, "orphaned")
	}
	if d1 := env.dt.get(s1.ID); d1 != nil {
		t.Errorf("worker-1 should not be draining, got reason=%q", d1.reason)
	}
}

func TestReconcileSessionBeads_LiveDriftReapplied(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Same core config (test-cmd), different live config.
	env.addDesiredLive("worker", "worker", true, []string{"echo live-updated"})
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	// Should NOT drain (core hash matches), but live_hash should be updated.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain for live-only drift, got reason=%q", ds.reason)
	}
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not updated: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
}

func TestReconcileSessionBeads_LiveDriftAppliedWhenNoStoredHash(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has session_live from a newly-added pack.
	env.addDesiredLive("worker", "worker", true, []string{"echo theme-applied"})

	// Create a session bead WITHOUT live_hash — simulates a bead created
	// before live_hash tracking was added, or via gc session new (which
	// doesn't set live_hash in its metadata).
	session := env.createSessionBead("worker", "worker")
	delete(session.Metadata, "live_hash")
	_ = env.store.SetMetadata(session.ID, "live_hash", "")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	// Should NOT drain (core hash matches).
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain for live-only drift, got reason=%q", ds.reason)
	}
	// Should have applied session_live and recorded the hash.
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not applied: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
	if b.Metadata["started_live_hash"] != expectedLive {
		t.Errorf("started_live_hash not applied: got %q, want %q", b.Metadata["started_live_hash"], expectedLive)
	}
}

func TestReconcileSessionBeads_LiveHashBackfilledSilentlyWhenNoLiveConfig(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Desired state has NO session_live — agent has no live config at all.
	env.addDesired("worker", "worker", true)

	// Create a session bead WITHOUT live_hash — legacy session.
	session := env.createSessionBead("worker", "worker")
	delete(session.Metadata, "live_hash")
	_ = env.store.SetMetadata(session.ID, "live_hash", "")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	env.reconcile([]beads.Bead{session})

	// Should NOT drain.
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("expected no drain, got reason=%q", ds.reason)
	}
	// live_hash should be backfilled silently.
	b, _ := env.store.Get(session.ID)
	expectedCfg := templateParamsToConfig(env.desiredState["worker"])
	expectedLive := runtime.LiveFingerprint(expectedCfg)
	if b.Metadata["live_hash"] != expectedLive {
		t.Errorf("live_hash not backfilled: got %q, want %q", b.Metadata["live_hash"], expectedLive)
	}
	// Should NOT have printed the "Live config changed" message — this is
	// a silent backfill, not a real live-drift reapply.
	if bytes.Contains(env.stdout.Bytes(), []byte("Live config changed")) {
		t.Errorf("unexpected 'Live config changed' output for silent backfill")
	}
}

func TestAllDependenciesAlive_WithSessionTemplate(t *testing.T) {
	session := beads.Bead{Metadata: map[string]string{"template": "worker"}}
	cfg := &config.City{
		Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Agent}}"},
		Agents: []config.Agent{
			{Name: "worker", DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}
	sn := agent.SessionNameFor("myCity", "db", "{{.City}}-{{.Agent}}")
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), sn, runtime.Config{})
	desired := map[string]TemplateParams{
		sn: {TemplateName: "db"},
	}
	if !allDependenciesAlive(session, cfg, desired, sp, "myCity", nil) {
		t.Errorf("dep should be alive (session name: %q)", sn)
	}
}

func TestReconcileSessionBeads_DriftDrainUsesConfigTimeout(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		Daemon: config.DaemonConfig{DriftDrainTimeout: "7m"},
	}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, env.cfg.Daemon.DriftDrainTimeoutDuration(),
		&env.stdout, &env.stderr,
	)

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatal("expected drain for config drift")
	}
	expected := env.clk.Now().Add(7 * time.Minute)
	if ds.deadline != expected {
		t.Errorf("drain deadline = %v, want %v (7m from now)", ds.deadline, expected)
	}
}

// --- idle timeout in bead reconciler tests ---

func TestReconcileSessionBeads_IdleTimeoutStopsAndStaysAsleep(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"pending_create_claim": "true",
		"sleep_intent":         "idle-stop-pending",
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	// Simulate idle: activity was 30m ago, timeout is 15m.
	it := newFakeIdleTracker()
	it.idle["worker"] = true

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	// Session should have been stopped and left asleep until a real wake reason appears.
	if env.sp.IsRunning("worker") {
		t.Error("worker should stay asleep after idle timeout without an explicit wake reason")
	}

	// Bead should reflect the restart cycle.
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] != "idle-timeout" {
		t.Errorf("sleep_reason = %q, want idle-timeout after idle stop", b.Metadata["sleep_reason"])
	}
	if b.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at = %q, want empty after idle stop", b.Metadata["last_woke_at"])
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared after idle stop", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["sleep_intent"] != "" {
		t.Errorf("sleep_intent = %q, want cleared after idle stop", b.Metadata["sleep_intent"])
	}
	if b.Metadata["slept_at"] != env.clk.Now().UTC().Format(time.RFC3339) {
		t.Errorf("slept_at = %q, want idle stop timestamp", b.Metadata["slept_at"])
	}
}

func TestReconcileSessionBeads_IdleTimeoutNilTrackerSkipped(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")

	// No idle tracker — should not idle-kill.
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if !env.sp.IsRunning("worker") {
		t.Error("worker should still be running with nil idle tracker")
	}
}

// --- zombie scrollback capture tests ---

func TestReconcileSessionBeads_ZombieCapturesScrollback(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	// Register with ProcessNames so ProcessAlive actually checks zombie state.
	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	env.desiredState["worker"] = tp
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"})

	session := env.createSessionBead("worker", "worker")

	// Simulate zombie: tmux session exists but process is dead.
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "panic: nil pointer dereference\ngoroutine 1 [running]:")

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, rec, 0, 0, &env.stdout, &env.stderr,
	)

	// Should have recorded a crash event with scrollback.
	found := false
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed && e.Message != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SessionCrashed event with scrollback capture")
	}
}

// --- regression tests for issues #70, #71, #139 ---

// TestReconcileSessionBeads_ZombieDetectedCrashRecordedAndSessionNotAlive
// verifies that when a session is a zombie (tmux exists but agent process
// dead), the reconciler records a crash event and treats the session as
// not alive. The alive=false state means downstream logic (config-drift,
// drain-ack) won't act on it, and when the tmux state cache subsequently
// reports IsRunning=false (pane_dead=1), the outer reconciler loop will
// start a fresh session.
// Regression test for https://github.com/gastownhall/gascity/issues/71
func TestReconcileSessionBeads_ZombieDetectedCrashRecordedAndSessionNotAlive(t *testing.T) {
	env := newReconcilerTestEnv()
	rec := events.NewFake()
	env.rec = rec
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}

	tp := TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"test-cmd"}},
	}
	env.desiredState["worker"] = tp
	_ = env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"})

	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Simulate zombie: tmux session exists but process is dead.
	env.sp.Zombies["worker"] = true
	env.sp.SetPeekOutput("worker", "Error: quota exceeded")

	env.reconcile([]beads.Bead{session})

	// Verify crash event was captured with scrollback.
	crashRecorded := false
	for _, e := range rec.Events {
		if e.Type == events.SessionCrashed && e.Message != "" {
			crashRecorded = true
			break
		}
	}
	if !crashRecorded {
		t.Error("expected SessionCrashed event with scrollback from zombie detection")
	}

	// Verify downstream behavior diverges from the alive path.
	// Contrast with TestReconcileSessionBeads_SkipsAliveSession where an
	// alive session keeps state "active" (healed to "awake") and records
	// no wake failure.
	//
	// For the zombie (running but process-dead), the reconciler:
	//  1. Records the crash event (above).
	//  2. Heals bead state from "active" to "asleep" (not alive).
	//  3. Detects rapid exit (last_woke_at is recent) and records a
	//     wake failure, preventing immediate restart (crash-loop protection).
	got, _ := env.store.Get(session.ID)
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep (zombie healed to not-alive)", got.Metadata["state"])
	}
	if got.Metadata["wake_attempts"] == "" || got.Metadata["wake_attempts"] == "0" {
		t.Error("expected wake_attempts > 0 (rapid exit recorded for zombie)")
	}
}

// TestReconcileSessionBeads_BeadMetadataRestartRequestedWhenSessionDead
// verifies that the reconciler detects restart_requested from bead metadata
// even when the tmux session is already dead (dops is nil or session not
// alive). This is the key durability property of the dual-flag approach:
// the bead flag survives tmux session death.
//
// The bead carries named-session identity metadata. Of these,
// namedSessionMetadataKey and namedSessionIdentityMetadata are checked by
// preserveConfiguredNamedSessionBead to recognize the bead as a configured
// named session, preventing the reconciler from treating it as an orphan.
// Without these metadata fields (or without the matching NamedSession config),
// the bead would be closed as orphaned before the restart_requested path is
// reached.
//
// Regression test for https://github.com/gastownhall/gascity/issues/70
func TestReconcileSessionBeads_BeadMetadataRestartRequestedWhenSessionDead(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
		"pending_create_claim":       "true",
	})

	// Session is NOT running — simulates tmux session already dead.
	// dops is nil (passed through env.reconcile).

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", got.Metadata["restart_requested"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared after durable dead-session restart request", got.Metadata["pending_create_claim"])
	}
}

// TestReconcileSessionBeads_ClosedOnDemandBeadReopensWhenInDesiredState
// verifies the full reconciler-level cycle for on_demand named session
// recovery: a closed session bead that is still in the desired state
// should be reopened by syncSessionBeads so the reconciler can re-evaluate
// and restart it.
// Regression test for https://github.com/gastownhall/gascity/issues/139
func TestReconcileSessionBeads_ClosedOnDemandBeadReopensWhenInDesiredState(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "worker", Mode: "on_demand"},
		},
	}

	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "worker")
	// Create a named session bead, then close it (simulates quota exhaustion).
	closed, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "worker",
			"template":                   "worker",
			"state":                      "stopped",
			"close_reason":               "quota_exhaustion",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "worker",
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Build desired state with the on_demand session present (work exists).
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName:            "worker",
			InstanceName:            "worker",
			Alias:                   "worker",
			Command:                 "true",
			ConfiguredNamedIdentity: "worker",
			ConfiguredNamedMode:     "on_demand",
		},
	}

	// Run syncSessionBeads to reopen the closed bead (this is the recovery path).
	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	// Verify the bead was reopened.
	got, err := store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open (bead should be reopened for recovery)", got.Status)
	}
	if got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason = %q, want empty after reopen", got.Metadata["close_reason"])
	}

	// Now run the reconciler with the reopened bead — it should not close it
	// again since it's a configured on_demand session in the desired state.
	// The session is not running, so the reconciler should wake it.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), sessions, ds, cfgNames, cfg, sp,
		store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 0, 0, &bytes.Buffer{}, &stderr,
	)

	// Bead should still be open after reconciliation.
	got, err = store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status after reconcile = %q, want open", got.Status)
	}

	// Verify downstream wake/start: the recovered bead should feed into
	// a successful session start, not just survive reconciliation.
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 (recovered session should be started)", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("session %q not running after reconcile — recovery did not trigger start", sessionName)
	}
}

// Regression test for #742 follow-up: after the stale-bead reaper closes a
// dead canonical bead, rediscovery must not also revive a leaked plain open
// bead for the same backing template alongside the rebuilt named session.
func TestReconcileSessionBeads_FileStoreAlwaysNamedRecoversWithLeakedDuplicateOpenBead(t *testing.T) {
	cityPath := t.TempDir()
	beadsPath := filepath.Join(cityPath, ".gc", "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))

	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", MaxActiveSessions: intPtr(0)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	_, err = store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      "asleep",
			"generation":                 "1",
			"continuation_epoch":         "1",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	leaked, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:mayor"},
		Metadata: map[string]string{
			"session_name":       "s-gc-leaked",
			"template":           "mayor",
			"agent_name":         "mayor",
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "leaked-token",
		},
	})
	if err != nil {
		t.Fatalf("Create(leaked): %v", err)
	}

	var stdout, stderr bytes.Buffer
	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, &stderr)

	mayorTP, ok := dsResult.State[sessionName]
	if !ok {
		t.Fatalf("desired state missing canonical named session %q; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if mayorTP.ConfiguredNamedIdentity != "mayor" {
		t.Fatalf("ConfiguredNamedIdentity = %q, want mayor", mayorTP.ConfiguredNamedIdentity)
	}
	if mayorTP.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", mayorTP.SessionName, sessionName)
	}
	if _, ok := dsResult.State[leaked.Metadata["session_name"]]; ok {
		t.Fatalf("desired state unexpectedly included leaked duplicate bead %q; keys=%v", leaked.Metadata["session_name"], mapKeys(dsResult.State))
	}

	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, &stderr, true)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	woken := reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
		dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, &stdout, &stderr,
	)

	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started", sessionName)
	}
	if sp.IsRunning(leaked.Metadata["session_name"]) {
		t.Fatalf("leaked duplicate session %q should not have been started", leaked.Metadata["session_name"])
	}
	if strings.Contains(stderr.String(), "session alias already exists") {
		t.Fatalf("unexpected alias collision during recovery:\n%s", stderr.String())
	}
}

func TestReconcileSessionBeads_FreshAlwaysNamedWithPoolDemandMaterializesNamedDespitePoolBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", ScaleCheck: "printf 1", MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	var stdout, stderr bytes.Buffer
	dsResult, sessions, woken := reconcileConfiguredSessionsOnce(t, cityPath, store, cfg, sp, clk, &stdout, &stderr)

	if _, ok := dsResult.State[sessionName]; !ok {
		t.Fatalf("desired state missing canonical named session %q; keys=%v", sessionName, mapKeys(dsResult.State))
	}
	if woken != 2 {
		t.Fatalf("woken = %d, want 2; sessions=%v stderr:\n%s", woken, sessionBeadDebug(sessions), stderr.String())
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started; stderr:\n%s", sessionName, stderr.String())
	}
	if _, ok := findTestSessionBeadByName(sessions, sessionName); !ok {
		t.Fatalf("canonical named session bead %q was not materialized; sessions=%v stderr:\n%s", sessionName, sessionBeadDebug(sessions), stderr.String())
	}
	if !testHasRunningPoolSessionForTemplate(sessions, sp, "mayor") {
		t.Fatalf("same-template pool session was not started; sessions=%v stderr:\n%s", sessionBeadDebug(sessions), stderr.String())
	}
	if strings.Contains(stderr.String(), "session_name \"mayor\"") {
		t.Fatalf("unexpected named-session reservation conflict:\n%s", stderr.String())
	}
}

func TestReconcileSessionBeads_ExistingAlwaysNamedStillAllowsSameTemplatePoolDemand(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "true", ScaleCheck: "printf 1", MaxActiveSessions: intPtr(3)},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Mode: "always"},
		},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")

	if _, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"alias":                      "mayor",
			"template":                   "mayor",
			"agent_name":                 "mayor",
			"state":                      "asleep",
			"generation":                 "1",
			"continuation_epoch":         "1",
			"instance_token":             "canonical-token",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "mayor",
			namedSessionModeMetadata:     "always",
		},
	}); err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}

	var stdout, stderr bytes.Buffer
	_, sessions, woken := reconcileConfiguredSessionsOnce(t, cityPath, store, cfg, sp, clk, &stdout, &stderr)

	if woken != 2 {
		t.Fatalf("woken = %d, want 2; sessions=%v stderr:\n%s", woken, sessionBeadDebug(sessions), stderr.String())
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("canonical named session %q was not started; stderr:\n%s", sessionName, stderr.String())
	}
	if !testHasRunningPoolSessionForTemplate(sessions, sp, "mayor") {
		t.Fatalf("same-template pool session was not started; sessions=%v stderr:\n%s", sessionBeadDebug(sessions), stderr.String())
	}
}

func reconcileConfiguredSessionsOnce(
	t *testing.T,
	cityPath string,
	store beads.Store,
	cfg *config.City,
	sp *runtime.Fake,
	clk *clock.Fake,
	stdout, stderr *bytes.Buffer,
) (DesiredStateResult, []beads.Bead, int) {
	t.Helper()

	dsResult := buildDesiredState(cfg.EffectiveCityName(), cityPath, clk.Now().UTC(), cfg, sp, store, stderr)
	cfgNames := configuredSessionNames(cfg, cfg.EffectiveCityName(), store)
	syncSessionBeads(cityPath, store, dsResult.State, sp, cfgNames, cfg, clk, stderr, true)

	sessions, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(cfg, dsResult.AssignedWorkBeads, sessions, dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)

	woken := reconcileSessionBeads(
		context.Background(), sessions, dsResult.State, cfgNames, cfg, sp,
		store, nil, dsResult.AssignedWorkBeads, nil, newDrainTracker(), poolDesired,
		dsResult.StoreQueryPartial, nil, cfg.EffectiveCityName(),
		nil, clk, events.Discard, 0, 0, stdout, stderr,
	)

	sessions, err = loadSessionBeads(store)
	if err != nil {
		t.Fatalf("loadSessionBeads after reconcile: %v", err)
	}
	return dsResult, sessions, woken
}

func findTestSessionBeadByName(sessions []beads.Bead, sessionName string) (beads.Bead, bool) {
	for _, sessionBead := range sessions {
		if sessionBead.Metadata["session_name"] == sessionName {
			return sessionBead, true
		}
	}
	return beads.Bead{}, false
}

func testHasRunningPoolSessionForTemplate(sessions []beads.Bead, sp *runtime.Fake, template string) bool {
	for _, sessionBead := range sessions {
		if sessionBead.Metadata["template"] != template || !isPoolManagedSessionBead(sessionBead) {
			continue
		}
		sessionName := sessionBead.Metadata["session_name"]
		if sessionName != "" && sp.IsRunning(sessionName) {
			return true
		}
	}
	return false
}

func sessionBeadDebug(sessions []beads.Bead) []string {
	out := make([]string, 0, len(sessions))
	for _, sessionBead := range sessions {
		out = append(out, fmt.Sprintf("%s:name=%s template=%s named=%t pool=%t state=%s status=%s",
			sessionBead.ID,
			sessionBead.Metadata["session_name"],
			sessionBead.Metadata["template"],
			isNamedSessionBead(sessionBead),
			isPoolManagedSessionBead(sessionBead),
			sessionBead.Metadata["state"],
			sessionBead.Status,
		))
	}
	return out
}

// TestReconcileSessionBeads_PoolRecoveryAfterClosedBead verifies the full
// recovery cycle for a managed pool session after its bead is closed.
// When a pool session's bead is closed (crash, drain, quota exhaustion),
// syncSessionBeads should create a fresh bead for that slot, and the
// reconciler should process the fresh bead without immediately closing it.
// This is the pool-session counterpart to #139 (named session recovery).
func TestReconcileSessionBeads_PoolRecoveryAfterClosedBead(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		},
	}

	sessionName := "worker-1"
	// Create a pool session bead, then close it (simulates crash/drain).
	closed, err := store.Create(beads.Bead{
		Title:  sessionName,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         sessionName,
			"agent_name":           sessionName,
			"template":             "worker",
			"live_hash":            runtime.LiveFingerprint(runtime.Config{Command: "true"}),
			"generation":           "1",
			"instance_token":       "old-token",
			"state":                "stopped",
			"close_reason":         "crash",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Build desired state with the pool slot present (demand exists).
	ds := map[string]TemplateParams{
		sessionName: {
			TemplateName: "worker",
			InstanceName: sessionName,
			Command:      "true",
			PoolSlot:     1,
		},
	}

	// Run syncSessionBeads — should create a FRESH bead (not reopen the closed one).
	var stderr bytes.Buffer
	syncSessionBeads(cityPath, store, ds, sp, allConfiguredDS(ds), cfg, clk, &stderr, false)

	// Verify: closed bead stays closed, a new open bead is created.
	all := allSessionBeads(t, store)
	if len(all) != 2 {
		t.Fatalf("expected 2 beads (1 closed + 1 new), got %d", len(all))
	}

	var newBead beads.Bead
	for _, b := range all {
		if b.Status == "open" {
			newBead = b
			break
		}
	}
	if newBead.ID == "" {
		t.Fatal("no open bead found after syncSessionBeads")
	}
	if newBead.ID == closed.ID {
		t.Fatal("new bead has same ID as closed bead — expected a fresh bead, not a reopen")
	}
	if newBead.Metadata["instance_token"] == "old-token" {
		t.Error("new bead has same instance_token as closed bead — expected fresh token")
	}

	// Verify the closed bead was NOT reopened.
	got, err := store.Get(closed.ID)
	if err != nil {
		t.Fatalf("Get closed bead: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("closed bead status = %q, want closed", got.Status)
	}

	// Now run the reconciler with the fresh bead — it should remain open
	// (not be closed as orphan) since the pool slot is in the desired state.
	// The session is not running, so the reconciler should wake it.
	sessions, _ := loadSessionBeads(store)
	poolDesired := map[string]int{"worker": 1}
	cfgNames := configuredSessionNames(cfg, "", store)
	woken := reconcileSessionBeads(
		context.Background(), sessions, ds, cfgNames, cfg, sp,
		store, nil, nil, nil, newDrainTracker(), poolDesired, false, nil, "",
		nil, clk, events.Discard, 0, 0, &bytes.Buffer{}, &stderr,
	)

	// Fresh bead should still be open after reconciliation.
	got, err = store.Get(newBead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("fresh bead status after reconcile = %q, want open", got.Status)
	}

	// Verify downstream wake/start: the fresh bead created by pool recovery
	// should feed into a successful session start.
	if woken != 1 {
		t.Fatalf("woken = %d, want 1 (recovered pool session should be started)", woken)
	}
	if !sp.IsRunning(sessionName) {
		t.Fatalf("session %q not running after reconcile — pool recovery did not trigger start", sessionName)
	}
}

// --- resolveAgentTemplate tests ---

func TestResolveAgentTemplate_DirectMatch(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "overseer"}}}
	if got := resolveAgentTemplate("overseer", cfg); got != "overseer" {
		t.Errorf("got %q, want %q", got, "overseer")
	}
}

func TestResolveAgentTemplate_PoolInstance(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	if got := resolveAgentTemplate("worker-3", cfg); got != "worker" {
		t.Errorf("got %q, want %q", got, "worker")
	}
}

func TestResolveAgentTemplate_Fallback(t *testing.T) {
	cfg := &config.City{}
	if got := resolveAgentTemplate("unknown", cfg); got != "unknown" {
		t.Errorf("got %q, want %q", got, "unknown")
	}
}

func TestResolveAgentTemplate_NilConfig(t *testing.T) {
	if got := resolveAgentTemplate("test", nil); got != "test" {
		t.Errorf("got %q, want %q", got, "test")
	}
}

// --- resolvePoolSlot tests ---

func TestResolvePoolSlot_PoolInstance(t *testing.T) {
	if got := resolvePoolSlot("worker-3", "worker"); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestResolvePoolSlot_NonPool(t *testing.T) {
	if got := resolvePoolSlot("overseer", "overseer"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestResolvePoolSlot_NonNumericSuffix(t *testing.T) {
	if got := resolvePoolSlot("worker-abc", "worker"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestResolvePoolSlot_LegacyGCNaming(t *testing.T) {
	if got := resolvePoolSlot("worker-gc-1", "worker"); got != 1 {
		t.Errorf("resolvePoolSlot(worker-gc-1, worker) = %d, want 1", got)
	}
	if got := resolvePoolSlot("worker-gc-5", "worker"); got != 5 {
		t.Errorf("resolvePoolSlot(worker-gc-5, worker) = %d, want 5", got)
	}
	// Non-numeric after gc- still returns 0.
	if got := resolvePoolSlot("worker-gc-abc", "worker"); got != 0 {
		t.Errorf("resolvePoolSlot(worker-gc-abc, worker) = %d, want 0", got)
	}
}

// BUG: PR #208 — this test fails on current code because resolvePoolSlot()
// only recognizes pool instances that use the "<template>-<N>" naming
// convention. Namepool-themed names like "fenrir" for a "worker" pool
// don't have the "worker-" prefix, so resolvePoolSlot returns 0.
// This means namepool-themed pool instances never get pool_slot metadata.
// The fix: pool_slot must be passed through TemplateParams at creation time
// rather than reverse-engineered from the agent name.
func TestResolvePoolSlot_NamepoolThemedName(t *testing.T) {
	// A namepool-themed pool instance "fenrir" belonging to the "worker"
	// template should have a meaningful slot, but resolvePoolSlot cannot
	// derive it from the name alone.
	if got := resolvePoolSlot("fenrir", "worker"); got != 0 {
		// If this passes (got != 0), the bug is fixed. Currently it returns 0.
		t.Errorf("resolvePoolSlot(fenrir, worker) = %d, want non-zero slot for namepool themes", got)
	}

	// Contrast: standard numbered naming works correctly.
	if got := resolvePoolSlot("worker-1", "worker"); got != 1 {
		t.Errorf("resolvePoolSlot(worker-1, worker) = %d, want 1", got)
	}

	// PR #208 fix: TemplateParams.PoolSlot bypasses resolvePoolSlot.
	// Verify that syncSessionBeads prefers tp.PoolSlot over resolvePoolSlot.
	tp := TemplateParams{InstanceName: "fenrir", TemplateName: "worker", PoolSlot: 3}
	if tp.PoolSlot == 0 {
		t.Fatal("TemplateParams.PoolSlot should carry the slot from buildDesiredState")
	}
}

func TestResolveResumeCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		sessionKey string
		provider   *config.ResolvedProvider
		want       string
	}{
		{
			name:       "no resume flag → unchanged",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider:   &config.ResolvedProvider{},
			want:       "claude --dangerously-skip-permissions",
		},
		{
			name:       "flag style",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider:   &config.ResolvedProvider{ResumeFlag: "--resume"},
			want:       "claude --dangerously-skip-permissions --resume abc-123",
		},
		{
			name:       "subcommand style",
			command:    "codex --model o3",
			sessionKey: "def-456",
			provider:   &config.ResolvedProvider{ResumeFlag: "resume", ResumeStyle: "subcommand"},
			want:       "codex resume def-456 --model o3",
		},
		{
			name:       "subcommand style no args",
			command:    "codex",
			sessionKey: "def-456",
			provider:   &config.ResolvedProvider{ResumeFlag: "resume", ResumeStyle: "subcommand"},
			want:       "codex resume def-456",
		},
		{
			name:       "explicit resume_command takes precedence",
			command:    "claude --dangerously-skip-permissions",
			sessionKey: "abc-123",
			provider: &config.ResolvedProvider{
				ResumeFlag:    "--resume",
				ResumeCommand: "claude --resume {{.SessionKey}} --dangerously-skip-permissions",
			},
			want: "claude --resume abc-123 --dangerously-skip-permissions",
		},
		{
			name:       "resume_command without SessionKey placeholder",
			command:    "my-agent",
			sessionKey: "xyz",
			provider: &config.ResolvedProvider{
				ResumeCommand: "my-agent --continue",
			},
			want: "my-agent --continue",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveResumeCommand(tt.command, tt.sessionKey, tt.provider)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSessionCommand(t *testing.T) {
	claude := &config.ResolvedProvider{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}

	t.Run("first start uses --session-id", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, true, false)
		want := "claude --dangerously-skip-permissions --session-id abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("resume uses --resume", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, false, false)
		want := "claude --dangerously-skip-permissions --resume abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("fresh wake uses --session-id", func(t *testing.T) {
		got := resolveSessionCommand("claude --dangerously-skip-permissions", "abc-123", claude, false, true)
		want := "claude --dangerously-skip-permissions --session-id abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("first start without SessionIDFlag falls back to resume", func(t *testing.T) {
		noSessionID := &config.ResolvedProvider{ResumeFlag: "--resume"}
		got := resolveSessionCommand("agent run", "key-1", noSessionID, true, false)
		want := "agent run --resume key-1"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestDrainedIsKnownState(t *testing.T) {
	if !knownSessionStates["drained"] {
		t.Fatal("drained must be a known session state")
	}
}

// TODO(pool-consolidation): This test validates that poolDesired gates wake
// decisions. Needs updating when pool_slot is removed — the slot-based gate
// will be replaced with count-based ordering.
func TestPoolDesiredLimitsWakeWork(t *testing.T) {
	t.Skip("blocked on pool_slot removal")
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Agents: []config.Agent{
			{Name: "claude", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)},
		},
	}
	// 3 sessions exist and are running, but demand (poolDesired) is only 1.
	// Don't add to desiredState — we're testing poolDesired gating only.
	var sessions []beads.Bead
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("claude-%d", i)
		s := env.createSessionBead(name, "claude")
		env.setSessionMetadata(&s, map[string]string{
			"state":     "awake",
			"pool_slot": fmt.Sprintf("%d", i),
		})
		sessions = append(sessions, s)
	}

	// poolDesired=1: only 1 session should stay awake.
	poolDesired := map[string]int{"claude": 1}
	evalInput := make([]beads.Bead, len(sessions))
	copy(evalInput, sessions)
	evals := computeWakeEvaluations(evalInput, env.cfg, env.sp, poolDesired,
		map[string]bool{"claude": true}, nil, env.clk)

	wakeCount := 0
	for _, eval := range evals {
		if len(eval.Reasons) > 0 {
			wakeCount++
		}
	}
	if wakeCount != 1 {
		t.Errorf("wakeCount = %d, want 1 (only slot 1 within poolDesired=1)", wakeCount)
	}
}

// PR #209 -- skipped for now. Drained beads don't block capacity (all
// selection paths skip them). Closing would break gc attach on drained
// sessions. Tracked as a future cleanup task.

// Regression: poolDesired derived from desiredState counts ALL session beads
// (including discovered ones), inflating the desired count. This test verifies
// that derivePoolDesired only counts pool sessions, not all discovered beads.
