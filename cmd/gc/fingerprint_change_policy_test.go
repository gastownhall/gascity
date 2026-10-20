package main

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The per-field change policy (ARCH-SIMPLIFICATION-REVIEW §3.5, Phase 0):
// pool sizing is Ignore, and prompt, skill content and wake_mode NextStart,
// so editing them never drains a running session; a field that changes the
// running process (the command, the operator env, the MCP projection) still
// restarts it.
//
// The v6-prefixed literals are the hashes the binary before the policy (v6)
// stored for each fixture, computed by that binary: a row it started carries
// them on deploy and on a rollback from v7.

// policyExtra is a FingerprintExtra carrying every key family.
func policyExtra() map[string]string {
	return map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1", "mcp:claude": "m1", "depends_on": "db", "wake_mode": "fresh"}
}

// policyTP is a worker template carrying every FingerprintExtra family.
func policyTP() TemplateParams {
	return TemplateParams{TemplateName: "worker", Command: "run", Prompt: "old prompt", FPExtra: policyExtra()}
}

// v6 baselines of policyTP on drainRow (and on baselineCase's row: the same
// config), as the v6 binary stored them.
const (
	v6PolicyCore      = "v6:48995566d003a3c5e7e8e3b9e9ee74cd0f29fdec585abc4c9e8a718c77ccc886"
	v6PolicyProvision = "v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8"
	v6PolicyLaunch    = "v6:6b2d83c0c474181632b8ac73197fcda0418eaeb2f15d75ba16688c5e752d55a4"
	v6PolicyLive      = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"
)

// v6Row is a row's four stored baselines.
func v6Row(core, provision, launch, live string) []string {
	return []string{"started_config_hash", core, "started_provision_hash", provision, "started_launch_hash", launch, "started_live_hash", live, "live_hash", live}
}

