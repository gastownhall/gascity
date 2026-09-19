package main

import (
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// explicitWakeInput builds the shape gastownhall/gascity#5739 part 2 describes:
// a pool seat that was suspended across a supervisor restart, re-projected to
// asleep by the wake path, carrying a durable explicit wake request, and idle
// past its template's idle timeout because it has not run since the restart.
func explicitWakeInput(idleSince time.Time, held, quarantined time.Time) AwakeInput {
	const (
		template    = "gascity/polecat"
		sessionName = "gascity--polecat-1"
	)
	return AwakeInput{
		Agents: []AwakeAgent{{
			QualifiedName:  template,
			SleepAfterIdle: 10 * time.Minute,
		}},
		SessionBeads: []AwakeSessionBead{{
			ID:               "gm-explicit",
			SessionName:      sessionName,
			Template:         template,
			State:            "asleep",
			ExplicitWake:     true,
			IdleSince:        idleSince,
			HeldUntil:        held,
			QuarantinedUntil: quarantined,
		}},
		ScaleCheckCounts: map[string]int{template: 1},
		Now:              time.Now().UTC(),
	}
}

// TestExplicitWakeSurvivesIdleSleep reproduces the wake-refusal half of
// gastownhall/gascity#5739.
//
// ComputeAwakeSet admits an explicit wake request into the desired set as
// reason "explicit-wake" (compute_awake_set.go:172-179), but the idle-sleep
// block (compute_awake_set.go:482-503) then flips ShouldWake back to false for
// any desired reason outside its exemption list — and "explicit-wake" is not on
// that list. The seat's durable wake request is silently unserved.
//
// This is the same defect PR #4644 fixed for "routed-demand"; see
// TestPR4644_RoutedDemandWakesAsleepNamedHolder in
// compute_awake_set_routed_demand_idle_test.go, which is the landed precedent
// and the shape of the fix.
func TestExplicitWakeSurvivesIdleSleep(t *testing.T) {
	const sessionName = "gascity--polecat-1"

	t.Run("A_no_idle_reference", func(t *testing.T) {
		d := ComputeAwakeSet(explicitWakeInput(time.Time{}, time.Time{}, time.Time{}))[sessionName]
		if !d.ShouldWake {
			t.Fatalf("precondition: explicit wake did not reach the desired set (reason=%q)", d.Reason)
		}
		if d.Reason != "explicit-wake" {
			t.Fatalf("precondition: reason = %q, want explicit-wake", d.Reason)
		}
	})

	t.Run("B_idle_past_timeout", func(t *testing.T) {
		idle := time.Now().UTC().Add(-45 * time.Minute)
		d := ComputeAwakeSet(explicitWakeInput(idle, time.Time{}, time.Time{}))[sessionName]
		if !d.ShouldWake {
			t.Fatalf("explicit wake was canceled by idle-sleep (final reason=%q); "+
				"a durable operator wake request must not be overridden by the idle timer — "+
				`add "explicit-wake" to the idle-sleep exemption list, as PR #4644 did for "routed-demand"`,
				d.Reason)
		}
	})
}

// TestExplicitWakeRefusalIsAttributable pins the *observability* half. Each of
// these refusals is legitimate policy, but the decision is the only record of
// it: no event is emitted and wake_attempts stays 0, because recordWakeFailure
// (cmd/gc/session_reconcile.go:642) only runs after a start has actually run
// and failed. This test documents which reason string each refusal carries, so
// a wake_refused emitter has a defined value to report.
func TestExplicitWakeRefusalIsAttributable(t *testing.T) {
	const sessionName = "gascity--polecat-1"
	now := time.Now().UTC()

	cases := []struct {
		name       string
		input      AwakeInput
		wantReason string
	}{
		{"held", explicitWakeInput(time.Time{}, now.Add(time.Hour), time.Time{}), "held"},
		{"quarantined", explicitWakeInput(time.Time{}, time.Time{}, now.Add(time.Hour)), "quarantined"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ComputeAwakeSet(tc.input)[sessionName]
			if d.ShouldWake {
				t.Fatalf("precondition: %s did not suppress the wake", tc.name)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("refusal reason = %q, want %q", d.Reason, tc.wantReason)
			}
			t.Logf("explicit wake refused with reason=%q and no event, no wake_attempts increment", d.Reason)
		})
	}
}

