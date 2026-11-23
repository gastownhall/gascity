package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// CONTRACT v5 §12.2 #3 ruling 4 (R16): a resume voids a suspended drain. A
// city suspend begins a "suspended" drain on a live named session and arms
// its ack in the same tick; once no operator suspend covers the row, legacy
// releases that drain and its ack instead of stopping the resumed session.
// Every test drives suspension through the production suspensionstate file
// and real legacy ticks with the production drain ops.

// suspendDrainFixture is a live, desired, always-on named session of template
// "worker" (in rig, when the fixture has one) in a city whose suspension the
// test toggles through suspensionstate.
type suspendDrainFixture struct {
	suspendDrainRow
	env   *reconcilerTestEnv
	rig   string
	dops  drainOps
	extra []suspendDrainRow
	// work is the work beads every tick hands the reconciler.
	work []beads.Bead
}

// suspendDrainRow is one live named session the fixture ticks.
type suspendDrainRow struct {
	id       string
	name     string
	template string
}

func newSuspendDrainFixture(t *testing.T) *suspendDrainFixture {
	t.Helper()
	return newSuspendDrainFixtureInRig(t, "")
}

// newSuspendDrainFixtureInRig is newSuspendDrainFixture for an agent scoped
// to rig, or city-scoped when rig is empty.
func newSuspendDrainFixtureInRig(t *testing.T, rig string) *suspendDrainFixture {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if rig != "" {
		rigPath := filepath.Join(env.city, rig)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		env.cfg.Rigs = []config.Rig{{Name: rig, Path: rigPath}}
	}
	f := &suspendDrainFixture{env: env, rig: rig, dops: newDrainOps(env.sp)}
	f.suspendDrainRow = f.startNamedSession(t, "worker")
	return f
}

// addRow adds a second live, desired, always-on named session of template
// agentName (in the fixture's rig) that every tick reconciles too.
func (f *suspendDrainFixture) addRow(t *testing.T, agentName string) suspendDrainRow {
	t.Helper()
	row := f.startNamedSession(t, agentName)
	f.extra = append(f.extra, row)
	return row
}

func (f *suspendDrainFixture) startNamedSession(t *testing.T, agentName string) suspendDrainRow {
	t.Helper()
	const mode = "always"
	env := f.env
	agent := config.Agent{Name: agentName, StartCommand: "test-cmd", Dir: f.rig}
	env.cfg.Agents = append(env.cfg.Agents, agent)
	env.cfg.NamedSessions = append(env.cfg.NamedSessions, config.NamedSession{Template: agentName, Mode: mode, Dir: f.rig})
	template := agent.QualifiedName()
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, template)
	env.desiredState[name] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             name,
		TemplateName:            template,
		ConfiguredNamedIdentity: template,
		ConfiguredNamedMode:     mode,
	}
	b := env.createSessionBead(name, template)
	env.setSessionMetadata(&b, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: template,
		namedSessionModeMetadata:     mode,
	})
	env.markSessionActive(&b)
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	return suspendDrainRow{id: b.ID, name: name, template: template}
}

// row returns a view of the fixture whose assertions target r.
func (f *suspendDrainFixture) row(r suspendDrainRow) *suspendDrainFixture {
	view := *f
	view.suspendDrainRow = r
	return &view
}

func (f *suspendDrainFixture) setCitySuspended(t *testing.T, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, f.env.city, &suspended); err != nil {
		t.Fatalf("SetCitySuspended(%v): %v", suspended, err)
	}
}

// tick runs one legacy reconciler tick over every fixture row as stored.
func (f *suspendDrainFixture) tick(t *testing.T) {
	t.Helper()
	f.reconcile(t, f.env.sp, false)
	waitAsyncStopsForTest(t, f.env.stops)
}

// reconcile is tick through sp, with storeQueryPartial and extra
// start-execution options, and without waiting for queued drain-ack stops.
func (f *suspendDrainFixture) reconcile(t *testing.T, sp runtime.Provider, storeQueryPartial bool, opts ...startExecutionOption) {
	t.Helper()
	var rows []beads.Bead
	poolDesired := make(map[string]int)
	for _, r := range append([]suspendDrainRow{f.suspendDrainRow}, f.extra...) {
		got, err := f.env.store.Get(r.id)
		if err != nil {
			t.Fatalf("Get(%s): %v", r.id, err)
		}
		rows = append(rows, got)
		poolDesired[r.template]++
	}
	reconcileSessionBeadsAtPath(
		context.Background(), f.env.city, rows, f.env.desiredState,
		configuredSessionNames(f.env.cfg, "", f.env.store), f.env.cfg, sp, f.env.store,
		f.dops, f.work, nil, nil, f.env.dt, poolDesired, storeQueryPartial, nil, "",
		nil, f.env.clk, f.env.rec, 0, 0, &f.env.stdout, &f.env.stderr,
		append(append([]startExecutionOption{}, f.env.startOptions...), opts...)...,
	)
}

