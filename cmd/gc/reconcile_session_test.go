package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The session controller's tests (P4 spec §4 P4.1a). Decide tests call
// decideSession on data; controller tests run one reconcile over a MemStore
// whose writes are counted and a recording runtime.Fake.

var sessNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// sessRow is a known-state session row whose runtime is s-<id>.
func sessRow(id string, mutate ...func(*session.Info)) session.Info {
	row := session.Info{ID: id, Template: "worker", MetadataState: "asleep", SessionName: "s-" + id, SessionNameMetadata: "s-" + id, Generation: "3"}
	for _, m := range mutate {
		m(&row)
	}
	return row
}

// sessInputs is a ranked, observed row: its entry matches its incarnation and
// its runtime is known absent.
func sessInputs(row session.Info) sessionInputs {
	k := rowKey{Leg: routerTestLeg, ID: row.ID}
	e := &selectionEntry{Key: k, Desired: desireWake, Basis: rowBasis{Incarnation: rowIncarnation(row)}}
	return sessionInputs{
		Key: k, Row: row, Found: true, Now: sessNow,
		Snap:  &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: e}},
		Entry: e,
		Obs:   rowObservation{Liveness: livenessAbsent},
	}
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func decided(t *testing.T, in sessionInputs) sessionDecision {
	t.Helper()
	d, ask := decideSession(in, probeAnswers{})
	if ask != 0 {
		t.Fatalf("decide asked probes %b; no arm in this slice needs one", ask)
	}
	return d
}

// Kills: a write, store read, provider call, clock read or lock in the
// decide's file, or a nondeterministic decision.
func TestSessionDecideIsPure(t *testing.T) {
	cases := []sessionInputs{
		sessInputs(sessRow("a", func(r *session.Info) { r.HeldUntil = rfc(sessNow.Add(-time.Minute)); r.SleepReason = "user-hold" })),
		sessInputs(sessRow("b", func(r *session.Info) {
			r.MetadataState, r.UnknownStateFirstSeen, r.UnknownStateValue = "archived", rfc(sessNow.Add(-time.Hour)), "archived"
		})),
		sessInputs(sessRow("c", func(r *session.Info) { r.UnknownStateValue, r.UnknownStateEscalatedAt = "x", rfc(sessNow) })),
	}
	for _, in := range cases {
		first := decided(t, in)
		for range 20 {
			if got := decided(t, in); !reflect.DeepEqual(got.Action.Patch, first.Action.Patch) || got.Reason != first.Reason || got.RequeueAfter != first.RequeueAfter {
				t.Fatalf("%s: decide is nondeterministic: %+v then %+v", in.Row.ID, first, got)
			}
		}
	}
	checkPureFiles(t, []string{"reconcile_session_decide.go"})
}

