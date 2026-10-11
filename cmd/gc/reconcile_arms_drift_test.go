package main

import (
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Kills A14 out of legacy's order or reaching past it (SESS-550..562, 565,
// 566/568): an attached (or attach-unknown) drift stamps its deferral,
// throttled; a recent attached deferral, a pending interaction and open work
// wait; a pinned named row stamps and never restarts; another named row, a
// launch-only drift a provider can relaunch, a live drift and an asleep
// named drift are C7b2's, which A14 leaves alone; otherwise the drain
// begins, once no stop request is active. A resolved drift clears its
// deferral keys, and a missing stored hash or an undesired row never
// drifts.
func TestDecideRowConfigDrift(t *testing.T) {
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	base := censusRowOf(t, drainRow()).Info
	cfg := sessionCoreConfigForHashInfo(tp, base)
	core, live := runtime.CoreFingerprint(cfg), runtime.LiveFingerprint(cfg)
	stale := runtime.FingerprintVersion + ":stale"
	key := stale + ":" + core
	launchOnly := []string{"started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", runtime.FingerprintVersion + ":old-launch"}
	named := []string{"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always"}
	for _, tc := range []struct {
		name             string
		meta             []string
		attached, unsure bool
		pending, work    bool
		relaunch         bool
		alive, undesired bool
		wantKind         string
		wantReason       string
	}{
		{name: "a drift drains", meta: []string{"started_config_hash", stale}, alive: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "attached stamps its deferral", meta: []string{"started_config_hash", stale}, alive: true, attached: true, wantKind: intentRowHeal, wantReason: decideDriftAttached},
		{name: "an attach unknown stamps its deferral", meta: []string{"started_config_hash", stale}, alive: true, unsure: true, wantKind: intentRowHeal, wantReason: decideDriftAttached},
		{name: "attached with a fresh stamp writes nothing", meta: []string{"started_config_hash", stale, "attached_config_drift_deferred_key", key, "attached_config_drift_deferred_at", rowAt(-time.Minute)}, alive: true, attached: true},
		{name: "recently attached-deferred waits", meta: []string{"started_config_hash", stale, "attached_config_drift_deferred_key", key, "attached_config_drift_deferred_at", rowAt(-3 * time.Minute)}, alive: true},
		{name: "a stale attached deferral drains", meta: []string{"started_config_hash", stale, "attached_config_drift_deferred_key", key, "attached_config_drift_deferred_at", rowAt(-6 * time.Minute)}, alive: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "a pinned named row stamps", meta: append([]string{"started_config_hash", stale, "pin_awake", "true"}, named...), alive: true, wantKind: intentRowHeal, wantReason: decideDriftPinned},
		{name: "a stamped pinned named row waits", meta: append([]string{"started_config_hash", stale, "pin_awake", "true", "config_drift_deferred_key", key, "config_drift_deferred_at", rowAt(-time.Hour)}, named...), alive: true},
		{name: "a named row's restart is C7b2's", meta: append([]string{"started_config_hash", stale}, named...), alive: true},
		{name: "a named launch-only drift's relaunch is C7b2's", meta: slices.Concat([]string{"started_config_hash", stale}, launchOnly, named), alive: true, relaunch: true},
		{name: "a pending interaction waits", meta: []string{"started_config_hash", stale}, alive: true, pending: true},
		{name: "open work waits", meta: []string{"started_config_hash", stale}, alive: true, work: true},
		{name: "a launch-only drift's relaunch is C7b2's", meta: append([]string{"started_config_hash", stale}, launchOnly...), alive: true, relaunch: true},
		{name: "a launch-only drift no provider can relaunch drains", meta: append([]string{"started_config_hash", stale}, launchOnly...), alive: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "an active stop request holds the drain", meta: append([]string{"started_config_hash", stale}, intentAt(drainIdle, "3")...), alive: true},
		{name: "a resolved drift clears its deferral keys", meta: []string{"started_config_hash", core, "started_live_hash", live, "attached_config_drift_deferred_key", key}, alive: true, wantKind: intentRowHeal, wantReason: decideDriftResolved},
		{name: "a converged row", meta: []string{"started_config_hash", core, "started_live_hash", live}, alive: true},
		{name: "a live drift's RunLive is C7b2's", meta: []string{"started_config_hash", core, "started_live_hash", runtime.FingerprintVersion + ":other"}, alive: true},
		{name: "an asleep named drift's repair is C7b2's", meta: append([]string{"state", "asleep", "started_config_hash", stale}, named...)},
		{name: "an asleep pool row's drift is no named repair", meta: []string{"state", "asleep", "started_config_hash", stale}},
		{name: "no stored hash never drifts", meta: []string{"started_live_hash", live}, alive: true},
		{name: "an undesired row", meta: []string{"started_config_hash", stale}, alive: true, undesired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, _ := decideDrain(t, tc.meta, func(e *selectionEntry) {
				e.InDesired, e.Desired = !tc.undesired, desireKeep
				if !tc.alive {
					e.Liveness = livenessGone
				}
				if tc.pending {
					e.WakeReasons = []WakeReason{WakePending}
				}
				if tc.work {
					e.AssignedWork = &assignedWorkView{BeadID: "gw-1", Claimed: true}
				}
			}, func(w *World) {
				info := w.Census.Rows[rowKeyOf("gc-1")].Info
				var sp runtime.Provider = struct{ runtime.Provider }{runtime.NewFake()} // no Relaunch
				if tc.relaunch {
					sp = runtime.NewFake() // a RelaunchProvider
				}
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: sp}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
				w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive, Attached: tc.attached, Uncertain: tc.unsure}}
			})
			drift := slices.Contains([]string{decideDriftAttached, decideDriftPinned, decideDriftResolved, decideDrainBegin + drainConfigDrift}, it.Reason)
			if tc.wantKind == "" && drift || tc.wantKind != "" && (it.Kind != tc.wantKind || it.Reason != tc.wantReason) { // another arm may take a row A14 leaves
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, tc.wantKind, tc.wantReason)
			}
			switch tc.wantReason {
			case decideDriftAttached:
				if it.Patch["attached_config_drift_deferred_key"] != key || it.Patch["attached_config_drift_deferred_at"] != gatherNow.UTC().Format(time.RFC3339) {
					t.Fatalf("stamp %v, want key %q at the pass", it.Patch, key)
				}
			case decideDriftPinned:
				if it.Patch["config_drift_deferred_key"] != key {
					t.Fatalf("stamp %v, want key %q", it.Patch, key)
				}
			case decideDriftResolved:
				if len(it.Patch) != 4 {
					t.Fatalf("clear %v, want the four deferral keys", it.Patch)
				}
			case decideDrainBegin + drainConfigDrift:
				if it.Patch[session.DrainIntentReasonKey] != drainConfigDrift || it.Event == nil || it.Event.Type != "session.draining" {
					t.Fatalf("begin %v event %+v, want the config-drift request and session.draining", it.Patch, it.Event)
				}
			}
		})
	}
}

