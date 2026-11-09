package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// heldWorkDrift is A14's world for an ordinary seat whose config drifted
// under its open work gw-1 (#8345): its drift key, and its decide over the
// row's extra metadata under city cfg (the test's own when nil).
type heldWorkDrift struct {
	key       string
	breakdown string // a stored breakdown whose Command differs from the template's
}

func newHeldWorkDrift(t *testing.T) heldWorkDrift {
	t.Helper()
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	cfg := sessionCoreConfigForHashInfo(tp, censusRowOf(t, drainRow()).Info)
	old := cfg
	old.Command = "old-run"
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(old))
	if err != nil {
		t.Fatalf("marshal breakdown: %v", err)
	}
	return heldWorkDrift{key: heldWorkStale + ":" + runtime.CoreFingerprint(cfg), breakdown: string(breakdown)}
}

const heldWorkStale = runtime.FingerprintVersion + ":stale"

func (h heldWorkDrift) decide(t *testing.T, city *config.City, meta ...string) (intent, time.Time) {
	t.Helper()
	return h.decideIn(t, city, nil, nil, meta...)
}

// decideIn is decide with entry's change to the row's allocation entry and
// world's to the pass's world, each when not nil.
func (h heldWorkDrift) decideIn(t *testing.T, city *config.City, entry func(*selectionEntry), world func(*World), meta ...string) (intent, time.Time) {
	t.Helper()
	withoutA7(t)
	return decideDrain(t, heldWorkMeta(meta...), func(e *selectionEntry) {
		heldWorkEntry(e)
		if entry != nil {
			entry(e)
		}
	}, func(w *World) {
		heldWorkWorld(w, city)
		if world != nil {
			world(w)
		}
	})
}

// heldWorkMeta is the held-work row's metadata: its stale started hash, then
// meta.
func heldWorkMeta(meta ...string) []string {
	return append([]string{"started_config_hash", heldWorkStale}, meta...)
}

// heldWorkEntry is the held-work row's allocation entry: desired and kept,
// under its claimed open work gw-1.
func heldWorkEntry(e *selectionEntry) {
	e.InDesired, e.Desired = true, desireKeep
	e.AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
}

// heldWorkWorld is the held-work row's pass under city (an empty one when
// nil): its worker template resolved and its runtime alive.
func heldWorkWorld(w *World, city *config.City) {
	if city == nil {
		city = &config.City{}
	}
	city.Agents = []config.Agent{{Name: "worker"}}
	info := w.Census.Rows[rowKeyOf("gc-1")].Info
	w.Env = &reconcileEnv{Cfg: city, SP: struct{ runtime.Provider }{runtime.NewFake()}}
	w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{TemplateName: "worker", Command: "run"}}}}
	w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
}

// driftDeferredPayload decodes ev's session.config_drift_deferred payload,
// failing on another event or a null drifted_fields.
func driftDeferredPayload(t *testing.T, ev *events.Event) api.SessionConfigDriftDeferredPayload {
	t.Helper()
	if ev == nil || ev.Type != events.SessionConfigDriftDeferred {
		t.Fatalf("event %+v, want %s", ev, events.SessionConfigDriftDeferred)
	}
	if strings.Contains(string(ev.Payload), `"drifted_fields":null`) {
		t.Fatalf("payload %s: drifted_fields is null", ev.Payload)
	}
	var p api.SessionConfigDriftDeferredPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode payload %s: %v", ev.Payload, err)
	}
	return p
}