// armSuspendDrain runs the city-suspend tick and asserts its premise: a
// tracked "suspended" drain whose reconciler-owned ack is published on the
// live runtime.
func (f *suspendDrainFixture) armSuspendDrain(t *testing.T) {
	t.Helper()
	f.setCitySuspended(t, true)
	f.tick(t)
	f.assertArmed(t)
	for _, r := range f.extra {
		f.row(r).assertArmed(t)
	}
}

// assertArmed asserts the suspend tick's premise for the fixture's row.
func (f *suspendDrainFixture) assertArmed(t *testing.T) {
	t.Helper()
	ds := f.env.dt.get(f.id)
	if ds == nil || ds.reason != drainSuspended || !ds.ackSet {
		t.Fatalf("suspend tick drain = %+v, want a tracked %q drain with its ack set (test premise)\nstdout:\n%s\nstderr:\n%s",
			ds, drainSuspended, f.env.stdout.String(), f.env.stderr.String())
	}
	if ack, _ := f.env.sp.GetMeta(f.name, "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK on %s = %q after the suspend tick, want 1 (test premise)", f.name, ack)
	}
	if !f.env.sp.IsRunning(f.name) {
		t.Fatalf("the suspend tick stopped %s (test premise)", f.name)
	}
}

// assertNotStopped fails if the runtime is down, was stopped since
// stopsBefore, or its row was marked for a drain-ack stop.
func (f *suspendDrainFixture) assertNotStopped(t *testing.T, stopsBefore int) {
	t.Helper()
	if !f.env.sp.IsRunning(f.name) || f.env.sp.CountCalls("Stop", f.name) != stopsBefore {
		t.Fatalf("the tick stopped the session; stdout:\n%s\nstderr:\n%s", f.env.stdout.String(), f.env.stderr.String())
	}
	if reason := f.env.sessionInfo(f.id).StateReason; reason == sessionpkg.DrainAckStopPendingReason {
		t.Fatalf("row state_reason = %q: the tick honored the suspended drain's ack\nstdout:\n%s", reason, f.env.stdout.String())
	}
	if ds := f.env.dt.get(f.id); ds != nil && ds.reason == drainSuspended {
		t.Fatalf("suspended drain still tracked: %+v", *ds)
	}
}

// assertReleased is assertNotStopped plus: the runtime carries no reconciler
// drain ack and the tracker holds no drain for the session.
func (f *suspendDrainFixture) assertReleased(t *testing.T, stopsBefore int) {
	t.Helper()
	f.assertNotStopped(t, stopsBefore)
	for _, key := range []string{"GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
		if v, _ := f.env.sp.GetMeta(f.name, key); v != "" {
			t.Fatalf("runtime metadata %s = %q, want cleared", key, v)
		}
	}
	if ds := f.env.dt.get(f.id); ds != nil {
		t.Fatalf("drain tracked: %+v", *ds)
	}
}

// assertHonored fails unless the tick honored the suspended drain's ack: the
// row was marked for the drain-ack stop or the runtime was stopped.
func (f *suspendDrainFixture) assertHonored(t *testing.T, stopsBefore int) {
	t.Helper()
	if f.env.sessionInfo(f.id).StateReason != sessionpkg.DrainAckStopPendingReason && f.env.sp.CountCalls("Stop", f.name) == stopsBefore {
		t.Fatalf("the suspended drain's ack was not honored: state_reason=%q drain=%+v\nstdout:\n%s\nstderr:\n%s",
			f.env.sessionInfo(f.id).StateReason, f.env.dt.get(f.id), f.env.stdout.String(), f.env.stderr.String())
	}
}

// AC-1: a resume with the runtime alive voids the armed suspended drain.
func TestReconcileSessionBeads_ResumeVoidsArmedSuspendDrainOnLiveNamedSession(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertReleased(t, stops)
}

// AC-2 (guard): while the city stays suspended, open assigned work does not
// cancel the armed suspended drain; its ack is honored.
func TestReconcileSessionBeads_SuspendDrainAckStillHonoredWhileCitySuspended(t *testing.T) {
	f := newSuspendDrainFixture(t)
	if _, err := f.env.store.Create(beads.Bead{Title: "assigned work", Type: "task", Status: "open", Assignee: f.id}); err != nil {
		t.Fatalf("Create(assigned work): %v", err)
	}
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)

	f.tick(t)

	f.assertHonored(t, stops)
}

