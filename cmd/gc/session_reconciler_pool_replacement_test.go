package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestReconcileSessionBeads_DrainAckNoWorkFreesSlotAndReallocates is the
// end-to-end regression guard for gastownhall/gascity#2520 ("pool over-counts
// supply when session drain-acks with no work and bead stays active").
//
// Scenario (min_active=0, max>=2 pool; two routed-ready beads; two sessions
// race to claim one): the winner takes bead-1 (in_progress), the loser gets
// "already claimed" and calls `gc runtime drain-ack` with NO work attached.
// The report claimed the loser's session bead lingers in state=active, the
// pool counts it as an occupied supply slot, and the next still-ready bead is
// never served until an operator runs `gc session close`.
//
// The maintainer classified #2520 as test-hardening: current main already
// behaves correctly (the drain-ack lands the loser in a terminal drained state,
// and a drained pool bead is excluded from the running-session supply count so
// the still-ready work is still served), but the full "no-work pool drain-ack
// PLUS replacement-allocation" path had no end-to-end coverage. Existing tests
// stop at the state transition or the pool-bead close; none then re-drives the
// supply probe to prove the drained loser is excluded AND a replacement slot is
// desired for the still-ready queue bead. This test locks in both halves.
//
// The second sub-test is the load-bearing regression assertion: it fails RED on
// the pre-#3419 revision (where poolSessionIsLive did not exclude drained pool
// beads, so a phantom drained bead counted toward runningSessions, forced
// isCold=false, suppressed the cold-wake probe, and stranded the ready bead —
// exactly #2520's over-count symptom) and passes on current main.
func TestReconcileSessionBeads_DrainAckNoWorkFreesSlotAndReallocates(t *testing.T) {
	// Part 1 — the real reconciler drains a no-work drain-acking loser to a
	// terminal state (it does NOT linger in state=active), which is the
	// precondition the #2520 report says was violated.
	t.Run("reconciler_drains_no_work_loser_to_terminal", func(t *testing.T) {
		now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
		cityDir := t.TempDir()
		writeCityTOML(t, cityDir, "trace-town", "worker")

		cfg := &config.City{
			Workspace: config.Workspace{Name: "trace-town"},
			Session:   config.SessionConfig{Provider: "fake"},
			Agents: []config.Agent{{
				Name:              "worker",
				Dir:               "repo",
				StartCommand:      "true",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(2),
			}},
		}
		store := beads.NewMemStore()
		sp := runtime.NewFake()

		// Two routed-ready beads. bead-1 goes in_progress under the winner;
		// bead-2 stays ready in the queue.
		beadOne := createRoutedReadyBeadForReplacement(t, store, "repo/worker", "queued work 1")
		createRoutedReadyBeadForReplacement(t, store, "repo/worker", "queued work 2")

		// Winner: slot 1, active, holds bead-1 in_progress.
		winner := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, 1)
		setPoolSessionActive(t, store, winner.ID)
		if err := sp.Start(context.Background(), winner.Metadata["session_name"], runtime.Config{}); err != nil {
			t.Fatalf("start winner runtime: %v", err)
		}
		statusInProgress := "in_progress"
		winnerAssignee := winner.ID
		if err := store.Update(beadOne.ID, beads.UpdateOpts{Status: &statusInProgress, Assignee: &winnerAssignee}); err != nil {
			t.Fatalf("assign bead-1 to winner: %v", err)
		}

		// Loser: slot 2, active, NO assigned work, agent-set drain-ack (the
		// #1425 stranded event never fires because hasAssignedWork=false).
		loser := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, 2)
		setPoolSessionActive(t, store, loser.ID)
		loser, err := store.Get(loser.ID)
		if err != nil {
			t.Fatalf("reload loser: %v", err)
		}
		loserName := loser.Metadata["session_name"]
		if err := sp.Start(context.Background(), loserName, runtime.Config{}); err != nil {
			t.Fatalf("start loser runtime: %v", err)
		}
		dops := newFakeDrainOps()
		if err := dops.setDrainAck(loserName); err != nil {
			t.Fatalf("setDrainAck(loser): %v", err)
		}

		ds := buildDesiredState("trace-town", cityDir, now, cfg, sp, store, io.Discard)
		dt := newDrainTracker()
		clk := &clock.Fake{Time: now}

		// Tick 1: alive + agent-sourced drain-ack -> mark stop-pending and queue
		// the async provider stop.
		reconcileSessionBeads(
			context.Background(), []beads.Bead{loser}, ds.State, map[string]bool{"repo/worker": true},
			cfg, sp, store, dops, nil, nil, dt, ds.PoolDesiredCounts, false, nil, "trace-town",
			nil, clk, events.Discard, 0, 0, io.Discard, io.Discard,
		)
		waitForProviderStopped(t, sp, loserName)

		reloaded, err := store.Get(loser.ID)
		if err != nil {
			t.Fatalf("reload loser after tick 1: %v", err)
		}

		// Tick 2: runtime is gone -> finalize the stop-pending session to a
		// terminal drained state (pool-managed + no work -> close the bead).
		reconcileSessionBeads(
			context.Background(), []beads.Bead{reloaded}, ds.State, map[string]bool{"repo/worker": true},
			cfg, sp, store, dops, nil, nil, dt, ds.PoolDesiredCounts, false, nil, "trace-town",
			nil, clk, events.Discard, 0, 0, io.Discard, io.Discard,
		)

		got, err := store.Get(loser.ID)
		if err != nil {
			t.Fatalf("reload loser after tick 2: %v", err)
		}
		// #2520's precondition for the over-count is the loser lingering as a
		// live session. Assert it did NOT: it reached a terminal drained state.
		if got.Metadata["state"] == "active" && got.Status != "closed" {
			t.Fatalf("no-work drain-acked loser lingered as a live supply slot: state=%q status=%q metadata=%v",
				got.Metadata["state"], got.Status, got.Metadata)
		}
		if got.Metadata["state"] != "drained" {
			t.Fatalf("loser state = %q, want drained", got.Metadata["state"])
		}
		if poolSessionIsLiveInfo(sessiontest.SeedBead(t, got)) {
			t.Fatalf("drained loser still reports poolSessionIsLiveInfo=true; it would over-count supply: metadata=%v", got.Metadata)
		}
	})

	// Part 2 — replacement-allocation. A drained phantom pool session (the exact
	// terminal state Part 1 produces, but left open in the store as the report
	// describes it "lingering") must be excluded from the running-session supply
	// count, so a min=0 pool with still-ready cross-store work is NOT treated as
	// warm: its cold-wake probe fires and desires a replacement slot to serve the
	// stranded bead.
	//
	// This is the RED assertion: on the pre-#3419 revision the drained phantom
	// counts toward runningSessions -> isCold=false -> cold-wake probe suppressed
	// -> demand 0 and no desired slot (the ready bead is stranded). On current
	// main the phantom is excluded -> isCold=true -> demand 1 and one desired
	// slot. The parallel "active" sub-case is the control: a genuinely-live
	// session MUST still suppress the probe.
	t.Run("drained_phantom_excluded_from_supply_reallocates", func(t *testing.T) {
		cases := []struct {
			name          string
			meta          map[string]string
			wantDemand    int
			wantSlots     int
			wantStillLive bool
		}{
			{
				name:       "drained_phantom_frees_slot",
				meta:       map[string]string{"state": "drained"},
				wantDemand: 1, wantSlots: 1, wantStillLive: false,
			},
			{
				name:       "asleep_idle_phantom_frees_slot",
				meta:       map[string]string{"state": "asleep", "sleep_reason": "idle"},
				wantDemand: 1, wantSlots: 1, wantStillLive: false,
			},
			{
				name:       "active_session_still_suppresses_probe",
				meta:       map[string]string{"state": "active"},
				wantDemand: 0, wantSlots: 0, wantStillLive: true,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				rigPath := tmpDir + "/rigs/rig-A"
				if err := os.MkdirAll(rigPath, 0o755); err != nil {
					t.Fatalf("mkdir rig path: %v", err)
				}
				maxSess := 5
				minSess := 0
				cfg := &config.City{
					Agents: []config.Agent{{
						Name:              "worker",
						MaxActiveSessions: &maxSess,
						MinActiveSessions: &minSess,
						ScaleCheck:        "printf 0", // custom check reports 0; only a cold-wake probe can raise demand
						Dir:               "rig-A",
						Provider:          "mock",
					}},
					Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
					Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
				}
				cityStore := beads.NewMemStore()
				rigStore := beads.NewMemStore()
				rigStores := map[string]beads.Store{"rig-A": rigStore}
				qualifiedName := "rig-A/worker"

				meta := map[string]string{
					"template":     qualifiedName,
					"session_name": "worker-1",
					"pool_slot":    "1",
				}
				for k, v := range tc.meta {
					meta[k] = v
				}
				phantom, err := rigStore.Create(beads.Bead{
					ID: "session-loser", Status: "open", Type: sessionBeadType, Metadata: meta,
				})
				if err != nil {
					t.Fatalf("create phantom pool session: %v", err)
				}
				if live := poolSessionIsLiveInfo(sessiontest.SeedBead(t, phantom)); live != tc.wantStillLive {
					t.Fatalf("poolSessionIsLiveInfo(%s phantom) = %v, want %v", tc.name, live, tc.wantStillLive)
				}

				// Still-ready routed bead delivered cross-store to the city store
				// (the sleeping rig pool's own-store probe cannot see it, so only a
				// cold-wake probe over all stores serves it).
				if _, err := cityStore.Create(beads.Bead{
					ID: "bead-ready", Status: "open", Type: "task",
					Metadata: map[string]string{"gc.routed_to": qualifiedName},
				}); err != nil {
					t.Fatalf("create still-ready routed bead: %v", err)
				}

				result := buildDesiredStateWithSessionBeads(
					"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
					cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
				)
				if demand := result.ScaleCheckCounts[qualifiedName]; demand != tc.wantDemand {
					t.Fatalf("ScaleCheckCounts[%s] = %d, want %d (drained/asleep phantom must not over-count supply; #2520)",
						qualifiedName, demand, tc.wantDemand)
				}
				workerSlots := 0
				for _, tp := range result.State {
					if tp.TemplateName == qualifiedName {
						workerSlots++
					}
				}
				if workerSlots != tc.wantSlots {
					t.Fatalf("desired %s slots = %d, want %d (replacement slot for the still-ready bead)",
						qualifiedName, workerSlots, tc.wantSlots)
				}
			})
		}
	})
}

func createRoutedReadyBeadForReplacement(t *testing.T, store beads.Store, template, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": template},
	})
	if err != nil {
		t.Fatalf("create routed ready bead %q: %v", title, err)
	}
	return b
}

func setPoolSessionActive(t *testing.T, store beads.Store, id string) {
	t.Helper()
	for k, v := range map[string]string{
		"state":                     "active",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	} {
		if err := store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("SetMetadata(%s=%s): %v", k, v, err)
		}
	}
}
