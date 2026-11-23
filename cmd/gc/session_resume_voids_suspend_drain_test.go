package main

import (
	"context"
	"testing"

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

// suspendDrainFixture is a live, desired named session of template "worker"
// in a city whose suspension the test toggles through suspensionstate.
type suspendDrainFixture struct {
	env  *reconcilerTestEnv
	id   string
	name string
	dops drainOps
}

func newSuspendDrainFixture(t *testing.T, mode string) *suspendDrainFixture {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: mode}},
	}
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[name] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             name,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     mode,
	}
	b := env.createSessionBead(name, "worker")
	env.setSessionMetadata(&b, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     mode,
	})
	env.markSessionActive(&b)
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return &suspendDrainFixture{env: env, id: b.ID, name: name, dops: newDrainOps(env.sp)}
}

func (f *suspendDrainFixture) setCitySuspended(t *testing.T, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, f.env.city, &suspended); err != nil {
		t.Fatalf("SetCitySuspended(%v): %v", suspended, err)
	}
}

// tick runs one legacy reconciler tick over the session's current row.
func (f *suspendDrainFixture) tick(t *testing.T) {
	t.Helper()
	got, err := f.env.store.Get(f.id)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.id, err)
	}
	f.env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"worker": 1}, f.dops)
	waitAsyncStopsForTest(t, f.env.stops)
}

// armSuspendDrain runs the city-suspend tick and asserts its premise: a
// tracked "suspended" drain whose reconciler-owned ack is published on the
// live runtime.
func (f *suspendDrainFixture) armSuspendDrain(t *testing.T) {
	t.Helper()
	f.setCitySuspended(t, true)
	f.tick(t)
	ds := f.env.dt.get(f.id)
	if ds == nil || ds.reason != drainSuspended || !ds.ackSet {
		t.Fatalf("suspend tick drain = %+v, want a tracked %q drain with its ack set (test premise)\nstdout:\n%s\nstderr:\n%s",
			ds, drainSuspended, f.env.stdout.String(), f.env.stderr.String())
	}
	if ack, _ := f.env.sp.GetMeta(f.name, "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q after the suspend tick, want 1 (test premise)", ack)
	}
	if !f.env.sp.IsRunning(f.name) {
		t.Fatal("the suspend tick stopped the runtime (test premise)")
	}
}

// assertReleased fails unless the resumed runtime is up, was not stopped
// since stopsBefore, and carries no reconciler drain ack or tracked drain.
func (f *suspendDrainFixture) assertReleased(t *testing.T, stopsBefore int) {
	t.Helper()
	if !f.env.sp.IsRunning(f.name) || f.env.sp.CountCalls("Stop", f.name) != stopsBefore {
		t.Fatalf("the resume tick stopped the session; stdout:\n%s\nstderr:\n%s", f.env.stdout.String(), f.env.stderr.String())
	}
	if reason := f.env.sessionInfo(f.id).StateReason; reason == sessionpkg.DrainAckStopPendingReason {
		t.Fatalf("row state_reason = %q after resume: the tick honored the suspended drain's ack", reason)
	}
	if ds := f.env.dt.get(f.id); ds != nil && ds.reason == drainSuspended {
		t.Fatalf("suspended drain still tracked after resume: %+v", *ds)
	}
	for _, key := range []string{"GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
		if v, _ := f.env.sp.GetMeta(f.name, key); v != "" {
			t.Fatalf("runtime metadata %s = %q after resume, want cleared", key, v)
		}
	}
}

// AC-1: a resume with the runtime alive voids the armed suspended drain.
func TestReconcileSessionBeads_ResumeVoidsArmedSuspendDrainOnLiveNamedSession(t *testing.T) {
	f := newSuspendDrainFixture(t, "always")
	f.armSuspendDrain(t)
	stops := f.env.sp.CountCalls("Stop", f.name)

	f.setCitySuspended(t, false)
	f.tick(t)

	f.assertReleased(t, stops)
	if ds := f.env.dt.get(f.id); ds != nil {
		t.Fatalf("drain tracked after resume: %+v", *ds)
	}
}