// Kills a pool-size, skill, prompt or wake_mode edit that drains a running
// session; a command, operator-env or MCP edit that no longer restarts it; a
// deploy (or a rollback) that drains a v6 row's unchanged config instead of
// A8's silent rebaseline, or whose rebaseline swallows a real drift, rewrites
// the live pair, or drains a pending launch-only change instead of
// relaunching it in its warm box.
func TestDecideRowFingerprintChangePolicy(t *testing.T) {
	withoutA7(t)
	base := censusRowOf(t, drainRow()).Info
	startedCfg := sessionCoreConfigForHashInfo(policyTP(), base)
	current := v6Row(runtime.CoreFingerprint(startedCfg), runtime.ProvisionFingerprint(startedCfg), runtime.LaunchFingerprint(startedCfg), runtime.LiveFingerprint(startedCfg))
	prePolicy := v6Row(v6PolicyCore, v6PolicyProvision, v6PolicyLaunch, v6PolicyLive)
	mutate := func(f func(*TemplateParams)) TemplateParams {
		tp := policyTP()
		tp.FPExtra = maps.Clone(tp.FPExtra)
		f(&tp)
		return tp
	}
	other := func(h string) string { return h[:len(h)-4] + "beef" }
	relaunch, drain := []string{intentDriftRelaunch, decideDriftRelaunch}, []string{intentDrainBegin, decideDrainBegin + drainConfigDrift}
	for _, tc := range []struct {
		name       string
		tp         TemplateParams
		stored     []string
		noRelaunch bool     // a provider that cannot relaunch
		want       []string // kind, reason; nil for no drift
	}{
		{name: "pool.max raised", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["pool.max"] = "5" }), stored: current},
		{name: "pool.min raised", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["pool.min"] = "1" }), stored: current},
		{name: "skill content edited", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:plan"] = "h2" }), stored: current},
		{name: "skill added", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:review"] = "h3" }), stored: current},
		{name: "prompt edited", tp: mutate(func(tp *TemplateParams) { tp.Prompt = "new prompt" }), stored: current},
		{name: "wake_mode changed", tp: mutate(func(tp *TemplateParams) { delete(tp.FPExtra, "wake_mode") }), stored: current},
		{name: "command changed relaunches", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: current, want: relaunch},
		{name: "command changed drains where no relaunch", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: current, noRelaunch: true, want: drain},
		{name: "mcp projection changed drains", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["mcp:claude"] = "m2" }), stored: current, want: drain},
		{name: "a v6 row with unchanged config rebaselines", tp: policyTP(), stored: prePolicy, want: []string{intentBaseline, decideBaselinePrePolicy}},
		{name: "a v6 row whose pool size changed since drains once", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["pool.max"] = "5" }), stored: prePolicy, want: drain},
		{name: "a v6 row whose skill changed since drains once", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:plan"] = "h2" }), stored: prePolicy, want: drain},
		{name: "a v6 row whose command changed since relaunches", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: prePolicy, want: relaunch},
		{name: "a v6 row whose operator env changed since relaunches", tp: mutate(func(tp *TemplateParams) { tp.OperatorEnv = map[string]string{"PATH": "/opt/new-pin:/usr/bin"} }), stored: prePolicy, want: relaunch},
		// The partition puts every core field in one half: a core drift whose
		// stored halves both still match is no launch-only drift.
		{name: "a v6 core drift whose stored halves match is no relaunch", tp: policyTP(), stored: v6Row(other(v6PolicyCore), v6PolicyProvision, v6PolicyLaunch, v6PolicyLive), want: drain},
		{name: "another v6 core hash drains once", tp: policyTP(), stored: v6Row(other(v6PolicyCore), other(v6PolicyProvision), v6PolicyLaunch, v6PolicyLive), want: drain},
		{name: "a v6 live hash of unchanged session_live rebaselines", tp: policyTP(), stored: v6Row(current[1], current[3], current[5], v6PolicyLive), want: []string{intentBaseline, decideBaselineVersion}},
		{name: "another v6 live hash runs session_live again", tp: policyTP(), stored: v6Row(current[1], current[3], current[5], other(v6PolicyLive)), want: []string{intentLiveApply, decideLiveApply}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, _ := decideDrain(t, tc.stored, func(e *selectionEntry) {
				e.InDesired, e.Desired = true, desireKeep
			}, func(w *World) {
				info := w.Census.Rows[rowKeyOf("gc-1")].Info
				var sp runtime.Provider = runtime.NewFake() // a RelaunchProvider
				if tc.noRelaunch {
					sp = struct{ runtime.Provider }{runtime.NewFake()}
				}
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: sp}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tc.tp}}}
				w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
			})
			drift := slices.Contains([]string{decideDriftAttached, decideDriftPinned, decideDrainBegin + drainConfigDrift, decideDriftRepair, decideDriftRestart, decideDriftRelaunch, decideLiveApply, decideBaselineVersion, decideBaselinePrePolicy}, it.Reason)
			if tc.want == nil && drift || tc.want != nil && (it.Kind != tc.want[0] || it.Reason != tc.want[1]) {
				t.Fatalf("decideRow = (%q, %q) %v, want %v", it.Kind, it.Reason, it.Patch, tc.want)
			}
			if it.Reason == decideBaselinePrePolicy {
				want := baselinePatch(startedCfg, baselinePrePolicy)
				if !maps.Equal(it.Patch, want) || len(want) != 4 || it.Patch["started_config_hash"] != current[1] {
					t.Fatalf("pre-policy rebaseline %v, want the core, provision, launch and breakdown only %v", it.Patch, want)
				}
			}
		})
	}
	if runtime.PrePolicyCoreFingerprint(startedCfg) != v6PolicyCore || runtime.PrePolicyProvisionFingerprint(startedCfg) != v6PolicyProvision ||
		runtime.PrePolicyLaunchFingerprint(startedCfg) != v6PolicyLaunch || runtime.PrePolicyLiveFingerprint(startedCfg) != v6PolicyLive {
		t.Fatal("the PrePolicy* hashes no longer reproduce the v6 binary's")
	}
}

// Kills A14 repairing (resetting the conversation of) an asleep named row
// the v6 binary started whose config is unchanged, where A8 leaves the row
// to the start it gets this pass (SESS-567/568): the start resumes it on
// its kept key.
func TestDecideRowAsleepNamedPrePolicyHashIsNoRepair(t *testing.T) {
	withoutA7(t)
	named := []string{"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always"}
	meta := slices.Concat([]string{"state", "asleep", "session_key", "k-1"}, v6Row(v6PolicyCore, v6PolicyProvision, v6PolicyLaunch, v6PolicyLive), named)
	it, _ := decideDrain(t, meta, func(e *selectionEntry) {
		e.InDesired, e.Desired, e.Liveness = true, desireWake, livenessGone
	}, func(w *World) {
		info := w.Census.Rows[rowKeyOf("gc-1")].Info
		w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: runtime.NewFake()}
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: policyTP()}}}
	})
	if it.Reason == decideDriftRepair || it.Patch["continuation_reset_pending"] == "true" { // a repair composed into the start resets the conversation
		t.Fatalf("decideRow = (%q, %q) %v, want no drift repair of a pre-policy hash", it.Kind, it.Reason, it.Patch)
	}
}

