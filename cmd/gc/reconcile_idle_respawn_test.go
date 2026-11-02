package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// C6d's idle respawn (CONTRACT v5 D2; SESS-614, DRAIN-004/044).

// respawnRow is an alive pool row detached an hour ago, gw-1 its current
// bead.
func respawnRow(meta ...string) []string {
	return append([]string{"pool_managed", "true", "detached_at", rowAt(-time.Hour), "currently_processing_bead_id", "gw-1"}, meta...)
}

// awakeForWork is a Wake entry awake only for ready, unclaimed work gw-1.
func awakeForWork(e *selectionEntry) {
	e.Desired, e.Reason, e.WakeReasons = desireWake, reasonAssignedWork, []WakeReason{WakeWork}
	e.AssignedWork = &assignedWorkView{BeadID: "gw-1"}
}

// napsFull gives every row an interactive Full-capability policy of ten
// minutes.
func napsFull(w *World) {
	if w.SleepPolicies == nil {
		w.SleepPolicies = map[string]resolvedSessionSleepPolicy{}
	}
	for _, row := range w.Census.Canonical() {
		w.SleepPolicies[row.Key.ID] = resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityFull, Effective: "10m", Duration: 10 * time.Minute}
	}
}

// TestIdleRespawnEligibility (v5 D2). Kills a respawn drain of a row any
// gate excludes: only an alive, pool-managed, unnamed row on an interactive
// Full-capability policy, awake only for ready, unclaimed assigned work,
// detached past its sleep time, begins one, by the fresh begin with its
// attempt for the bead.
func TestIdleRespawnEligibility(t *testing.T) {
	policy := func(f func(*resolvedSessionSleepPolicy)) func(*World) {
		return func(w *World) {
			napsFull(w)
			for id, p := range w.SleepPolicies {
				f(&p)
				w.SleepPolicies[id] = p
			}
		}
	}
	for _, c := range []struct {
		name  string
		meta  []string
		entry func(*selectionEntry)
		world func(*World)
		begin bool
	}{
		{"eligible", respawnRow(), awakeForWork, napsFull, true},
		{"not awake for work", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.Desired = desireSleep }, napsFull, false},
		{"awake for another reason", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.Reason = "pinned" }, napsFull, false},
		{"another wake reason too", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.WakeReasons = append(e.WakeReasons, WakePending) }, napsFull, false},
		{"the work claimed", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.AssignedWork.Claimed = true }, napsFull, false},
		{"no bead", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.AssignedWork.BeadID = "" }, napsFull, false},
		{"not alive (A20's own gate)", respawnRow(), func(e *selectionEntry) { awakeForWork(e); e.Liveness = livenessDead }, napsFull, false},
		{"not pool-managed", []string{"detached_at", rowAt(-time.Hour), "currently_processing_bead_id", "gw-1"}, awakeForWork, napsFull, false},
		{"named", respawnRow("configured_named_session", "true", "configured_named_identity", "worker"), awakeForWork, napsFull, false},
		{"non-interactive", respawnRow(), awakeForWork, policy(func(p *resolvedSessionSleepPolicy) { p.Class = config.SessionSleepNonInteractive }), false},
		{"not Full capability", respawnRow(), awakeForWork, policy(func(p *resolvedSessionSleepPolicy) { p.Capability = runtime.SessionSleepCapabilityTimedOnly }), false},
		{"no sleep time", respawnRow(), awakeForWork, policy(func(p *resolvedSessionSleepPolicy) { p.Duration = 0 }), false},
		{"detached inside its sleep time", []string{"pool_managed", "true", "detached_at", rowAt(-time.Minute), "currently_processing_bead_id", "gw-1"}, awakeForWork, napsFull, false},
	} {
		it, _ := decideDrain(t, c.meta, c.entry, c.world)
		begun := it.Kind == intentDrainBeginFresh && it.Reason == decideDrainBegin+idleRespawnDrainReason
		if begun != c.begin {
			t.Errorf("%s: decided %s %q, want a respawn begin %t", c.name, it.Kind, it.Reason, c.begin)
		}
		if begun && (it.Patch[idleRespawnAttemptsMetadataKey] != "1" || it.Patch[idleRespawnBeadIDMetadataKey] != "gw-1" || it.Patch[session.DrainIntentReasonKey] != idleRespawnDrainReason || it.Rests != restRuntime) {
			t.Errorf("%s: patch %v rests %d, want the request with its attempt for gw-1, resting on the runtime", c.name, it.Patch, it.Rests)
		}
	}
}