// Kills: decide probing in arms that do not need it; a controller that
// probes more than twice, re-runs a probe it already ran, or acts on a
// decide still asking after its rounds.
func TestSessionDecideTwoPhaseProbesBoundedToTwoRounds(t *testing.T) {
	for _, in := range []sessionInputs{
		{Found: false},
		sessInputs(sessRow("k", func(r *session.Info) {
			r.MetadataState, r.StateReason, r.SleepReason, r.SleptAt = "asleep", session.KillPendingReason, "killed", rfc(sessNow)
		})),
		sessInputs(sessRow("u", func(r *session.Info) { r.MetadataState = "archived" })),
		sessInputs(sessRow("h", func(r *session.Info) { r.HeldUntil = rfc(sessNow.Add(-time.Minute)) })),
		func() sessionInputs { in := sessInputs(sessRow("l")); in.Obs.Liveness = livenessUnknown; return in }(),
		func() sessionInputs { in := sessInputs(sessRow("n")); in.Entry, in.Snap = nil, nil; return in }(),
	} {
		decided(t, in)
	}

	f := newSessionCtlFixture(t, sessBead("p", nil))
	f.sp.SetMeta("s-p", "x", "y") //nolint:errcheck // fake
	var decides, writes int
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		decides++
		d := sessionDecision{Reason: "probing", Action: sessionAction{Kind: actWrite, Patch: session.MetadataPatch{"x": "y"}}}
		switch ans.Asked {
		case 0:
			return d, probeAttach | probePending // round one asks both
		case probeAttach | probePending:
			return d, probePending // already answered: must not run again
		}
		return d, 0
	}
	if _, err := f.reconcile(t, "p"); err != nil {
		t.Fatal(err)
	}
	if decides != 3 || f.sp.CountCalls("Pending", "s-p") != 1 {
		t.Fatalf("decides=%d pending probes=%d, want 2 decides plus the commit's and the pending probe run once", decides, f.sp.CountCalls("Pending", "s-p"))
	}
	if writes = f.store.writes(); writes != 1 {
		t.Fatalf("writes = %d, want the decided write", writes)
	}

	f = newSessionCtlFixture(t, sessBead("q", nil))
	decides = 0
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		decides++
		d := sessionDecision{Reason: "probing", RequeueAfter: time.Hour, Action: sessionAction{Kind: actWrite, Patch: session.MetadataPatch{"x": "y"}}}
		return d, ans.Asked<<1 | probeAttach // always asks for one more
	}
	requeue, err := f.reconcile(t, "q")
	if err != nil {
		t.Fatal(err)
	}
	if decides != 3 || f.store.writes() != 0 || requeue != time.Hour {
		t.Fatalf("decides=%d writes=%d requeue=%s, want 3 decides (two probe rounds), no action and the timer kept", decides, f.store.writes(), requeue)
	}
}

// Kills: a desire-dependent arm reached for a row with no entry or before
// S_1, for an entry decided at an older incarnation, or past unknown
// liveness (arm 4, GUAR-053); row-local arms gated by the snapshot.
func TestSessionDecideUnrankedRowTakesRowLocalOnly(t *testing.T) {
	expired := func(r *session.Info) { r.HeldUntil = rfc(sessNow.Add(-time.Minute)) }
	ranked := sessInputs(sessRow("r", expired))
	noEntry := ranked
	noEntry.Entry = nil
	beforeS1 := ranked
	beforeS1.Snap, beforeS1.Entry = nil, nil
	lagging := ranked
	lagging.Entry = &selectionEntry{Key: ranked.Key, Desired: desireWake, Basis: rowBasis{Incarnation: 2}}
	unobserved := ranked
	unobserved.Obs.Liveness = livenessUnknown
	for _, c := range []struct {
		name string
		in   sessionInputs
		want string
	}{
		{"ranked", ranked, decideNoAction},
		{"no entry", noEntry, decideUnranked},
		{"before S_1", beforeS1, decideUnranked},
		{"incarnation lag", lagging, decideIncarnationLag},
		{"liveness unknown", unobserved, decideLivenessUnknown},
	} {
		d := decided(t, c.in)
		if d.Reason != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, d.Reason, c.want)
		}
		if d.Action.Kind != actWrite || d.Action.Patch["held_until"] != "" || d.Action.Authorize != nil {
			t.Errorf("%s: action %+v, want the row-local hold clear, not desire-dependent", c.name, d.Action)
		}
	}
}

// Kills: heal, start or close during `gc session kill` (the arm missing, or
// running after the timer heal); a fence never revisited at its expiry.
func TestSessionDecideKillFenceSkipsRow(t *testing.T) {
	in := sessInputs(sessRow("k", func(r *session.Info) {
		r.StateReason, r.SleepReason, r.SleptAt = session.KillPendingReason, "killed", rfc(sessNow.Add(-time.Minute))
		r.HeldUntil = rfc(sessNow.Add(-time.Second))
	}))
	d := decided(t, in)
	if d.Reason != decideKillFence || d.Action.Kind != actNone {
		t.Fatalf("decision %+v, want a kill-fence skip with no action", d)
	}
	if want := session.KillPendingGrace - time.Minute; d.RequeueAfter != want {
		t.Fatalf("requeue %s, want the fence's expiry in %s", d.RequeueAfter, want)
	}
	in.Now = sessNow.Add(session.KillPendingGrace)
	if d := decided(t, in); d.Reason == decideKillFence {
		t.Fatal("an aged-out fence still skips the row")
	}
}

