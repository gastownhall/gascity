package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The per-field change policy (runtime.FingerprintExtraChangePolicy): a
// Restart input's change drains or restarts a running session onto it, a
// NextStart one's reaches the session's next start, and an Ignore one's
// touches no session. Pool sizing is Ignore and prompt and skill content
// NextStart, so editing them never drains a running session; a field that
// changes the running process (here the command, and the MCP projection)
// still does.

// policyTP is a worker template carrying every FingerprintExtra family.
func policyTP() TemplateParams {
	return TemplateParams{TemplateName: "worker", Command: "run", Prompt: "old prompt", FPExtra: map[string]string{
		"pool.min": "0", "pool.max": "2", "skills:plan": "h1", "mcp:claude": "m1", "depends_on": "db",
	}}
}

// policyTP's core, provision, launch and live hashes for drainRow as the v6
// binary, before the change policy, computed them (pool sizing and skill
// hashes inside the first two), as production rows carry them on deploy.
const (
	storedBeforePolicy    = "v6:c1e79facd911536834b03eef3c8fb2ac62231e5b704540477d3de7f51b34acd5"
	provisionBeforePolicy = "v6:8a6b242b1cdcfe263d978ef8c469565f934caf637bd4df30dd6ba1fe9f0316e4"
	launchBeforePolicy    = "v6:6b2d83c0c474181632b8ac73197fcda0418eaeb2f15d75ba16688c5e752d55a4"
	liveBeforePolicy      = "v6:4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a"
)

