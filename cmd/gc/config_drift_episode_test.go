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
	withoutA7(t)
	if city == nil {
		city = &config.City{}
	}
	city.Agents = []config.Agent{{Name: "worker"}}
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	return decideDrain(t, append([]string{"started_config_hash", heldWorkStale}, meta...), func(e *selectionEntry) {
		e.InDesired, e.Desired = true, desireKeep
		e.AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
	}, func(w *World) {
		info := w.Census.Rows[rowKeyOf("gc-1")].Info
		w.Env = &reconcileEnv{Cfg: city, SP: struct{ runtime.Provider }{runtime.NewFake()}}
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
		w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
	})
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