// TestIdleRespawnOneAttemptPerBead (v5 D2; maxIdleRespawnAttemptsPerBead).
// Kills a second respawn for the same bead: a row that spent its attempt on
// gw-1 begins none for it, and one whose attempt was for another bead
// begins.
func TestIdleRespawnOneAttemptPerBead(t *testing.T) {
	for _, c := range []struct {
		bead  string
		begin bool
	}{{"gw-1", false}, {"gw-0", true}} {
		it, _ := decideDrain(t, respawnRow(idleRespawnAttemptsMetadataKey, "1", idleRespawnBeadIDMetadataKey, c.bead), awakeForWork, napsFull)
		if begun := it.Reason == decideDrainBegin+idleRespawnDrainReason; begun != c.begin {
			t.Errorf("attempt spent on %s: decided %+v, want a respawn begin %t", c.bead, it, c.begin)
		}
	}
}

// TestIdleRespawnBeginProvesIdle (v5 D2; legacy's shouldBeginIdleDrainInfo).
// Kills a begin that trusts the pass: the effect proves the agent idle
// (WaitForIdle, then no activity since the pass) and lands the request with
// its attempt, clearing a pending idle stop in the same write; activity
// since the pass begins nothing.
func TestIdleRespawnBeginProvesIdle(t *testing.T) {
	for _, active := range []bool{false, true} {
		p, it, store, leaf := admittedBeginOn(t, respawnRow("sleep_intent", "idle-stop-pending"), awakeForWork, napsFull)
		if it.Kind != intentDrainBeginFresh {
			t.Fatalf("decided %+v, want the fresh respawn begin", it)
		}
		leaf.WaitForIdleErrors["s-gc-1"] = nil
		if active {
			leaf.SetActivity("s-gc-1", gatherNow.Add(time.Second))
		}
		s := runBegin(p, it)
		got, _ := store.Get(it.Key.ID)
		m := got.Metadata
		switch {
		case active && (s.Outcome == settledLanded || m[session.DrainIntentReasonKey] != ""):
			t.Errorf("active since the pass: %+v, row %v; want no begin", s, m)
		case !active && (s.Outcome != settledLanded || m[session.DrainIntentReasonKey] != idleRespawnDrainReason || m[idleRespawnAttemptsMetadataKey] != "1" || m[idleRespawnBeadIDMetadataKey] != "gw-1" || m["sleep_intent"] != ""):
			t.Errorf("idle: %+v, row %v; want the request, the attempt for gw-1, the idle stop ended", s, m)
		}
	}
}

// TestIdleRespawnRevalidationVoidsOnClaimReadErrorOrActivity (v5 D2;
// DRAIN-004, DRAIN-044). Kills a respawn signal that skips its
// revalidation, or treats a failed read as safe: the seat's claim, a claim
// read that fails, activity since the drain began, or an activity read that
// fails each void the drain in the signal's stead, keeping its attempt; a
// clean revalidation signals.
func TestIdleRespawnRevalidationVoidsOnClaimReadErrorOrActivity(t *testing.T) {
	for _, c := range []struct {
		name    string
		arrange func(p *effectPass, leaf *fenceLeaf, store beads.Store, id string)
		void    string
	}{
		{"clean", func(*effectPass, *fenceLeaf, beads.Store, string) {}, ""},
		{"claimed", func(_ *effectPass, _ *fenceLeaf, store beads.Store, id string) { claimWork(t, store, id) }, "claimed-work"},
		{"no work legs to read", func(p *effectPass, _ *fenceLeaf, _ beads.Store, _ string) { p.reads.legs = WorkLegs{} }, "claim-observation-error"},
		{"the claim read failed", func(p *effectPass, _ *fenceLeaf, store beads.Store, _ string) {
			p.reads.legs = testWorkLegs(t.TempDir(), &config.City{Agents: []config.Agent{{Name: "worker"}}}, failingListStore{store}, nil)
		}, "claim-observation-error"},
		{"active since the begin", func(_ *effectPass, leaf *fenceLeaf, _ beads.Store, _ string) {
			leaf.SetActivity("s-gc-1", gatherNow)
		}, "activity-resumed"},
		{"the activity read failed", func(p *effectPass, _ *fenceLeaf, _ beads.Store, id string) {
			obs := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: "1"}, env: map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": "tok-3"}, activityErr: errors.New("boom")}
			if err := obs.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			p.Runtime = obs.tmux()
		}, "activity-observation-error"},
	} {
		meta := respawnRow(append(intentBegunAt(idleRespawnDrainReason, time.Hour), idleRespawnAttemptsMetadataKey, "1", idleRespawnBeadIDMetadataKey, "gw-1")...)
		p, it, store, leaf := admittedBeginOn(t, meta, awakeForWork, napsFull)
		if it.Kind != intentSignalFresh {
			t.Fatalf("%s: decided %+v, want the respawn signal", c.name, it)
		}
		c.arrange(p, leaf, store, it.Key.ID)
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		m, pending := got.Metadata, isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		switch {
		case c.void == "" && (s.Outcome != settledLanded || !pending):
			t.Errorf("%s: %+v, want the signal", c.name, s)
		case c.void != "" && (s.Outcome != settledLanded || s.Cause != causeIdleRespawnVoid+c.void || pending || m[session.DrainIntentReasonKey] != "" || m[idleRespawnAttemptsMetadataKey] != "1"):
			t.Errorf("%s: %+v, row %v; want the drain voided (%s), its attempt kept", c.name, s, m, c.void)
		case c.void != "" && (s.Facts.Transition == nil || s.Facts.Transition.Transition != "cancel" || s.Facts.Transition.Reason != idleRespawnDrainReason):
			t.Errorf("%s: transition %+v, want legacy's idle-respawn cancel", c.name, s.Facts.Transition)
		}
	}
}