// Kills the relaunch effect's anti-skew gate reading a v6 row's provision
// hash as another box: a launch-only drift on it relaunches in the warm box.
func TestRelaunchCallPrePolicyRow(t *testing.T) {
	tp := TemplateParams{TemplateName: "worker", Command: "run", FPExtra: policyExtra()}
	c, _ := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active", "session_key", "k-1",
		"started_config_hash", "v6:stale", "started_provision_hash", v6PolicyProvision, "started_launch_hash", "v6:old-launch")
	row := c.w.Census.Rows[c.k].Info
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row): {TP: tp}}}
	fake := runtime.NewFake()
	if err := fake.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	c.w.Env = &reconcileEnv{Cfg: workerCity(1)}
	caps := txCaps{start: startCaps{sp: runtime.NewFake()}, reads: effectReads{city: c.store}}
	out, err := relaunchCall(t.Context(), caps, relaunchPlan{w: c.w, row: row, tp: tp, hash: sessionCoreConfigForHashInfo(tp, row), name: "s-heal", leaf: fake, now: gatherNow})
	if err != nil || !out.OK || fake.CountCalls("Relaunch", "s-heal") != 1 {
		t.Fatalf("relaunch %+v (%v), Relaunch calls %d; want it landed in the warm box", out, err, fake.CountCalls("Relaunch", "s-heal"))
	}
}

// Kills the relaunch effect's anti-skew gate missing a v6 launch hash that
// still matches the prepared config: no launch drift is left to relaunch.
func TestRelaunchCallPrePolicyLaunchUnchangedIsSkew(t *testing.T) {
	tp := TemplateParams{TemplateName: "worker", Command: "run", FPExtra: policyExtra()}
	c, _ := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active", "session_key", "k-1",
		"started_config_hash", "v6:stale", "started_provision_hash", v6PolicyProvision, "started_launch_hash", v6PolicyLaunch)
	row := c.w.Census.Rows[c.k].Info
	c.w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(row): {TP: tp}}}
	fake := runtime.NewFake()
	if err := fake.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	c.w.Env = &reconcileEnv{Cfg: workerCity(1)}
	caps := txCaps{start: startCaps{sp: runtime.NewFake()}, reads: effectReads{city: c.store}}
	out, err := relaunchCall(t.Context(), caps, relaunchPlan{w: c.w, row: row, tp: tp, hash: sessionCoreConfigForHashInfo(tp, row), name: "s-heal", leaf: fake, now: gatherNow})
	if err != nil || out.OK || out.Err == nil || out.Err.Error() != causeRelaunchSkew || fake.CountCalls("Relaunch", "s-heal") != 0 {
		t.Fatalf("relaunch %+v (%v), Relaunch calls %d; want the skew refusal", out, err, fake.CountCalls("Relaunch", "s-heal"))
	}
}

// Kills the legacy reconciler draining a session the v6 binary started whose
// config is unchanged (its alive drift block, SESS-551), or rewriting its
// live pair: the core baselines are silently rebaselined, the live pair left
// to its own check.
func TestReconcilerRebaselinesPrePolicyHash(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = policyExtra()
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	const v6Live = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"
	env.setSessionMetadata(&session, sliceMap(v6Row(
		"v6:47160838d4b095c47ee45ad4515a6728282015a11a01affa0549a73b244ce41a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:3169d460594ef6b246d2e6876dd3b2fd9678c16fe73673d3f8a79bb35143b608",
		v6Live)))
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("pre-policy hash of unchanged config drained: %+v; stderr=%s", ds, env.stderr.String())
	}
	got := env.sessionInfo(session.ID)
	if got.StartedConfigHash != runtime.CoreFingerprint(cfg) || got.StartedProvisionHash != runtime.ProvisionFingerprint(cfg) || got.StartedLaunchHash != runtime.LaunchFingerprint(cfg) {
		t.Fatalf("baselines (%q, %q, %q), want rebaselined to this binary's", got.StartedConfigHash, got.StartedProvisionHash, got.StartedLaunchHash)
	}
	if got.StartedLiveHash != v6Live {
		t.Fatalf("started_live_hash = %q, want left %q to the live check", got.StartedLiveHash, v6Live)
	}
}