// Kills a pool-size, skill or prompt edit that drains a running session, a
// command or MCP edit that no longer does, a deploy that reads a
// pre-policy row's unchanged config as drift (a drain wave) instead of A8's
// silent rebaseline of its core baseline, one whose rebaseline swallows a
// real drift, and one that reads a pre-policy row's unchanged box as
// changed, so its launch-only drift drains instead of relaunching.
func TestDecideRowFingerprintChangePolicy(t *testing.T) {
	withoutA7(t)
	base := censusRowOf(t, drainRow()).Info
	startedCfg := sessionCoreConfigForHashInfo(policyTP(), base)
	started, live := runtime.CoreFingerprint(startedCfg), runtime.LiveFingerprint(startedCfg)
	mutate := func(f func(*TemplateParams)) TemplateParams {
		tp := policyTP()
		tp.FPExtra = maps.Clone(tp.FPExtra)
		f(&tp)
		return tp
	}
	for _, pin := range []struct{ name, got, want string }{
		{"PrePolicyCoreFingerprint", runtime.PrePolicyCoreFingerprint(startedCfg), storedBeforePolicy},
		{"PrePolicyProvisionFingerprint", runtime.PrePolicyProvisionFingerprint(startedCfg), provisionBeforePolicy},
		{"the v6 LaunchFingerprint", v6(runtime.LaunchFingerprint(startedCfg)), launchBeforePolicy},
		{"the v6 LiveFingerprint", v6(live), liveBeforePolicy},
	} {
		if pin.got != pin.want {
			t.Fatalf("%s = %q, want the v6 binary's %q", pin.name, pin.got, pin.want)
		}
	}
	prePolicy := []string{"started_config_hash", storedBeforePolicy, "started_provision_hash", provisionBeforePolicy, "started_launch_hash", launchBeforePolicy, "started_live_hash", liveBeforePolicy}
	for _, tc := range []struct {
		name       string
		tp         TemplateParams
		stored     []string
		relaunch   bool // the provider can relaunch
		wantKind   string
		wantReason string
	}{
		{name: "pool.max raised", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["pool.max"] = "5" })},
		{name: "pool.min raised", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["pool.min"] = "1" })},
		{name: "skill content edited", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:plan"] = "h2" })},
		{name: "skill added", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:review"] = "h3" })},
		{name: "prompt edited", tp: mutate(func(tp *TemplateParams) { tp.Prompt = "new prompt" })},
		{name: "command changed drains", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "mcp projection changed drains", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["mcp:claude"] = "m2" }), wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "a pre-policy row with unchanged config rebaselines its core", tp: policyTP(), stored: prePolicy, wantKind: intentBaseline, wantReason: decideBaselinePrePolicy},
		{name: "a pre-policy row whose skill changed since drains one last time", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:plan"] = "h2" }), stored: prePolicy, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "a pre-policy row whose command changed since drains", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: prePolicy, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "a pre-policy launch-only drift relaunches", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: prePolicy, relaunch: true, wantKind: intentDriftRelaunch, wantReason: decideDriftRelaunch},
		{name: "a pre-policy launch drift whose pool size changed too drains", tp: mutate(func(tp *TemplateParams) { tp.Command, tp.FPExtra["pool.max"] = "run --other", "5" }), stored: prePolicy, relaunch: true, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := tc.stored
			if meta == nil {
				meta = []string{"started_config_hash", started, "started_live_hash", live}
			}
			it, _ := decideDrain(t, meta, func(e *selectionEntry) {
				e.InDesired, e.Desired = true, desireKeep
			}, func(w *World) {
				info := w.Census.Rows[rowKeyOf("gc-1")].Info
				var sp runtime.Provider = struct{ runtime.Provider }{runtime.NewFake()} // no Relaunch
				if tc.relaunch {
					sp = runtime.NewFake() // a RelaunchProvider
				}
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: sp}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tc.tp}}}
				w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
			})
			drift := slices.Contains([]string{decideDriftAttached, decideDriftPinned, decideDrainBegin + drainConfigDrift, decideDriftRepair, decideDriftRestart, decideDriftRelaunch}, it.Reason)
			if tc.wantKind == "" && drift || tc.wantKind != "" && (it.Kind != tc.wantKind || it.Reason != tc.wantReason) {
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, tc.wantKind, tc.wantReason)
			}
			if want := baselinePatch(sessionCoreConfigForHashInfo(tc.tp, base), baselinePrePolicy); tc.wantReason == decideBaselinePrePolicy && (!maps.Equal(it.Patch, want) || len(want) != 4) {
				t.Fatalf("patch %v, want the core baseline alone %v, the live pair left to the live check", it.Patch, want)
			}
		})
	}
	if started == storedBeforePolicy {
		t.Fatal("policyTP hashes as it did before the policy: the policy filters nothing")
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
	startedLive := cfg
	startedLive.SessionLive = []string{"echo old"}
	decide := func(meta []string) intent {
		it, _ := decideDrain(t, meta, func(e *selectionEntry) {
			e.InDesired, e.Desired = true, desireKeep
		}, func(w *World) {
			info := w.Census.Rows[rowKeyOf("gc-1")].Info
			w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: struct{ runtime.Provider }{runtime.NewFake()}}
			w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
			w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
		})
		return it
	}
	for _, tc := range []struct{ name, storedLive string }{
		{"another session_live's v6 live hash", v6(runtime.LiveFingerprint(startedLive))},
		{"an empty live hash", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := []string{"started_config_hash", runtime.PrePolicyCoreFingerprint(cfg), "started_live_hash", tc.storedLive, "live_hash", tc.storedLive}
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

// Kills A14 repairing (resetting the conversation of) an asleep named row
// started before the policy whose config is unchanged, where A8 leaves the
// row to the start it gets this pass (SESS-567/568): the start resumes it on
// its kept key.
func TestDecideRowAsleepNamedPrePolicyHashIsNoRepair(t *testing.T) {
	withoutA7(t)
	named := []string{"configured_named_session", "true", "configured_named_identity", "worker", "configured_named_mode", "always"}
	meta := append([]string{"state", "asleep", "started_config_hash", storedBeforePolicy, "session_key", "k-1"}, named...)
	it, _ := decideDrain(t, meta, func(e *selectionEntry) {
		e.InDesired, e.Desired, e.Liveness = true, desireWake, livenessGone
	}, func(w *World) {
		info := w.Census.Rows[rowKeyOf("gc-1")].Info
		w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: struct{ runtime.Provider }{runtime.NewFake()}}
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: policyTP()}}}
	})
	if it.Reason == decideDriftRepair || it.Patch["continuation_reset_pending"] == "true" { // a repair composed into the start resets the conversation
		t.Fatalf("decideRow = (%q, %q) %v, want no drift repair of a pre-policy hash", it.Kind, it.Reason, it.Patch)
	}
}

