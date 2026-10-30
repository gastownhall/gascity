package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
		reconcileSessionBeadsAtPath(
			context.Background(), t.TempDir(), []beads.Bead{loser}, ds.State, map[string]bool{"repo/worker": true},
			cfg, sp, store, dops, nil, nil, nil, dt, ds.PoolDesiredCounts, false, nil, "trace-town",
			nil, clk, events.Discard, 0, 0, io.Discard, io.Discard,
		)
		waitForProviderStopped(t, sp, loserName)

		reloaded, err := store.Get(loser.ID)
		if err != nil {
			t.Fatalf("reload loser after tick 1: %v", err)
		}

		// Tick 2: runtime is gone -> finalize the stop-pending session to a
		// terminal drained state (pool-managed + no work -> close the bead).
		reconcileSessionBeadsAtPath(
			context.Background(), t.TempDir(), []beads.Bead{reloaded}, ds.State, map[string]bool{"repo/worker": true},
			cfg, sp, store, dops, nil, nil, nil, dt, ds.PoolDesiredCounts, false, nil, "trace-town",
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

// TestReusablePoolSessionInfo_DrainAckStopPendingNotReusable is the regression
// guard for the drain-ack + min_sessions=1 replacement bug.
//
// A pool session in state=draining (set by drain-ack-stop-pending) must NOT be
// returned as reusable. Without this exclusion, the min-floor fires (draining
// doesn't consume demand via poolSessionConsumesNewDemandInfo), but
// selectOrPlanPoolSessionBead maps the draining bead as the desired slot via
// reusablePoolSessionInfos, so no replacement session is started until the
// draining bead is eventually closed — requiring another reload or several more
// patrol ticks.
func TestReusablePoolSessionInfo_DrainAckStopPendingNotReusable(t *testing.T) {
	t.Parallel()

	drainingInfo := session.Info{
		ID:                  "session-draining",
		Template:            "repo/worker",
		SessionNameMetadata: "worker-1",
		PoolManaged:         true,
		PoolSlot:            "1",
		MetadataState:       string(session.StateDraining),
	}

	bp := &agentBuildParams{}
	cfgAgent := &config.Agent{Name: "worker", Dir: "repo"}

	if got := reusablePoolSessionInfo(bp, cfgAgent, "repo/worker", drainingInfo, nil); got {
		t.Fatalf("reusablePoolSessionInfo(state=%q) = true; "+
			"a drain-ack-stop-pending session must not be reusable as the desired slot "+
			"(fixes min_sessions=1 replacement stall, #drain-ack-min1-bug)",
			drainingInfo.MetadataState)
	}
}

// TestReusablePoolSessionInfo_OneShotAsleepFreeableIsReusable is the
// regression guard for the recurring one-shot pool-name-holder outage
// (EDGE-RECEIPT-asleep-holder): a one_shot pool session that finishes its
// bounded unit of work exits, and the generic heal path lands it in
// state=asleep with a freeable reason (idle, idle-timeout, ...) — not a
// crash, not a deliberate hold. Nothing in the reconciler closes that bead
// (the "reconciler closes orphaned asleep beads" comment on the caller is
// aspirational, not implemented), so it sits open forever holding the
// identity's runtime session_name. Because this function excluded EVERY
// asleep session from reuse, selectOrPlanPoolSessionBead fell through to a
// fresh create, which collided with the very name this bead still holds:
// derivePoolSessionName -> errPoolSessionNameUnavailable, and buildDesiredState
// refuses to mint ("pool session name unavailable ... session name already
// exists") on every tick until an operator runs `gc session close` by hand.
//
// A one_shot pool's own freeable-asleep exit must be reusable so the ordinary
// wake path (the same one that already handles this fine for other pool
// states, per the "Woke session" / outcome=success log lines) retires the
// exit and remints the identity — matching the reuse this function already
// grants a closed/swept slot with the pool markers intact.
func TestReusablePoolSessionInfo_OneShotAsleepFreeableIsReusable(t *testing.T) {
	t.Parallel()

	bp := &agentBuildParams{}
	cfgAgent := &config.Agent{Name: "claude-sonnet-one-shot", Dir: "fable-nomad", Lifecycle: config.AgentLifecycleOneShot}
	template := "fable-nomad/claude-sonnet-one-shot"

	asleepFreeable := session.Info{
		ID:                  "session-asleep-freeable",
		Template:            template,
		SessionNameMetadata: "fable-nomad--claude-sonnet-one-shot-1-pool",
		PoolManaged:         true,
		PoolSlot:            "1",
		MetadataState:       "asleep",
		SleepReason:         "idle",
	}
	if got := reusablePoolSessionInfo(bp, cfgAgent, template, asleepFreeable, nil); !got {
		t.Fatalf("reusablePoolSessionInfo(one_shot, asleep, sleep_reason=idle) = false; "+
			"want true — a freeable-asleep one_shot exit must be reusable or it blocks its own "+
			"identity name forever (session %s)", asleepFreeable.ID)
	}

	// Control: a deliberately held asleep one_shot session (e.g. quarantine)
	// must stay excluded — freeable-only, not asleep-blanket.
	asleepHeld := asleepFreeable
	asleepHeld.ID = "session-asleep-quarantine"
	asleepHeld.SleepReason = "quarantine"
	if got := reusablePoolSessionInfo(bp, cfgAgent, template, asleepHeld, nil); got {
		t.Fatalf("reusablePoolSessionInfo(one_shot, asleep, sleep_reason=quarantine) = true; " +
			"want false — a deliberately held sleep must still block reuse")
	}

	// Control: a persistent (non-one_shot) pool keeps today's behavior — an
	// asleep session is never reused, even when freeable, so a genuine crash
	// gets a fresh identity instead of resuming a possibly-corrupt conversation.
	persistentAgent := &config.Agent{Name: "claude-sonnet-one-shot", Dir: "fable-nomad"}
	if got := reusablePoolSessionInfo(bp, persistentAgent, template, asleepFreeable, nil); got {
		t.Fatalf("reusablePoolSessionInfo(persistent, asleep, sleep_reason=idle) = true; " +
			"want false — the freeable-asleep reuse carve-out is scoped to lifecycle=one_shot")
	}
}

// TestReusablePoolSessionInfo_OneShotAsleepFreeableWithOpenAssignedWorkNotReusable
// is the regression guard for the wake-reuse-orphans-work outage: a one_shot
// pool session lands asleep with a freeable reason (idle, ...) per the
// carve-out above, but the identity still has an open/in-progress assigned
// work bead attached from before the exit (the step never finished). The
// ordinary wake path (reusablePoolSessionInfo -> true) "reuses" the identity
// without giving the step a real execution turn: the reconciler logs
// `session lifecycle: op=start ... outcome=success`, then the reused session
// drains and lands `orphaned` ~45-55s later with no work done, and the step
// stays in_progress pinned to the now-dead name — retry control never
// re-attempts it because the step still looks claimed.
//
// A one_shot exit that still holds assigned work is not a clean exit: it must
// fall through to fresh-identity creation (the pre-6f1ee854b behavior for
// this case) instead of being reused. This mirrors the assigned-work
// visibility the caller already provides via bp.assignedWorkBeads (the same
// slice sessionBeadHasAssignedWorkInfo checks a few lines below the asleep
// carve-out), not a bypass around it.
func TestReusablePoolSessionInfo_OneShotAsleepFreeableWithOpenAssignedWorkNotReusable(t *testing.T) {
	t.Parallel()

	cfgAgent := &config.Agent{Name: "claude-sonnet-one-shot", Dir: "fable-nomad", Lifecycle: config.AgentLifecycleOneShot}
	template := "fable-nomad/claude-sonnet-one-shot"

	asleepFreeableWithWork := session.Info{
		ID:                  "session-asleep-freeable-with-work",
		Template:            template,
		SessionNameMetadata: "fable-nomad--claude-sonnet-one-shot-2-pool",
		Alias:               "fable-nomad/claude-sonnet-one-shot-2",
		PoolManaged:         true,
		PoolSlot:            "2",
		MetadataState:       "asleep",
		SleepReason:         "idle",
	}

	bp := &agentBuildParams{
		assignedWorkBeads: []beads.Bead{
			{
				// Claimed work is assigned under the session's actor identity
				// (Alias — GC_ALIAS/BEADS_ACTOR, per session.AssigneeIdentifier),
				// not necessarily the raw runtime SessionNameMetadata.
				ID:       "work-in-progress",
				Status:   "in_progress",
				Assignee: asleepFreeableWithWork.Alias,
			},
		},
	}

	if got := reusablePoolSessionInfo(bp, cfgAgent, template, asleepFreeableWithWork, nil); got {
		t.Fatalf("reusablePoolSessionInfo(one_shot, asleep, sleep_reason=idle, open assigned work) = true; "+
			"want false — an asleep one_shot exit that still holds in-progress assigned work must fall "+
			"through to fresh-identity creation, not be wake-reused (session %s)",
			asleepFreeableWithWork.ID)
	}
}

// TestReusablePoolSessionInfo_PersistentRestartHandoffIsReusable is the
// regression guard for the restart-handoff pool stall: a restart request on a
// live pool runtime hands the row off (restartHandoff folds
// RestartRequestPatchAt: continuation_reset_pending=true with a committed
// reset_committed_at, state=asleep, no sleep reason — the restart is no
// sleep). The row keeps holding the pool's runtime session name while every
// reuse path rejects it (this predicate refused every asleep row of a
// persistent pool), the floor's fresh create is refused on the held name, and
// neither close path frees a row with no sleep reason and no slept_at: only
// the session sweep retires it, ~13 minutes with no running session per pool
// after a supervisor restart.
//
// A committed handoff must be reusable: its reset-pending wake starts a FRESH
// conversation under the same identity (RestartRequestPatchAt already rotated
// or cleared the resume key), so nothing resumes and the fresh-identity rule
// for crashed persistent rows is not weakened. The sibling controls in
// TestReusablePoolSessionInfo_RestartHandoffControlsStayRejected pin every
// other asleep shape to today's answer.
func TestReusablePoolSessionInfo_PersistentRestartHandoffIsReusable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	awake := session.Info{
		ID:                  "session-restart-handoff",
		Template:            "rig/worker",
		SessionNameMetadata: "rig--worker-canonical",
		Alias:               "rig/worker-handoff",
		PoolManaged:         true,
		MetadataState:       "awake",
	}
	// Fold the real handoff (not a hand-written asleep shape) so the fixture
	// tracks restartHandoff itself.
	handedOff := awake.ApplyPatch(restartHandoff("", true, finalizeFacts{Now: now}))

	// Fixture contract: asleep, reset committed, no sleep reason stamped. A
	// drift in restartHandoff fails here as a fixture failure, not a
	// predicate failure.
	if handedOff.MetadataState != string(session.StateAsleep) {
		t.Fatalf("fixture state = %q, want asleep", handedOff.MetadataState)
	}
	if _, _, ok := resetPendingCommittedAtInfo(handedOff); !ok {
		t.Fatalf("fixture has no committed reset (continuation_reset_pending=%q reset_committed_at=%q)",
			handedOff.ContinuationResetPending, handedOff.ResetCommittedAt)
	}
	if strings.TrimSpace(handedOff.SleepReason) != "" {
		t.Fatalf("fixture sleep_reason = %q, want empty (the restart is no sleep)", handedOff.SleepReason)
	}

	persistentAgent := &config.Agent{Name: "worker", Dir: "rig"}
	template := "rig/worker"

	if got := reusablePoolSessionInfo(&agentBuildParams{}, persistentAgent, template, handedOff, nil); !got {
		t.Fatalf("reusablePoolSessionInfo(persistent, asleep restart handoff) = false; "+
			"want true — a committed handoff holds the pool's runtime name while nothing reuses or frees "+
			"it, so the pool runs empty until the session sweep (session %s)",
			handedOff.ID)
	}

	// Edge: a one_shot pool's handed-off row is reusable for the same
	// reason. It carries no sleep reason, so the one_shot freeable carve-out
	// alone cannot take it back.
	oneShotAgent := &config.Agent{Name: "worker", Dir: "rig", Lifecycle: config.AgentLifecycleOneShot}
	if got := reusablePoolSessionInfo(&agentBuildParams{}, oneShotAgent, template, handedOff, nil); !got {
		t.Fatalf("reusablePoolSessionInfo(one_shot, asleep restart handoff) = false; "+
			"want true — the committed-handoff admission is lifecycle-independent (session %s)",
			handedOff.ID)
	}
}