// Kills a held-work config-drift deferral on an ordinary seat that stays
// silent (#8345, AC-1): v2's A14 stamps the episode's key and start on its
// first observation, in a row heal whose event names the session, the
// drifted fields (or says their details are unavailable), the work bead and
// how long the drift has waited; a stamp of another drift key, or one that
// does not parse or lies in the future, starts the episode again; this
// key's stamp writes nothing. Nothing drains.
func TestHeldWorkDriftFirstObservation(t *testing.T) {
	h := newHeldWorkDrift(t)
	now := gatherNow.UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name       string
		meta       []string
		wantStamp  bool
		wantFields []string
	}{
		{name: "an unstamped episode with its breakdown", meta: []string{"core_hash_breakdown", h.breakdown}, wantStamp: true, wantFields: []string{"Command"}},
		{name: "an unstamped episode without a breakdown", wantStamp: true, wantFields: []string{}},
		{name: "another drift key's stamp", meta: []string{"config_drift_deferred_key", "v6:a:v6:b", "config_drift_deferred_at", rowAt(-2 * time.Hour)}, wantStamp: true, wantFields: []string{}},
		{name: "a stamp that does not parse", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", "yesterday"}, wantStamp: true, wantFields: []string{}},
		{name: "a stamp in the future", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(time.Hour)}, wantStamp: true, wantFields: []string{}},
		{name: "this key's stamp", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-10 * time.Minute)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, _ := h.decide(t, nil, tc.meta...)
			if !tc.wantStamp {
				if it.Reason == decideDriftHeldWork || it.Kind == intentDrainBegin {
					t.Fatalf("decideRow = (%q, %q) %v, want no write", it.Kind, it.Reason, it.Patch)
				}
				return
			}
			if it.Kind != intentRowHeal || it.Reason != decideDriftHeldWork {
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, intentRowHeal, decideDriftHeldWork)
			}
			if it.Patch["config_drift_deferred_key"] != h.key || it.Patch["config_drift_deferred_at"] != now {
				t.Fatalf("stamp %v, want key %q at %s", it.Patch, h.key, now)
			}
			ev := it.Event
			p := driftDeferredPayload(t, ev)
			want := api.SessionConfigDriftDeferredPayload{SessionID: "gc-1", SessionName: "s-gc-1", Template: "worker", DriftedFields: tc.wantFields, WorkBeadID: "gw-1", DeferredSince: now}
			if p.SessionID != want.SessionID || p.SessionName != want.SessionName || p.Template != want.Template || !slices.Equal(p.DriftedFields, want.DriftedFields) ||
				p.WorkBeadID != want.WorkBeadID || p.DeferredSince != want.DeferredSince || p.PendingSeconds != 0 || p.Escalated {
				t.Fatalf("payload %+v, want %+v", p, want)
			}
			if ev.Subject != "gc-1" || ev.SessionID != "gc-1" || ev.Actor != "gc" || !ev.Ts.Equal(gatherNow) {
				t.Fatalf("envelope %+v, want gc-1 by gc at the pass", ev)
			}
			detail := "field details unavailable"
			if len(tc.wantFields) > 0 {
				detail = "Command"
			}
			if !strings.Contains(ev.Message, detail) || !strings.Contains(ev.Message, "gw-1") || !strings.Contains(ev.Message, "s-gc-1") {
				t.Fatalf("message %q, want the session, %q and the work bead", ev.Message, detail)
			}
		})
	}
}

// heldWorkReconcilerEnv is legacy's reconciler over an ordinary worker seat
// whose config drifted under its in-progress work: the env, the session and
// the work bead.
func heldWorkReconcilerEnv(t *testing.T) (*reconcilerTestEnv, *events.Fake, beads.Bead, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	rec := events.NewFake()
	env.rec = rec
	env.addRunningWorkerDesiredWithNewConfig()
	sess := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&sess, map[string]string{"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})})
	work, err := env.store.Create(beads.Bead{Title: "in-flight work", Type: "task", Status: "in_progress", Assignee: sess.ID})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	return env, rec, sess, work
}

// reconcileFresh runs one legacy pass over sess as the store holds it now,
// and returns it after.
func (e *reconcilerTestEnv) reconcileFresh(t *testing.T, sess beads.Bead) beads.Bead {
	t.Helper()
	cur, err := e.store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sess.ID, err)
	}
	e.reconcile([]beads.Bead{cur})
	after, err := e.store.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", sess.ID, err)
	}
	return after
}

// driftDeferredEvents is rec's session.config_drift_deferred events.
func driftDeferredEvents(rec *events.Fake) []events.Event {
	var out []events.Event
	for _, e := range rec.Events {
		if e.Type == events.SessionConfigDriftDeferred {
			out = append(out, e)
		}
	}
	return out
}