// Kills the legacy reconciler draining a session started before the policy
// whose config is unchanged (its alive drift block, SESS-551): the stored
// pre-policy hash is silently rebaselined to the current core hash instead.
func TestReconcilerRebaselinesPrePolicyHash(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	stored := runtime.PrePolicyCoreFingerprint(cfg)
	if stored == runtime.CoreFingerprint(cfg) {
		t.Fatal("test setup: the pre-policy hash equals the current one")
	}
	env.setSessionMetadata(&session, map[string]string{"started_config_hash": stored})

	env.reconcile([]beads.Bead{session})

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("pre-policy hash of unchanged config drained: %+v; stderr=%s", ds, env.stderr.String())
	}
	if got := env.sessionInfo(session.ID).StartedConfigHash; got != runtime.CoreFingerprint(cfg) {
		t.Fatalf("started_config_hash = %q, want rebaselined to %q", got, runtime.CoreFingerprint(cfg))
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
	tp.FPExtra = map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}
	tp.Hints.SessionLive = []string{"echo new"}
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	startedLive := cfg
	startedLive.SessionLive = []string{"echo old"}
	storedLive := v6(runtime.LiveFingerprint(startedLive))
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.PrePolicyCoreFingerprint(cfg),
		"started_live_hash":   storedLive,
		"live_hash":           storedLive,
	})

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
	tp.FPExtra = map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}
	env.desiredState[name] = tp
	session := env.createSessionBead(name, "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	oldCfg := cfg
	oldCfg.Command = "stale-" + cfg.Command
	stored, storedLive := runtime.PrePolicyCoreFingerprint(oldCfg), v6(runtime.LiveFingerprint(cfg))
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

// Kills the legacy reconciler draining, rather than relaunching, a launch-only
// drift on a session whose v6 provision hash names its unchanged box (B2.3):
// the box hash is matched as the v6 binary computed it, the relaunch lands in
// the warm box, and the baselines move to this binary's hashes.
func TestReconcilerRelaunchesPrePolicyLaunchOnlyDrift(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.FPExtra = map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}
	env.desiredState["worker"] = tp
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	oldCfg := cfg
	oldCfg.Command = "stale-" + cfg.Command
	if runtime.PrePolicyProvisionFingerprint(oldCfg) == v6(runtime.ProvisionFingerprint(oldCfg)) {
		t.Fatal("test setup: the policy leaves the provision hash unchanged")
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.PrePolicyCoreFingerprint(oldCfg),
		"started_provision_hash": runtime.PrePolicyProvisionFingerprint(oldCfg),
		"started_launch_hash":    v6(runtime.LaunchFingerprint(oldCfg)),
	})

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1 (a launch-only drift relaunches); stderr=%s", got, env.stderr.String())
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("launch-only drift drained: reason=%q", ds.reason)
	}
	got := env.sessionInfo(session.ID)
	if got.StartedConfigHash != runtime.CoreFingerprint(cfg) || got.StartedProvisionHash != runtime.ProvisionFingerprint(cfg) || got.StartedLaunchHash != runtime.LaunchFingerprint(cfg) {
		t.Fatalf("baselines (%q, %q, %q), want this binary's (%q, %q, %q)", got.StartedConfigHash, got.StartedProvisionHash, got.StartedLaunchHash,
			runtime.CoreFingerprint(cfg), runtime.ProvisionFingerprint(cfg), runtime.LaunchFingerprint(cfg))
	}
}

// Kills the legacy reconciler repairing an asleep named session started
// before the policy whose config is unchanged (its asleep named drift block,
// SESS-567/568): the stored pre-policy hash is rebaselined, its conversation
// kept, never reset as drifted.
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
	tp.FPExtra = map[string]string{"pool.min": "0", "pool.max": "2", "skills:plan": "h1"}
	env.desiredState[sessionName] = tp
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"session_key":                "conversation-1",
	})
	cfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	env.setSessionMetadata(&session, map[string]string{"started_config_hash": runtime.PrePolicyCoreFingerprint(cfg)})

	env.reconcile([]beads.Bead{session})

	got := env.sessionInfo(session.ID)
	if got.StartedConfigHash != runtime.CoreFingerprint(cfg) || got.SessionKey != "conversation-1" {
		t.Fatalf("started_config_hash = %q, session_key = %q; want rebaselined to %q keeping conversation-1; stderr=%s",
			got.StartedConfigHash, got.SessionKey, runtime.CoreFingerprint(cfg), env.stderr.String())
	}
}

// Kills the policy applying only to hand-built TemplateParams: through
// resolveTemplate, a pool-size or skill-content edit leaves the started
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
