package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// killFencedSessionBead creates a worker session carrying a kill fence stamped
// at sleptAt, with conversation state a crash heal would wipe.
func killFencedSessionBead(t *testing.T, env *reconcilerTestEnv, sleptAt time.Time) beads.Bead {
	t.Helper()
	session := env.createSessionBead("worker", "worker")
	patch := sessionpkg.KillPendingPatch(sleptAt)
	patch["session_key"] = "conversation-1"
	patch["started_config_hash"] = "config-1"
	env.setSessionMetadata(&session, patch)
	return session
}

func TestReconcileSessionBeads_KillFencedRowIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired bool
		running bool
	}{
		// Stop still in flight: an asleep row next to a live runtime must not
		// be healed back to awake (or drained a second time).
		{name: "runtime alive, desired", desired: true, running: true},
		{name: "runtime alive, undesired", desired: false, running: true},
		// Stop landed, fence not yet lifted: neither restart nor close.
		{name: "runtime gone, desired", desired: true, running: false},
		{name: "runtime gone, undesired", desired: false, running: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			if tc.desired {
				env.addDesired("worker", "worker", tc.running)
			} else if tc.running {
				if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
					t.Fatalf("Start: %v", err)
				}
			}
			session := killFencedSessionBead(t, env, env.clk.Now())
			before, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			callsBefore := len(env.sp.SnapshotCalls())

			env.reconcile([]beads.Bead{before})

			after, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if after.Status != before.Status {
				t.Fatalf("status = %q, want %q (a fenced row must not be closed); stdout=%s stderr=%s", after.Status, before.Status, env.stdout.String(), env.stderr.String())
			}
			for key, want := range before.Metadata {
				if got := after.Metadata[key]; got != want {
					t.Errorf("metadata %s = %q, want unchanged %q", key, got, want)
				}
			}
			for _, call := range env.sp.SnapshotCalls()[callsBefore:] {
				switch call.Method {
				case "Start", "Stop", "Nudge", "Interrupt", "Relaunch":
					t.Errorf("fenced row reached runtime effect %s(%s)", call.Method, call.Name)
				}
			}
			if got := env.sp.IsRunning("worker"); got != tc.running {
				t.Errorf("runtime running = %v, want %v", got, tc.running)
			}
		})
	}
}

// TestReconcileSessionBeads_StaleKillFenceHealsNormally pins the crash
// recovery: a fence older than KillPendingGrace (the killing CLI died
// mid-kill) is ordinary asleep state again, so a live runtime heals the row
// back to awake.
func TestReconcileSessionBeads_StaleKillFenceHealsNormally(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := killFencedSessionBead(t, env, env.clk.Now().Add(-sessionpkg.KillPendingGrace-time.Minute))
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	env.reconcile([]beads.Bead{before})

	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := after.Metadata["state"]; got != string(sessionpkg.StateAwake) {
		t.Fatalf("state = %q, want awake: a stale fence must not pin a live session asleep; stderr=%s", got, env.stderr.String())
	}
}

func TestCleanupDeadRuntimeSessionCorpsesSkipsKillFencedRow(t *testing.T) {
	testCity := t.TempDir()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sp := newDeadRuntimeArtifactProvider()
	sp.visible["killed-worker"] = true
	sp.dead["killed-worker"] = true

	meta := map[string]string{
		"session_name": "killed-worker",
		"template":     "worker",
	}
	for key, value := range sessionpkg.KillPendingPatch(now) {
		meta[key] = value
	}
	snapshot := newSessionBeadSnapshot([]beads.Bead{{ID: "s1", Status: "open", Metadata: meta}})

	var stderr bytes.Buffer
	if got := cleanupDeadRuntimeSessionCorpses(testCity, nil, nil, nil, snapshot, nil, sp, nil, &clock.Fake{Time: now}, &stderr); got != 0 {
		t.Fatalf("cleanupDeadRuntimeSessionCorpses() = %d, want 0: the dead pane is the kill in progress; stderr=%q", got, stderr.String())
	}
	if len(sp.stopped) != 0 {
		t.Fatalf("stopped = %v, want none", sp.stopped)
	}
}
