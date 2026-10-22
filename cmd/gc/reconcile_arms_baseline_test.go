package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// baselineCase is a healCase whose template resolves (a worker whose
// session_live config is live), its row's stored hashes from meta.
func baselineCase(t *testing.T, liveness rowLiveness, desired desire, live []string, meta ...string) (*healCase, runtime.Config) {
	t.Helper()
	c := newHealCase(t, liveness, desired, meta...)
	tp := TemplateParams{TemplateName: "worker", Command: "run-worker", Hints: agent.StartupHints{SessionLive: live}}
	info := c.w.Census.Rows[c.k].Info
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
	return c, sessionCoreConfigForHashInfo(tp, info)
}

// v5 is h under an older fingerprint version, as production rows carry.
func v5(h string) string { return "v5:" + strings.TrimPrefix(h, runtime.FingerprintVersion+":") }

// v6 is h under the version before the FingerprintExtra change policy, for a
// hash whose digest that policy left unchanged.
func v6(h string) string { return "v6:" + strings.TrimPrefix(h, runtime.FingerprintVersion+":") }

// Kills a cutover drain storm and a rebaseline of what legacy leaves (v5
// M2, SESS-551/563/564/567): a legacy or version-mismatched stored config
// hash is rebaselined, all six keys, at any liveness but a start's this
// pass, and a v6 one only for unchanged config, the core baseline alone;
// a missing one and a real drift are not; once the core matches, an alive
// row's empty live hash with no session_live config is backfilled and a
// mismatched one rebaselined, a v6 one only for the same session_live,
// and a real live drift is not.
func TestDecideRowBaseline(t *testing.T) {
	probe, _ := baselineCase(t, livenessAlive, desireKeep, nil)
	cfg := sessionCoreConfigForHashInfo(TemplateParams{TemplateName: "worker", Command: "run-worker"}, probe.w.Census.Rows[probe.k].Info)
	core, live := runtime.CoreFingerprint(cfg), runtime.LiveFingerprint(cfg)
	prePolicy := runtime.PrePolicyCoreFingerprint(cfg)
	for _, tc := range []struct {
		name     string
		liveness rowLiveness
		desired  desire
		session  []string // session_live config
		meta     []string
		want     string // the decide reason, "" for none
	}{
		{"a v5 core hash on a live row", livenessAlive, desireKeep, nil, []string{"started_config_hash", v5(core), "started_live_hash", v5(live)}, decideBaselineVersion},
		{"a v5 core hash on an asleep row", livenessGone, desireSleep, nil, []string{"state", "asleep", "started_config_hash", v5(core)}, decideBaselineVersion},
		{"an unversioned core hash", livenessAlive, desireKeep, nil, []string{"started_config_hash", "abc123", "started_live_hash", live}, decideBaselineVersion},
		{"a v5 core hash its start commits this pass", livenessGone, desireWake, nil, []string{"state", "asleep", "started_config_hash", v5(core)}, ""},
		{"a v5 core hash on a wanted row its quarantine holds", livenessGone, desireWake, nil, []string{"state", "asleep", "started_config_hash", v5(core), "quarantined_until", rowAt(time.Hour)}, decideBaselineVersion},
		{"no stored hash", livenessAlive, desireKeep, nil, []string{"started_live_hash", ""}, ""},
		{"a real drift", livenessAlive, desireKeep, nil, []string{"started_config_hash", runtime.FingerprintVersion + ":other", "started_live_hash", live}, ""},
		{"a v6 core hash of the config on a live row", livenessAlive, desireKeep, nil, []string{"started_config_hash", prePolicy, "started_live_hash", v6(live)}, decideBaselinePrePolicy},
		{"a v6 core hash of the config on an asleep row", livenessGone, desireSleep, nil, []string{"state", "asleep", "started_config_hash", prePolicy}, decideBaselinePrePolicy},
		{"another config's v6 core hash", livenessAlive, desireKeep, nil, []string{"started_config_hash", "v6:other", "started_live_hash", live}, ""},
		{"an empty live hash, no session_live", livenessAlive, desireKeep, nil, []string{"started_config_hash", core}, decideBaselineLive},
		{"an empty live hash on an asleep row", livenessGone, desireSleep, nil, []string{"state", "asleep", "started_config_hash", core}, ""},
		{"an empty live hash with session_live", livenessAlive, desireKeep, []string{"echo live"}, []string{"started_config_hash", core}, ""},
		{"a v5 live hash", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", v5(live)}, decideBaselineVersion},
		{"a v6 live hash of the session_live", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", v6(live)}, decideBaselineVersion},
		{"another session_live's v6 live hash", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", "v6:other"}, ""},
		{"a live drift", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", runtime.FingerprintVersion + ":other"}, ""},
		{"converged", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", live}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cfg := baselineCase(t, tc.liveness, tc.desired, tc.session, append([]string{"state", "active"}, tc.meta...)...)
			it := c.decide()
			if tc.want == "" {
				if it.Kind == intentBaseline {
					t.Fatalf("decideRow = (%q, %q) %v, want no baseline", it.Kind, it.Reason, it.Patch)
				}
				return
			}
			want := baselinePatch(cfg, map[string]baselineReason{decideBaselineVersion: baselineVersionArtifact, decideBaselineLive: baselineLiveBackfill, decideBaselinePrePolicy: baselinePrePolicy}[tc.want])
			keys := map[string]int{decideBaselineVersion: 6, decideBaselineLive: 2, decideBaselinePrePolicy: 4}[tc.want] // all six; the live pair; the core four
			if it.Kind != intentBaseline || it.Reason != tc.want || len(it.Patch) != keys || len(want) != keys {
				t.Fatalf("decideRow = (%q, %q) %v, want %q writing %v", it.Kind, it.Reason, it.Patch, tc.want, want)
			}
			for k, v := range want {
				if it.Patch[k] != v || !strings.HasPrefix(v, runtime.FingerprintVersion+":") && k != "core_hash_breakdown" {
					t.Fatalf("patch %s = %q, want %q under %s", k, it.Patch[k], v, runtime.FingerprintVersion)
				}
			}
		})
	}
}

// Kills a soft reload that leaves drift to drain, or accepts what legacy
// skips (MAINT-025): under an open request for the pass's generation, a
// desired row's drifted config is accepted, the core baseline only, under
// the request's own reason, canceling a config-drift request still only
// requested, and no other reason's (M11); a row already attempted, an
// undesired one, one with no stored hash, one whose template errors (M13),
// and a request for another generation are left alone; a v5 hash is the
// version rebaseline, all six keys, first (M14).
func TestDecideRowSoftReloadAcceptance(t *testing.T) {
	stale := runtime.FingerprintVersion + ":stale"
	requested := func(reason string) []string {
		return []string{"drain_intent_reason", reason, "drain_intent_at", rowAt(-time.Minute), "drain_intent_incarnation", "3"}
	}
	q := newSoftReloadRequest(1)
	for _, tc := range []struct {
		name                 string
		meta                 []string
		attempted, undesired bool
		gen                  uint64
		broken               bool // the template errors
		want                 string
		cancels              bool
	}{
		{"a drifted row", []string{"started_config_hash", stale}, false, false, 0, false, q.reason, false},
		{"a drifted row under a config-drift request", append([]string{"started_config_hash", stale}, requested(drainConfigDrift)...), false, false, 0, false, q.reason, true},
		{"a drifted row under an execution-stalled request", append([]string{"started_config_hash", stale}, requested(executionStalledDrainReason)...), false, false, 0, false, q.reason, false},
		{"a drifted row whose config-drift request is acked (its stop is due)", append([]string{"started_config_hash", stale, "drain_ack_incarnation", "3"}, requested(drainConfigDrift)...), false, false, 0, false, q.reason, false},
		{"a v5 hash under a soft reload", []string{"started_config_hash", "v5:core"}, false, false, 0, false, decideBaselineVersion, false},
		{"attempted", []string{"started_config_hash", stale}, true, false, 0, false, "", false},
		{"undesired", []string{"started_config_hash", stale}, false, true, 0, false, "", false},
		{"no stored hash", nil, false, false, 0, false, "", false},
		{"a template that errors", []string{"started_config_hash", stale}, false, false, 0, true, "", false},
		{"another generation", []string{"started_config_hash", stale}, false, false, 9, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cfg := baselineCase(t, livenessAlive, desireKeep, nil, append([]string{"state", "active"}, tc.meta...)...)
			c.w.Env = &reconcileEnv{Gen: 1}
			c.a.Snapshot.Entries[c.k].InDesired = !tc.undesired
			if tc.broken {
				for k, res := range c.w.Templates.entries {
					res.Err = errors.New("unresolvable")
					c.w.Templates.entries[k] = res
				}
			}
			v := q.view()
			v.Gen, v.Attempted = 1+tc.gen, map[rowKey]bool{c.k: tc.attempted}
			c.w.SoftReload = v
			it := c.decide()
			if it.Reason != tc.want && (tc.want != "" || it.Kind == intentBaseline) {
				t.Fatalf("decideRow = (%q, %q) %v, want %q", it.Kind, it.Reason, it.Patch, tc.want)
			}
			if tc.want == decideBaselineVersion && len(it.Patch) != 6 {
				t.Fatalf("patch %v, want all six baseline keys", it.Patch)
			}
			if tc.want != q.reason {
				return
			}
			want := baselinePatch(cfg, baselineSoftReload)
			keys := len(want) + map[bool]int{true: 3}[tc.cancels] // the core four, and the request's three
			if len(want) != 4 || len(it.Patch) != keys || it.Patch["started_config_hash"] != runtime.CoreFingerprint(cfg) {
				t.Fatalf("patch %v, want the core baseline %v", it.Patch, want)
			}
			if _, canceled := it.Patch["drain_intent_reason"]; canceled != tc.cancels {
				t.Fatalf("patch %v, want the config-drift request canceled %v", it.Patch, tc.cancels)
			}
		})
	}
}

// Kills one transient refusal dropping a row from the soft reload, and an
// acceptance credited to another request (C7c review must-fix 1, item 4):
// a landing counts accepted; a premise, CAS, redecided or name-busy
// refusal retries, counting failed only at its third; another refusal
// fails at once; another request's acceptance, or a rebaseline, counts for
// none; each row once, and the reply carries both counts.
func TestSoftReloadRequestRecordsEachRowOnce(t *testing.T) {
	q, other := newSoftReloadRequest(1), newSoftReloadRequest(1)
	row := func(id string) rowKey { return rowKey{Leg: rowLeg, ID: id} }
	settle := func(id, reason string, outcome settleOutcome, cause string) {
		q.settle(settlement{Key: row(id), Kind: intentBaseline, Reason: reason, Outcome: outcome, Cause: cause})
	}
	settle("gc-a", q.reason, settledLanded, "")
	settle("gc-a", q.reason, settledLanded, "")
	settle("gc-b", q.reason, settledRefused, causePremise)
	settle("gc-b", q.reason, settledRefused, causeCAS)
	settle("gc-f", q.reason, settledRefused, causeRedecided) // a redecided refusal retries too (the C7c review's D02)
	if v := q.view(); v.Attempted[row("gc-b")] || v.Attempted[row("gc-f")] {
		t.Fatal("a transient refusal dropped its row")
	}
	settle("gc-b", q.reason, settledRefused, causeNameBusy)
	settle("gc-c", q.reason, settledFailed, causeWrite)
	settle("gc-d", other.reason, settledLanded, "")
	settle("gc-e", decideBaselineVersion, settledLanded, "")
	if v := q.view(); !v.Attempted[row("gc-a")] || !v.Attempted[row("gc-b")] || !v.Attempted[row("gc-c")] || len(v.Attempted) != 3 {
		t.Fatalf("attempted %v, want gc-a, gc-b (its third refusal) and gc-c only", v.Attempted)
	}
	var r reloadControlReply
	q.reply(&r)
	if r.AcceptedDriftCount == nil || *r.AcceptedDriftCount != 1 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "gc-b, gc-c") {
		t.Fatalf("reply %+v, want one accepted and gc-b's and gc-c's failures warned", r)
	}
	q.close(0, false)
	if q.view() != nil {
		t.Fatal("a closed request still has a view")
	}
}

// Kills a soft reload's acceptance refused by a heartbeat (C7c review
// must-fix 1), and its in-CAS cancel of a config-drift request left out of
// the drain telemetry (item 8): an agent's held_until landing between the
// pass and the write does not move the baseline premise, so the acceptance
// lands, recording legacy's "cancel" transition.
func TestSoftReloadAcceptanceSurvivesAHeartbeat(t *testing.T) {
	c, cfg := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active", "started_config_hash", runtime.FingerprintVersion+":stale",
		"drain_intent_reason", drainConfigDrift, "drain_intent_at", rowAt(-time.Minute), "drain_intent_incarnation", "3")
	c.w.Env = &reconcileEnv{}
	c.a.Snapshot.Entries[c.k].InDesired = true
	c.w.SoftReload = newSoftReloadRequest(0).view() // the effect's env (healCase.pass) is generation 0
	c.before = func() {
		if err := c.store.SetMetadataBatch(c.k.ID, map[string]string{"held_until": rowAt(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if it, s := c.run(t, nil, nil); s.Outcome != settledLanded || c.meta(t)["started_config_hash"] != runtime.CoreFingerprint(cfg) {
		t.Fatalf("%s settled %+v, row %v: want the acceptance landed", it.Reason, s, c.meta(t))
	} else if tr := s.Facts.Transition; tr == nil || tr.Reason != drainConfigDrift || tr.Transition != "cancel" {
		t.Fatalf("transition %+v, want the config-drift request's cancel recorded", tr)
	}
}

// Kills a pass closing a request it did not read, a newer generation that
// never closes one (M20), and a close over a partial census (C7c review
// items 3, 7): a pass closes only its view's request, at once under a newer
// generation, otherwise once its complete census owes no row.
func TestCloseSoftReload(t *testing.T) {
	stale := runtime.FingerprintVersion + ":stale"
	for _, tc := range []struct {
		name            string
		hash            string
		envGen, viewGen uint64
		partial         bool
		closed          bool
	}{
		{"a drifted row is owed", stale, 1, 1, false, false},
		{"nothing owed", "", 1, 1, false, true},
		{"a newer generation, over a partial census", stale, 2, 1, true, true},
		{"an older generation (the pass read its env before the reload)", "", 1, 2, false, false},
		{"a partial census", "", 1, 1, true, false},
		{"a drifted row its start commits this pass is not owed", stale, 1, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := []string{"state", "active"}
			if tc.hash != "" {
				meta = append(meta, "started_config_hash", tc.hash)
			}
			liveness, desired := livenessAlive, desireKeep
			if strings.HasSuffix(tc.name, "this pass is not owed") {
				liveness, desired, meta = livenessGone, desireWake, append(meta, "state", "asleep")
			}
			c, _ := baselineCase(t, liveness, desired, nil, meta...)
			census := rowKey{Leg: "rig", ID: "rg-1"} // a rig leg's row, which the open count leaves out
			c.w.Census.Rows[census] = censusRow{Key: census, Info: session.Info{ID: "rg-1"}}
			c.a.Snapshot.Entries[census] = &selectionEntry{Key: census}
			c.w.Env = &reconcileEnv{Gen: tc.envGen}
			c.a.Snapshot.Entries[c.k].InDesired = true
			if tc.partial {
				c.w.Census.Legs[0].Err = errors.New("read failed")
			}
			mine, current := newSoftReloadRequest(tc.viewGen), newSoftReloadRequest(tc.viewGen)
			c.w.SoftReload = mine.view()
			closeSoftReload(c.w, c.a)
			if got := mine.view() == nil; got != tc.closed || current.view() == nil {
				t.Fatalf("closed %v (another request closed %v), want %v and the other open", got, current.view() == nil, tc.closed)
			}
			if tc.name == "nothing owed" && mine.result.OpenSessions != 1 {
				t.Fatalf("open sessions %d, want the sessions leg's one", mine.result.OpenSessions)
			}
		})
	}
}

// Kills the soft-reload hook replying before its acceptance (M24), under
// another generation (M23), without amending the reply (M22), without
// poking the planner (M28), or holding a stopping controller to the bound
// (C7c review item 6): it opens a request for the published env's
// generation and marks the planner dirty, replies with the acceptance once
// the request closes, and returns at once when its context ends or the
// planner stops.
func TestAcceptSoftReloadHook(t *testing.T) {
	newRT := func() *plannerRuntime {
		rt := newDefaultPlanner(io.Discard)
		rt.planner.clock = newFakePlannerClock(plannerT0) // the bound never fires
		rt.planner.effects = rt.exec
		rt.env.Store(&reconcileEnv{Gen: 7})
		return rt
	}
	run := func(rt *plannerRuntime, ctx context.Context) <-chan reloadControlReply {
		out := make(chan reloadControlReply, 1)
		go func() {
			var r reloadControlReply
			rt.acceptSoftReload(ctx, reloadIntent{Soft: true}, &r)
			out <- r
		}()
		return out
	}
	rt := newRT()
	replied := run(rt, context.Background())
	select {
	case <-rt.planner.dirty: // openSoftReload marks it after storing the request
	case <-time.After(5 * time.Second):
		t.Fatal("the hook did not poke the planner")
	}
	q := rt.planner.softReload.Load()
	if q.gen != 7 {
		t.Fatalf("request generation %d, want the published env's 7", q.gen)
	}
	q.settle(settlement{Key: rowKey{Leg: rowLeg, ID: "gc-1"}, Kind: intentBaseline, Reason: q.reason, Outcome: settledLanded})
	q.close(1, false)
	if r := <-replied; r.AcceptedDriftCount == nil || *r.AcceptedDriftCount != 1 {
		t.Fatalf("reply %+v, want the request's one acceptance", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	replied = run(newRT(), ctx)
	cancel()
	select {
	case <-replied:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook held an ended context")
	}
	stopped := newRT()
	close(stopped.planner.done)
	select {
	case <-run(stopped, context.Background()):
	case <-time.After(5 * time.Second):
		t.Fatal("the hook held a stopped planner")
	}
}
