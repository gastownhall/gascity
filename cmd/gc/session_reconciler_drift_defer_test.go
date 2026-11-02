package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinRefreshInterval
// verifies that a second deferral with the same drift key, taken well within
// the refresh interval, does NOT re-stamp the deferred_at timestamp.
//
// On parent (pre-fix), recordSessionAttachedConfigDriftDeferral rewrote
// deferred_at once the existing stamp was older than the 30s validity window /
// 2 = 15s. Because the patrol interval (30s default) exceeds that, it re-stamped
// on essentially every reconcile tick, producing a bead.updated event — and a
// Dolt commit — on every attached session bead with persistent drift. This test
// fails on parent and passes after the fix.
func TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteWithinRefreshInterval(t *testing.T) {
	env := newReconcilerTestEnv(t)
	sess := env.createSessionInfo("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(sess, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first := env.sessionInfo(sess.ID)
	firstStamp := first.AttachedConfigDriftDeferredAt
	if firstStamp == "" {
		t.Fatal("first call must stamp deferred_at")
	}
	if first.AttachedConfigDriftDeferredKey != driftKey {
		t.Fatalf("first key = %q, want %q", first.AttachedConfigDriftDeferredKey, driftKey)
	}

	// Advance the clock well within the refresh interval (2m; advance 5s).
	env.clk.Time = env.clk.Time.Add(5 * time.Second)

	if err := recordSessionAttachedConfigDriftDeferral(first, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("second record: %v", err)
	}
	secondStamp := env.sessionInfo(sess.ID).AttachedConfigDriftDeferredAt
	if secondStamp != firstStamp {
		t.Fatalf("deferred_at must not be re-stamped within the refresh interval; got %q want unchanged %q",
			secondStamp, firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_RewritesWhenKeyChanges verifies
// that a different drift key forces a rewrite even within the TTL window — the
// guard must only suppress writes for the same drift situation, not for
// genuinely new drift.
func TestRecordSessionAttachedConfigDriftDeferral_RewritesWhenKeyChanges(t *testing.T) {
	env := newReconcilerTestEnv(t)
	sess := env.createSessionInfo("worker", "worker")

	if err := recordSessionAttachedConfigDriftDeferral(sess, sessionFrontDoor(env.store), env.clk, "key-A"); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first := env.sessionInfo(sess.ID)
	firstStamp := first.AttachedConfigDriftDeferredAt

	env.clk.Time = env.clk.Time.Add(5 * time.Second)

	if err := recordSessionAttachedConfigDriftDeferral(first, sessionFrontDoor(env.store), env.clk, "key-B"); err != nil {
		t.Fatalf("second record: %v", err)
	}
	second := env.sessionInfo(sess.ID)
	if second.AttachedConfigDriftDeferredKey != "key-B" {
		t.Fatalf("key after key-change call = %q, want key-B", second.AttachedConfigDriftDeferredKey)
	}
	if second.AttachedConfigDriftDeferredAt == firstStamp {
		t.Fatalf("deferred_at must be re-stamped on key change; got unchanged %q", firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteAcrossManyTicks
// verifies the churn fix: with the default 30s patrol interval, the deferral
// is re-observed every tick, but record() must NOT rewrite the stamp on each
// one. The refresh interval is decoupled from (and much larger than) the
// patrol interval, so a long run of same-key ticks produces no metadata write
// (hence no Dolt commit) until the refresh interval elapses.
//
// This is the regression guard for the per-tick re-stamp churn: on the buggy
// version (rewrite once the stamp is older than the 30s validity limit / 2 =
// 15s), every 30s tick rewrote the stamp. It fails on that version and passes
// after the fix.
func TestRecordSessionAttachedConfigDriftDeferral_SkipsWriteAcrossManyTicks(t *testing.T) {
	env := newReconcilerTestEnv(t)
	sess := env.createSessionInfo("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(sess, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("first record: %v", err)
	}
	first := env.sessionInfo(sess.ID)
	firstStamp := first.AttachedConfigDriftDeferredAt

	// Simulate many reconciler ticks at the default 30s patrol interval, all
	// well within the refresh interval. None of them may rewrite the stamp.
	const patrol = 30 * time.Second
	cur := first
	for elapsed := patrol; elapsed < sessionAttachedConfigDriftRefreshInterval; elapsed += patrol {
		env.clk.Time = env.clk.Time.Add(patrol)
		if err := recordSessionAttachedConfigDriftDeferral(cur, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
			t.Fatalf("record at +%s: %v", elapsed, err)
		}
		cur = env.sessionInfo(sess.ID)
		if got := cur.AttachedConfigDriftDeferredAt; got != firstStamp {
			t.Fatalf("deferred_at re-stamped at +%s (got %q, want unchanged %q) — per-tick churn not eliminated",
				elapsed, got, firstStamp)
		}
	}

	// Once the existing stamp is older than the refresh interval, the next call
	// refreshes it so the deferral cannot age out of the false-negative window.
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval + time.Second)
	if err := recordSessionAttachedConfigDriftDeferral(cur, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("refresh record: %v", err)
	}
	if env.sessionInfo(sess.ID).AttachedConfigDriftDeferredAt == firstStamp {
		t.Fatalf("deferred_at must be refreshed past the refresh interval; got unchanged %q", firstStamp)
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_RefreshKeepsWithinValidityWindow
// exercises the actual read path (recentlyDeferredSessionAttachedConfigDrift)
// to prove the decoupling preserves the safety contract: a stamp that record()
// left un-refreshed because it was younger than the refresh interval must still
// be treated as valid by the reader. This catches regressions the constant-only
// invariant check would miss (wrong metadata key, reader still on the old 30s
// limit, off-by-one at the boundary).
func TestRecordSessionAttachedConfigDriftDeferral_RefreshKeepsWithinValidityWindow(t *testing.T) {
	// Structural invariant: the reader's validity window must exceed the
	// writer's refresh interval, or a just-skipped refresh could read as lapsed.
	if sessionAttachedConfigDriftRefreshInterval >= sessionAttachedConfigDriftFalseNegativeLimit {
		t.Fatalf("refresh interval (%s) must be < false-negative limit (%s) or the deferral can lapse between refreshes",
			sessionAttachedConfigDriftRefreshInterval, sessionAttachedConfigDriftFalseNegativeLimit)
	}

	env := newReconcilerTestEnv(t)
	sess := env.createSessionInfo("worker", "worker")
	const driftKey = "old-hash:new-hash"

	if err := recordSessionAttachedConfigDriftDeferral(sess, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("record: %v", err)
	}
	stamped := env.sessionInfo(sess.ID)

	// Just below the refresh interval: record() skips the rewrite. The reader
	// must still consider the deferral valid (this is the whole point of the
	// decoupling — a skipped refresh cannot create a false-negative gap).
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval - time.Second)
	if err := recordSessionAttachedConfigDriftDeferral(stamped, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
		t.Fatalf("record near refresh boundary: %v", err)
	}
	afterSkip := env.sessionInfo(sess.ID)
	if !recentlyDeferredSessionAttachedConfigDrift(afterSkip, env.clk, driftKey) {
		t.Fatal("deferral must read as valid just below the refresh interval (un-refreshed stamp)")
	}

	// Just below the validity limit (and a refresh has NOT happened since the
	// original stamp): the reader must still treat it as valid — proving the
	// window the reader uses really is the 5m limit, not the old 30s value.
	env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftFalseNegativeLimit - sessionAttachedConfigDriftRefreshInterval - time.Second)
	if !recentlyDeferredSessionAttachedConfigDrift(env.sessionInfo(sess.ID), env.clk, driftKey) {
		t.Fatal("deferral must read as valid just below the false-negative limit")
	}

	// Past the validity limit with no refresh: the reader must now treat it as
	// lapsed (so genuine post-detach drift can proceed).
	env.clk.Time = env.clk.Time.Add(2 * time.Second)
	if recentlyDeferredSessionAttachedConfigDrift(env.sessionInfo(sess.ID), env.clk, driftKey) {
		t.Fatal("deferral must read as lapsed past the false-negative limit")
	}
}

// TestRecordSessionAttachedConfigDriftDeferral_SurvivesSkippedRefreshThenFlicker
// guards the COMPOSED safety invariant that actually keeps an attached session
// from being drained mid-conversation:
//
//	sessionAttachedConfigDriftRefreshInterval + patrol < sessionAttachedConfigDriftFalseNegativeLimit
//
// The worst case the validity window must absorb is: record() SKIPS a rewrite
// because the stamp is just under the refresh interval, and then exactly one
// patrol tick later attachment detection flickers to "not attached". The reader
// at that moment sees stamp age = refreshInterval + patrol and must STILL read
// the deferral as valid, or a still-attached session is drained/restarted in the
// middle of a live conversation.
//
// patrol_interval is operator-configurable (cfg.Daemon.PatrolIntervalDuration,
// default 30s) with no upper clamp, so this is checked across a range of patrol
// values. The structural assertion catches a future constant change that shrinks
// the headroom; the behavioral assertion proves the reader actually honors it.
func TestRecordSessionAttachedConfigDriftDeferral_SurvivesSkippedRefreshThenFlicker(t *testing.T) {
	const driftKey = "old-hash:new-hash"
	// Worst-case stamp age the reader must tolerate is (refreshInterval + patrol):
	// a rewrite skipped at age just below refreshInterval, then one patrol tick.
	patrols := []time.Duration{30 * time.Second, 60 * time.Second, 90 * time.Second, 2 * time.Minute}
	for _, patrol := range patrols {
		worst := sessionAttachedConfigDriftRefreshInterval + patrol
		// Structural guard: the composed invariant, not just refresh < validity.
		if worst >= sessionAttachedConfigDriftFalseNegativeLimit {
			t.Fatalf("patrol=%s: refreshInterval(%s)+patrol(%s)=%s must be < falseNegativeLimit(%s); "+
				"a skipped refresh followed by an attachment flicker would drain a still-attached session",
				patrol, sessionAttachedConfigDriftRefreshInterval, patrol, worst,
				sessionAttachedConfigDriftFalseNegativeLimit)
		}

		// Behavioral guard: drive the real reader at exactly the worst-case age.
		env := newReconcilerTestEnv(t)
		sess := env.createSessionInfo("worker", "worker")
		if err := recordSessionAttachedConfigDriftDeferral(sess, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
			t.Fatalf("patrol=%s: record: %v", patrol, err)
		}
		stamped := env.sessionInfo(sess.ID)
		stamp0 := stamped.AttachedConfigDriftDeferredAt

		// Tick at age just under the refresh interval: record() must SKIP (stamp unchanged).
		env.clk.Time = env.clk.Time.Add(sessionAttachedConfigDriftRefreshInterval - time.Second)
		if err := recordSessionAttachedConfigDriftDeferral(stamped, sessionFrontDoor(env.store), env.clk, driftKey); err != nil {
			t.Fatalf("patrol=%s: record near refresh boundary: %v", patrol, err)
		}
		afterSkip := env.sessionInfo(sess.ID)
		if afterSkip.AttachedConfigDriftDeferredAt != stamp0 {
			t.Fatalf("patrol=%s: stamp must be unchanged just under the refresh interval", patrol)
		}

		// One patrol tick later attachment flickers detached: total age is now
		// (refreshInterval - 1s) + patrol < worst. The reader must still hold.
		env.clk.Time = env.clk.Time.Add(patrol)
		if !recentlyDeferredSessionAttachedConfigDrift(afterSkip, env.clk, driftKey) {
			t.Fatalf("patrol=%s: deferral must read as valid at age refreshInterval+patrol "+
				"(skipped refresh then flicker) or a still-attached session is drained", patrol)
		}
	}
}

// TestReconcileSessionBeads_ConfigDriftHeldWorkDeferralIsVisible kills the
// invisible held-work deferral (#8345): an ordinary session holding assigned
// work defers a real config-drift drain indefinitely — no age bound, no
// signal. The first deferral now records a durable episode (the drift key and
// time stamped on the session bead) and emits session.config_drift_deferred
// naming the session, its drifted fields, the observed work bead and the
// duration so far; a later tick observes the same episode without
// re-stamping or re-emitting; once the work closes, the drift drain proceeds
// exactly as before. The held work itself is never acted on.
func TestReconcileSessionBeads_ConfigDriftHeldWorkDeferralIsVisible(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	rec := &recordingRecorder{}
	env.rec = rec

	// The stored hash and breakdown are the desired config with Command alone
	// drifted, so the episode's diagnostics name Command and nothing else.
	tp := env.desiredState["worker"]
	current := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	storedCfg := current
	storedCfg.Command = "test-cmd"
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(storedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	startedHash, currentHash := runtime.CoreFingerprint(storedCfg), runtime.CoreFingerprint(current)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
		"core_hash_breakdown": string(breakdown),
	})

	// Hold the session with live in-progress work.
	work, err := env.store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("held work must still defer the drain, got %+v; stderr=%s", ds, env.stderr.String())
	}
	stamped := env.sessionInfo(session.ID)
	firstStamp := stamped.ConfigDriftDeferredAt
	if stamped.ConfigDriftDeferredKey != startedHash+":"+currentHash || firstStamp == "" {
		t.Fatalf("episode stamp = (%q, %q), want key %q at the first pass", stamped.ConfigDriftDeferredKey, firstStamp, startedHash+":"+currentHash)
	}
	firstAt, err := time.Parse(time.RFC3339, firstStamp)
	if err != nil || !firstAt.Equal(env.clk.Now()) {
		t.Fatalf("episode stamp %q, want the first pass's time %s", firstStamp, env.clk.Now())
	}
	evs := rec.ofType(events.SessionConfigDriftDeferred)
	if len(evs) != 1 {
		t.Fatalf("held-work deferral events = %d, want exactly the first observation; stderr=%s", len(evs), env.stderr.String())
	}
	ev := evs[0]
	if ev.SessionID != session.ID || ev.Subject != session.ID {
		t.Fatalf("event %+v, want it to name session %s", ev, session.ID)
	}
	if !strings.Contains(ev.Message, "Command") || !strings.Contains(ev.Message, work.ID) {
		t.Fatalf("message %q, want it to name the drifted field and work bead %s", ev.Message, work.ID)
	}
	var p api.SessionConfigDriftDeferredPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.SessionID != session.ID || p.SessionName != "worker" || p.Template != "worker" || p.WorkBeadID != work.ID || p.Escalated {
		t.Fatalf("payload %+v, want session %s/worker/worker held by work bead %s, first observation", p, session.ID, work.ID)
	}
	if !slices.Equal(p.DriftedFields, []string{"Command"}) {
		t.Fatalf("payload drifted_fields %v, want [Command]", p.DriftedFields)
	}
	if p.DeferredSince != firstStamp || p.PendingSeconds != 0 {
		t.Fatalf("payload deferred_since %q pending %d, want the episode's own stamp with nothing pending yet", p.DeferredSince, p.PendingSeconds)
	}

	// A later tick observes the same episode: no re-stamp, no second event,
	// and still no drain. The feed is re-read from the store, as the real
	// pass's snapshot is seeded from the store's rows.
	env.clk.Advance(time.Minute)
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	if again := env.sessionInfo(session.ID); again.ConfigDriftDeferredAt != firstStamp {
		t.Fatalf("episode stamp re-stamped to %q, want unchanged %q", again.ConfigDriftDeferredAt, firstStamp)
	}
	if evs := rec.ofType(events.SessionConfigDriftDeferred); len(evs) != 1 {
		t.Fatalf("held-work deferral events after the second tick = %d, want still 1", len(evs))
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("held work must keep deferring the drain, got %+v", ds)
	}

	// Once the work closes, the config-drift drain proceeds as before.
	if err := env.store.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}
	env.clk.Advance(time.Minute)
	fresh, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("drain = %+v, want the config-drift drain once the work closes; stderr=%s", ds, env.stderr.String())
	}
}

// TestReconcileSessionBeads_ConfigDriftHeldWorkDeferralEscalatesOnce bounds
// the held-work deferral (#8345): an episode that outlives
// [daemon] drift_deferral_escalate_after (1h by default) escalates exactly
// once — the durable config_drift_escalated_at marker counts — and the
// escalated session.config_drift_deferred event carries the episode's own
// start and its pending age, while the deferral itself still holds until
// the work closes. The drift defer seam's repro runs this same regression
// (TestDriftDeferStepOrdinarySeatOverWorkIsBounded).
func TestReconcileSessionBeads_ConfigDriftHeldWorkDeferralEscalatesOnce(t *testing.T) {
	reconcileHeldWorkDriftEpisodeBounded(t)
}

// reconcileHeldWorkDriftEpisodeBounded is the full reconciliation regression
// of the held-work deferral's bound (#8345): the episode stamps on its first
// observation, escalates exactly once past the drift_deferral_escalate_after
// horizon — the durable config_drift_escalated_at marker counts — never
// repeats, still defers the drain while the work stays open, and proceeds to
// the config-drift drain once the work closes.
func reconcileHeldWorkDriftEpisodeBounded(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	rec := &recordingRecorder{}
	env.rec = rec
	horizon := env.cfg.Daemon.DriftDeferralEscalateAfterDuration()

	tp := env.desiredState["worker"]
	current := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	storedCfg := current
	storedCfg.Command = "test-cmd"
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(storedCfg))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	startedHash := runtime.CoreFingerprint(storedCfg)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": startedHash,
		"core_hash_breakdown": string(breakdown),
	})

	work, err := env.store.Create(beads.Bead{
		Title:    "in-flight work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	env.reconcile([]beads.Bead{session})
	firstStamp := env.sessionInfo(session.ID).ConfigDriftDeferredAt
	if firstStamp == "" {
		t.Fatalf("the first pass must stamp the episode; stderr=%s", env.stderr.String())
	}

	// The horizon elapses with the work still open: the episode escalates
	// once, its marker set and its age reported, the deferral still holding.
	env.clk.Advance(horizon)
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	escalatedAt := env.sessionInfo(session.ID)
	if escalatedAt.ConfigDriftDeferredAt != firstStamp {
		t.Fatalf("escalation re-stamped the episode to %q, want the first observation %q", escalatedAt.ConfigDriftDeferredAt, firstStamp)
	}
	if marker := escalatedAt.ConfigDriftEscalatedAt; marker == "" {
		t.Fatal("the escalated episode must set its marker")
	} else if at, err := time.Parse(time.RFC3339, marker); err != nil || !at.Equal(env.clk.Now()) {
		t.Fatalf("marker %q, want the escalation pass's time %s", marker, env.clk.Now())
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("an escalated episode still defers the drain, got %+v", ds)
	}
	evs := rec.ofType(events.SessionConfigDriftDeferred)
	if len(evs) != 2 {
		t.Fatalf("held-work deferral events = %d, want the first observation and the one escalation; stderr=%s", len(evs), env.stderr.String())
	}
	ev := evs[1]
	var p api.SessionConfigDriftDeferredPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.DeferredSince != firstStamp {
		t.Fatalf("payload deferred_since %q, want the episode's own start %q", p.DeferredSince, firstStamp)
	}
	if !p.Escalated || p.PendingSeconds != int64(horizon/time.Second) {
		t.Fatalf("payload escalated %v pending %d, want the horizon reported once due", p.Escalated, p.PendingSeconds)
	}
	if p.WorkBeadID != work.ID {
		t.Fatalf("payload work bead %q, want %s", p.WorkBeadID, work.ID)
	}

	// A later tick observes the escalated episode without repeating it: no
	// third event, no marker rewrite, still no drain.
	env.clk.Advance(time.Minute)
	fresh, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	if again := env.sessionInfo(session.ID); again.ConfigDriftEscalatedAt != escalatedAt.ConfigDriftEscalatedAt {
		t.Fatalf("marker re-stamped to %q, want unchanged %q", again.ConfigDriftEscalatedAt, escalatedAt.ConfigDriftEscalatedAt)
	}
	if evs := rec.ofType(events.SessionConfigDriftDeferred); len(evs) != 2 {
		t.Fatalf("held-work deferral events = %d, want still 2", len(evs))
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("the escalated episode must keep deferring the drain, got %+v", ds)
	}

	// The work closing still proceeds to the config-drift drain.
	if err := env.store.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}
	env.clk.Advance(time.Minute)
	fresh, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	env.reconcile([]beads.Bead{fresh})
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != "config-drift" {
		t.Fatalf("drain = %+v, want the config-drift drain once the work closes; stderr=%s", ds, env.stderr.String())
	}
}