// Kills the legacy reconciler repairing an asleep named session the v6
// binary started whose config is unchanged (its asleep named drift block,
// SESS-567/568): the stored hash is rebaselined, its conversation kept.
func TestReconcilerRebaselinesAsleepNamedPrePolicyHash(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.addDesired(sessionName, "worker", false)
	tp := env.desiredState[sessionName]
	tp.FPExtra = policyExtra()
	env.desiredState[sessionName] = tp
	session := env.createSessionBead(sessionName, "worker")
	meta := sliceMap(v6Row(
		"v6:47160838d4b095c47ee45ad4515a6728282015a11a01affa0549a73b244ce41a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:3169d460594ef6b246d2e6876dd3b2fd9678c16fe73673d3f8a79bb35143b608",
		"v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"))
	maps.Copy(meta, map[string]string{
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "worker", namedSessionModeMetadata: "on_demand", "session_key": "conversation-1",
	})
	env.setSessionMetadata(&session, meta)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))

	env.reconcile([]beads.Bead{session})

	got := env.sessionInfo(session.ID)
	if got.StartedConfigHash != runtime.CoreFingerprint(cfg) || got.SessionKey != "conversation-1" {
		t.Fatalf("started_config_hash = %q, session_key = %q; want rebaselined to %q keeping conversation-1; stderr=%s",
			got.StartedConfigHash, got.SessionKey, runtime.CoreFingerprint(cfg), env.stderr.String())
	}
}

// Kills the legacy launch-only verdict or its anti-skew gate reading a v6
// row's provision hash as another box: a launch-only drift (the command)
// on a session the v6 binary started relaunches in the warm box.
func TestReconcileSessionBeads_LaunchDriftRelaunchesPrePolicyRow(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{
		Command: "claude", SessionName: "worker", TemplateName: "worker", InstanceName: "worker", Alias: "worker",
		Prompt: "do the work", ResolvedProvider: forkClaude(), FPExtra: policyExtra(),
	}
	env.desiredState["worker"] = tp
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	// The v6 binary's baselines of the same config under "stale-claude".
	meta := sliceMap(v6Row(
		"v6:9d63e81f3e25e9fce995221249c632b932ef6a564418c00cd48524bf95a26d2a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:839c045398cbfb6f95317af405a482903d69d1a201b4f70ae551c80ae615b65b",
		"v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"))
	meta["session_key"] = "warm-conversation"
	env.setSessionMetadata(&session, meta)

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1 (warm-box relaunch); stderr=%s", got, env.stderr.String())
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("launch-only drift on a v6 row drained: %+v", ds)
	}
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	if got := env.sessionInfo(session.ID); got.StartedConfigHash != runtime.CoreFingerprint(cfg) || got.StartedProvisionHash != runtime.ProvisionFingerprint(cfg) || got.StartedLaunchHash != runtime.LaunchFingerprint(cfg) {
		t.Fatalf("baselines (%q, %q, %q), want this binary's", got.StartedConfigHash, got.StartedProvisionHash, got.StartedLaunchHash)
	}
}

