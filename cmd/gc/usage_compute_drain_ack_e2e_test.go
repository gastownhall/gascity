package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// TestDrainAckCloseThenUsageTickSettlesInterval is the end-to-end #6672
// regression. Instead of closing the bead by hand, it drives the REAL
// reconciler drain-ack close (finalizeDrainAckStoppedSession via
// reconcileSessionBeads, the same path as
// TestReconcileSessionBeads_UndesiredDrainAckStopsAndCloses and
// TestReconcileSessionBeads_DrainAckOwnDrainStepClosesWithoutEvent) on a codex
// session whose rollout is keyed by session_key, then runs the usage lane on the
// next tick. The closed interval must yield exactly one compute fact and one
// model fact per completed turn, including the turn that landed after the last
// live sweep, and a further tick must add nothing.
func TestDrainAckCloseThenUsageTickSettlesInterval(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownDrainStep bool
	}{
		{name: "undesired-drain-ack"},
		{name: "own-drain-step", ownDrainStep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			workDir := t.TempDir()
			codexRoot := t.TempDir()
			start := liveSweepStart()
			// The drain-ack close stamps closed_at from the reconciler clock.
			closedAt := start.Add(30 * time.Minute).Truncate(time.Second)
			env.clk.Time = start

			session := env.createSessionBead("worker", "worker")
			env.markSessionActive(&session)
			usageMeta := liveCodexSessionMeta(start, workDir, codexSweepSessionKey)
			delete(usageMeta, "state")
			delete(usageMeta, "session_name")
			env.setSessionMetadata(&session, usageMeta)
			if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("Start(worker): %v", err)
			}
			if tc.ownDrainStep {
				root, err := env.store.Create(beads.Bead{
					Title:    "Run of mol-do-work",
					Type:     "task",
					Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: "mol-do-work"},
				})
				if err != nil {
					t.Fatalf("Create(root): %v", err)
				}
				if _, err := env.store.Create(beads.Bead{
					Title:    "Close drain step and signal completion",
					Type:     "task",
					Status:   "in_progress",
					Assignee: session.ID,
					Metadata: map[string]string{
						beadmeta.StepRefMetadataKey:    "mol-do-work.drain",
						beadmeta.RootBeadIDMetadataKey: root.ID,
					},
				}); err != nil {
					t.Fatalf("Create(drainStep): %v", err)
				}
			}

			cityPath := t.TempDir()
			sinkPath := filepath.Join(cityPath, ".gc", "usage.jsonl")
			cs := &controllerState{cityBeadStore: env.store, usageSink: usage.NewLocalSink(sinkPath), cityName: "demo", cityPath: cityPath}
			cr := &CityRuntime{
				cs:       cs,
				cfg:      &config.City{Daemon: config.DaemonConfig{ObservePaths: []string{codexRoot}}},
				sp:       runtime.NewFake(),
				cityName: "demo",
				cityPath: cityPath,
				stderr:   io.Discard,
			}
			h := liveSweepHarness{cr: cr, sinkPath: sinkPath}
			snapshot := func() []sessionpkg.Info {
				info, err := sessionFrontDoor(env.store).Get(session.ID)
				if err != nil {
					t.Fatalf("front-door Get: %v", err)
				}
				if info.Closed {
					return nil
				}
				return []sessionpkg.Info{info}
			}

			// Tick 1: awake; the live lane bills the first invocation.
			writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{{150, 100, 50}})
			cr.emitDueComputeFacts(context.Background(), snapshot(), false)
			if compute, model := closeBackstopFacts(t, h); len(compute) != 0 || model != 1 {
				t.Fatalf("tick 1: compute=%d model=%d, want 0/1", len(compute), model)
			}

			// A second turn lands inside the live-sweep floor; then the agent
			// drain-acks and the reconciler closes the bead straight from active.
			writeCodexRolloutForSweepAt(t, codexRoot, start, workDir, codexSweepSessionKey, [][3]int{
				{150, 100, 50},
				{450, 200, 100},
			})
			env.clk.Time = closedAt
			dops := newFakeDrainOps()
			if err := dops.setDrainAck("worker"); err != nil {
				t.Fatalf("setDrainAck: %v", err)
			}
			current, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken := reconcileSessionBeads(
				context.Background(), []beads.Bead{current}, env.desiredState, nil, env.cfg, env.sp,
				env.store, dops, nil, nil, env.dt, nil, false, nil, "",
				nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
			); woken != 0 {
				t.Fatalf("woken = %d, want 0", woken)
			}
			closed := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, nil)
			if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
				t.Fatalf("drain-ack did not close: status=%q state=%q", closed.Status, closed.Metadata["state"])
			}
			if closed.Metadata["session_key"] != codexSweepSessionKey {
				t.Fatalf("close cleared session_key (%q); the settle needs it", closed.Metadata["session_key"])
			}

			// Tick 2: the closed session has left the open snapshot.
			cr.emitDueComputeFacts(context.Background(), snapshot(), false)
			compute, model := closeBackstopFacts(t, h)
			if len(compute) != 1 || model != 2 {
				t.Fatalf("tick after drain-ack close: compute=%d model=%d, want 1/2", len(compute), model)
			}
			if got, want := compute[0].WallSeconds, closedAt.Sub(start.Truncate(time.Second)).Seconds(); got != want {
				t.Fatalf("compute WallSeconds = %v, want %v (closed_at - awake_started_at)", got, want)
			}
			if compute[0].SessionID != session.ID {
				t.Fatalf("compute fact session = %q, want %q", compute[0].SessionID, session.ID)
			}

			// Tick 3: idempotent.
			cr.emitDueComputeFacts(context.Background(), snapshot(), false)
			if compute, model := closeBackstopFacts(t, h); len(compute) != 1 || model != 2 {
				t.Fatalf("re-tick: compute=%d model=%d, want 1/2", len(compute), model)
			}
		})
	}
}