// Kills legacy's held-work deferral staying silent (#8345, AC-1): its first
// deferral stamps the episode's key and start and records the
// first-observation event naming the session, its template, the work bead
// and the episode's start; a later pass on the same drift records nothing
// more and keeps the start; nothing drains.
func TestSessionConfigDriftDeferredLegacyFirstObservation(t *testing.T) {
	env, rec, sess, work := heldWorkReconcilerEnv(t)
	start := env.clk.Now().UTC().Format(time.RFC3339)
	got := env.reconcileFresh(t, sess)
	if ds := env.dt.get(sess.ID); ds != nil {
		t.Fatalf("drain %+v over held work", ds)
	}
	stored := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"})
	if got.Metadata["config_drift_deferred_at"] != start || !strings.HasPrefix(got.Metadata["config_drift_deferred_key"], stored+":") {
		t.Fatalf("episode at=%q key=%q, want %s under %s:<desired>", got.Metadata["config_drift_deferred_at"], got.Metadata["config_drift_deferred_key"], start, stored)
	}
	evs := driftDeferredEvents(rec)
	if len(evs) != 1 {
		t.Fatalf("events %+v, want one first observation; stderr=%s", evs, env.stderr.String())
	}
	p := driftDeferredPayload(t, &evs[0])
	if p.SessionID != sess.ID || p.Template != "worker" || p.WorkBeadID != work.ID || p.DeferredSince != start || p.PendingSeconds != 0 || p.Escalated {
		t.Fatalf("payload %+v, want session %s, template worker, work %s since %s", p, sess.ID, work.ID, start)
	}
	if evs[0].Subject != sess.ID || evs[0].SessionID != sess.ID || evs[0].Actor != "gc" || !evs[0].Ts.Equal(env.clk.Now()) {
		t.Fatalf("envelope %+v", evs[0])
	}

	env.clk.Advance(10 * time.Minute)
	got = env.reconcileFresh(t, sess)
	if n := len(driftDeferredEvents(rec)); n != 1 || got.Metadata["config_drift_deferred_at"] != start {
		t.Fatalf("after a later pass: %d events, at=%q; want 1 and %s", n, got.Metadata["config_drift_deferred_at"], start)
	}
}

// Kills a held-work episode that escalates early, never, more than once, or
// off its configured horizon (#8345, AC-2): a stamped episode under the
// horizon waits with A14 requeued at it; past it, an episode without a valid
// escalation marker writes the marker alone, its event escalated and its
// start kept; a marker between the start and the pass writes nothing more,
// and one before the start (another episode's) or after the pass does not
// count.
func TestHeldWorkDriftEscalation(t *testing.T) {
	h := newHeldWorkDrift(t)
	now := gatherNow.UTC().Format(time.RFC3339)
	stamped := func(age time.Duration, meta ...string) []string {
		return append([]string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-age)}, meta...)
	}
	for _, tc := range []struct {
		name      string
		horizon   string
		meta      []string
		wantDue   time.Duration // a wait's requeue, from the pass
		escalates bool
		age       time.Duration
	}{
		{name: "under the default horizon waits for it", meta: stamped(10 * time.Minute), wantDue: 50 * time.Minute},
		{name: "past the default horizon escalates", meta: stamped(36*time.Hour, "core_hash_breakdown", h.breakdown), escalates: true, age: 36 * time.Hour},
		{name: "under a configured horizon waits for it", horizon: "15m", meta: stamped(10 * time.Minute), wantDue: 5 * time.Minute},
		{name: "past a configured horizon escalates", horizon: "15m", meta: stamped(20 * time.Minute), escalates: true, age: 20 * time.Minute},
		{name: "an invalid horizon reads the default", horizon: "soon", meta: stamped(20 * time.Minute), wantDue: 40 * time.Minute},
		{name: "an escalated episode writes nothing", meta: stamped(36*time.Hour, "config_drift_escalated_at", rowAt(-time.Hour))},
		{name: "another episode's marker does not count", meta: stamped(2*time.Hour, "config_drift_escalated_at", rowAt(-3*time.Hour)), escalates: true, age: 2 * time.Hour},
		{name: "a marker after the pass does not count", meta: stamped(2*time.Hour, "config_drift_escalated_at", rowAt(time.Hour)), escalates: true, age: 2 * time.Hour},
		{name: "a marker that does not parse does not count", meta: stamped(2*time.Hour, "config_drift_escalated_at", "done"), escalates: true, age: 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, next := h.decide(t, &config.City{Daemon: config.DaemonConfig{DriftDeferralEscalateAfter: tc.horizon}}, tc.meta...)
			if !tc.escalates {
				if it.Reason == decideDriftHeldWork || it.Reason == decideDriftEscalated || it.Kind == intentDrainBegin {
					t.Fatalf("decideRow = (%q, %q) %v, want no write", it.Kind, it.Reason, it.Patch)
				}
				if due := gatherNow.Add(tc.wantDue); tc.wantDue > 0 && !next.Equal(due) {
					t.Fatalf("requeue at %v, want the horizon at %v", next, due)
				}
				if tc.wantDue == 0 && next.Equal(gatherNow.Add(time.Nanosecond)) {
					t.Fatalf("an escalated episode requeued at its past horizon")
				}
				return
			}
			if it.Kind != intentRowHeal || it.Reason != decideDriftEscalated {
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, intentRowHeal, decideDriftEscalated)
			}
			if len(it.Patch) != 1 || it.Patch["config_drift_escalated_at"] != now {
				t.Fatalf("patch %v, want the escalation marker alone at %s", it.Patch, now)
			}
			p := driftDeferredPayload(t, it.Event)
			if !p.Escalated || p.DeferredSince != gatherNow.Add(-tc.age).UTC().Format(time.RFC3339) || p.PendingSeconds != int64(tc.age/time.Second) || p.WorkBeadID != "gw-1" {
				t.Fatalf("payload %+v, want escalated since %v ago", p, tc.age)
			}
		})
	}
}

