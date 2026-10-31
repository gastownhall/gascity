package main

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The restart and relaunch effects' tests (C7b2-2).

// driftView is a txView of a drifted named row (or an ordinary one) whose
// template resolves, with fence f.
func driftView(t *testing.T, named bool, f txFence, meta ...string) txView {
	t.Helper()
	base := []string{"started_config_hash", runtime.FingerprintVersion + ":stale"}
	if named {
		base = append(base, "configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always")
	}
	c, _ := baselineCase(t, livenessAlive, desireKeep, nil, append(base, meta...)...)
	row := c.w.Census.Rows[c.k].Info
	b, err := c.store.Get(c.k.ID)
	if err != nil {
		t.Fatal(err)
	}
	return txView{It: intent{Key: c.k}, World: c.w, Alloc: c.a, Row: row, Meta: b.Metadata, Fence: f, Now: gatherNow}
}

// asDecided is v carrying the kind its row decides on its fresh read, the
// relaunch fallback's re-decision (TestDriftRelaunchThroughTheEffectPass
// pins a row that decides another).
func asDecided(v txView) txView {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	fresh, _ := decideRow(&w, v.Alloc, v.It.Key)
	v.It.Kind = fresh.Kind
	return v
}

// Kills a deferral dropped, unbounded, or unstamped (SESS-555, legacy's
// shouldDeferNamedSessionConfigDrift and its bounds): a pending interaction
// (but under a wait hold) and an attach refuse, named or not, so backoff
// paces them; an activity the runtime cannot report is stamped on first
// sight of the drift key (or of an unreadable stamp), deferred inside 2m,
// restarted past it; recent activity likewise inside 30s; an ordinary
// row's activity is not its deferral; an unreadable or expired activity
// read refuses.
func TestDriftDeferStep(t *testing.T) {
	reportable := func(last time.Duration) *activityRead {
		return &activityRead{Reportable: true, Last: gatherNow.Add(-last)}
	}
	unreported := &activityRead{}
	v := driftView(t, true, txFence{})
	key := driftKeyOf(v)
	stamped := func(ago time.Duration) []string {
		return []string{"config_drift_deferred_key", key, "config_drift_deferred_at", gatherNow.Add(-ago).UTC().Format(time.RFC3339)}
	}
	for _, tc := range []struct {
		name   string
		named  bool
		fence  txFence
		meta   []string
		defers bool   // Done with no write
		stamps bool   // Done with the stamp
		refuse string // a refusal
	}{
		{name: "a pending interaction", named: true, fence: txFence{Pending: fencePending, Activity: reportable(time.Hour)}, refuse: causeDriftDeferred + fencePending},
		{name: "an unknown pending", named: true, fence: txFence{Pending: fencePendingUnknown, Activity: reportable(time.Hour)}, refuse: causeDriftDeferred + fencePendingUnknown},
		{name: "a pending interaction under a wait hold", named: true, fence: txFence{Pending: fencePending, Activity: reportable(time.Hour)}, meta: []string{"wait_hold", "true"}},
		{name: "a pending interaction under a hold", named: true, fence: txFence{Pending: fencePending, Activity: reportable(time.Hour)}, meta: []string{"held_until", rowAt(time.Hour)}},
		{name: "a pending interaction under a quarantine", named: true, fence: txFence{Pending: fencePending, Activity: reportable(time.Hour)}, meta: []string{"quarantined_until", rowAt(time.Hour)}},
		{name: "attached", named: true, fence: txFence{Attach: fenceAttached, Activity: reportable(time.Hour)}, refuse: causeDriftDeferred + fenceAttached},
		{name: "activity unknown, first sight", named: true, fence: txFence{Activity: unreported}, stamps: true},
		{name: "activity unknown, inside 2m", named: true, fence: txFence{Activity: unreported}, meta: stamped(time.Minute), defers: true},
		{name: "activity unknown, past 2m", named: true, fence: txFence{Activity: unreported}, meta: stamped(3 * time.Minute)},
		{name: "activity unknown, an unreadable stamp", named: true, fence: txFence{Activity: unreported}, meta: []string{"config_drift_deferred_key", key, "config_drift_deferred_at", "not-a-time"}, stamps: true},
		{name: "activity unknown, another drift's stamp", named: true, fence: txFence{Activity: unreported}, meta: []string{"config_drift_deferred_key", "old", "config_drift_deferred_at", gatherNow.Add(-time.Hour).UTC().Format(time.RFC3339)}, stamps: true},
		{name: "recent activity, first sight", named: true, fence: txFence{Activity: reportable(time.Minute)}, stamps: true},
		{name: "recent activity, inside 30s", named: true, fence: txFence{Activity: reportable(time.Minute)}, meta: stamped(10 * time.Second), defers: true},
		{name: "recent activity, past 30s", named: true, fence: txFence{Activity: reportable(time.Minute)}, meta: stamped(time.Minute)},
		{name: "idle", named: true, fence: txFence{Activity: reportable(time.Hour)}},
		{name: "an activity read that failed", named: true, fence: txFence{Activity: &activityRead{Reportable: true, Err: runtime.ErrRuntimeUnavailable}}, refuse: fenceLivenessUnknown},
		{name: "an activity read past its bound", named: true, fence: txFence{Activity: &activityRead{Reportable: true, Err: errProbeExpired}}, refuse: fenceLivenessUnknown},
		{name: "an ordinary row's relaunch, pending", fence: txFence{Pending: fencePending}, refuse: causeDriftDeferred + fencePending},
		{name: "an ordinary row's relaunch, its activity unread", fence: txFence{Activity: unreported, Work: &txWork{Free: true}}},
		{name: "an ordinary row's relaunch over work", fence: txFence{Work: &txWork{}}, refuse: causeHasWork},
		{name: "an ordinary row's relaunch, its work read failed", fence: txFence{Work: &txWork{Free: true, Err: errNoReadStore}}, refuse: causeHasWork},
		{name: "an ordinary row's relaunch, its work unread", refuse: causeHasWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := driftDeferStep(driftView(t, tc.named, tc.fence, tc.meta...))
			switch {
			case tc.refuse != "":
				if step.Refuse != tc.refuse {
					t.Fatalf("step %+v, want refused %q", step, tc.refuse)
				}
			case tc.stamps:
				if !step.Done || step.Write["config_drift_deferred_key"] != key || step.Write["config_drift_deferred_at"] != gatherNow.UTC().Format(time.RFC3339) {
					t.Fatalf("step %+v, want the stamp for %q", step, key)
				}
			case tc.defers:
				if !step.Done || len(step.Write) != 0 || step.Refuse != "" {
					t.Fatalf("step %+v, want deferred, nothing written", step)
				}
			default:
				if step.Done || len(step.Write) != 0 || step.Refuse != "" {
					t.Fatalf("step %+v, want it to go on to the stop", step)
				}
			}
		})
	}
}

