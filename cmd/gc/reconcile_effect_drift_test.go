package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drift effects' tests (C7b2-1): RunLive's fence and record, and the
// asleep named repair's key.

// Kills the repair's key mint riding another heal (the C7b2-1 review's
// must-fix; its repro): the pass decides the repair on an inventory that
// reads the runtime gone; an operator's attach starts the row's runtime at
// its own token, which writes no row, so the effect's fresh read decides
// A6's awake heal. The repair, of its own kind, refuses: the attached
// session keeps its conversation's key and the row its hash.
func TestDriftRepairRefusesARuntimeThatCameUp(t *testing.T) {
	withoutA7(t)
	stale := runtime.FingerprintVersion + ":stale"
	c := newHealCase(t, livenessGone, desireKeep, "state", "asleep", "sleep_reason", "idle", "started_config_hash", stale, "session_key", "conversation-1",
		"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "on_demand")
	info := c.w.Census.Rows[c.k].Info
	tp := TemplateParams{TemplateName: "worker", Command: "run", ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
	c.w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}, NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}}}}
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
	c.a.Snapshot.Entries[c.k].InDesired = true
	if it := c.decide(); it.Reason != decideDriftRepair {
		t.Fatalf("decided (%q, %q), want the repair", it.Kind, it.Reason)
	}
	attached := &freshObserver{
		Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true},
		env: map[string]string{"GC_SESSION_ID": c.k.ID, "GC_INSTANCE_TOKEN": "tok-3"},
	}
	_, s := c.run(t, attached.tmux(), nil)
	if m := c.meta(t); s.Outcome != settledRefused || m["session_key"] != "conversation-1" || m["started_config_hash"] != stale || m["state"] != "asleep" {
		t.Fatalf("settled %+v, row %v: want the repair refused and the attached session's key kept", s, m)
	}
}

// Kills RunLive run on a runtime that is not the row's, a failed RunLive
// recorded as applied, and a half-written record (the C7b2-1 review's L03,
// L04, L07; SESS-565): decided on the row's own runtime, the effect reads
// it fresh; its own runs session_live and records both live hashes;
// another row's under the name refuses and never runs; a RunLive error
// settles failed and writes nothing.
func TestLiveApplyFencesAndRecords(t *testing.T) {
	withoutA7(t)
	for _, tc := range []struct {
		name, sid, tok string
		err            error
		ran, recorded  bool
	}{
		{"own runtime", "", "tok-3", nil, true, true},
		{"another row's runtime", "gc-9", "tok-9", nil, false, false},
		{"RunLive fails", "", "tok-3", errors.New("no pane"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, cfg := baselineCase(t, livenessAlive, desireKeep, []string{"echo live"}, "state", "active")
			_ = probe
			core, live := runtime.CoreFingerprint(cfg), runtime.LiveFingerprint(cfg)
			other := runtime.FingerprintVersion + ":other"
			c, _ := baselineCase(t, livenessAlive, desireKeep, []string{"echo live"}, "state", "active", "started_config_hash", core, "started_live_hash", other, "live_hash", other)
			c.a.Snapshot.Entries[c.k].InDesired = true
			c.w.Obs, c.w.ObsMaxAge = observedOwn(c.w, c.k.ID), observeMaxAge
			if it := c.decide(); it.Kind != intentLiveApply {
				t.Fatalf("decided (%q, %q), want live-apply", it.Kind, it.Reason)
			}
			sid := tc.sid
			if sid == "" {
				sid = c.k.ID
			}
			obs := &freshObserver{Fake: runtime.NewFake(), l: runtime.Liveness{Running: true, Alive: true}, env: map[string]string{"GC_SESSION_ID": sid, "GC_INSTANCE_TOKEN": tc.tok}, runLive: tc.err}
			_, s := c.run(t, obs.tmux(), nil)
			m := c.meta(t)
			ran := obs.Fake.CountCalls("RunLive", "s-heal") > 0 || tc.err != nil && s.Cause == causeRunLive
			recorded := m["started_live_hash"] == live && m["live_hash"] == live
			if ran != tc.ran || recorded != tc.recorded || !recorded && (m["started_live_hash"] != other || m["live_hash"] != other) {
				t.Fatalf("settled %+v, RunLive ran %v, row %v: want ran %v, recorded %v", s, ran, m, tc.ran, tc.recorded)
			}
			if tc.err != nil && (s.Outcome != settledFailed || s.Cause != causeRunLive) {
				t.Fatalf("settled %+v, want failed with cause %q", s, causeRunLive)
			}
		})
	}
}

// Kills the key plan drifting from freshRestartSessionKeyInfo (the C7b2-1
// review's R05): a provider that takes a session id mints; one that only
// resumes clears; with no provider resolved, the row's stored
// session_id_flag decides.
func TestRestartKeyMints(t *testing.T) {
	for _, tc := range []struct {
		name string
		tp   TemplateParams
		info session.Info
		want bool
	}{
		{"a provider taking a session id", TemplateParams{ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}, session.Info{}, true},
		{"a resume-only provider", TemplateParams{ResolvedProvider: &config.ResolvedProvider{ResumeFlag: "--resume"}}, session.Info{SessionIDFlag: "--session-id"}, false},
		{"no provider, the stored flag", TemplateParams{}, session.Info{SessionIDFlag: "--session-id"}, true},
		{"no provider, no stored flag", TemplateParams{}, session.Info{}, false},
	} {
		if got := restartKeyMints(tc.tp, tc.info); got != tc.want {
			t.Errorf("%s: restartKeyMints = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Kills the resume-preserving guard ignoring its lease (the C7b2-1
// review's R09): an in-place restart leased in creating keeps its hash and
// key; once its lease has run out, it is repaired.
func TestPendingResumePreservingNamedRestartAt(t *testing.T) {
	row := func(started time.Duration) session.Info {
		return session.Info{
			MetadataState: string(session.StateCreating), PendingCreateClaim: true, SessionKey: "k-1", StartedConfigHash: "v6:x",
			PendingCreateStartedAt: gatherNow.Add(-started).UTC().Format(time.RFC3339),
		}
	}
	if !pendingResumePreservingNamedRestartAt(row(10*time.Second), gatherNow, time.Minute) {
		t.Fatal("a fresh lease: want the restart preserved")
	}
	if pendingResumePreservingNamedRestartAt(row(time.Hour), gatherNow, time.Minute) {
		t.Fatal("an expired lease: want the restart no longer preserved")
	}
}

// Kills a refused composed start writing the repair with no key (the
// C7b2-1 review's should-fix): the repair the start carries, written alone
// while the runtime reads absent, mints the key its provider takes, as the
// repair's own effect does.
func TestComposedRepairMintsWhenWrittenAlone(t *testing.T) {
	info := session.Info{ID: "gc-1", SessionKey: "conversation-1"}
	w := &World{Templates: &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: TemplateParams{ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}}}}}
	heal := intent{Kind: intentDriftRepair, Reason: decideDriftRepair, Patch: session.MetadataPatch{"state": "asleep", "session_key": ""}}
	step := healInstead(txView{It: intent{Heal: &heal}, World: w, Row: info, RT: &txRuntime{Class: rtAbsent}}, txStep{Refuse: causeHeld})
	if key := step.Write["session_key"]; key == "" || key == "conversation-1" || heal.Patch["session_key"] != "" {
		t.Fatalf("write %v (the intent's patch %v), want a fresh key on a copy", step.Write, heal.Patch)
	}
}