// Kills legacy escalating early, never, or again after a later pass
// (#8345, AC-2): the held-work episode escalates once its horizon passes,
// keeping its start, records exactly one escalated event, and a later pass
// (a fresh reconciler reads the same persisted marker) records nothing more.
func TestSessionConfigDriftDeferredLegacyEscalatesOnce(t *testing.T) {
	env, rec, sess, work := heldWorkReconcilerEnv(t)
	start := env.clk.Now()
	env.reconcileFresh(t, sess)
	env.clk.Advance(59 * time.Minute)
	env.reconcileFresh(t, sess)
	if n := len(driftDeferredEvents(rec)); n != 1 {
		t.Fatalf("%d events under the horizon, want the first observation alone", n)
	}
	env.clk.Advance(2 * time.Minute)
	got := env.reconcileFresh(t, sess)
	evs := driftDeferredEvents(rec)
	if len(evs) != 2 {
		t.Fatalf("events %+v past the horizon, want one escalation; stderr=%s", evs, env.stderr.String())
	}
	p := driftDeferredPayload(t, &evs[1])
	if !p.Escalated || p.WorkBeadID != work.ID || p.DeferredSince != start.UTC().Format(time.RFC3339) || p.PendingSeconds != int64((61*time.Minute)/time.Second) {
		t.Fatalf("escalation %+v, want escalated since %v", p, start)
	}
	if got.Metadata["config_drift_escalated_at"] != env.clk.Now().UTC().Format(time.RFC3339) || got.Metadata["config_drift_deferred_at"] != start.UTC().Format(time.RFC3339) {
		t.Fatalf("metadata %v, want the marker at the pass and the start kept", got.Metadata)
	}
	for range 3 {
		env.clk.Advance(time.Hour)
		env.reconcileFresh(t, sess)
	}
	if n := len(driftDeferredEvents(rec)); n != 2 {
		t.Fatalf("%d events after later passes, want no repeat", n)
	}
	if ds := env.dt.get(sess.ID); ds != nil {
		t.Fatalf("drain %+v over held work", ds)
	}
}

// Kills an escalation marker that outlives its episode (#8345, AC-3): a
// desired-hash change restarts the episode at now and clears the marker, in
// v2 and legacy alike, and the new episode's next event is a first
// observation, not an escalation.
func TestHeldWorkDriftNewKeyResetsEpisode(t *testing.T) {
	h := newHeldWorkDrift(t)
	t.Run("v2", func(t *testing.T) {
		it, _ := h.decide(t, nil, "config_drift_deferred_key", "v6:a:v6:b", "config_drift_deferred_at", rowAt(-2*time.Hour), "config_drift_escalated_at", rowAt(-time.Hour))
		if it.Reason != decideDriftHeldWork || it.Patch["config_drift_deferred_at"] != gatherNow.UTC().Format(time.RFC3339) || it.Patch["config_drift_deferred_key"] != h.key {
			t.Fatalf("decideRow = (%q, %q) %v, want the episode restamped at the pass", it.Kind, it.Reason, it.Patch)
		}
		if marker, ok := it.Patch["config_drift_escalated_at"]; !ok || marker != "" {
			t.Fatalf("patch %v, want the escalation marker cleared", it.Patch)
		}
		if p := driftDeferredPayload(t, it.Event); p.Escalated {
			t.Fatalf("payload %+v, want a first observation", p)
		}
	})
	t.Run("legacy", func(t *testing.T) {
		env, rec, sess, _ := heldWorkReconcilerEnv(t)
		old := env.clk.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
		env.setSessionMetadata(&sess, map[string]string{"config_drift_deferred_key": "v6:a:v6:b", "config_drift_deferred_at": old, "config_drift_escalated_at": old})
		got := env.reconcileFresh(t, sess)
		if got.Metadata["config_drift_deferred_at"] != env.clk.Now().UTC().Format(time.RFC3339) || got.Metadata["config_drift_escalated_at"] != "" {
			t.Fatalf("metadata %v, want the episode restamped at the pass and its marker cleared", got.Metadata)
		}
		evs := driftDeferredEvents(rec)
		if len(evs) != 1 || driftDeferredPayload(t, &evs[0]).Escalated {
			t.Fatalf("events %+v, want one first observation", evs)
		}
	})
}