// Kills: a write racing an in-flight effect's commit (C5.5); a key never
// revisited if the effect's completion were lost.
func TestSessionDecideEffectInFlightIsReadOnly(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("e", map[string]string{"held_until": rfc(time.Now().Add(-time.Minute))}))
	release := make(chan struct{})
	defer close(release)
	k := rowKey{Leg: routerTestLeg, ID: "e"}
	deadline := time.Now().Add(time.Hour)
	if err := f.exec.submit(k, sessionEffect{Kind: effectStart, Deadline: deadline, Run: func(context.Context) error { <-release; return nil }, Settle: func(error) {}}); err != nil {
		t.Fatal(err)
	}
	requeue, err := f.reconcile(t, "e")
	if err != nil {
		t.Fatal(err)
	}
	if f.store.writes() != 0 {
		t.Fatal("decide wrote while an effect was in flight")
	}
	if requeue <= 0 || requeue > time.Hour {
		t.Fatalf("requeue %s, want the effect's deadline", requeue)
	}
}

// Kills: a replayed row (no due deadline) probing or calling the provider,
// or writing: replays arrive several times a second at maintainer-city scale.
func TestSessionReplayMakesNoProviderCall(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("r", map[string]string{"held_until": rfc(time.Now().Add(time.Hour))}))
	k := rowKey{Leg: routerTestLeg, ID: "r"}
	f.snap = &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Key: k, Desired: desireWake, Basis: rowBasis{Incarnation: 3}}}}
	// A complete inventory pass that lists nothing: the row's liveness is
	// known, so the replay runs every arm through the desire gate.
	now := time.Now()
	f.obs.PublishInventory(InventoryPass{
		Epoch: "epoch", Seq: 1, ProviderGen: 1, StartedAt: now, FinishedAt: now,
		Backends: []BackendPass{{Label: "default", Outcome: OutcomeComplete, Attested: true}},
	}, nil)
	for range 3 {
		if _, err := f.reconcile(t, "r", routeReasonReplay); err != nil {
			t.Fatal(err)
		}
	}
	if d := f.lastDecision(); d.Reason != decideNoAction {
		t.Fatalf("replay decided %q, want every arm reached (%q)", d.Reason, decideNoAction)
	}
	if calls := f.sp.Calls; len(calls) != 0 || f.store.writes() != 0 {
		t.Fatalf("a replay made provider calls %v and %d writes, want none", calls, f.store.writes())
	}
}

// Kills: a missed timer: a running hold or quarantine with no requeue at
// its expiry, or a clear that never comes when it is due.
func TestSessionDeadlineReevaluatedOnTimer(t *testing.T) {
	in := sessInputs(sessRow("t", func(r *session.Info) {
		r.HeldUntil, r.QuarantinedUntil = rfc(sessNow.Add(10*time.Minute)), rfc(sessNow.Add(time.Hour))
		r.SleepReason = "user-hold"
	}))
	d := decided(t, in)
	if d.Action.Kind != actNone || d.RequeueAfter != 10*time.Minute+time.Second {
		t.Fatalf("decision %+v, want no write and a requeue just past the hold", d)
	}
	in.Now = in.Now.Add(d.RequeueAfter)
	d = decided(t, in)
	want := session.MetadataPatch{"held_until": "", "sleep_reason": ""}
	if !maps.Equal(d.Action.Patch, want) || d.RequeueAfter <= 0 {
		t.Fatalf("at the hold's expiry: %+v, want patch %v and a requeue for the quarantine", d, want)
	}
	// An expired quarantine is judged on the post-hold sleep_reason.
	in.Row.QuarantinedUntil, in.Row.SleepReason = rfc(sessNow), "quarantine"
	in.Row.HeldUntil = ""
	if d := decided(t, in); d.Action.Patch["sleep_reason"] != "" || d.Action.Patch["wake_attempts"] != "0" {
		t.Fatalf("quarantine clear %v, want sleep_reason and counters cleared", d.Action.Patch)
	}
}

