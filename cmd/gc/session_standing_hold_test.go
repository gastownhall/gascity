package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestReconcileSessionBeads_SuspendHoldSurvivesDrainAckWithAssignedWork pins
// gastownhall/gascity#5561: a standing sleep intent must outlive the drain it
// provoked.
//
// `gc session suspend` writes held_until + sleep_intent="user-hold" +
// state="suspended" (cmd_session.go). The hold correctly drains the seat. When
// the drain-ack lands while the session still holds assigned work,
// finalizeDrainAckStoppedSession takes CompleteDrainPatch -> SleepPatch, which
// blanks sleep_intent while held_until survives. sleep_intent is the ONLY
// discriminator the #3994 heartbeat crash-recovery override uses to tell an
// agent's keep-alive hold from an operator's suspend hold, so the erasure
// defeats that override's own documented exclusion and the suspended seat is
// respawned on the next tick.
//
// The sibling TestReconcileSessionBeads_HeartbeatHeldDeadSessionRespawns
// injects sleep_intent="user-hold" directly rather than letting the drain-ack
// produce it, which is why the erasure shipped unnoticed.
func TestReconcileSessionBeads_SuspendHoldSurvivesDrainAckWithAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	// Exactly what cmdSessionSuspend writes on the managed path.
	env.setSessionMetadata(&session, map[string]string{
		"held_until":   env.clk.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339),
		"sleep_intent": string(sessionpkg.SleepReasonUserHold),
		"state":        "suspended",
		// Woke well before this tick, so the seat is past the rapid-crash and
		// churn-productivity windows and reaches the wake loop as a plain
		// dead-but-desired session rather than a crash-loop capture.
		"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})

	task, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status := "in_progress"
	assignee := session.ID
	if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	task, err = env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(task): %v", err)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}

	cfgNames := map[string]bool{"worker": true}
	woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, dops, []beads.Bead{task}, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
	if woken != 0 {
		t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}

	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, cfgNames)

	// (a) Root cause: the operator's standing intent must survive the drain
	// completion. Today CompleteDrainPatch -> SleepPatch writes "".
	if intent := got.Metadata["sleep_intent"]; intent != string(sessionpkg.SleepReasonUserHold) {
		t.Errorf("sleep_intent after drain-ack with assigned work = %q, want %q (the suspend hold must survive its own drain)",
			intent, sessionpkg.SleepReasonUserHold)
	}
	// The hold itself is untouched by the drain-ack: it is what keeps the seat
	// down until `gc session wake` clears both markers.
	if got.Metadata["held_until"] == "" {
		t.Fatalf("held_until cleared by the drain-ack; metadata=%v", got.Metadata)
	}

	// (b) Symptom: the next tick must leave the suspended seat asleep. Today
	// the #3994 crash-recovery override fires on the blanked sleep_intent and
	// respawns it.
	final, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	woken = reconcileSessionBeads(
		context.Background(), []beads.Bead{final}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
	if woken != 0 {
		t.Errorf("woken on the tick after the drain-ack = %d, want 0 (a suspended seat with assigned work must stay asleep until `gc session wake`); stderr=%s",
			woken, env.stderr.String())
	}
	if env.sp.IsRunning("worker") {
		t.Errorf("suspended seat was respawned after its drain-ack; it must stay down until `gc session wake`")
	}

	// (c) The preserved intent must not strand the seat: `gc session wake`
	// clears held_until and sleep_intent together (ClearWakeBlockersPatch), and
	// the very next tick brings the seat back to serve its hook.
	if _, err := sessionFrontDoor(env.store).WakeSession(session.ID, env.clk.Now().UTC(), sessionpkg.WakeOpts{}); err != nil {
		t.Fatalf("WakeSession: %v", err)
	}
	if intent := env.sessionInfo(session.ID).SleepIntent; intent != "" {
		t.Fatalf("sleep_intent after `gc session wake` = %q, want cleared", intent)
	}
	awake, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	woken = reconcileSessionBeads(
		context.Background(), []beads.Bead{awake}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
	if woken != 1 {
		t.Errorf("woken after `gc session wake` = %d, want 1 (the release must bring the seat back); stderr=%s", woken, env.stderr.String())
	}
	if !env.sp.IsRunning("worker") {
		t.Errorf("`gc session wake` did not restart the suspended seat")
	}
}

// TestReconcileSessionBeads_WaitHoldSurvivesDrainTimeout covers the B23 sibling
// of the drain-ack path through the real reconciler: when the agent never acks,
// the controller's drain-TIMEOUT arm completes the drain via completeDrain,
// which reaches the same CompleteDrainPatch. A pool singleton parked on a gate
// with `gc session wait --sleep` — the variant reported in
// gastownhall/gascity#5561 — must come out of that completion still holding its
// intent, so the wake side can still see the park.
//
// The tick sequence is the one TestReconcileSessionBeads_HeartbeatHoldSurvivesDrainTimeout
// uses: tick 1 begins the drain on a live session with no wake reason, then the
// clock crosses defaultDrainTimeout and tick 2 stops the runtime and completes.
func TestReconcileSessionBeads_WaitHoldSurvivesDrainTimeout(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"wait_hold":    "true",
		"sleep_intent": string(sessionpkg.SleepReasonWaitHold),
	})
	if err := env.sp.SetMeta("worker", "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	runTick := func() {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		reconcileSessionBeads(
			context.Background(), []beads.Bead{got}, env.desiredState,
			configuredSessionNames(env.cfg, "", env.store), env.cfg, env.sp, env.store,
			nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
	}

	// Tick 1: no wake reason and an explicit sleep intent, so the seat drains
	// under its own hold reason rather than the keep-alive guard.
	runTick()
	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("wait-held seat did not begin a drain; stderr=%s", env.stderr.String())
	}
	if ds.reason != string(sessionpkg.SleepReasonWaitHold) {
		t.Fatalf("drain reason = %q, want %q", ds.reason, sessionpkg.SleepReasonWaitHold)
	}

	// Tick 2: cross the drain deadline with no agent ack. The timeout arm stops
	// the runtime and completes the drain through completeDrain.
	env.clk.Time = env.clk.Now().Add(defaultDrainTimeout + time.Minute)
	runTick()
	waitForProviderStopped(t, env.sp, "worker")

	got := env.sessionInfo(session.ID)
	if string(got.State) != "asleep" {
		t.Fatalf("state after drain timeout = %q, want asleep", got.State)
	}
	if got.SleepIntent != string(sessionpkg.SleepReasonWaitHold) {
		t.Errorf("sleep_intent after drain timeout = %q, want %q (the wait gate must survive its own drain)",
			got.SleepIntent, sessionpkg.SleepReasonWaitHold)
	}
	// The timeout arm already passes the drain's own reason, which for a held
	// drain IS the intent — so unlike the drain-ack path it never needed the
	// idle relabel. Pinned here so the two completion paths cannot drift.
	if got.SleepReason != string(sessionpkg.SleepReasonWaitHold) {
		t.Errorf("sleep_reason after drain timeout = %q, want %q (the pool gate reads the reason)",
			got.SleepReason, sessionpkg.SleepReasonWaitHold)
	}
	if got.WaitHold == "" {
		t.Errorf("wait_hold cleared by the drain timeout; the gate is released by the wait, not the drain")
	}
}