// TestWokenFromSuspendedIsInvisibleToStrandedLane is the join between the two
// halves of gastownhall/gascity#5739, and it corrects the issue's own account
// of why nothing appeared in .gc/events.jsonl.
//
// The issue reasons that "a silently refused pool seat can be released on the
// very same tick its explicit wake went unserved", citing
// poolFreeable := !shouldWake && !target.alive && ...
// (session_reconciler.go:3705). For THIS record that is not what happens, and
// the difference matters: poolFreeable also requires
// isPoolSessionSlotFreeableInfo (session_state_helpers.go:88-103), which admits
// an asleep bead only when sleep_reason is one of a closed set. The bead the
// wake path produced carries sleep_reason="" — ClearWakeBlockersPatch clears the
// recognized reasons and stamps no replacement — so it is admitted by neither
// arm.
//
// The consequence is worse than a premature release: the seat is not released,
// and it is also never diagnosed. emitSessionStrandedDiagnostic
// (session_reconciler.go:3724) — the one emitter that would have produced a
// session.stranded event for a not-alive pool seat still holding assigned work
// — sits inside `if poolFreeable && hasAssignedWork`, so it never runs. That is
// why the operator saw a seat holding real work, no resumption, and no event of
// any kind: the incoherent record from part 1 puts the session outside every
// classifier's vocabulary at once.
func TestWokenFromSuspendedIsInvisibleToStrandedLane(t *testing.T) {
	// The exact post-wake record, as produced end-to-end by
	// TestSuspendThenWakeIsSingleVoiced in internal/session: that test pins
	// md["slept_at"] != "" on the re-projected bead (ClearWakeBlockersPatch
	// stamps it whenever it transitions Suspended/Drained -> Asleep), so a
	// faithful fixture here must carry it too.
	woken := sessionpkg.Info{
		ID:            "gm-explicit",
		MetadataState: "asleep",
		SleepReason:   "",
		SleptAt:       time.Now().UTC().Format(time.RFC3339),
		Template:      "gascity/polecat",
		SessionName:   "gascity--polecat-1",
	}

	if isDrainedSessionInfo(woken) {
		t.Fatal("precondition: the woken record is not drained")
	}
	if !isPoolSessionSlotFreeableInfo(woken) {
		t.Errorf("isPoolSessionSlotFreeableInfo = false for state=asleep sleep_reason=%q: "+
			"the seat is admitted to neither the release lane nor the stranded-diagnostic lane, "+
			"so emitSessionStrandedDiagnostic never runs and no session.stranded event is emitted",
			woken.SleepReason)
	}

	// Control: the same seat slept by the ordinary path IS admitted, which is
	// what makes the difference attributable to the wake re-projection rather
	// than to pool policy.
	slept := woken
	slept.SleepReason = string(sessionpkg.SleepReasonIdle)
	if !isPoolSessionSlotFreeableInfo(slept) {
		t.Fatal("control failed: an ordinarily-idle asleep pool seat must be freeable")
	}
}

// TestExplicitWakeIdleSleepScopeWithClaimedWork bounds the blast radius of the
// idle-sleep defect, and is the falsification pass on "the idle-sleep flip is
// what stranded the reporter's seats".
//
// d3e0536b80 (#5174, landed 2026-08-10) added a holdsClaimedWork veto to the
// idle-sleep block. A seat that owns an in_progress, non-blocked claim is
// therefore ALREADY exempt, whatever its desired reason — so for the exact
// scenario the issue describes (pool seats holding assigned work) the
// exemption-list omission is not the operative cause.
//
// It still bites for every explicit wake whose session does not hold a matching
// non-blocked in_progress claim: an operator waking an idle seat to attach to
// it, a seat whose claim is blocked, or one whose work is open/ready rather than
// in_progress.
func TestExplicitWakeIdleSleepScopeWithClaimedWork(t *testing.T) {
	const sessionName = "gascity--polecat-1"
	idle := time.Now().UTC().Add(-45 * time.Minute)

	withWork := func(status string, blocked bool) AwakeInput {
		in := explicitWakeInput(idle, time.Time{}, time.Time{})
		in.SessionBeads[0].NamedIdentity = "gascity/polecat-1"
		in.WorkBeads = []AwakeWorkBead{{
			ID: "ga-work", Assignee: "gascity/polecat-1",
			Status: status, Ready: true, Blocked: blocked,
		}}
		return in
	}

	t.Run("claimed_in_progress_is_already_exempt", func(t *testing.T) {
		d := ComputeAwakeSet(withWork("in_progress", false))[sessionName]
		if !d.ShouldWake {
			t.Fatalf("the #5174 holdsClaimedWork veto did not hold (reason=%q)", d.Reason)
		}
	})

	t.Run("blocked_claim_still_slept", func(t *testing.T) {
		d := ComputeAwakeSet(withWork("in_progress", true))[sessionName]
		t.Logf("blocked in_progress claim: ShouldWake=%v reason=%q", d.ShouldWake, d.Reason)
	})

	t.Run("open_ready_work_still_slept", func(t *testing.T) {
		d := ComputeAwakeSet(withWork("open", false))[sessionName]
		t.Logf("open ready work: ShouldWake=%v reason=%q", d.ShouldWake, d.Reason)
	})
}

// TestQuarantinedAsleepStaysUnfreeable is a regression guard on refutation 3 in
// ga-deekj5's notes: "freeable if asleep AND slept_at != ”" was considered and
// REFUTED as unsafe, because SleepPatch stamps slept_at for deliberate parks
// too. A deliberately-quarantined seat must stay denied even though it may
// carry no slept_at of its own — pin the empty-slept_at quarantine shape here
// so a future relaxation of the freeability arm to "slept_at alone" is caught.
func TestQuarantinedAsleepStaysUnfreeable(t *testing.T) {
	i := sessionpkg.Info{
		MetadataState: "asleep",
		SleepReason:   string(sessionpkg.SleepReasonQuarantine),
		SleptAt:       "",
	}
	if isPoolSessionSlotFreeableInfo(i) {
		t.Fatalf("isPoolSessionSlotFreeableInfo = true for state=asleep sleep_reason=quarantine slept_at=%q, want false", i.SleptAt)
	}
}