// Kills legacy's clears leaving the escalation marker behind (#8345, AC-3):
// the convergence clear fires on a marker alone and clears it, and the
// named drift reset clears it with the deferral stamps.
func TestSessionConfigDriftClearsEscalationMarker(t *testing.T) {
	marked := map[string]string{"config_drift_escalated_at": "2026-03-08T12:00:00Z"}
	t.Run("convergence", func(t *testing.T) {
		env := newReconcilerTestEnv(t)
		sess := env.createSessionBead("worker", "worker")
		env.setSessionMetadata(&sess, marked)
		if err := clearSessionConfigDriftDeferral(env.sessionInfo(sess.ID), sessionFrontDoor(env.store)); err != nil {
			t.Fatalf("clearSessionConfigDriftDeferral: %v", err)
		}
		if got := env.sessionInfo(sess.ID).ConfigDriftEscalatedAt; got != "" {
			t.Fatalf("marker %q after the clear, want empty", got)
		}
	})
	t.Run("named reset", func(t *testing.T) {
		env := newReconcilerTestEnv(t)
		sess := env.createSessionBead("worker", "worker")
		env.setSessionMetadata(&sess, marked)
		batch := resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(sess.ID), driftResumeSessionIDCapableTemplateParams(), env.store, env.sp, "worker", false, "asleep", time.Now().UTC(), &env.stderr)
		if marker, ok := batch["config_drift_escalated_at"]; !ok || marker != "" {
			t.Fatalf("batch %v, want the escalation marker cleared", batch)
		}
		if got := env.sessionInfo(sess.ID).ConfigDriftEscalatedAt; got != "" {
			t.Fatalf("marker %q after the reset, want empty", got)
		}
	})
}

// Kills the held-work signal weakening the protection or starving A16
// (#8345, AC-4): an episode of any age, escalated or due, never begins the
// config-drift drain; a config-drift drain requested before the work is
// still canceled by a pending interaction; and while the execution backstop's
// request for the row's generation stands, the episode's first observation
// or escalation owns one heal pass, after which its execution-stalled begin
// is decided.
func TestHeldWorkDriftKeepsItsSafety(t *testing.T) {
	h := newHeldWorkDrift(t)
	stalled := func(w *World) {
		w.ExecutionStalled = map[string]executionStalledRequest{"gc-1": {ID: "gc-1", Generation: "3"}}
	}
	for _, tc := range []struct {
		name       string
		meta       []string
		entry      func(*selectionEntry)
		world      func(*World)
		wantKind   string
		wantReason string
		thenStall  bool // the heal's patch applied, A16 begins next
	}{
		{name: "an escalated episode days old", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-72 * time.Hour), "config_drift_escalated_at", rowAt(-48 * time.Hour)}},
		{name: "an escalation due", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-72 * time.Hour)}, wantKind: intentRowHeal, wantReason: decideDriftEscalated},
		{name: "a config-drift drain requested before the work, then a pending interaction", meta: slices.Concat([]string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-time.Hour)}, intentAt(drainConfigDrift, "3")), entry: func(e *selectionEntry) {
			e.WakeReasons = []WakeReason{WakePending}
		}, wantKind: intentDrainCancel, wantReason: decideDrainCancel + drainConfigDrift},
		{name: "an execution stall over a first observation", world: stalled, wantKind: intentRowHeal, wantReason: decideDriftHeldWork, thenStall: true},
		{name: "an execution stall over an escalation due", meta: []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-72 * time.Hour)}, world: stalled, wantKind: intentRowHeal, wantReason: decideDriftEscalated, thenStall: true},
		{name: "another generation's execution stall", world: func(w *World) {
			w.ExecutionStalled = map[string]executionStalledRequest{"gc-1": {ID: "gc-1", Generation: "2"}}
		}, wantKind: intentRowHeal, wantReason: decideDriftHeldWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, _ := h.decideIn(t, nil, tc.entry, tc.world, tc.meta...)
			if it.Reason == decideDrainBegin+drainConfigDrift {
				t.Fatalf("decideRow = (%q, %q) %v: the config-drift drain begun over held work", it.Kind, it.Reason, it.Patch)
			}
			if tc.wantKind == "" {
				if it.Reason == decideDriftHeldWork || it.Reason == decideDriftEscalated {
					t.Fatalf("decideRow = (%q, %q) %v, want no write", it.Kind, it.Reason, it.Patch)
				}
				return
			}
			if it.Kind != tc.wantKind || it.Reason != tc.wantReason {
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, tc.wantKind, tc.wantReason)
			}
			if !tc.thenStall {
				return
			}
			healed := slices.Clone(tc.meta)
			for _, k := range slices.Sorted(maps.Keys(it.Patch)) {
				healed = append(healed, k, it.Patch[k])
			}
			next, _ := h.decideIn(t, nil, tc.entry, tc.world, healed...)
			if next.Kind != intentDrainBegin || next.Reason != decideDrainBegin+executionStalledDrainReason {
				t.Fatalf("after the heal decideRow = (%q, %q) %v, want the execution-stalled begin", next.Kind, next.Reason, next.Patch)
			}
		})
	}
}