// Kills a pre-policy rebaseline traced as a version mismatch, and either
// other outcome moved.
func TestRebaselineLegacyHashOutcome(t *testing.T) {
	for _, tc := range []struct {
		stored, prePolicy string
		want              TraceOutcomeCode
	}{
		{v6PolicyCore, v6PolicyCore, TraceOutcomeRebaselinedPrePolicy},
		{"v5:" + strings.Repeat("a", 64), v6PolicyCore, TraceOutcomeRebaselinedVersionMismatch},
		{strings.Repeat("a", 64), v6PolicyCore, TraceOutcomeRebaselinedUnversioned},
	} {
		if got := rebaselineLegacyHashOutcome(tc.stored, tc.prePolicy); got != tc.want {
			t.Errorf("rebaselineLegacyHashOutcome(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// sliceMap is a key, value list as a map.
func sliceMap(kv []string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// Kills the policy applying only to hand-built TemplateParams: through
// resolveTemplate, a pool-size, wake_mode or skill-content edit leaves the started
// config's core fingerprint unchanged, a prompt edit too while the next
// start's config carries the new prompt, and a command edit moves it.
func TestResolveTemplateFingerprintChangePolicy(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"),
		[]byte("[pack]\nname = \"fp-test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cityPath, "skills", "plan")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(cityPath, "prompts", "worker.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(skillDir, "SKILL.md"), "---\nname: plan\ndescription: test\n---\nbody v1\n")
	write(promptPath, "prompt body v1")

	type knobs struct {
		max     int
		command string
		wake    string
	}
	resolve := func(k knobs) (TemplateParams, runtime.Config) {
		t.Helper()
		sharedCat, err := materialize.LoadCityCatalog(filepath.Join(cityPath, "skills"))
		if err != nil {
			t.Fatal(err)
		}
		bp := &agentBuildParams{
			cityName:  "city",
			cityPath:  cityPath,
			workspace: &config.Workspace{Provider: "claude"},
			providers: map[string]config.ProviderSpec{
				"claude": {Command: k.command, PromptMode: "arg"},
			},
			lookPath:        func(string) (string, error) { return "/bin/echo", nil },
			fs:              fsys.OSFS{},
			rigs:            []config.Rig{},
			beaconTime:      time.Unix(0, 0),
			beadNames:       make(map[string]string),
			stderr:          io.Discard,
			skillCatalog:    &sharedCat,
			sessionProvider: "tmux",
		}
		a := &config.Agent{
			Name: "worker", Scope: "city", Provider: "claude",
			PromptTemplate:    "prompts/worker.md",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(k.max),
			WakeMode: k.wake,
		}
		tp, err := resolveTemplate(bp, a, a.QualifiedName(), buildFingerprintExtra(a))
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		return tp, templateParamsToConfig(tp)
	}
	startedTP, startedCfg := resolve(knobs{max: 2, command: "echo"})
	started := runtime.CoreFingerprint(startedCfg)
	if startedTP.FPExtra["pool.max"] != "2" || startedTP.FPExtra["skills:plan"] == "" {
		t.Fatalf("FPExtra = %v, want pool sizing and the skill hash recorded (the policy filters at hash time)", startedTP.FPExtra)
	}

	if _, cfg := resolve(knobs{max: 5, command: "echo"}); runtime.CoreFingerprint(cfg) != started {
		t.Errorf("pool.max 2 -> 5 moved the core fingerprint: a running session would drift-drain")
	}
	if tp, cfg := resolve(knobs{max: 2, command: "echo", wake: "fresh"}); tp.FPExtra["wake_mode"] != "fresh" {
		t.Fatalf("test setup: FPExtra = %v, want wake_mode recorded", tp.FPExtra)
	} else if runtime.CoreFingerprint(cfg) != started {
		t.Errorf("wake_mode resume -> fresh moved the core fingerprint: a running session would drift-drain")
	}

	write(filepath.Join(skillDir, "SKILL.md"), "---\nname: plan\ndescription: test\n---\nbody v2\n")
	skillTP, skillCfg := resolve(knobs{max: 2, command: "echo"})
	if skillTP.FPExtra["skills:plan"] == startedTP.FPExtra["skills:plan"] {
		t.Fatal("test setup: the skill edit did not change its content hash")
	}
	if runtime.CoreFingerprint(skillCfg) != started {
		t.Errorf("skill content edit moved the core fingerprint: a running session would drift-drain")
	}

	// Compared with the config just before the edit, so the prompt alone is
	// under test.
	write(promptPath, "prompt body v2")
	_, cfg := resolve(knobs{max: 2, command: "echo"})
	if runtime.CoreFingerprint(cfg) != runtime.CoreFingerprint(skillCfg) {
		t.Errorf("prompt edit moved the core fingerprint: a running session would drift-drain")
	}
	if !strings.Contains(cfg.PromptSuffix, "prompt body v2") || strings.Contains(cfg.PromptSuffix, "prompt body v1") {
		t.Errorf("next start's PromptSuffix = %q, want the edited prompt", cfg.PromptSuffix)
	}

	if _, cfg := resolve(knobs{max: 2, command: "echo --changed"}); runtime.CoreFingerprint(cfg) == started {
		t.Errorf("command change left the core fingerprint unchanged: a running session would never restart onto it")
	}
}

// Kills the legacy live check rebaselining a v6 live hash whose session_live
// changed since start as a version artifact: session_live runs again.
func TestReconcileSessionBeads_PrePolicyLiveDriftReapplied(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	live := []string{"echo live-updated"}
	env.desiredState["worker"] = TemplateParams{Command: "test-cmd", SessionName: "worker", TemplateName: "worker", Hints: agent.StartupHints{SessionLive: live}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd", SessionLive: live}); err != nil {
		t.Fatal(err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	const v6NoLive = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a" // the v6 binary's live hash of no session_live
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
		"started_live_hash":   v6NoLive,
		"live_hash":           v6NoLive,
	})

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("RunLive", "worker"); got != 1 {
		t.Fatalf("RunLive calls = %d, want 1 (a live drift since start re-applies session_live); stderr=%s", got, env.stderr.String())
	}
}

// Kills A8's pre-policy rebaseline recording a session_live change as run
// (SESS-551/565): a v6 core hash of unchanged config moves the core
// baseline alone, so the next pass runs the session_live that changed since
// the start, or that an empty live hash never recorded.
func TestDecideRowPrePolicyBaselineLeavesLiveDrift(t *testing.T) {
	withoutA7(t)
	tp := policyTP()
	tp.Hints.SessionLive = []string{"echo new"}
	cfg := sessionCoreConfigForHashInfo(tp, censusRowOf(t, drainRow()).Info)
	decide := func(meta []string) intent {
		it, _ := decideDrain(t, meta, func(e *selectionEntry) {
			e.InDesired, e.Desired = true, desireKeep
		}, func(w *World) {
			info := w.Census.Rows[rowKeyOf("gc-1")].Info
			w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: runtime.NewFake()}
			w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
		})
		return it
	}
	for _, tc := range []struct{ name, storedLive string }{
		{"another session_live's v6 live hash", "v6:fb69842ce33baefb690b98d9424794432f592e34b684d4ef8220a768d9b6787f"}, // "echo old", as the v6 binary hashed it
		{"an empty live hash", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := []string{"started_config_hash", v6PolicyCore, "started_provision_hash", v6PolicyProvision, "started_launch_hash", v6PolicyLaunch, "started_live_hash", tc.storedLive, "live_hash", tc.storedLive}
			it := decide(meta)
			if want := baselinePatch(cfg, baselinePrePolicy); it.Kind != intentBaseline || it.Reason != decideBaselinePrePolicy || !maps.Equal(it.Patch, want) {
				t.Fatalf("pass 1: decideRow = (%q, %q) %v, want (%q, %q) writing the core baseline alone %v", it.Kind, it.Reason, it.Patch, intentBaseline, decideBaselinePrePolicy, want)
			}
			for k, v := range it.Patch {
				meta = append(meta, k, v)
			}
			if it := decide(meta); it.Kind != intentLiveApply || it.Reason != decideLiveApply {
				t.Fatalf("pass 2: decideRow = (%q, %q) %v, want session_live run again (%q, %q)", it.Kind, it.Reason, it.Patch, intentLiveApply, decideLiveApply)
			}
		})
	}
}

// Kills the legacy reconciler's pre-policy rebaseline recording a
// session_live change as run (its alive drift block, SESS-551/565): the first
// tick moves the core baseline alone, and the next runs the session_live that
// changed since the start and records it.
func TestReconcilerPrePolicyRebaselineLeavesLiveDrift(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = policyExtra()
	tp.Hints.SessionLive = []string{"echo new"}
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	const storedLive = "v6:fb69842ce33baefb690b98d9424794432f592e34b684d4ef8220a768d9b6787f" // "echo old", as the v6 binary hashed it
	env.setSessionMetadata(&session, sliceMap(v6Row(
		"v6:47160838d4b095c47ee45ad4515a6728282015a11a01affa0549a73b244ce41a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:3169d460594ef6b246d2e6876dd3b2fd9678c16fe73673d3f8a79bb35143b608",
		storedLive)))

	env.reconcile([]beads.Bead{session})
	b, _ := env.store.Get(session.ID)
	if got := b.Metadata["started_config_hash"]; got != runtime.CoreFingerprint(cfg) || b.Metadata["started_provision_hash"] != runtime.ProvisionFingerprint(cfg) {
		t.Fatalf("tick 1: started_config_hash = %q, started_provision_hash = %q; want the core baseline %q; stderr=%s", got, b.Metadata["started_provision_hash"], runtime.CoreFingerprint(cfg), env.stderr.String())
	}
	if got := b.Metadata["started_live_hash"]; got != storedLive || b.Metadata["live_hash"] != storedLive {
		t.Fatalf("tick 1: started_live_hash = %q, live_hash = %q; want both left %q for the live check", got, b.Metadata["live_hash"], storedLive)
	}
	if got := env.sp.CountCalls("RunLive", "worker"); got != 0 {
		t.Fatalf("tick 1: RunLive calls = %d, want 0", got)
	}

	env.reconcile([]beads.Bead{b})
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("pre-policy hash of unchanged config drained: %+v; stderr=%s", ds, env.stderr.String())
	}
	if got := env.sp.CountCalls("RunLive", "worker"); got != 1 {
		t.Fatalf("tick 2: RunLive calls = %d, want 1 (the changed session_live run); stderr=%s", got, env.stderr.String())
	}
	if got := env.sessionInfo(session.ID).StartedLiveHash; got != runtime.LiveFingerprint(cfg) {
		t.Fatalf("tick 2: started_live_hash = %q, want %q", got, runtime.LiveFingerprint(cfg))
	}
}

// Kills the legacy reconciler recording a pre-policy row's real drift as
// applied when another holder's runtime lease refuses its named restart in
// place, which falls through to the live check (#2574): the row's v6 live
// hash of the same session_live read as an artifact, and its rebaseline moved
// the core baseline over the drift. The drift is kept, and the next tick, the
// lease free, restarts the session onto it.
func TestReconcilerRefusedNamedRestartKeepsPrePolicyDrift(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.addDesired(name, "worker", true)
	tp := env.desiredState[name]
	tp.ConfiguredNamedIdentity, tp.ConfiguredNamedMode = "worker", "always"
	tp.FPExtra = policyExtra()
	env.desiredState[name] = tp
	session := env.createSessionBead(name, "worker")
	env.markSessionActive(&session)
	// The v6 binary's core hash of this config under "stale-test-cmd", and
	// its live hash of the same (no) session_live.
	const (
		stored     = "v6:b0ad3b64224b678cb4d3b66dd096aa3f6a53f3fd2e76e423b2ed59a12c0b4add"
		storedLive = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"
	)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"started_config_hash":        stored,
		"started_live_hash":          storedLive,
		"live_hash":                  storedLive,
	})
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(env.store), sessionpkg.RuntimeLeaseRequest{City: env.city, Name: name, ID: session.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	starts := env.sp.CountCalls("Start", name)

	env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})
	if got := env.sessionInfo(session.ID).StartedConfigHash; got != stored {
		t.Fatalf("tick 1, restart refused: started_config_hash = %q, want the drifted baseline %q kept; stderr=%s", got, stored, env.stderr.String())
	}
	if got := env.sp.CountCalls("Start", name); got != starts {
		t.Fatalf("tick 1, restart refused: Start calls = %d, want %d", got, starts)
	}

	held.Release()
	env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})
	if got := env.sp.CountCalls("Start", name); got != starts+1 {
		t.Fatalf("tick 2: Start calls = %d, want %d (the restart onto the drift); stderr=%s", got, starts+1, env.stderr.String())
	}
	if got := env.sessionInfo(session.ID).StartedConfigHash; got == stored || got == "" {
		t.Fatalf("tick 2: started_config_hash = %q, want the restarted session's", got)
	}
}