// AC-3 (guard): a resumed city whose narrower operator suspend still covers
// the row keeps the suspended drain and honors its ack.
func TestReconcileSessionBeads_ResumeKeepsSuspendDrainUnderNarrowerSuspend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rig     string
		suspend func(t *testing.T, f *suspendDrainFixture)
	}{
		{name: "agent", suspend: func(_ *testing.T, f *suspendDrainFixture) {
			f.env.cfg.Agents[0].Suspended = true
		}},
		{name: "rig", rig: "r1", suspend: func(t *testing.T, f *suspendDrainFixture) {
			suspended := true
			if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, f.env.city, f.rig, &suspended); err != nil {
				t.Fatalf("SetRigSuspended: %v", err)
			}
		}},
		// The hold lands after the drain began, which moves the row off the
		// tracked drain's basis and voids it on its own; the controller that
		// restarted since has no basis, so only the release decides the ack.
		{name: "session hold", suspend: func(t *testing.T, f *suspendDrainFixture) {
			if err := f.env.store.SetMetadata(f.id, "sleep_intent", "user-hold"); err != nil {
				t.Fatalf("SetMetadata(sleep_intent): %v", err)
			}
			f.env.dt = newDrainTracker()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuspendDrainFixtureInRig(t, tc.rig)
			f.armSuspendDrain(t)
			stops := f.env.sp.CountCalls("Stop", f.name)
			tc.suspend(t, f)

			f.setCitySuspended(t, false)
			f.tick(t)

			f.assertHonored(t, stops)
		})
	}
}

// AC-3 (guard): GC_SUSPENDED=1 keeps the city effectively suspended after the
// suspensionstate resume, so the armed suspended drain's ack is honored.
func TestReconcileSessionBeads_GcSuspendedEnvKeepsSuspendDrainHonored(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)
	t.Setenv("GC_SUSPENDED", "1")

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertHonored(t, stops)
}

// AC-3 (guard): the release is per row. After a city resume, a row no
// operator suspend covers is released while its sibling under a session hold
// keeps the suspended drain and has its ack honored. The tracker restarts so
// the hold's basis move cannot void the held row's drain: only the release
// decides each ack.
func TestReconcileSessionBeads_ResumeReleasesOnlyRowsNoSuspendCovers(t *testing.T) {
	f := newSuspendDrainFixture(t)
	held := f.addRow(t, "helper")
	f.armSuspendDrain(t)
	freeStops := f.env.sp.CountCalls("Stop", f.name)
	heldStops := f.env.sp.CountCalls("Stop", held.name)
	if err := f.env.store.SetMetadata(held.id, "sleep_intent", "user-hold"); err != nil {
		t.Fatalf("SetMetadata(sleep_intent): %v", err)
	}
	f.env.dt = newDrainTracker()

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertReleased(t, freeStops)
	f.row(held).assertHonored(t, heldStops)
}

// AC-4: an armed suspended ack the restarted controller no longer tracks is
// cleared once no operator suspend covers the row.
func TestReconcileSessionBeads_ResumeClearsUntrackedSuspendAckAfterControllerRestart(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)
	f.env.dt = newDrainTracker()

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertReleased(t, stops)
}

// AC-4 (edge): a refused ack clear is logged, leaves the runtime up with the
// ack armed, and the next tick's release retries the clear without a stop.
func TestReconcileSessionBeads_ResumeReleaseSurvivesFailedAckClear(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)
	// Every key the clear removes is refused, so no partial clear can strand
	// the ack as a bare legacy flag.
	ackKeys := []string{
		"GC_DRAIN_ACK",
		reconcilerDrainAckSourceKey,
		drainAckRequesterInstanceTokenKey,
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
	}
	f.env.sp.RemoveMetaErrors[f.name] = map[string]error{}
	for _, key := range ackKeys {
		f.env.sp.RemoveMetaErrors[f.name][key] = errors.New("remove denied")
	}

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertNotStopped(t, stops)
	if want := "clearing released " + drainSuspended + " drain ack for " + f.name; !strings.Contains(f.env.stderr.String(), want) {
		t.Fatalf("stderr lacks %q: the failed clear must be logged\nstderr:\n%s", want, f.env.stderr.String())
	}
	if ack, err := f.env.sp.GetMeta(f.name, "GC_DRAIN_ACK"); err != nil || ack != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, err %v after the refused clear; want it armed for the retry", ack, err)
	}

	for _, key := range ackKeys {
		delete(f.env.sp.RemoveMetaErrors[f.name], key)
	}
	f.env.clk.Time = f.env.clk.Time.Add(time.Minute)
	f.tick(t)

	f.assertReleased(t, stops)
}