// Kills: a write decided from one read and fenced on another (C0.6, C2.9),
// a lost CAS that writes anyway or is not re-decided at once, and a blind
// write on a store with no conditional writer (C0.7).
func TestSessionWriteCASFirstReDecidesOnRefusal(t *testing.T) {
	expired := map[string]string{"held_until": rfc(time.Now().Add(-time.Minute))}

	// A `gc session kill` lands after the gather read: the commit decides
	// again on the read its CAS uses, and writes nothing.
	f := newSessionCtlFixture(t, sessBead("c", expired))
	f.store.beforeGet = func(n int) {
		if n == 2 {
			_ = f.store.MemStore.SetMetadataBatch("c", session.KillPendingPatch(time.Now())) //nolint:errcheck // MemStore
		}
	}
	if _, err := f.reconcile(t, "c", wakeReasonSocket); err != nil {
		t.Fatal(err)
	}
	if f.store.casWrites != 0 || f.lastDecision().Reason != decideKillFence {
		t.Fatalf("cas writes=%d last decision %q, want none and the kill fence seen at commit", f.store.casWrites, f.lastDecision().Reason)
	}

	// Another writer lands between that read and the CAS: refused, and the
	// key re-decides at once.
	f = newSessionCtlFixture(t, sessBead("c", expired))
	f.store.beforeCAS = func() {
		_ = f.store.MemStore.SetMetadataBatch("c", map[string]string{"nudge": "1"}) //nolint:errcheck // MemStore
	}
	requeue, err := f.reconcile(t, "c")
	if err != nil || requeue != time.Nanosecond || f.store.casWrites != 0 {
		t.Fatalf("lost CAS: err=%v requeue=%s writes=%d, want a refusal re-decided at once", err, requeue, f.store.casWrites)
	}
	f.store.beforeCAS = nil
	if _, err := f.reconcile(t, "c"); err != nil || f.store.casWrites != 1 {
		t.Fatalf("re-decide: err=%v writes=%d, want the hold cleared", err, f.store.casWrites)
	}

	// No conditional writer: refuse, never write blind.
	f = newSessionCtlFixture(t, sessBead("c", expired))
	f.store = &sessCountingStore{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{sessBead("c", expired)}, nil)}
	if _, err := f.reconcile(t, "c"); err == nil || f.store.blindWrites != 0 || f.store.casWrites != 0 {
		t.Fatalf("unfenced store: err=%v blind=%d cas=%d, want a refusal and no write", err, f.store.blindWrites, f.store.casWrites)
	}
	if f.store.liveGets != 0 {
		t.Fatalf("live reads = %d, want none: an operator poke reads through the cache too", f.store.liveGets)
	}
}

// Kills: a write after the worker's context ended (an abandoned worker
// writing after stop), the context checked before the row lock rather than
// inside it, and a desire-dependent write the latest entry no longer
// authorizes (§4.3 rule 1).
func TestWritesHoldRowLockAndCheckCtx(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("w", map[string]string{"held_until": rfc(time.Now().Add(-time.Minute))}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var locked []string
	f.ctl.lockRow = func(id string, fn func() error) error {
		return session.WithSessionMutationLock(id, func() error {
			locked = append(locked, id)
			cancel() // the runtime stops while this worker waited for the lock
			return fn()
		})
	}
	_, err := f.ctl.reconcile(ctx, f.env(), workqueue.Item[rowKey]{Key: rowKey{Leg: routerTestLeg, ID: "w"}})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(locked, []string{"w"}) {
		t.Fatalf("reconcile = %v under locks %v, want the canceled context seen inside row w's lock", err, locked)
	}
	if f.store.writes() != 0 {
		t.Fatal("an abandoned worker wrote after its context ended")
	}
	f.ctl.lockRow = session.WithSessionMutationLock

	k := rowKey{Leg: routerTestLeg, ID: "w"}
	f.snap = &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Desired: desireSleep, Basis: rowBasis{Incarnation: 3}}}}
	f.ctl.decide = func(sessionInputs, probeAnswers) (sessionDecision, probeKinds) {
		return sessionDecision{Action: sessionAction{Kind: actWrite, Patch: session.MetadataPatch{"x": "y"}, Authorize: func(e *selectionEntry) bool {
			return e.Desired == desireDrain
		}}}, 0
	}
	requeue, err := f.reconcile(t, "w")
	if err != nil || f.store.writes() != 0 || requeue != time.Nanosecond {
		t.Fatalf("unauthorized write: err=%v writes=%d requeue=%s, want refused and re-decided", err, f.store.writes(), requeue)
	}
	f.snap.Entries[k].Desired = desireDrain
	if _, err := f.reconcile(t, "w"); err != nil || f.store.writes() != 1 {
		t.Fatalf("authorized write: err=%v writes=%d, want it written", err, f.store.writes())
	}
}