// Kills A14 rebaselining, rather than repairing, an asleep named row the v6
// binary started whose config changed since (SESS-566/568): as under v6,
// its drift is repaired, its conversation reset, never resumed on the
// drifted config.
func TestDecideRowAsleepNamedV6DriftIsRepaired(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*TemplateParams)
	}{
		{"pool.max changed since", func(tp *TemplateParams) { tp.FPExtra["pool.max"] = "5" }},
		{"command changed since", func(tp *TemplateParams) { tp.Command = "run --other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withoutA7(t)
			named := []string{"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always"}
			meta := slices.Concat([]string{"state", "asleep", "session_key", "k-1"}, v6Row(v6PolicyCore, v6PolicyProvision, v6PolicyLaunch, v6PolicyLive), named)
			tp := policyTP()
			tc.edit(&tp)
			it, _ := decideDrain(t, meta, func(e *selectionEntry) {
				e.InDesired, e.Desired, e.Liveness = true, desireWake, livenessGone
			}, func(w *World) {
				info := w.Census.Rows[rowKeyOf("gc-1")].Info
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: runtime.NewFake()}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			})
			if it.Reason != decideDriftRepair && it.Patch["continuation_reset_pending"] != "true" { // a repair composed into the start resets the conversation
				t.Fatalf("decideRow = (%q, %q) %v, want the drift repair of a v6 drift", it.Kind, it.Reason, it.Patch)
			}
		})
	}
}