// TestReusablePoolSessionInfo_RestartHandoffControlsStayRejected pins the
// boundary of the committed-handoff admission in
// TestReusablePoolSessionInfo_PersistentRestartHandoffIsReusable: every other
// asleep shape of a persistent pool keeps today's answer (false), so the fix
// admits exactly a committed handoff with no sleep reason and no assigned
// work. These controls hold both before and after the fix; they exist to fail
// if a future widening admits more than the handoff shape.
func TestReusablePoolSessionInfo_RestartHandoffControlsStayRejected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	awake := session.Info{
		ID:                  "session-restart-handoff-control",
		Template:            "rig/worker",
		SessionNameMetadata: "rig--worker-canonical",
		Alias:               "rig/worker-handoff",
		PoolManaged:         true,
		MetadataState:       "awake",
	}
	handedOff := awake.ApplyPatch(restartHandoff("", true, finalizeFacts{Now: now}))

	persistentAgent := &config.Agent{Name: "worker", Dir: "rig"}
	oneShotAgent := &config.Agent{Name: "worker", Dir: "rig", Lifecycle: config.AgentLifecycleOneShot}
	template := "rig/worker"

	notHandoff := func(name string, mutate func(*session.Info)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := handedOff
			mutate(&row)
			if got := reusablePoolSessionInfo(&agentBuildParams{}, persistentAgent, template, row, nil); got {
				t.Fatalf("reusablePoolSessionInfo(persistent, %s) = true; want false", name)
			}
		})
	}

	// An asleep row with no reset markers is today's plain asleep row.
	notHandoff("asleep without reset markers", func(row *session.Info) {
		row.ContinuationResetPending = ""
		row.ResetCommittedAt = ""
	})
	// Reset pending but never committed: no handoff to admit.
	notHandoff("reset pending with empty reset_committed_at", func(row *session.Info) {
		row.ResetCommittedAt = ""
	})
	// Reset pending with an unparseable committed marker: the reader is
	// parse-strict, so this is no handoff either.
	notHandoff("reset pending with non-RFC3339 reset_committed_at", func(row *session.Info) {
		row.ResetCommittedAt = "not-a-time"
	})
	// A fresh-wake drain sleep: reset pending, but a sleep reason and
	// slept_at are stamped and the reset was never committed.
	notHandoff("fresh-wake drain sleep", func(row *session.Info) {
		row.ResetCommittedAt = ""
		row.SleepReason = "idle"
		row.SleptAt = now.Format(time.RFC3339)
	})
	// A handoff that a later sleep stamped a reason on is no longer waiting
	// for its fresh start.
	notHandoff("handed off then slept", func(row *session.Info) {
		row.SleepReason = "idle"
		row.SleptAt = now.Format(time.RFC3339)
	})

	// A committed handoff that still holds open assigned work under any of
	// its identities is not taken back by new or floor demand, for either
	// lifecycle: its own resume request serves it.
	t.Run("handed off with open assigned work", func(t *testing.T) {
		t.Parallel()
		bp := &agentBuildParams{assignedWorkBeads: []beads.Bead{{
			// Claimed work is assigned under the session's actor identity
			// (Alias — GC_ALIAS/BEADS_ACTOR), as in
			// TestReusablePoolSessionInfo_OneShotAsleepFreeableWithOpenAssignedWorkNotReusable.
			ID:       "work-in-progress",
			Status:   "in_progress",
			Assignee: handedOff.Alias,
		}}}
		if got := reusablePoolSessionInfo(bp, persistentAgent, template, handedOff, nil); got {
			t.Fatalf("reusablePoolSessionInfo(persistent, asleep restart handoff with open assigned work) = true; " +
				"want false — the any-identity assigned-work check must stay in front of every admitted asleep row")
		}
		if got := reusablePoolSessionInfo(bp, oneShotAgent, template, handedOff, nil); got {
			t.Fatalf("reusablePoolSessionInfo(one_shot, asleep restart handoff with open assigned work) = true; " +
				"want false — the any-identity assigned-work check must stay in front of every admitted asleep row")
		}
	})

	// A row already consumed by this pass's selection is not reusable again.
	t.Run("handed off already used", func(t *testing.T) {
		t.Parallel()
		used := map[string]bool{handedOff.ID: true}
		if got := reusablePoolSessionInfo(&agentBuildParams{}, persistentAgent, template, handedOff, used); got {
			t.Fatalf("reusablePoolSessionInfo(persistent, asleep restart handoff already in used) = true; want false")
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