// TestIdleRespawnSignalVoidsOnAClaimSinceThePass (#7688 review). Kills a
// respawn signal that lands over a claim recorded after the probe's claim
// read (the drain basis does not compare it), or that treats the sticky
// most-recent-claim marker as a claim: current_claim_bead_id moved since the
// pass voids the drain; one the pass already read, unchanged, signals.
func TestIdleRespawnSignalVoidsOnAClaimSinceThePass(t *testing.T) {
	for _, c := range []struct {
		name        string
		pass, fresh string
		void        bool
	}{
		{"no claim marker", "", "", false},
		{"the pass's marker, unchanged", "gw-0", "gw-0", false},
		{"a claim recorded since the pass", "", "gw-9", true},
		{"a newer claim since the pass", "gw-0", "gw-9", true},
	} {
		extra := []string{idleRespawnAttemptsMetadataKey, "1", idleRespawnBeadIDMetadataKey, "gw-1"}
		if c.pass != "" {
			extra = append(extra, beadmeta.CurrentClaimBeadIDMetadataKey, c.pass)
		}
		meta := respawnRow(append(intentBegunAt(idleRespawnDrainReason, time.Hour), extra...)...)
		p, it, store, _ := admittedBeginOn(t, meta, awakeForWork, napsFull)
		if it.Kind != intentSignalFresh {
			t.Fatalf("%s: decided %+v, want the respawn signal", c.name, it)
		}
		if c.fresh != c.pass {
			if err := store.SetMetadata(it.Key.ID, beadmeta.CurrentClaimBeadIDMetadataKey, c.fresh); err != nil {
				t.Fatal(err)
			}
		}
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		pending := isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		switch {
		case !c.void && (s.Outcome != settledLanded || !pending):
			t.Errorf("%s: %+v, want the signal", c.name, s)
		case c.void && (s.Outcome != settledLanded || s.Cause != causeIdleRespawnVoid+"claim-moved" || pending || got.Metadata[session.DrainIntentReasonKey] != ""):
			t.Errorf("%s: %+v, row %v; want the drain voided (claim-moved)", c.name, s, got.Metadata)
		}
	}
}

// TestIdleRespawnFinalizeSleepsNeverCloses (v5 D2). Kills a respawn finalize
// that closes the seat: an idle-respawn stop of a pool seat with no work
// left, the allocation no longer wanting it, ends asleep, open.
func TestIdleRespawnFinalizeSleepsNeverCloses(t *testing.T) {
	s, got, _ := stopRun(t, signaled(idleRespawnDrainReason, "pool_managed", "true"), undesired(drainOrphaned), nil)
	if s.Outcome != settledLanded || got.Status == "closed" || got.Metadata["state"] != string(session.StateAsleep) || got.Metadata["sleep_reason"] != drainIdle {
		t.Fatalf("settlement %+v, status %s, row %v; want asleep idle and open", s, got.Status, got.Metadata)
	}
}