// Kills the legacy asleep named block rebaselining, rather than repairing
// (SESS-568), a session the v6 binary started whose config changed since:
// its baseline is cleared and its conversation key not kept.
func TestReconcilerRepairsAsleepNamedV6Drift(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.addDesired(sessionName, "worker", false)
	tp := env.desiredState[sessionName]
	tp.FPExtra = policyExtra()
	tp.FPExtra["pool.max"] = "5" // the stored hashes are of pool.max=2
	env.desiredState[sessionName] = tp
	session := env.createSessionBead(sessionName, "worker")
	meta := sliceMap(v6Row(
		"v6:47160838d4b095c47ee45ad4515a6728282015a11a01affa0549a73b244ce41a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:3169d460594ef6b246d2e6876dd3b2fd9678c16fe73673d3f8a79bb35143b608",
		"v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"))
	maps.Copy(meta, map[string]string{
		namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "worker", namedSessionModeMetadata: "on_demand", "session_key": "conversation-1",
	})
	env.setSessionMetadata(&session, meta)

	env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})

	got := env.sessionInfo(session.ID)
	if got.StartedConfigHash != "" || got.SessionKey == "conversation-1" {
		t.Fatalf("started_config_hash = %q, session_key = %q; want the drift repaired (hash cleared, conversation-1 not kept); stderr=%s",
			got.StartedConfigHash, got.SessionKey, env.stderr.String())
	}
}

