package main

import (
	"context"
	"io"
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

// A session that is awake ONLY because it owns ready assigned work is the
// sleep-and-respawn case; anything with another (or no) wake reason is not.
func TestIdleAssignedWorkOnly(t *testing.T) {
	if !idleAssignedWorkOnly(wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}}) {
		t.Fatal("assigned-work-only eval should qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork, WakePending}}) {
		t.Fatal("eval with an extra wake reason must not qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{Reason: "min-active", Reasons: []WakeReason{WakeConfig}}) {
		t.Fatal("non-assigned-work eval must not qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{}) {
		t.Fatal("empty eval must not qualify")
	}
}

// The idle-respawn drain must survive the per-tick cancel checks so the
// persistent assigned-work demand cannot undo it before the session sleeps.
func TestDrainReasonCancelable_IdleRespawnNotCancelable(t *testing.T) {
	if drainReasonCancelable(idleRespawnDrainReason) {
		t.Fatalf("%q must be non-cancelable (drainReasonCancelable)", idleRespawnDrainReason)
	}
	if assignedWorkDrainReasonCancelable(idleRespawnDrainReason) {
		t.Fatalf("%q must not be cancelable by the assigned-work cancel path", idleRespawnDrainReason)
	}
	if !drainReasonCancelable("idle") {
		t.Fatal("ordinary idle drain should remain cancelable")
	}
}

// An idle session awake solely for assigned work must be eligible for an idle
// probe (so it can sleep-and-respawn) even though it carries a wake reason and
// is not ConfigSuppressed — the original gate skipped it.
func TestSelectIdleProbeTargets_IncludesAssignedWorkOnly(t *testing.T) {
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "s1",
		Status:   "open",
		Type:     "session",
		Metadata: map[string]string{"session_name": "run-operator-1"},
	})
	target := wakeTarget{
		info:  info,
		alive: true,
	}
	wakeEvals := map[string]wakeEvaluation{
		"s1": {Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy},
	}
	dt := newDrainTracker()
	infoByID := map[string]session.Info{"s1": info}
	got := selectIdleProbeTargets([]wakeTarget{target}, wakeEvals, dt, infoByID)
	if !got["s1"] {
		t.Fatalf("assigned-work-only idle session must be idle-probe-eligible, got %v", got)
	}
}

// A non-assigned-work (or no-reason) session must still be skipped unless it is
// the classic ConfigSuppressed-with-no-reasons idle case.
func TestSelectIdleProbeTargets_SkipsOtherWakeReasons(t *testing.T) {
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "s1",
		Status:   "open",
		Type:     "session",
		Metadata: map[string]string{"session_name": "worker-1"},
	})
	target := wakeTarget{
		info:  info,
		alive: true,
	}
	// A pending wake reason (not assigned-work-only) must NOT be probe-eligible.
	wakeEvals := map[string]wakeEvaluation{
		"s1": {Reason: "pending", Reasons: []WakeReason{WakePending}, Policy: policy},
	}
	infoByID := map[string]session.Info{"s1": info}
	got := selectIdleProbeTargets([]wakeTarget{target}, wakeEvals, newDrainTracker(), infoByID)
	if got["s1"] {
		t.Fatalf("a pending-wake session must not be idle-probe-eligible, got %v", got)
	}
}