// effect decides A14 on the held-work row store holds and runs the intent's
// registered effect writing through writer, under the default horizon: the
// intent, and the settlement when it ran one.
func (h heldWorkDrift) effect(t *testing.T, store *beads.MemStore, writer beads.Store) (intent, settlement) {
	t.Helper()
	withoutA7(t)
	k := rowKeyOf("gc-1")
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), Mislabelled: map[rowKey]bool{}, CityPath: t.TempDir()}
	w.Obs, w.ObsMaxAge = observedOwn(w, "gc-1"), observeMaxAge
	heldWorkWorld(w, nil)
	w.LegStores = map[string]beads.Store{rowLeg: writer}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Key: k, Liveness: livenessAlive}}}}
	heldWorkEntry(a.Snapshot.Entries[k])
	it, _ := decideRow(w, a, k)
	if !effectSpecs[it.Kind].runs() {
		return it, settlement{}
	}
	return it, runTx(context.Background(), newEffectPass(w, a), it, effectSpecs[it.Kind], nil)
}

// heldWorkStore is a fenced store holding the held-work row with meta.
func heldWorkStore(t *testing.T, meta ...string) *beads.MemStore {
	t.Helper()
	store := censusStore(drainRow(heldWorkMeta(meta...)...))
	stampedMemStore(t, store)
	return store
}

// driftEpisodeCASStore is a fenced store whose conditional writes fail with
// err, or, for the first races of them, lose to another writer that writes
// moved onto the row just before.
type driftEpisodeCASStore struct {
	*beads.MemStore
	err    error
	races  int
	moved  map[string]string
	writes int
}