// TestReconcileSessionBeads_ExpiredSuspendHoldReleasesUserHoldIntent is the
// other half of the standing-hold contract: a user-hold outlives the drain it
// provoked, but NOT its own timer. `held_until` is the suspend hold's
// expiry, so when the Phase-0 timer heal clears it the intent must go with it —
// otherwise the surviving marker keeps vetoing wake after the hold is over
// (`wakeDemandOverridesSleepSuppression` refuses to let assigned work override
// an enabled idle-sleep policy while any intent is set), and the crash-recovery
// override cannot rescue the seat either because its `user_hold` blocker is
// gone with `held_until`. A wait-hold is released by the wait's own resolution,
// not by this timer, so it is deliberately not cleared here.
func TestReconcileSessionBeads_ExpiredSuspendHoldReleasesUserHoldIntent(t *testing.T) {
	env := newReconcilerTestEnv()
	// An enabled idle-sleep policy is what makes the surviving intent decisive:
	// after the drain-ack the seat is asleep with sleep_reason=idle, so config
	// suppression applies and only the assigned-work demand override can beat it.
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", SleepAfterIdle: "30m"}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"held_until":   env.clk.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
		"sleep_intent": string(sessionpkg.SleepReasonUserHold),
		"state":        "suspended",
		"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	})

	task, err := env.store.Create(beads.Bead{Title: "assigned task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	status := "in_progress"
	assignee := session.ID
	if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(task): %v", err)
	}
	task, err = env.store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get(task): %v", err)
	}

	cfgNames := map[string]bool{"worker": true}
	runTick := func() int {
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", session.ID, err)
		}
		return reconcileSessionBeads(
			context.Background(), []beads.Bead{got}, env.desiredState, cfgNames,
			env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, env.dt,
			map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
	}

	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	if woken := reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, dops, []beads.Bead{task}, nil, env.dt,
		map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	); woken != 0 {
		t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
	}
	got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, cfgNames)
	if intent := got.Metadata["sleep_intent"]; intent != string(sessionpkg.SleepReasonUserHold) {
		t.Fatalf("sleep_intent after drain-ack = %q, want %q (setup precondition)", intent, sessionpkg.SleepReasonUserHold)
	}

	// Control: while the hold is still live the seat stays asleep.
	if woken := runTick(); woken != 0 {
		t.Fatalf("woken while the hold is still live = %d, want 0; stderr=%s", woken, env.stderr.String())
	}

	// Advance past held_until. The Phase-0 timer heal clears the expired hold;
	// the intent it was paired with must be released with it.
	env.clk.Time = env.clk.Now().Add(10 * time.Minute)
	woken := runTick()
	if intent := env.sessionInfo(session.ID).SleepIntent; intent != "" {
		t.Errorf("sleep_intent after the hold expired = %q, want cleared with held_until", intent)
	}
	if woken != 1 {
		t.Errorf("woken after the hold expired = %d, want 1 (an expired suspend must release its seat); stderr=%s", woken, env.stderr.String())
	}
	if !env.sp.IsRunning("worker") {
		t.Errorf("seat stayed down after its suspend hold expired")
	}
}

