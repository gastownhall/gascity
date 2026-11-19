package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// TestLegacySuspendedRigSessionIsNotAStartCandidate is AC-8 of ga-z4joq0: a
// configured named session whose rig is effectively suspended, by its
// suspended_on_start default or by a runtime override, while its row holds a
// pending-create claim, is not a start candidate under free budget: no
// PreWake, no wake slot, no provider start and no failure accounting. Its
// claim is kept, so the first pass after the rig resumes starts it.
func TestLegacySuspendedRigSessionIsNotAStartCandidate(t *testing.T) {
	const rig, identity = "frontend", "frontend/helper"
	saveRig := func(t *testing.T, city string, suspended bool) {
		t.Helper()
		st := suspensionstate.State{Rigs: map[string]suspensionstate.Override{rig: {Suspended: &suspended}}}
		if err := suspensionstate.Save(fsys.OSFS{}, city, st); err != nil {
			t.Fatalf("saving suspension state: %v", err)
		}
	}
	for _, c := range []struct {
		name    string
		suspend func(t *testing.T, env *reconcilerTestEnv)
		resume  func(t *testing.T, env *reconcilerTestEnv)
	}{
		{
			name:    "suspended_on_start",
			suspend: func(_ *testing.T, env *reconcilerTestEnv) { env.cfg.Rigs[0].SuspendedOnStart = true },
			resume:  func(_ *testing.T, env *reconcilerTestEnv) { env.cfg.Rigs[0].SuspendedOnStart = false },
		},
		{
			name:    "runtime override",
			suspend: func(t *testing.T, env *reconcilerTestEnv) { saveRig(t, env.city, true) },
			resume:  func(t *testing.T, env *reconcilerTestEnv) { saveRig(t, env.city, false) },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{
				Workspace:     config.Workspace{Name: "test-city"},
				Rigs:          []config.Rig{{Name: rig, Path: t.TempDir()}},
				Agents:        []config.Agent{{Name: "helper", Dir: rig, StartCommand: "true"}},
				NamedSessions: []config.NamedSession{{Template: "helper", Dir: rig, Mode: "always"}},
			}
			sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, identity)
			env.desiredState[sessionName] = TemplateParams{Command: "test-cmd", SessionName: sessionName, TemplateName: identity, RigName: rig}
			row := env.createSessionBead(sessionName, identity)
			env.setSessionMetadata(&row, map[string]string{
				"session_name_explicit":      "true",
				"pending_create_claim":       "true",
				"state":                      "start-pending",
				"generation":                 "1",
				"continuation_epoch":         "1",
				"instance_token":             "tok-1",
				namedSessionMetadataKey:      "true",
				namedSessionIdentityMetadata: identity,
				namedSessionModeMetadata:     "always",
			})
			c.suspend(t, env)

			if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{row}, nil, nil); woken != 0 {
				t.Fatalf("woken = %d on a suspended rig, want 0", woken)
			}
			if n := env.sp.CountCalls("Start", sessionName); n != 0 {
				t.Fatalf("provider Start calls = %d on a suspended rig, want none", n)
			}
			got, err := env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"state": "start-pending", "generation": "1", "instance_token": "tok-1", "last_woke_at": "",
				"pending_create_claim": "true", "wake_attempts": "", lastStartFailedAt: "",
			} {
				if got.Metadata[key] != want {
					t.Errorf("%s = %q on a suspended rig, want %q (no PreWake, no failure accounting, claim kept)", key, got.Metadata[key], want)
				}
			}
			if n := startupHealthCount(t, env.store, sessionName); n != 0 {
				t.Errorf("startup-health failures = %d on a suspended rig, want 0", n)
			}

			c.resume(t, env)
			env.clk.Advance(time.Second)
			row, err = env.store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{row}, nil, nil); woken != 1 {
				t.Fatalf("woken = %d after the rig resumed, want 1; stderr=%s", woken, env.stderr.String())
			}
			if !env.sp.IsRunning(sessionName) {
				t.Fatalf("%s not running after the rig resumed", sessionName)
			}
		})
	}
}

// TestAllocatorSuspendedRigPendingCreateIsNotWoken pins AC-8's v2 half of
// ga-z4joq0, which needs no change: the allocator does not wake a configured
// named session's pending-create row while its rig is suspended.
func TestAllocatorSuspendedRigPendingCreateIsNotWoken(t *testing.T) {
	const identity = "rig-a/worker"
	cfg := &config.City{
		Rigs:          []config.Rig{{Name: "rig-a", Path: "/rigs/rig-a"}},
		Agents:        []config.Agent{{Name: "worker", Scope: "rig"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always", Scope: "rig", Dir: "rig-a"}},
	}
	row := sessionRow("gc-1",
		"template", identity, "state", "start-pending", "generation", "1", "pending_create_claim", "true", "pending_create_started_at", ago(time.Second),
		"session_name", config.NamedSessionRuntimeName("city", cfg.Workspace, identity),
		"configured_named_session", "true", "configured_named_identity", identity, "configured_named_mode", "always")
	if e := entryOf(t, newAllocFixture(t, cfg).sessions(row).decide(), "gc-1"); e.Desired != desireWake {
		t.Fatalf("premise: pending create on a live rig = %s/%s, want a wake", e.Desired, e.Reason)
	}
	f := newAllocFixture(t, cfg).sessions(row)
	f.in.SuspendedRigPaths = map[string]bool{"/rigs/rig-a": true}
	if e := entryOf(t, f.decide(), "gc-1"); e.Desired == desireWake {
		t.Fatalf("pending create on a suspended rig = %s/%s, want no wake", e.Desired, e.Reason)
	}
}
