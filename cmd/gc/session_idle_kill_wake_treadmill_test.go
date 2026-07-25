//go:build treadmillproof

// Package main test: RED proof for ga-3ox7rk.
//
// GATED BY DESIGN behind the `treadmillproof` build tag. Both tests assert the
// POST-fix invariant, so they fail on today's code. They are excluded from
// normal builds rather than left red because the pre-push hook runs the full Go
// suite and a permanently-red test would block every unrelated push. An env-var
// gate does NOT work in this package: cmd/gc's TestMain calls
// clearProcessLiveEnvForTests(), which unsets every GC_/BEADS_/DOLT_ variable.
//
// Run the proof with:
//
//	go test -tags treadmillproof ./cmd/gc/ -run TestIdleKill
//
// BUILDER: delete this build tag as the first step of the fix bead and drive
// both tests to green.
package main

import (
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestIdleKillLadderFightsAwakeSetExemptions is the RED proof for ga-3ox7rk:
// the repeating wake -> idle_killed -> wake cycle on ProjectWrenUnity/architect
// (102 wake/idle_killed pairs in a single events.jsonl, same session record
// gm-0vqg, re-woken 6-20s after every kill, ~12min period, zero work claimed).
//
// Two independent decision engines evaluate the SAME idle session and reach
// OPPOSITE conclusions:
//
//   - ComputeAwakeSet (cmd/gc/compute_awake_set.go:467-472) exempts a set of
//     wake reasons from idle-sleep. A session desired for "assigned-work",
//     "min-active", "reset-pending", "named-demand" or "work-query" — or one
//     that is Pinned — keeps ShouldWake=true no matter how long it has been
//     idle.
//
//   - DecideIdleTimeout (internal/session/lifecycle_timers.go:132) honors only
//     the two blockers supplied by lifecycleTimerBlockerInfo
//     (cmd/gc/session_reconciler.go): user_hold and quarantine. It consults
//     none of the awake-engine exemptions. Its own doc comment states the
//     asymmetry outright: "Idle stops never consult assigned work."
//
// The reconciler closes the loop. Both live inside
// reconcileSessionBeadsTracedWithNamedDemand (session_reconciler.go:1279): the
// idle kill runs at :3168-3245 and ComputeAwakeSet runs afterwards at :3310, so
// the kill is decided with NO knowledge of the wake reasons. After emitting
// session.idle_killed (:3217) the kill path deliberately falls through to the
// wake pass — "Mark for immediate re-wake on this same tick" (:3222) and "Fall
// through to wakeReasons — it will re-wake immediately if config present"
// (:3244). So the kill lands, the awake engine still says wake, and the session
// is revived within seconds. Forever.
//
// Each subtest asserts the INVARIANT (a session the awake engine holds awake
// must not be idle-killed) and therefore FAILS on current code.
func TestIdleKillLadderFightsAwakeSetExemptions(t *testing.T) {
	const (
		sessionName = "ProjectWrenUnity--architect"
		template    = "ProjectWrenUnity/architect"
		beadID      = "gm-0vqg"
	)
	now := time.Date(2026, 7, 24, 18, 34, 4, 0, time.UTC)
	idleSince := now.Add(-12 * time.Minute)

	baseAgent := AwakeAgent{
		QualifiedName:  template,
		SleepAfterIdle: 10 * time.Minute, // pack.toml idle_timeout = "10m"
	}
	baseBead := AwakeSessionBead{
		ID:          beadID,
		SessionName: sessionName,
		Template:    template,
		State:       "active",
		IdleSince:   idleSince,
		CreatedAt:   now.Add(-50 * 24 * time.Hour),
	}

	cases := []struct {
		name       string
		wantReason string
		mutate     func(in *AwakeInput)
	}{
		{
			// THE LIVE CASE. projectwrenunity-r4z5kq.2 is status=deferred with
			// assignee=ProjectWrenUnity/architect, and is the session's
			// currently_processing_bead_id. It reaches the awake engine as
			// Status:"open", Ready:true because of a two-step status erasure:
			//
			//   internal/beads/bdstore.go:861 mapBdStatus  -> default: "open"
			//     (deferred is not a case, so it collapses to "open")
			//   internal/beads/native_dolt_store.go:131    -> the ready scan
			//     keeps StatusDeferred, justified by "IsDeferred independently
			//     re-checks DeferUntil". But this bead's defer_until is NULL,
			//     so the re-check finds no live deferral and it stays ready.
			//
			// workBeadHasAwakeDemand (compute_awake_set.go:697) then returns
			// true for open+Ready, anchoring permanent "assigned-work" demand.
			name:       "assigned-work/deferred-bead-erased-to-open",
			wantReason: "assigned-work",
			mutate: func(in *AwakeInput) {
				in.WorkBeads = []AwakeWorkBead{{
					ID:       "projectwrenunity-r4z5kq.2",
					Assignee: sessionName,
					Status:   "open", // real status is "deferred"; erased upstream
					Ready:    true,   // defer_until is NULL, so nothing re-defers it
				}}
				in.SessionBeads[0].CurrentlyProcessingBeadID = "projectwrenunity-r4z5kq.2"
			},
		},
		{
			name:       "assigned-work/in-progress",
			wantReason: "assigned-work",
			mutate: func(in *AwakeInput) {
				in.WorkBeads = []AwakeWorkBead{{
					ID:       "ga-stuck1",
					Assignee: sessionName,
					Status:   "in_progress",
				}}
			},
		},
		{
			name:       "reset-pending",
			wantReason: "reset-pending",
			mutate: func(in *AwakeInput) {
				in.SessionBeads[0].ContinuationResetPending = true
			},
		},
		{
			name:       "pin",
			wantReason: "pin",
			mutate: func(in *AwakeInput) {
				in.SessionBeads[0].Pinned = true
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := AwakeInput{
				Agents:       []AwakeAgent{baseAgent},
				SessionBeads: []AwakeSessionBead{baseBead},
				Now:          now,
			}
			tc.mutate(&input)

			decision := ComputeAwakeSet(input)[sessionName]

			// Engine 1: the awake engine holds this session awake despite it
			// being idle well past idle_timeout.
			if !decision.ShouldWake {
				t.Fatalf("precondition: ComputeAwakeSet should hold %s awake for %q, got ShouldWake=false reason=%q",
					sessionName, tc.wantReason, decision.Reason)
			}
			if decision.Reason != tc.wantReason {
				t.Fatalf("precondition: expected wake reason %q, got %q", tc.wantReason, decision.Reason)
			}

			// Engine 2: the idle-kill ladder evaluates the same idle session.
			// lifecycleTimerBlockerInfo yields "" here — neither HeldUntil nor
			// QuarantinedUntil is set — and there is no pending interaction.
			dec := sessionpkg.DecideIdleTimeout(sessionpkg.TimerFacts{
				Triggered: true,
				Blocker:   "",
				Pending:   sessionpkg.PendingNo,
			})

			// THE INVARIANT: the two engines must agree. A session the awake
			// engine refuses to idle-sleep must not be idle-killed, or the
			// reconciler's post-kill fall-through re-wakes it immediately and
			// the session thrashes forever.
			if dec.Action == sessionpkg.TimerActionStop {
				t.Fatalf("TREADMILL: ComputeAwakeSet holds %s awake (reason=%q, exempt from idle-sleep) "+
					"but DecideIdleTimeout returns TimerActionStop (sleep_reason=%q) for the same idle session. "+
					"The reconciler kills it, falls through to the wake pass, and re-wakes it within seconds — "+
					"the 102x wake/idle_killed cycle in ga-3ox7rk.",
					sessionName, decision.Reason, dec.SleepReason)
			}
		})
	}
}

// TestIdleTimeoutLadderIsAsymmetricWithMaxSessionAge pins the narrower,
// mechanical half of the same defect: the two lifecycle timer ladders are
// handed identical facts and disagree about assigned work.
//
// DecideMaxSessionAge defers ("deferred_busy") when the session holds open
// assigned work. DecideIdleTimeout ignores the fact entirely and stops. Since
// the reconciler re-wakes an assigned-work session immediately after the kill,
// the idle ladder's stop is never durable — it only burns a session lifecycle
// (~3 min and ~136K context per wake, measured on gm-0vqg).
func TestIdleTimeoutLadderIsAsymmetricWithMaxSessionAge(t *testing.T) {
	facts := sessionpkg.TimerFacts{
		Triggered:    true,
		Pending:      sessionpkg.PendingNo,
		AssignedWork: sessionpkg.AssignedWorkHas,
	}

	age := sessionpkg.DecideMaxSessionAge(facts)
	if age.Action != sessionpkg.TimerActionDefer {
		t.Fatalf("precondition: DecideMaxSessionAge should defer on assigned work, got action=%v outcome=%q",
			age.Action, age.TraceOutcome)
	}

	idle := sessionpkg.DecideIdleTimeout(facts)
	if idle.Action == sessionpkg.TimerActionStop {
		t.Fatalf("ASYMMETRY: identical TimerFacts{AssignedWork: Has} — DecideMaxSessionAge defers (%q) "+
			"but DecideIdleTimeout stops (sleep_reason=%q). An idle session holding assigned work is killed "+
			"and then immediately re-woken by ComputeAwakeSet's assigned-work exemption.",
			age.TraceOutcome, idle.SleepReason)
	}
}