// Kills: a pending Yes noted into I3 that never re-runs selection (P4 F12),
// and an allocator wake for a note that changed nothing.
func TestNoteFlipWakesAllocator(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("n", nil))
	if err := f.sp.Start(context.Background(), "s-n", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.sp.PendingInteractions = map[string]*runtime.PendingInteraction{"s-n": {RequestID: "r1", Kind: "approval"}}
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		if ans.Asked == 0 {
			return sessionDecision{}, probePending
		}
		return sessionDecision{Reason: "probed"}, 0
	}
	for range 2 {
		if _, err := f.reconcile(t, "n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.obs.Snapshot().ByName["s-n"].PendingInteraction.Value; got != ObsYes {
		t.Fatalf("I3 pending = %v, want the probe's Yes", got)
	}
	if !reflect.DeepEqual(f.wakes, []string{v2ReasonNote}) {
		t.Fatalf("allocator wakes %v, want one for the flip and none for the repeat", f.wakes)
	}
}

// Kills: SESS-044..047 drift: an unknown state that writes outside the
// throttle markers, emits on every pass, never escalates, or leaves stale
// markers on a known-state row.
func TestSessionDecideUnknownStateDiagnostics(t *testing.T) {
	unknown := sessInputs(sessRow("u", func(r *session.Info) { r.MetadataState = "draining" }))
	d := decided(t, unknown)
	if d.Event == nil || d.Event.Type != events.SessionUnknownState || d.RequeueAfter != unknownStateEscalationAge ||
		!maps.Equal(d.Action.Patch, session.MetadataPatch{unknownStateFirstSeenKey: rfc(sessNow), unknownStateValueKey: "draining"}) {
		t.Fatalf("first sight: %+v", d)
	}
	unknown.Row.UnknownStateFirstSeen, unknown.Row.UnknownStateValue = rfc(sessNow.Add(-time.Minute)), "draining"
	if d := decided(t, unknown); d.Action.Kind != actNone || d.Event != nil || d.RequeueAfter != unknownStateEscalationAge-time.Minute+time.Second {
		t.Fatalf("same state within 30m: %+v, want silence and a requeue at escalation", d)
	}
	unknown.Now = sessNow.Add(unknownStateEscalationAge)
	if d := decided(t, unknown); d.Event == nil || d.Action.Patch[unknownStateEscalatedKey] == "" {
		t.Fatalf("past 30m: %+v, want one escalation", d)
	}
	known := sessInputs(sessRow("k", func(r *session.Info) { r.UnknownStateFirstSeen, r.UnknownStateValue = rfc(sessNow), "draining" }))
	if d := decided(t, known); !maps.Equal(d.Action.Patch, session.MetadataPatch{unknownStateFirstSeenKey: "", unknownStateValueKey: ""}) {
		t.Fatalf("known state with markers: %v, want them cleared", d.Action.Patch)
	}
}