// TestRespawnEligiblePolicyGates (v5 D2). Kills an eligibility that admits a
// policy legacy's idle respawn never runs on: a non-interactive or
// non-Full-capability policy, or none, is never eligible, whatever the row's
// detached_at (A6 clears it for such a policy, so A20's decide never shows
// the gate alone).
func TestRespawnEligiblePolicyGates(t *testing.T) {
	full := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityFull, Effective: "10m", Duration: 10 * time.Minute}
	for _, c := range []struct {
		name     string
		policy   *resolvedSessionSleepPolicy
		eligible bool
	}{
		{"interactive, Full", &full, true},
		{"non-interactive", &resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive, Capability: runtime.SessionSleepCapabilityFull, Duration: 10 * time.Minute}, false},
		{"not Full", &resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityTimedOnly, Duration: 10 * time.Minute}, false},
		{"no policy", nil, false},
	} {
		w, a := rowWorld(t, drainRow(respawnRow()...))
		w.SleepPolicies = map[string]resolvedSessionSleepPolicy{}
		if c.policy != nil {
			w.SleepPolicies["gc-1"] = *c.policy
		}
		e := a.Snapshot.Entries[rowKeyOf("gc-1")]
		awakeForWork(e)
		r := &rowFacts{w: w, k: rowKeyOf("gc-1"), entry: e, row: w.Census.Rows[rowKeyOf("gc-1")]}
		if got := respawnEligible(r); got != c.eligible {
			t.Errorf("%s: eligible %t, want %t", c.name, got, c.eligible)
		}
	}
}

// hangingListStore is a work leg whose assignee reads block until released:
// a claim read the probe's bound gives up on.
type hangingListStore struct {
	beads.Store
	release chan struct{}
}

func (s hangingListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return s.Store.List(q)
}

func (s hangingListStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	<-s.release
	return s.Store.ListByAssignee(assignee, status, limit)
}

// TestIdleRespawnLostRevalidationVoids (#7688 review, m30). Kills a lost
// revalidation that signals: a claim read that hangs past the probe's bound
// leaves the revalidation unchecked, which voids the drain in the signal's
// stead, never a stop of a seat whose claim is unknown.
func TestIdleRespawnLostRevalidationVoids(t *testing.T) {
	meta := respawnRow(append(intentBegunAt(idleRespawnDrainReason, time.Hour), idleRespawnAttemptsMetadataKey, "1", idleRespawnBeadIDMetadataKey, "gw-1")...)
	p, it, store, _ := admittedBeginOn(t, meta, awakeForWork, napsFull)
	if it.Kind != intentSignalFresh {
		t.Fatalf("decided %+v, want the respawn signal", it)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p.reads.legs = testWorkLegs(t.TempDir(), &config.City{Agents: []config.Agent{{Name: "worker"}}}, hangingListStore{Store: store, release: release}, nil)
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	got, _ := store.Get(it.Key.ID)
	if s.Outcome != settledLanded || s.Cause != causeIdleRespawnVoid+"unchecked" || isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("settlement %+v, row %v; want the drain voided unchecked, no signal", s, got.Metadata)
	}
}

// TestIdleRespawnAgentAckSignalsUnrevalidated (#7688 review, m31; legacy's
// DRAIN-004 covers only the acks it wrote). Kills a revalidation of an
// agent's own ack: an acked respawn drain signals (signal-ack) even over a
// claim the controller's own signal would void on.
func TestIdleRespawnAgentAckSignalsUnrevalidated(t *testing.T) {
	meta := respawnRow(append(intentBegunAt(idleRespawnDrainReason, time.Minute), session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, rowAt(-time.Second))...)
	p, it, store, _ := admittedBeginOn(t, meta, awakeForWork, napsFull)
	if it.Kind != intentSignalAck {
		t.Fatalf("decided %+v, want the ack signal", it)
	}
	claimWork(t, store, it.Key.ID)
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	got, _ := store.Get(it.Key.ID)
	if s.Outcome != settledLanded || !isDrainAckStopPendingInfo(censusRowOf(t, got).Info) {
		t.Fatalf("settlement %+v, row %v; want the agent's ack signaled", s, got.Metadata)
	}
}

// TestIdleRespawnFinalizeIsTheDrainCompletion (#7688 review, m38). Kills a
// respawn finalize built from SleepPatch: legacy's drain completion
// (CompleteDrainPatch) resets a fresh-wake row's conversation, so the
// respawn starts it fresh.
func TestIdleRespawnFinalizeIsTheDrainCompletion(t *testing.T) {
	s, got, _ := stopRun(t, signaled(idleRespawnDrainReason, "pool_managed", "true", "wake_mode", "fresh", "session_key", "key-1"), undesired(drainOrphaned), nil)
	if s.Outcome != settledLanded || got.Metadata["session_key"] != "" || got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("settlement %+v, row %v; want the drain completion's fresh-wake reset", s, got.Metadata)
	}
}