// v6NoLive is the v6 binary's live hash of no session_live.
const v6NoLive = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"

// Kills the legacy live check keeping a v6 live pair it rebaselines (the
// rebaseline moved nothing, so it repeats every tick): a row whose core
// baselines are this binary's but whose live pair is the v6 binary's of the
// same session_live has the live pair rebaselined, without RunLive.
func TestReconcilerRebaselinesV6LiveArtifact(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = policyExtra()
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	env.setSessionMetadata(&session, sliceMap(v6Row(runtime.CoreFingerprint(cfg), runtime.ProvisionFingerprint(cfg), runtime.LaunchFingerprint(cfg), v6NoLive)))

	env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})

	b := mustGetBead(t, env.store, session.ID)
	if b.Metadata["started_live_hash"] != runtime.LiveFingerprint(cfg) || b.Metadata["live_hash"] != runtime.LiveFingerprint(cfg) {
		t.Fatalf("live pair = (%q, %q), want rebaselined to %q; stderr=%s", b.Metadata["started_live_hash"], b.Metadata["live_hash"], runtime.LiveFingerprint(cfg), env.stderr.String())
	}
	if n := env.sp.CountCalls("RunLive", "worker"); n != 0 {
		t.Fatalf("RunLive calls = %d, want 0", n)
	}
}

// Kills a legacy migration of a v6 row of unchanged config that does not
// converge: tick 1 moves the core four, tick 2 the live pair, tick 3
// nothing; never a drain or RunLive.
func TestReconcilerV6RowConvergesInTwoTicks(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = policyExtra()
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, sliceMap(v6Row(
		"v6:47160838d4b095c47ee45ad4515a6728282015a11a01affa0549a73b244ce41a",
		"v6:c5da5d5d182c30f841fb059a443ec834928885fa2aae393587698334fa885ab8",
		"v6:3169d460594ef6b246d2e6876dd3b2fd9678c16fe73673d3f8a79bb35143b608",
		v6NoLive)))
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	for tick := 1; tick <= 3; tick++ {
		env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})
		if ds := env.dt.get(session.ID); ds != nil {
			t.Fatalf("tick %d drained: %+v", tick, ds)
		}
	}
	b := mustGetBead(t, env.store, session.ID)
	for k, want := range map[string]string{
		"started_config_hash": runtime.CoreFingerprint(cfg), "started_provision_hash": runtime.ProvisionFingerprint(cfg),
		"started_launch_hash": runtime.LaunchFingerprint(cfg), "started_live_hash": runtime.LiveFingerprint(cfg), "live_hash": runtime.LiveFingerprint(cfg),
	} {
		if b.Metadata[k] != want {
			t.Errorf("after 3 ticks %s = %q, want %q", k, b.Metadata[k], want)
		}
	}
	if n := env.sp.CountCalls("RunLive", "worker"); n != 0 {
		t.Fatalf("RunLive calls = %d, want 0", n)
	}
}

// Kills the v6 rule leaking to other versions: a future (v8) row is still
// a silent rebaseline of all six keys.
func TestReconcilerFutureVersionRebaselinesAllSix(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = policyExtra()
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, sliceMap(v6Row("v8:aaaa", "v8:bbbb", "v8:cccc", "v8:dddd")))
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))

	env.reconcile([]beads.Bead{mustGetBead(t, env.store, session.ID)})

	b := mustGetBead(t, env.store, session.ID)
	if b.Metadata["started_config_hash"] != runtime.CoreFingerprint(cfg) || b.Metadata["started_live_hash"] != runtime.LiveFingerprint(cfg) {
		t.Fatalf("v8 row: core=%q live=%q, want both rebaselined; stderr=%s", b.Metadata["started_config_hash"], b.Metadata["started_live_hash"], env.stderr.String())
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("v8 row drained: %+v", ds)
	}
}