// Kills: a closed row reconciled: timer heals or unknown-state markers
// written onto a closed row (arm 0a).
func TestSessionDecideClosedRowTakesNoAction(t *testing.T) {
	closed := sessInputs(sessRow("c", func(r *session.Info) {
		r.Closed, r.HeldUntil, r.UnknownStateValue = true, rfc(sessNow.Add(-time.Minute)), "draining"
	}))
	if d := decided(t, closed); d.Reason != decideNoRow || d.Action.Kind != actNone || d.RequeueAfter != 0 {
		t.Fatalf("closed row: %+v, want no-row and nothing written", d)
	}
}

// Kills: the commit point acting on a probe it asked for but this reconcile
// never ran, and an unasked pending answer read as the type's zero value,
// No: a runtime with a pending interaction then drains (C0.6).
func TestSessionCommitNeedingUnrunProbeWritesNothing(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("p", nil))
	if err := f.sp.Start(context.Background(), "s-p", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.sp.PendingInteractions = map[string]*runtime.PendingInteraction{"s-p": {RequestID: "r1", Kind: "approval"}}
	calls := 0
	var commitAns probeAnswers
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		calls++
		if calls == 1 { // the gather read: a write that needs no probe
			return sessionDecision{Reason: "a", Action: sessionAction{Kind: actWrite, Patch: session.MetadataPatch{"x": "a"}}}, 0
		}
		commitAns = ans // the commit read: needs the pending probe
		if ans.Pending == pendingInteractionNo {
			return sessionDecision{Reason: "b", Action: sessionAction{Kind: actWrite, Patch: session.MetadataPatch{"drain": "begin"}}}, probePending
		}
		return sessionDecision{Reason: "b-held"}, probePending
	}
	requeue, err := f.reconcile(t, "p")
	if err != nil {
		t.Fatal(err)
	}
	if f.store.writes() != 0 || requeue != time.Nanosecond {
		t.Fatalf("writes=%d requeue=%s, want nothing written on an unrun probe and a re-decide at once", f.store.writes(), requeue)
	}
	if commitAns.Asked != 0 || commitAns.Pending != pendingInteractionUnknown {
		t.Fatalf("commit answers %+v, want nothing asked and pending unknown", commitAns)
	}
}

// Kills: probing, or trusting the composite, while the row's route is
// unknown (R28): no probe reaches either backend, nothing is noted into I3,
// and the decide reads pending unknown and attach not known.
func TestSessionProbeUnknownRouteProbesNothing(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("p", nil))
	tmux, acp := runtime.NewFake(), runtime.NewFake()
	if err := tmux.Start(context.Background(), "s-p", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	tmux.PendingInteractions = map[string]*runtime.PendingInteraction{"s-p": {RequestID: "r1", Kind: "approval"}}
	before := len(tmux.Calls)
	var got probeAnswers
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		got = ans
		return sessionDecision{Reason: "probing"}, probePending | probeAttach
	}
	env := &reconcileEnv{Gen: 1, Cfg: &config.City{}, SP: auto.New(tmux, acp)} // never seeded
	if _, err := f.ctl.reconcile(context.Background(), env, workqueue.Item[rowKey]{Key: rowKey{Leg: routerTestLeg, ID: "p"}}); err != nil {
		t.Fatal(err)
	}
	if len(tmux.Calls) != before || len(acp.Calls) != 0 {
		t.Fatalf("probed under an unknown route: %v %v", tmux.Calls[before:], acp.Calls)
	}
	if got.Pending != pendingInteractionUnknown || got.AttachKnown {
		t.Fatalf("answers %+v, want pending unknown and attach not known", got)
	}
	if o := f.obs.Snapshot().ByName["s-p"]; o.PendingInteraction.Value != ObsUnknown || o.Attached.Value != ObsUnknown || len(f.wakes) != 0 {
		t.Fatalf("I3 %+v wakes %v, want nothing noted", o, f.wakes)
	}
}

