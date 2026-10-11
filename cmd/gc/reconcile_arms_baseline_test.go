package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/runtime"
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

// v5 is h under the previous fingerprint version, as production rows carry.
func v5(h string) string { return "v5:" + strings.TrimPrefix(h, runtime.FingerprintVersion+":") }

// Kills a cutover drain storm and a rebaseline of what legacy leaves (v5
// M2, SESS-551/563/564/567): a legacy or version-mismatched stored config
// hash is rebaselined, all six keys, at any liveness but a start's this
// pass; a missing one and a real drift are not; once the core matches, an
// alive row's empty live hash with no session_live config is backfilled
// and a mismatched one rebaselined, and a real live drift is not.
func TestDecideRowBaseline(t *testing.T) {
	probe, _ := baselineCase(t, livenessAlive, desireKeep, nil)
	cfg := sessionCoreConfigForHashInfo(TemplateParams{TemplateName: "worker", Command: "run-worker"}, probe.w.Census.Rows[probe.k].Info)
	core, live := runtime.CoreFingerprint(cfg), runtime.LiveFingerprint(cfg)
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
		{"no stored hash", livenessAlive, desireKeep, nil, []string{"started_live_hash", ""}, ""},
		{"a real drift", livenessAlive, desireKeep, nil, []string{"started_config_hash", runtime.FingerprintVersion + ":other", "started_live_hash", live}, ""},
		{"an empty live hash, no session_live", livenessAlive, desireKeep, nil, []string{"started_config_hash", core}, decideBaselineLive},
		{"an empty live hash on an asleep row", livenessGone, desireSleep, nil, []string{"state", "asleep", "started_config_hash", core}, ""},
		{"an empty live hash with session_live", livenessAlive, desireKeep, []string{"echo live"}, []string{"started_config_hash", core}, ""},
		{"a v5 live hash", livenessAlive, desireKeep, nil, []string{"started_config_hash", core, "started_live_hash", v5(live)}, decideBaselineVersion},
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
			want := baselinePatch(cfg, map[string]baselineReason{decideBaselineVersion: baselineVersionArtifact, decideBaselineLive: baselineLiveBackfill}[tc.want])
			keys := map[string]int{decideBaselineVersion: 6, decideBaselineLive: 2}[tc.want] // all six; the live pair
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
// desired row's drifted config is accepted once, the core baseline only,
// canceling a config-drift request still only requested; a row already
// attempted, an undesired one, one with no stored hash, and a request for
// another generation are left alone.
func TestDecideRowSoftReloadAcceptance(t *testing.T) {
	stale := runtime.FingerprintVersion + ":stale"
	drifting := []string{"drain_intent_reason", drainConfigDrift, "drain_intent_at", rowAt(-time.Minute), "drain_intent_incarnation", "3"}
	for _, tc := range []struct {
		name                 string
		meta                 []string
		attempted, undesired bool
		gen                  uint64
		want                 bool
		cancels              bool
	}{
		{"a drifted row", []string{"started_config_hash", stale}, false, false, 0, true, false},
		{"a drifted row under a config-drift request", append([]string{"started_config_hash", stale}, drifting...), false, false, 0, true, true},
		{"a drifted row whose config-drift request is acked (its stop is due)", append([]string{"started_config_hash", stale, "drain_ack_incarnation", "3"}, drifting...), false, false, 0, true, false},
		{"attempted", []string{"started_config_hash", stale}, true, false, 0, false, false},
		{"undesired", []string{"started_config_hash", stale}, false, true, 0, false, false},
		{"no stored hash", nil, false, false, 0, false, false},
		{"another generation", []string{"started_config_hash", stale}, false, false, 9, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cfg := baselineCase(t, livenessAlive, desireKeep, nil, append([]string{"state", "active"}, tc.meta...)...)
			c.w.Env = &reconcileEnv{Gen: 1}
			c.a.Snapshot.Entries[c.k].InDesired = !tc.undesired
			c.w.SoftReload = &softReloadView{Gen: 1 + tc.gen, Attempted: map[rowKey]bool{c.k: tc.attempted}}
			it := c.decide()
			if got := it.Reason == decideBaselineSoftReload; got != tc.want {
				t.Fatalf("decideRow = (%q, %q) %v, want an acceptance %v", it.Kind, it.Reason, it.Patch, tc.want)
			}
			if !tc.want {
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

// Kills an acceptance recorded twice, a refused one counted accepted or
// never marked (so re-proposed every pass), and a reply that drops the
// failures (legacy's warnings): a landing counts accepted, a refusal
// failed by row, each once, and the reply carries both.
func TestSoftReloadRequestRecordsEachRowOnce(t *testing.T) {
	q := newSoftReloadRequest(1)
	a, b := rowKey{Leg: rowLeg, ID: "gc-a"}, rowKey{Leg: rowLeg, ID: "gc-b"}
	for _, s := range []settlement{
		{Key: a, Kind: intentBaseline, Reason: decideBaselineSoftReload, Outcome: settledLanded},
		{Key: a, Kind: intentBaseline, Reason: decideBaselineSoftReload, Outcome: settledLanded},
		{Key: b, Kind: intentBaseline, Reason: decideBaselineSoftReload, Outcome: settledRefused},
		{Key: rowKey{Leg: rowLeg, ID: "gc-c"}, Kind: intentBaseline, Reason: decideBaselineVersion, Outcome: settledLanded},
	} {
		q.settle(s)
	}
	if v := q.view(); !v.Attempted[a] || !v.Attempted[b] || len(v.Attempted) != 2 {
		t.Fatalf("attempted %v, want gc-a and gc-b only", v.Attempted)
	}
	var r reloadControlReply
	q.reply(&r)
	if r.AcceptedDriftCount == nil || *r.AcceptedDriftCount != 1 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "gc-b") {
		t.Fatalf("reply %+v, want one accepted and gc-b's failure warned", r)
	}
	q.close(0, false)
	if q.view() != nil {
		t.Fatal("a closed request still has a view")
	}
}