// Kills the restart losing a resumable conversation or resuming one it
// must not (SESS-557, legacy's resetConfiguredNamedSessionForConfigDriftInfo
// for start-pending): a row with its key and stored hash keeps both; one
// without rotates the key for a provider that takes one and clears it for
// another; each goes start-pending with its claim at now and its deferral
// stamps cleared.
func TestDriftRestartPatch(t *testing.T) {
	minting := TemplateParams{ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
	for _, tc := range []struct {
		name     string
		tp       TemplateParams
		row      session.Info
		key      string // "" cleared, "*" a fresh one
		keepHash bool
	}{
		{"resumable", minting, session.Info{SessionKey: "k-1", StartedConfigHash: "v6:old"}, "k-1", true},
		{"no key, a minting provider", minting, session.Info{StartedConfigHash: "v6:old"}, "*", false},
		{"no key, another provider", TemplateParams{}, session.Info{StartedConfigHash: "v6:old"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := driftRestartPatch(tc.tp, tc.row, gatherNow)
			key, set := p["session_key"]
			switch {
			case p["state"] != string(session.StateStartPending) || p["pending_create_claim"] != "true" || p["pending_create_started_at"] != gatherNow.UTC().Format(time.RFC3339):
				t.Fatalf("patch %v, want start-pending with its claim at now", p)
			case tc.key == "*" && (key == "" || key == "k-1"), tc.key != "*" && (!set || key != tc.key):
				t.Fatalf("patch %v: session_key %q (set %v), want %q", p, key, set, tc.key)
			case tc.keepHash != (p["started_config_hash"] == "v6:old"), !tc.keepHash && p["started_config_hash"] != "":
				t.Fatalf("patch %v, want the stored hash kept %v", p, tc.keepHash)
			case p["config_drift_deferred_key"] != "" || p["attached_config_drift_deferred_at"] != "":
				t.Fatalf("patch %v, want the deferral stamps cleared", p)
			}
			if _, ok := p["config_drift_deferred_key"]; !ok {
				t.Fatalf("patch %v, want the deferral keys written", p)
			}
		})
	}
}

// Kills a failed relaunch left without its fallback, and a relaunch
// recorded without its baseline (SESS-556/560): a relaunch that lands
// writes the core baseline, not the live pair, with session.updated; one
// that failed goes on to the restart for a named row, and begins the
// config-drift drain for an ordinary one, in this write.
func TestRelaunchResultStep(t *testing.T) {
	v := driftView(t, false, txFence{})
	cfg, _, _ := v.World.coreHash(v.Row)
	ok := v
	ok.prev = relaunched{OK: true, Hash: cfg}
	step := relaunchResultStep(ok)
	if want := baselinePatch(cfg, baselineRelaunch); !step.Done || !maps.Equal(step.Write, want) || len(step.Facts.Events) != 1 {
		t.Fatalf("landed: step %+v, want the relaunch baseline %v and session.updated", step, want)
	}
	if _, live := step.Write["started_live_hash"]; live {
		t.Fatalf("landed: step %+v writes the live hash", step)
	}
	named := v
	named.prev = relaunched{Named: true, Err: errors.New("no box")}
	if step := relaunchResultStep(named); step.Done || len(step.Write) != 0 || step.Refuse != "" {
		t.Fatalf("named failure: step %+v, want it to go on to the restart", step)
	}
	ordinary := v
	ordinary.prev = relaunched{Err: errors.New("no box")}
	ordinary.RT = &txRuntime{Class: rtAlive, Same: true, Identity: runtimeIdentity{Known: true, SessionID: v.Row.ID, Token: v.Row.InstanceToken}}
	ordinary.Fence = txFence{Read: legAttach | legWork, Work: &txWork{Free: true}}
	ordinary = asDecided(ordinary)
	if step := relaunchResultStep(ordinary); !step.Done || step.Write[session.DrainIntentReasonKey] != drainConfigDrift || len(step.Facts.Events) != 1 || step.Facts.Transition == nil {
		t.Fatalf("ordinary failure: step %+v, want the config-drift drain begun with its transition", step)
	}
	held := ordinary
	held.prev = relaunched{Held: fenceAttached}
	if step := relaunchResultStep(held); step.Refuse != fenceAttached || len(step.Write) != 0 {
		t.Fatalf("an attach before the relaunch: step %+v, want refused, no fallback", step)
	}
	stopping := driftView(t, false, txFence{}, append([]string{"state", "active"}, intentAt(drainIdle, "3")...)...)
	stopping.prev, stopping.RT, stopping.Fence = relaunched{Err: errors.New("no box")}, ordinary.RT, ordinary.Fence
	if step := relaunchResultStep(stopping); step.Refuse != causeStopActive || len(step.Write) != 0 {
		t.Fatalf("a stop request standing: step %+v, want refused %q", step, causeStopActive)
	}
}

// Kills a relaunch past its gates (SESS-556/560, legacy's
// relaunchAgentForLaunchDrift), or through a provider other than the leaf
// its section fenced (C8.7): a launch-only drift relaunches in the warm box,
// on the fenced leaf, never through the composite, its box hash this
// binary's or the v6 one's (pool sizing inside); a leaf that cannot
// relaunch, a prepared config no longer launch-only (skew), a preparation
// that would mint a speculative resume key, and a refused Relaunch each fail
// into the fallback, the last two after preparing.
func TestRelaunchCall(t *testing.T) {
	tp := TemplateParams{TemplateName: "worker", Command: "run", FPExtra: map[string]string{"pool.max": "2"}}
	probe, _ := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active")
	cfg := sessionCoreConfigForHashInfo(tp, probe.w.Census.Rows[probe.k].Info)
	stale := runtime.FingerprintVersion + ":stale"
	launchOnly := []string{"started_config_hash", stale, "started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", runtime.FingerprintVersion + ":old-launch"}
	for _, tc := range []struct {
		name     string
		meta     []string
		minting  bool
		leaf     func(*runtime.Fake) runtime.Provider
		relaunch bool // the fenced leaf's Relaunch ran and landed
	}{
		{name: "launch-only", meta: append([]string{"session_key", "k-1"}, launchOnly...), relaunch: true},
		{name: "a v6 launch-only drift", meta: []string{"session_key", "k-1", "started_config_hash", "v6:stale", "started_provision_hash", runtime.PrePolicyProvisionFingerprint(cfg), "started_launch_hash", "v6:old-launch"}, relaunch: true},
		{name: "a leaf that cannot relaunch", meta: append([]string{"session_key", "k-1"}, launchOnly...), leaf: func(f *runtime.Fake) runtime.Provider { return struct{ runtime.Provider }{f} }},
		{name: "skew", meta: []string{"session_key", "k-1", "started_config_hash", stale, "started_provision_hash", runtime.FingerprintVersion + ":other-box", "started_launch_hash", runtime.FingerprintVersion + ":old-launch"}},
		{name: "a speculative key", meta: launchOnly, minting: true},
		{name: "a refused relaunch", meta: append([]string{"session_key", "k-1"}, launchOnly...), leaf: func(f *runtime.Fake) runtime.Provider {
			f.RelaunchErrors = map[string]error{"s-heal": errors.New("box gone")}
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := baselineCase(t, livenessAlive, desireKeep, nil, append([]string{"state", "active"}, tc.meta...)...)
			row := c.w.Census.Rows[c.k].Info
			p := tp
			if tc.minting {
				p.ResolvedProvider = &config.ResolvedProvider{SessionIDFlag: "--session-id"}
			}
			fake := runtime.NewFake()
			if err := fake.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			var leaf runtime.Provider = fake
			if tc.leaf != nil {
				leaf = tc.leaf(fake)
			}
			composite := runtime.NewFake() // the start caps' provider, which can relaunch too: the Call must not route through it
			if err := composite.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			c.w.Env = &reconcileEnv{Cfg: workerCity(1)}
			caps := txCaps{start: startCaps{sp: composite}, reads: effectReads{city: c.store}}
			out, err := relaunchCall(t.Context(), caps, relaunchPlan{w: c.w, row: row, tp: p, hash: sessionCoreConfigForHashInfo(p, row), name: "s-heal", leaf: leaf, now: gatherNow})
			if err != nil || out.OK != tc.relaunch || tc.relaunch != (fake.CountCalls("Relaunch", "s-heal") > 0 && out.Err == nil) {
				t.Fatalf("relaunch %+v (%v), Relaunch calls %d; want landed %v", out, err, fake.CountCalls("Relaunch", "s-heal"), tc.relaunch)
			}
			if n := composite.CountCalls("Relaunch", "s-heal"); n != 0 {
				t.Fatalf("%d Relaunch call(s) through the composite: the relaunch must go to the leaf it fenced (C8.7)", n)
			}
		})
	}
}