// Kills: a failed attach probe noted into I3 as detached, which the
// allocator would read as an idle runtime it may drain.
func TestSessionFailedAttachProbeNotNoted(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("p", nil))
	if err := f.sp.Start(context.Background(), "s-p", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.sp.AttachedErrors["s-p"] = errors.New("tmux: no server")
	var got probeAnswers
	f.ctl.decide = func(_ sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		got = ans
		return sessionDecision{Reason: "probing"}, probeAttach
	}
	if _, err := f.reconcile(t, "p"); err != nil {
		t.Fatal(err)
	}
	if got.Asked != probeAttach || got.AttachKnown {
		t.Fatalf("answers %+v, want the attach probe run and not known", got)
	}
	if v := f.obs.Snapshot().ByName["s-p"].Attached.Value; v != ObsUnknown || len(f.wakes) != 0 {
		t.Fatalf("I3 attached = %v, wakes %v, want nothing noted", v, f.wakes)
	}
}

// Kills: an unknown state's first-sight event never recorded once its
// markers are written (SESS-044), or recorded again on every pass.
func TestSessionUnknownStateEventRecordedOnce(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("u", map[string]string{"state": "draining"}))
	rec := events.NewFake()
	f.ctl.host.rec = rec
	for range 2 {
		if _, err := f.reconcile(t, "u"); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionUnknownState || f.store.writes() != 1 {
		t.Fatalf("events %+v after %d writes, want one unknown-state event with its marker write", rec.Events, f.store.writes())
	}
}

