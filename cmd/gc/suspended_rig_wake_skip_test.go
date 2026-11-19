package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// TestWakeArmSkipsSuspendedRigsSessions (#7680, change 5): an always-mode
// named session whose rig is suspended must not take a wake arm at all — no
// start, no PreWake writes, no wake-attempt accounting, no failed-start
// stamp — so the rig's idle always-mode patrol restarts cannot spend start
// slots routed pool work needs. Resuming the rig releases the hold: the
// next pass starts the session from the same row.
func TestWakeArmSkipsSuspendedRigsSessions(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig1", Path: t.TempDir()}},
		Agents: []config.Agent{{
			Name:         "helper",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "helper")
	session := env.createSessionBead(sessionName, "helper")
	env.setSessionMetadata(&session, map[string]string{
		"session_name_explicit":      "true",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "helper",
		namedSessionModeMetadata:     "always",
	})
	env.desiredState[sessionName] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  sessionName,
		TemplateName: "helper",
		RigName:      "rig1",
	}

	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, env.city, "rig1", &suspended); err != nil {
		t.Fatalf("suspending rig1: %v", err)
	}

	if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, nil, nil); woken != 0 {
		t.Fatalf("woken = %d, want 0 (the session's rig is suspended)", woken)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatal("the session started although its rig is suspended")
	}
	held, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep (a held row takes no PreWake writes)", held.Metadata["state"])
	}
	if got := held.Metadata["last_woke_at"]; got != "" {
		t.Fatalf("last_woke_at = %q, want empty (a held row takes no PreWake writes)", got)
	}
	if got, ok := held.Metadata["wake_attempts"]; ok && got != "" {
		t.Fatalf("wake_attempts = %q, want none (a held row spends no wake accounting)", got)
	}
	if got, ok := held.Metadata["last_start_failed_at"]; ok && got != "" {
		t.Fatalf("last_start_failed_at = %q, want none (a held row is not a failed start)", got)
	}

	// The hold is the rig's, not the row's: resuming the rig releases it and
	// the next pass starts the same row.
	resumed := false
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, env.city, "rig1", &resumed); err != nil {
		t.Fatalf("resuming rig1: %v", err)
	}
	env.clk.Advance(time.Second)
	if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{held}, nil, nil); woken != 1 {
		t.Fatalf("after the resume woken = %d, want 1 (the hold is gone; the row starts)\nstderr:\n%s", woken, env.stderr.String())
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("the session did not start after its rig resumed")
	}
}
