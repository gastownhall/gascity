package main

import (
	"errors"
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
// withoutA7 takes A7 out of the arms for the test: A14's tests decide on
// rows whose identity A7 would converge first, a pass ahead.
func withoutA7(t *testing.T) {
	t.Helper()
	saved := rowArms
	rowArms = slices.DeleteFunc(slices.Clone(saved), func(a rowArm) bool { return a.name == "A7" })
	t.Cleanup(func() { rowArms = saved })
}

func TestDecideRowConfigDrift(t *testing.T) {
	withoutA7(t)
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
		{name: "a named row restarts in place", meta: append([]string{"started_config_hash", stale}, named...), alive: true, wantKind: intentDriftRestart, wantReason: decideDriftRestart},
		{name: "a named row in use restarts, its effect deferring", meta: append([]string{"started_config_hash", stale}, named...), alive: true, pending: true, wantKind: intentDriftRestart, wantReason: decideDriftRestart},
		{name: "a named launch-only drift relaunches", meta: slices.Concat([]string{"started_config_hash", stale}, launchOnly, named), alive: true, relaunch: true, wantKind: intentDriftRelaunch, wantReason: decideDriftRelaunch},
		{name: "a pending interaction waits", meta: []string{"started_config_hash", stale}, alive: true, pending: true},
		{name: "open work waits", meta: []string{"started_config_hash", stale}, alive: true, work: true},
		{name: "a launch-only drift relaunches", meta: append([]string{"started_config_hash", stale}, launchOnly...), alive: true, relaunch: true, wantKind: intentDriftRelaunch, wantReason: decideDriftRelaunch},
		{name: "a launch-only drift with open work waits", meta: append([]string{"started_config_hash", stale}, launchOnly...), alive: true, relaunch: true, work: true},
		{name: "a provision hash without a launch hash drains", meta: []string{"started_config_hash", stale, "started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", ""}, alive: true, relaunch: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "an active stop request holds a launch-only drift", meta: slices.Concat([]string{"started_config_hash", stale}, launchOnly, intentAt(drainIdle, "3")), alive: true, relaunch: true},
		{name: "a launch-only drift no provider can relaunch drains", meta: append([]string{"started_config_hash", stale}, launchOnly...), alive: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "an active stop request holds the drain", meta: append([]string{"started_config_hash", stale}, intentAt(drainIdle, "3")...), alive: true},
		// A11 sits above A14 (v5 §4): an ack is signaled first, attached or
		// not; the stop verb then waits out the attach (L3).
		{name: "an acked drift is A11's", meta: append([]string{"started_config_hash", stale}, ackMeta("3")...), alive: true, wantKind: intentSignalAck, wantReason: decideDrainSignal},
		{name: "an acked attached drift is A11's", meta: append([]string{"started_config_hash", stale}, ackMeta("3")...), alive: true, attached: true, wantKind: intentSignalAck, wantReason: decideDrainSignal},
		{name: "a resolved drift clears its deferral keys", meta: []string{"started_config_hash", core, "started_live_hash", live, "attached_config_drift_deferred_key", key}, alive: true, wantKind: intentRowHeal, wantReason: decideDriftResolved},
		{name: "a converged row", meta: []string{"started_config_hash", core, "started_live_hash", live}, alive: true},
		{name: "a live drift runs session_live again", meta: []string{"started_config_hash", core, "started_live_hash", runtime.FingerprintVersion + ":other"}, alive: true, wantKind: intentLiveApply, wantReason: decideLiveApply},
		{name: "another version's live hash is A8's", meta: []string{"started_config_hash", core, "started_live_hash", "v5:other"}, alive: true},
		{name: "an asleep named drift is repaired", meta: append([]string{"state", "asleep", "started_config_hash", stale}, named...), wantKind: intentDriftRepair, wantReason: decideDriftRepair},
		{name: "an asleep named v5 hash is A8's, never repaired", meta: append([]string{"state", "asleep", "started_config_hash", "v5:core"}, named...)},
		{name: "an in-place restart leased in creating keeps its hash", meta: append([]string{"state", "creating", "started_config_hash", stale, "session_key", "k-1", "pending_create_claim", "true", "pending_create_started_at", rowAt(-10 * time.Second)}, named...)},
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
			drift := slices.Contains([]string{decideDriftAttached, decideDriftPinned, decideDriftResolved, decideDrainBegin + drainConfigDrift, decideDriftRepair, decideLiveApply, decideDriftRestart, decideDriftRelaunch}, it.Reason)
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

// Kills a drifted named row resuming its old conversation on the new
// config, and a repair that pre-empts a wanted row's start (C7b2-1; the
// #7603 wedge; SESS-566/568): an unwanted asleep named row is repaired
// alone, its key cleared for a provider that takes none or minted by the
// effect for one that does; a wanted one gets its start, the repair in the
// start's patch with the key cleared, so the PreWake's set-once fill mints
// the fresh conversation's, and the repair kept for when the start cannot
// run.
func TestDriftRepairComposesIntoTheStart(t *testing.T) {
	withoutA7(t)
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	stale := runtime.FingerprintVersion + ":stale"
	meta := []string{
		"state", "asleep", "started_config_hash", stale, "session_key", "conversation-1",
		"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always",
	}
	minting := TemplateParams{TemplateName: "worker", Command: "run", ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
	for _, tc := range []struct {
		name  string
		want  desire
		start bool
		tp    TemplateParams
	}{{"unwanted", desireKeep, false, tp}, {"wanted", desireWake, true, tp}, {"unwanted, its provider minting", desireKeep, false, minting}, {"wanted, its provider minting", desireWake, true, minting}} {
		t.Run(tc.name, func(t *testing.T) {
			tp := tc.tp
			it, _ := decideDrain(t, meta, func(e *selectionEntry) {
				e.InDesired, e.Desired, e.Liveness = true, tc.want, livenessGone
			}, func(w *World) {
				info := w.Census.Rows[rowKeyOf("gc-1")].Info
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: runtime.NewFake()}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			})
			repair := it
			if tc.start {
				if it.Kind != intentStart || it.Heal == nil || it.Heal.Reason != decideDriftRepair {
					t.Fatalf("decideRow = (%q, %q), heal %+v; want the start carrying the repair", it.Kind, it.Reason, it.Heal)
				}
				repair = *it.Heal
			} else if it.Kind != intentDriftRepair || it.Reason != decideDriftRepair {
				t.Fatalf("decideRow = (%q, %q), want the repair alone", it.Kind, it.Reason)
			}
			for k, v := range map[string]string{"started_config_hash": "", "continuation_reset_pending": "true"} {
				if got, ok := it.Patch[k]; !ok || got != v {
					t.Fatalf("patch %v, want %s=%q", it.Patch, k, v)
				}
			}
			// The key: cleared where the start's PreWake mints it, or a provider
			// that takes none launches bare; a lone repair for a minting
			// provider leaves it to the effect's mint.
			if key, ok := it.Patch["session_key"]; ok == (!tc.start && tc.tp.ResolvedProvider != nil) || key != "" {
				t.Fatalf("patch %v: session_key %q (set %v) wrong for this case", it.Patch, key, ok)
			}
			if repair.Patch["state"] != "asleep" {
				t.Fatalf("repair %v, want it asleep", repair.Patch)
			}
		})
	}
}

// Kills a lone repair resuming its old conversation (SESS-568): for a
// provider that takes a session id, the repair's write mints a new key in
// its effect; another kind's write is left alone.
func TestMintRepairKey(t *testing.T) {
	info := session.Info{ID: "gc-1", SessionKey: "conversation-1"}
	w := &World{Templates: &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}}}}}
	step := mintRepairKey(txView{It: intent{Kind: intentDriftRepair, Reason: decideDriftRepair}, World: w, Row: info}, txStep{Write: session.MetadataPatch{"state": "asleep"}})
	if key := step.Write["session_key"]; key == "" || key == "conversation-1" {
		t.Fatalf("repair write %v, want a fresh key", step.Write)
	}
	other := mintRepairKey(txView{It: intent{Kind: intentRowHealFresh, Reason: decideDriftRepair}, World: w, Row: info}, txStep{Write: session.MetadataPatch{"state": "asleep"}})
	if _, ok := other.Write["session_key"]; ok {
		t.Fatalf("another heal's write %v, want no key", other.Write)
	}
}