func (s *driftEpisodeCASStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	s.writes++
	if s.err != nil {
		return s.err
	}
	if s.writes <= s.races {
		if err := s.SetMetadataBatch(id, s.moved); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *driftEpisodeCASStore) ConditionalWritesResolveTarget() beads.Store { return s }

// Kills an episode signal recorded for a write that did not land (#8345,
// AC-6): v2's row heal whose write fails, or whose CAS loses every attempt,
// settles with no session.config_drift_deferred and leaves the row as it
// was, so a later pass writes the episode and records its one event; a CAS
// lost once lands on its retry with one event, not one per attempt; one lost
// to another writer's stamp of the same episode records nothing, and leaves
// a later pass nothing to write.
func TestHeldWorkDriftUnlandedWriteRecordsNothing(t *testing.T) {
	h := newHeldWorkDrift(t)
	now := gatherNow.UTC().Format(time.RFC3339)
	escalationDue := []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-36 * time.Hour)}
	for _, tc := range []struct {
		name         string
		meta         []string
		writer       func(*beads.MemStore) *driftEpisodeCASStore
		wantOutcome  settleOutcome
		wantCause    string
		wantKey      string // the episode write's key, and its value once landed
		wantEscalate bool
		retry        bool // a later pass writes the episode
	}{
		{
			name: "a failed first observation",
			writer: func(m *beads.MemStore) *driftEpisodeCASStore {
				return &driftEpisodeCASStore{MemStore: m, err: errors.New("store unavailable")}
			},
			wantOutcome: settledFailed, wantCause: causeWrite, wantKey: "config_drift_deferred_at", retry: true,
		},
		{
			name: "a failed escalation",
			meta: escalationDue,
			writer: func(m *beads.MemStore) *driftEpisodeCASStore {
				return &driftEpisodeCASStore{MemStore: m, err: errors.New("store unavailable")}
			},
			wantOutcome: settledFailed, wantCause: causeWrite, wantKey: "config_drift_escalated_at", wantEscalate: true, retry: true,
		},
		{
			name: "a CAS lost on every attempt",
			writer: func(m *beads.MemStore) *driftEpisodeCASStore {
				return &driftEpisodeCASStore{MemStore: m, races: 99, moved: map[string]string{txKeyNote: "moved"}}
			},
			wantOutcome: settledRefused, wantCause: causeCAS, wantKey: "config_drift_deferred_at", retry: true,
		},
		{
			name: "a CAS lost once",
			writer: func(m *beads.MemStore) *driftEpisodeCASStore {
				return &driftEpisodeCASStore{MemStore: m, races: 1, moved: map[string]string{txKeyNote: "moved"}}
			},
			wantOutcome: settledLanded, wantKey: "config_drift_deferred_at",
		},
		{
			name: "a CAS lost to another writer's stamp of the episode",
			writer: func(m *beads.MemStore) *driftEpisodeCASStore {
				return &driftEpisodeCASStore{MemStore: m, races: 1, moved: map[string]string{"config_drift_deferred_key": h.key, "config_drift_deferred_at": rowAt(-time.Minute)}}
			},
			wantOutcome: settledRefused, wantCause: causeRedecided,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := heldWorkStore(t, append([]string{"core_hash_breakdown", h.breakdown}, tc.meta...)...)
			before, err := store.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			it, s := h.effect(t, store, tc.writer(store))
			if it.Kind != intentRowHeal || it.Event == nil {
				t.Fatalf("decideRow = (%q, %q), want a row heal carrying the episode's event", it.Kind, it.Reason)
			}
			after, err := store.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			if s.Outcome != tc.wantOutcome || s.Cause != tc.wantCause {
				t.Fatalf("settlement %v %q (%v), want %v %q", s.Outcome, s.Cause, s.Err, tc.wantOutcome, tc.wantCause)
			}
			if tc.wantOutcome == settledLanded {
				if len(s.Facts.Events) != 1 || after.Metadata[tc.wantKey] != now {
					t.Fatalf("events %+v, %s=%q; want one event and the episode at %s", s.Facts.Events, tc.wantKey, after.Metadata[tc.wantKey], now)
				}
				return
			}
			if len(s.Facts.Events) != 0 {
				t.Fatalf("events %+v on an unlanded write, want none", s.Facts.Events)
			}
			if tc.wantKey != "" && after.Metadata[tc.wantKey] != before.Metadata[tc.wantKey] {
				t.Fatalf("%s = %q after an unlanded write, want %q", tc.wantKey, after.Metadata[tc.wantKey], before.Metadata[tc.wantKey])
			}
			it, s = h.effect(t, store, store)
			if !tc.retry {
				if it.Reason == decideDriftHeldWork || it.Reason == decideDriftEscalated {
					t.Fatalf("a later pass decides (%q, %q), want the other writer's episode kept", it.Kind, it.Reason)
				}
				return
			}
			after, err = store.Get("gc-1")
			if err != nil {
				t.Fatal(err)
			}
			if s.Outcome != settledLanded || len(s.Facts.Events) != 1 || after.Metadata[tc.wantKey] != now {
				t.Fatalf("later pass: settlement %v %q, events %+v, %s=%q; want it landed with one event", s.Outcome, s.Cause, s.Facts.Events, tc.wantKey, after.Metadata[tc.wantKey])
			}
			if p := driftDeferredPayload(t, &s.Facts.Events[0]); p.Escalated != tc.wantEscalate {
				t.Fatalf("later pass escalated %t, want %t", p.Escalated, tc.wantEscalate)
			}
		})
	}
}

// driftEpisodeFailStore fails the held-work episode's metadata writes (its
// stamp or its escalation marker) with err.
type driftEpisodeFailStore struct {
	beads.Store
	err error
}