// TestReconcileSessionBeads_StandingHoldKeepsPoolSlotAndClaim is the symptom
// gastownhall/gascity#5561 actually reports: a POOL seat parked on a standing
// hold is reaped.
//
// The pool-slot gate reads only state + sleep_reason
// (isPoolSessionSlotFreeableInfo, cmd/gc/session_state_helpers.go) — never
// sleep_intent or wait_hold. The drain-ack path used to hardcode
// SleepReasonIdle as the completion reason, so a held seat landed on
// state=asleep/sleep_reason=idle: freeable. With its runtime gone and its work
// still claimed, the stranded arm then fires and, once the confirmation window
// has aged, repairStrandedPoolWorkerBead unclaims the work and closes the bead —
// the park is destroyed rather than honored. Carrying the standing intent as the
// drain reason takes the seat out of that switch.
func TestReconcileSessionBeads_StandingHoldKeepsPoolSlotAndClaim(t *testing.T) {
	for _, tt := range []struct {
		name   string
		intent sessionpkg.SleepReason
		extra  map[string]string
	}{
		{
			name:   "wait hold",
			intent: sessionpkg.SleepReasonWaitHold,
			extra:  map[string]string{"wait_hold": "true"},
		},
		{
			name:   "user hold",
			intent: sessionpkg.SleepReasonUserHold,
			extra:  map[string]string{"state": "suspended"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			session := env.createSessionBead("worker", "worker")
			env.markSessionActive(&session)
			meta := map[string]string{
				"pool_managed": "true",
				"held_until":   env.clk.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339),
				"sleep_intent": string(tt.intent),
				"last_woke_at": env.clk.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			}
			for k, v := range tt.extra {
				meta[k] = v
			}
			env.setSessionMetadata(&session, meta)

			task, err := env.store.Create(beads.Bead{Title: "claimed task", Type: "task"})
			if err != nil {
				t.Fatalf("Create(task): %v", err)
			}
			status := "in_progress"
			assignee := session.ID
			if err := env.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
				t.Fatalf("Update(task): %v", err)
			}
			task, err = env.store.Get(task.ID)
			if err != nil {
				t.Fatalf("Get(task): %v", err)
			}

			cfgNames := map[string]bool{"worker": true}
			dops := newFakeDrainOps()
			if err := dops.setDrainAck("worker"); err != nil {
				t.Fatalf("setDrainAck: %v", err)
			}
			if woken := reconcileSessionBeads(
				context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
				env.cfg, env.sp, env.store, dops, []beads.Bead{task}, nil, env.dt,
				map[string]int{"worker": 1}, false, nil, "",
				nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
			); woken != 0 {
				t.Fatalf("woken on the drain-ack tick = %d, want 0; stderr=%s", woken, env.stderr.String())
			}
			got := env.reconcileStopPendingToTerminal(t, env.sp, session, dops, cfgNames)
			if intent := got.Metadata["sleep_intent"]; intent != string(tt.intent) {
				t.Fatalf("sleep_intent after drain-ack = %q, want %q", intent, tt.intent)
			}
			if reason := got.Metadata["sleep_reason"]; reason != string(tt.intent) {
				t.Errorf("sleep_reason after drain-ack = %q, want %q (the pool gate reads the reason, so a held seat must not read as idle)",
					reason, tt.intent)
			}
			if info := env.sessionInfo(session.ID); isPoolSessionSlotFreeableInfo(info) {
				t.Errorf("a %s-held pool seat reads as slot-freeable (state=%q sleep_reason=%q); the reaper will take its slot",
					tt.intent, info.MetadataState, info.SleepReason)
			}

			// Two ticks past the stranded confirmation window: the repair arm
			// must never unclaim the work or close the parked bead.
			for _, advance := range []time.Duration{0, strandedRepairConfirmGrace + time.Minute} {
				env.clk.Time = env.clk.Now().Add(advance)
				cur, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatalf("Get(%s): %v", session.ID, err)
				}
				reconcileSessionBeads(
					context.Background(), []beads.Bead{cur}, env.desiredState, cfgNames,
					env.cfg, env.sp, env.store, nil, []beads.Bead{task}, nil, env.dt,
					map[string]int{"worker": 1}, false, nil, "",
					nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
				)
			}

			final, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", session.ID, err)
			}
			if final.Status == "closed" {
				t.Errorf("parked %s pool seat was closed by the reaper; metadata=%v", tt.intent, final.Metadata)
			}
			work, err := env.store.Get(task.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", task.ID, err)
			}
			if work.Assignee != session.ID {
				t.Errorf("work assignee = %q, want %q (the reaper unclaimed a parked seat's work)", work.Assignee, session.ID)
			}
			if work.Status != "in_progress" {
				t.Errorf("work status = %q, want in_progress (the reaper reopened a parked seat's claim)", work.Status)
			}
		})
	}
}