// AC-4 (edge): the release reads the ack metadata and suspension state, not
// the bead store, so a partial store query on the resume tick does not defer
// it.
func TestReconcileSessionBeads_ResumeReleaseRunsUnderPartialStoreQuery(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)

	f.setCitySuspended(t, false)
	f.reconcile(t, f.env.sp, true)
	waitAsyncStopsForTest(t, f.env.stops)

	f.assertReleased(t, stops)
}

// AC-5: a tracked suspended drain whose runtime is gone and whose ack is
// unreadable (a destroyed runtime) does not survive to the drain scan of the
// resume tick, so the scan cannot complete it over the replacement's creating
// row while the async start is still in flight.
func TestReconcileSessionBeads_ResumeReplacementSurvivesSuspendDrainOnDestroyedRuntime(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	if err := f.env.sp.Stop(f.name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	f.env.sp.GetMetaErrors[f.name] = map[string]error{"GC_DRAIN_ACK": runtime.ErrSessionNotFound}
	gated := &gatedStartProvider{Fake: f.env.sp, startSignals: make(chan string, 32), releaseByName: make(map[string]chan struct{})}
	starts := &asyncStartTracker{}
	t.Cleanup(func() {
		gated.release(f.name)
		starts.wait(-1)
	})

	f.setCitySuspended(t, false)
	f.reconcile(t, gated, false, withAsyncStartExecution(), withAsyncStartTracker(starts))

	select {
	case <-gated.startSignals:
	case <-time.After(10 * time.Second):
		t.Fatalf("the resume tick planned no replacement start; stdout:\n%s\nstderr:\n%s", f.env.stdout.String(), f.env.stderr.String())
	}
	if ds := f.env.dt.get(f.id); ds != nil {
		t.Fatalf("drain still tracked after the resume tick: %+v", *ds)
	}
	// The re-woken row carries no pending_create_claim (only a newly created
	// row does); what must hold is that it stays creating, not slept by a
	// drain completion, until its own commit.
	info := f.env.sessionInfo(f.id)
	if info.MetadataState != string(sessionpkg.StateCreating) || info.SleepReason != "" || info.SleptAt != "" {
		t.Fatalf("replacement row state=%q sleep_reason=%q slept_at=%q, want creating until its own commit\nstdout:\n%s\nstderr:\n%s",
			info.MetadataState, info.SleepReason, info.SleptAt, f.env.stdout.String(), f.env.stderr.String())
	}

	gated.release(f.name)
	if !starts.wait(10 * time.Second) {
		t.Fatal("the replacement start did not finish")
	}
	info = f.env.sessionInfo(f.id)
	if !f.env.sp.IsRunning(f.name) || info.MetadataState != string(sessionpkg.StateActive) {
		t.Fatalf("replacement running=%v state=%q sleep_reason=%q, want it committed and running\nstderr:\n%s",
			f.env.sp.IsRunning(f.name), info.MetadataState, info.SleepReason, f.env.stderr.String())
	}
}

// AC-5: with open assigned work and no ack on the runtime (its write never
// landed), only the tracked drain remains, so the armed-ack release cannot
// fire; the row's forward pass still voids the drain on the resume tick, and
// no later tick resurrects it.
func TestReconcileSessionBeads_ResumeVoidsSuspendDrainUnderAssignedWorkDemand(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	if err := clearReconcilerDrainAckMetadata(f.env.sp, f.name); err != nil {
		t.Fatalf("clearReconcilerDrainAckMetadata: %v", err)
	}
	task, err := f.env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status, assignee := "in_progress", f.template
	if err := f.env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	if task, err = f.env.store.Get(task.ID); err != nil {
		t.Fatalf("Get(task): %v", err)
	}
	f.work = []beads.Bead{task}
	stops := f.env.sp.CountCalls("Stop", f.name)

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertReleased(t, stops)
	f.env.clk.Time = f.env.clk.Time.Add(time.Minute)
	f.tick(t)
	f.assertReleased(t, stops)
}

// AC-5 (guard): a live row whose armed ack cannot be read is out of the
// armed-ack release's reach; its forward pass voids the tracked drain and
// clears the ack, so no later tick re-arms it.
func TestReconcileSessionBeads_ResumeVoidsTrackedSuspendDrainWhenAckReadFails(t *testing.T) {
	f := newSuspendDrainFixture(t)
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)
	f.env.sp.GetMetaErrors[f.name] = map[string]error{"GC_DRAIN_ACK": errors.New("meta read failed")}

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertNotStopped(t, stops)
	if ds := f.env.dt.get(f.id); ds != nil {
		t.Fatalf("drain tracked after the resume tick: %+v", *ds)
	}
	delete(f.env.sp.GetMetaErrors[f.name], "GC_DRAIN_ACK")
	f.env.clk.Time = f.env.clk.Time.Add(time.Minute)
	f.tick(t)
	f.assertReleased(t, stops)
}