// Kills a failed RunLive recorded as applied (SESS-565: legacy logs, writes
// nothing, and tries again): the record step fails with the cause and
// writes no hash.
func TestLiveAppliedStepFailsOnARunLiveError(t *testing.T) {
	step := liveAppliedStep(txView{prevErr: errors.New("no pane")})
	if step.Fail != causeRunLive || len(step.Write) != 0 {
		t.Fatalf("step %+v, want failed with no write", step)
	}
	ok := liveAppliedStep(txView{prev: "v6:live", World: &World{Templates: &templateMemo{}}})
	if ok.Write["started_live_hash"] != "v6:live" || len(ok.Facts.Events) != 1 {
		t.Fatalf("step %+v, want the live hashes and session.updated", ok)
	}
}

// Kills A14 repairing drift on a classified dead runtime (E02, the C5c2
// delta review): a named row's classified zombie is no gone runtime, so its
// drift gets no repair; gone, it is repaired.
func TestDriftRepairSkipsAClassifiedDeadRuntime(t *testing.T) {
	withoutA7(t)
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	stale := runtime.FingerprintVersion + ":stale"
	named := []string{"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always"}
	for _, gone := range []bool{false, true} {
		it, _ := decideDrain(t, append([]string{"state", "asleep", "started_config_hash", stale}, named...), func(e *selectionEntry) {
			e.InDesired, e.Desired, e.Liveness = true, desireKeep, livenessDead
			if gone {
				e.Liveness = livenessGone
			}
		}, func(w *World) {
			info := w.Census.Rows[rowKeyOf("gc-1")].Info
			id := runtimeIdentity{Known: true, SessionID: info.ID, Epoch: info.Generation, Token: info.InstanceToken}
			w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: struct{ runtime.Provider }{runtime.NewFake()}}
			w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessDead, Identity: id}}
			w.Classified = map[rowKey]string{rowKeyOf("gc-1"): deadToken(id)}
		})
		if repaired := it.Reason == decideDriftRepair || it.Heal != nil && it.Heal.Reason == decideDriftRepair; repaired != gone {
			t.Fatalf("gone %v: decideRow = (%q, %q), want the drift repair %v", gone, it.Kind, it.Reason, gone)
		}
	}
}