// Kills: a decision never traced, a standing skip traced on every pass, and
// a missing row traced on every reconcile (C0.5, S10).
func TestSessionTraceOnChangeOnly(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("t", map[string]string{"held_until": rfc(time.Now().Add(time.Hour))}))
	for range 2 {
		if _, err := f.reconcile(t, "t"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.traces.Load(); got != 1 {
		t.Fatalf("traces = %d after two identical reconciles, want 1", got)
	}
	for range 2 {
		if _, err := f.reconcile(t, "gone"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.traces.Load(); got != 2 {
		t.Fatalf("traces = %d after two reconciles of a missing row, want one more", got)
	}
}

// Kills: the commit path working only over MemStore: over the production
// CachingStore a write must land through the cache's conditional writer, and
// a cached row gone stale under an out-of-band write must be refused at the
// CAS, re-decided at once, and then written on the fresh read (C2.9).
func TestSessionWriteOverCachingStore(t *testing.T) {
	expired := map[string]string{"held_until": rfc(time.Now().Add(-time.Minute))}
	mem := beads.NewMemStoreFrom(0, []beads.Bead{sessBead("c", expired)}, nil)
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := newSessionCtlFixture(t)
	f.ctl.host.sessionsStore = func() beads.Store { return cache }
	if err := mem.SetMetadataBatch("c", map[string]string{"nudge": "1"}); err != nil { // no event: the cache is stale
		t.Fatal(err)
	}
	requeue, err := f.reconcile(t, "c")
	if err != nil || requeue != time.Nanosecond {
		t.Fatalf("stale cached row: err=%v requeue=%s, want the CAS refused and a re-decide at once", err, requeue)
	}
	if b, _ := mem.Get("c"); b.Metadata["held_until"] == "" {
		t.Fatal("a write fenced on a stale cached revision landed")
	}
	if _, err := f.reconcile(t, "c"); err != nil {
		t.Fatal(err)
	}
	if b, _ := mem.Get("c"); b.Metadata["held_until"] != "" || b.Metadata["nudge"] != "1" {
		t.Fatalf("after the re-decide: %v, want the hold cleared and the other writer's key kept", b.Metadata)
	}
}

// sessBead is session row id as a bead, known state asleep, generation 3.
func sessBead(id string, meta map[string]string) beads.Bead {
	m := map[string]string{"session_name": "s-" + id, "state": "asleep", "template": "worker", "generation": "3"}
	maps.Copy(m, meta)
	return routerSessionBead(id, m)
}

// sessionCtlFixture is one session controller over a counted MemStore, a
// recording Fake and a real observation cache.
type sessionCtlFixture struct {
	store  *sessCountingStore
	sp     *runtime.Fake
	obs    *ObservationCache
	exec   *effectExecutor
	ctl    *sessionController
	snap   *selectionSnapshot
	wakes  []string
	traces atomic.Int32 // traces begun

	mu        sync.Mutex
	decisions []sessionDecision
}

func newSessionCtlFixture(t *testing.T, rows ...beads.Bead) *sessionCtlFixture {
	t.Helper()
	f := &sessionCtlFixture{
		store: &sessCountingStore{MemStore: beads.NewMemStoreFrom(0, rows, nil)},
		sp:    runtime.NewFake(),
		obs:   NewObservationCache(clock.Real{}, time.Minute, "epoch"),
	}
	rt := newV2Runtime(v2Host{
		sessionsLeg:   routerTestLeg,
		sessionsStore: func() beads.Store { return f.store },
		observations:  func() *ObservationCache { return f.obs },
		beginTrace:    func(string) *sessionReconcilerTraceCycle { f.traces.Add(1); return nil },
		rec:           events.Discard,
		stderr:        io.Discard,
	}, v2Controllers{}, newV2Metrics())
	if err := beads.StampOpenedStore(f.store.MemStore, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.exec = rt.exec
	t.Cleanup(func() { f.exec.stop(time.Now()) })
	f.ctl = newSessionController(rt, func() *selectionSnapshot { return f.snap })
	f.ctl.wakeAllocator = func(kind string) { f.wakes = append(f.wakes, kind) }
	f.ctl.decide = func(in sessionInputs, ans probeAnswers) (sessionDecision, probeKinds) {
		d, ask := decideSession(in, ans)
		f.mu.Lock()
		f.decisions = append(f.decisions, d)
		f.mu.Unlock()
		return d, ask
	}
	return f
}

func (f *sessionCtlFixture) env() *reconcileEnv {
	return &reconcileEnv{Gen: 1, Cfg: &config.City{}, SP: f.sp}
}

func (f *sessionCtlFixture) reconcile(t *testing.T, id string, reasons ...string) (time.Duration, error) {
	t.Helper()
	it := workqueue.Item[rowKey]{Key: rowKey{Leg: routerTestLeg, ID: id}}
	for _, r := range reasons {
		it.Reasons = append(it.Reasons, workqueue.Reason{Kind: r})
	}
	return f.ctl.reconcile(context.Background(), f.env(), it)
}

func (f *sessionCtlFixture) lastDecision() sessionDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.decisions) == 0 {
		return sessionDecision{}
	}
	return f.decisions[len(f.decisions)-1]
}

// sessCountingStore is a MemStore (carrying its conditional-writes stamp)
// that counts CAS writes, blind writes and live reads, with hooks before
// each Get and each CAS.
type sessCountingStore struct {
	*beads.MemStore
	mu                     sync.Mutex
	casWrites, blindWrites int
	liveGets, gets         int
	beforeGet              func(n int)
	beforeCAS              func()
}

func (s *sessCountingStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.casWrites + s.blindWrites
}

func (s *sessCountingStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	s.gets++
	n, hook := s.gets, s.beforeGet
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return s.MemStore.Get(id)
}

func (s *sessCountingStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.beforeCAS != nil {
		s.beforeCAS()
	}
	if err := s.MemStore.UpdateIfMatch(id, rev, opts); err != nil {
		return err
	}
	s.mu.Lock()
	s.casWrites++
	s.mu.Unlock()
	return nil
}

func (s *sessCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.mu.Lock()
	s.blindWrites++
	s.mu.Unlock()
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *sessCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.mu.Lock()
	s.blindWrites++
	s.mu.Unlock()
	return s.MemStore.Update(id, opts)
}

// Handles counts live reads.
func (s *sessCountingStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.MemStore)
	h.Live = sessLiveReader{LiveReader: h.Live, s: s}
	return h
}

type sessLiveReader struct {
	beads.LiveReader
	s *sessCountingStore
}

func (r sessLiveReader) Get(id string) (beads.Bead, error) {
	r.s.mu.Lock()
	r.s.liveGets++
	r.s.mu.Unlock()
	return r.LiveReader.Get(id)
}