// Kills an execution-stalled request lost, applied to another incarnation,
// or begun over a stop already requested (NUDGE-022, A16): C8's request for
// the row's generation, compared as integers (an empty one reads 0, as
// legacy's), begins the execution-stalled drain on a live row.
func TestDecideRowExecutionStalled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gen   string
		meta  []string
		alive bool
		want  bool
	}{
		{"its generation", "3", nil, true, true},
		{"an empty generation on a row at generation 0", "", []string{"generation", ""}, true, true},
		{"another generation", "2", nil, true, false},
		{"a stop already requested", "3", intentAt(drainIdle, "3"), true, false},
		{"not alive", "3", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, _ := decideArm(armExecutionStalled)(t, tc.meta, func(e *selectionEntry) {
				e.Desired = desireKeep
				if !tc.alive {
					e.Liveness = livenessGone
				}
			}, func(w *World) {
				w.ExecutionStalled = map[string]executionStalledRequest{"gc-1": {ID: "gc-1", Generation: tc.gen}}
			})
			got := it.Kind == intentDrainBegin && it.Reason == decideDrainBegin+executionStalledDrainReason && it.Patch[session.DrainIntentReasonKey] == executionStalledDrainReason
			if got != tc.want {
				t.Fatalf("decideRow = (%q, %q) %v, want the execution-stalled begin %v", it.Kind, it.Reason, it.Patch, tc.want)
			}
		})
	}
}

// decideArm is decideDrain deciding gc-1 by arm alone, past the arms ahead
// of it.
func decideArm(arm func(*rowFacts) (intent, bool)) func(*testing.T, []string, func(*selectionEntry), ...func(*World)) (intent, time.Time) {
	return func(t *testing.T, meta []string, entry func(*selectionEntry), world ...func(*World)) (intent, time.Time) {
		t.Helper()
		w, a := rowWorld(t, drainRow(meta...))
		w.Obs, w.ObsMaxAge = observedOwn(w, "gc-1"), observeMaxAge
		k := rowKeyOf("gc-1")
		if entry != nil {
			entry(a.Snapshot.Entries[k])
		}
		for _, f := range world {
			f(w)
		}
		r := &rowFacts{w: w, k: k, entry: a.Snapshot.Entries[k]}
		r.row, r.found = w.Census.Rows[k]
		it, _ := arm(r)
		return it, r.next
	}
}