// With a COMPLETED idle probe proving the agent idle, an alive assigned-work
// session begins an idle-respawn drain (→ asleep, resume-on-ready re-spawns it).
// Without a completed probe, or for a non-assigned-work session, it does not.
func TestBeginIdleRespawnDrainIfIdle(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	name := "run-operator-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "s1",
		Status:   "open",
		Type:     "session",
		Metadata: map[string]string{"session_name": name, "generation": "1"},
	})
	policy := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityFull}
	eval := wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy}

	// Positive: completed, successful idle probe + idle agent.
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	if !beginIdleRespawnDrainIfIdle(info, eval, dt, sp, clk) {
		t.Fatal("idle assigned-work session with a completed idle probe should begin an idle-respawn drain")
	}
	if ds := dt.get(info.ID); ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("expected an idle-respawn drain, got %+v", ds)
	}

	// Negative: not assigned-work-only (a different wake reason) → no drain.
	dt2 := newDrainTracker()
	p2 := dt2.startIdleProbe(info.ID)
	dt2.finishIdleProbe(info.ID, p2, true, clk.Now().Add(-time.Second))
	other := wakeEvaluation{Reason: "min-active", Reasons: []WakeReason{WakeConfig}, Policy: policy}
	if beginIdleRespawnDrainIfIdle(info, other, dt2, sp, clk) {
		t.Fatal("non-assigned-work session must not begin an idle-respawn drain")
	}

	// Negative: no completed idle probe → no drain (guards against false sleep).
	dt3 := newDrainTracker()
	if beginIdleRespawnDrainIfIdle(info, eval, dt3, sp, clk) {
		t.Fatal("without a completed idle probe, no idle-respawn drain should begin")
	}
}

// Non-interactive sessions short-circuit shouldBeginIdleDrainInfo to true without
// a probe; they must be excluded from idle-respawn so an assigned non-interactive
// (e.g. named) session is not wrongly drained.
func TestBeginIdleRespawnDrainIfIdle_SkipsNonInteractive(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	name := "ni-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := sessiontest.SeedBead(t, beads.Bead{
		ID:       "ni",
		Status:   "open",
		Type:     "session",
		Metadata: map[string]string{"session_name": name, "generation": "1"},
	})
	eval := wakeEvaluation{
		Reason:  "assigned-work",
		Reasons: []WakeReason{WakeWork},
		Policy:  resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive},
	}
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	if beginIdleRespawnDrainIfIdle(info, eval, dt, sp, clk) {
		t.Fatal("a non-interactive assigned-work session must not be idle-respawn-drained")
	}
}

// A named session that is idleAssignedWorkOnly (assigned-work sole wake reason)
// and has an active cancelable drain must have that drain canceled when the
// reconciler processes it — even though beginIdleRespawnDrainIfIdle returns
// false for named sessions. Before the fix, the else-if !idleAssignedWorkOnly
// guard left this branch unreachable for such sessions.
func TestReconciler_CancelsDrainForNamedIdleAssignedWorkOnlySession(t *testing.T) {
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	sp := runtime.NewFake()
	sessionName := "named-worker-1"
	if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "cmd"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Status: "in_progress",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                  sessionName,
			"template":                      "named-worker",
			"generation":                    "1",
			session.NamedSessionMetadataKey: "true",
			"state":                         "active",
			"last_woke_at":                  clk.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	// Work bead assigned to the session bead.
	workBead := beads.Bead{
		ID:       "work-1",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
	}

	// Pre-seed a cancelable "idle" drain for the session.
	dt := newDrainTracker()
	info := sessiontest.SeedBead(t, sessionBead)
	beginSessionDrainInfo(info, sp, dt, "idle", clk, defaultDrainTimeout)
	if dt.get(sessionBead.ID) == nil {
		t.Fatal("pre-condition: expected an idle drain to be seeded")
	}

	reconcileSessionBeads(
		context.Background(),
		[]beads.Bead{sessionBead},
		nil, // desiredState — session is NOT configured; assigned-work is sole reason
		nil, // configuredNames
		&config.City{},
		sp,
		store,
		nil,                    // dops
		[]beads.Bead{workBead}, // assignedWorkBeads → gives "assigned-work" wake reason
		nil,                    // readyWaitSet
		dt,
		nil,   // poolDesired
		false, // storeQueryPartial
		nil,   // workSet
		"",    // cityName
		nil,   // idleTracker
		clk,
		events.Discard,
		0, 0,
		io.Discard, io.Discard,
	)

	if ds := dt.get(sessionBead.ID); ds != nil {
		t.Fatalf("expected idle drain to be canceled for named idleAssignedWorkOnly session, got %+v", ds)
	}
}