func (s driftEpisodeFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs[namedSessionConfigDriftDeferredAtMetadata]; ok {
		return s.err
	}
	if _, ok := kvs[configDriftEscalatedAtMetadata]; ok {
		return s.err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// Kills legacy recording an episode signal for a write that failed (#8345,
// AC-6): a failed first observation or escalation is logged, records no
// session.config_drift_deferred and leaves the row as it was; the next pass
// over a healthy store writes it and records its one event.
func TestSessionConfigDriftDeferredLegacyFailedWriteRetries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		escalation bool
		key        string
	}{
		{name: "first observation", key: "config_drift_deferred_at"},
		{name: "escalation", escalation: true, key: "config_drift_escalated_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, rec, sess, _ := heldWorkReconcilerEnv(t)
			healthy := env.store
			if tc.escalation {
				env.reconcileFresh(t, sess)
				env.clk.Advance(61 * time.Minute)
			}
			prior := len(driftDeferredEvents(rec))
			writeErr := errors.New("episode write failed")
			env.store = driftEpisodeFailStore{Store: healthy, err: writeErr}
			got := env.reconcileFresh(t, sess)
			if n := len(driftDeferredEvents(rec)); n != prior || got.Metadata[tc.key] != "" {
				t.Fatalf("after a failed write: %d events (want %d), %s=%q (want unset)", n, prior, tc.key, got.Metadata[tc.key])
			}
			if !strings.Contains(env.stderr.String(), writeErr.Error()) {
				t.Fatalf("stderr %q, want the failed write logged", env.stderr.String())
			}
			env.store = healthy
			env.clk.Advance(time.Minute)
			got = env.reconcileFresh(t, sess)
			evs := driftDeferredEvents(rec)
			if len(evs) != prior+1 || got.Metadata[tc.key] != env.clk.Now().UTC().Format(time.RFC3339) {
				t.Fatalf("after a healthy pass: %d events (want %d), %s=%q (want the pass)", len(evs), prior+1, tc.key, got.Metadata[tc.key])
			}
			if p := driftDeferredPayload(t, &evs[len(evs)-1]); p.Escalated != tc.escalation {
				t.Fatalf("retry escalated %t, want %t", p.Escalated, tc.escalation)
			}
		})
	}
}

// Kills the two reconcilers signaling one held-work episode differently
// (#8345, AC-6): for a first observation and for an escalation an hour and a
// minute in, legacy's and v2's events share their type, actor, envelope,
// payload and message, up to each one's own session, work bead and clock.
func TestSessionConfigDriftDeferredLegacyAndV2Agree(t *testing.T) {
	h := newHeldWorkDrift(t)
	for _, escalation := range []bool{false, true} {
		t.Run(fmt.Sprintf("escalated=%t", escalation), func(t *testing.T) {
			env, rec, sess, work := heldWorkReconcilerEnv(t)
			var meta []string
			if escalation {
				env.reconcileFresh(t, sess)
				env.clk.Advance(61 * time.Minute)
				meta = []string{"config_drift_deferred_key", h.key, "config_drift_deferred_at", rowAt(-61 * time.Minute)}
			}
			env.reconcileFresh(t, sess)
			evs := driftDeferredEvents(rec)
			if len(evs) == 0 {
				t.Fatalf("legacy recorded no event; stderr=%s", env.stderr.String())
			}
			legacy := normalizeDriftDeferred(t, evs[len(evs)-1], sess.ID, sess.Metadata["session_name"], work.ID, env.clk.Now())
			store := heldWorkStore(t, meta...)
			_, s := h.effect(t, store, store)
			if len(s.Facts.Events) != 1 {
				t.Fatalf("v2 settlement %v %q, events %+v; want one event", s.Outcome, s.Cause, s.Facts.Events)
			}
			v2 := normalizeDriftDeferred(t, s.Facts.Events[0], "gc-1", "s-gc-1", "gw-1", gatherNow)
			if legacy != v2 {
				t.Fatalf("legacy and v2 differ:\nlegacy %+v\nv2     %+v", legacy, v2)
			}
		})
	}
}

// driftDeferredShape is a session.config_drift_deferred event with its own
// session, work bead and clock factored out.
type driftDeferredShape struct {
	Type, Actor, Message, Template, Fields string
	SinceAgo                               time.Duration
	PendingSeconds                         int64
	Escalated                              bool
}

// normalizeDriftDeferred is ev's shape, failing unless it names session id
// (named name) on its envelope and payload, work bead work, and now.
func normalizeDriftDeferred(t *testing.T, ev events.Event, id, name, work string, now time.Time) driftDeferredShape {
	t.Helper()
	p := driftDeferredPayload(t, &ev)
	if ev.Subject != id || ev.SessionID != id || !ev.Ts.Equal(now) || p.SessionID != id || p.SessionName != name || p.WorkBeadID != work {
		t.Fatalf("event %+v payload %+v, want session %s (%s), work %s at %v", ev, p, id, name, work, now)
	}
	since, err := time.Parse(time.RFC3339, p.DeferredSince)
	if err != nil {
		t.Fatalf("deferred_since %q: %v", p.DeferredSince, err)
	}
	msg := strings.ReplaceAll(strings.ReplaceAll(ev.Message, work, "<work>"), name, "<session>")
	return driftDeferredShape{
		Type: ev.Type, Actor: ev.Actor, Message: msg, Template: p.Template, Fields: strings.Join(p.DriftedFields, ","),
		SinceAgo: now.Sub(since), PendingSeconds: p.PendingSeconds, Escalated: p.Escalated,
	}
}
