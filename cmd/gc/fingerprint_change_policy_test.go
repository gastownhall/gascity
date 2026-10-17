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
)

// The per-field change policy (ARCH-SIMPLIFICATION-REVIEW §3.5, Phase 0):
// pool sizing is Ignore and prompt and skill content NextStart, so editing
// them never drains a running session; a field that changes the running
// process (here the command, and the MCP projection) still does.

// policyTP is a worker template carrying every FingerprintExtra family.
func policyTP() TemplateParams {
	return TemplateParams{TemplateName: "worker", Command: "run", Prompt: "old prompt", FPExtra: map[string]string{
		"pool.min": "0", "pool.max": "2", "skills:plan": "h1", "mcp:claude": "m1", "depends_on": "db",
	}}
}

// storedBeforePolicy is policyTP's core hash for drainRow as the binary
// before the change policy computed it (pool sizing and skill hashes
// inside), as production rows carry it on deploy.
const storedBeforePolicy = "v6:c1e79facd911536834b03eef3c8fb2ac62231e5b704540477d3de7f51b34acd5"

// Kills a pool-size, skill or prompt edit that drains a running session, a
// command or MCP edit that no longer does, a deploy that reads a
// pre-policy row's unchanged config as drift (a drain wave) instead of A8's
// silent rebaseline, and one whose rebaseline swallows a real drift.
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
	if got := runtime.PrePolicyCoreFingerprint(startedCfg); got != storedBeforePolicy {
		t.Fatalf("PrePolicyCoreFingerprint = %q, want the previous binary's %q", got, storedBeforePolicy)
	}
	prePolicy := []string{"started_config_hash", storedBeforePolicy, "started_live_hash", live}
	for _, tc := range []struct {
		name       string
		tp         TemplateParams
		stored     []string
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
		{name: "a pre-policy row with unchanged config rebaselines", tp: policyTP(), stored: prePolicy, wantKind: intentBaseline, wantReason: decideBaselineVersion},
		{name: "a pre-policy row whose skill changed since drains one last time", tp: mutate(func(tp *TemplateParams) { tp.FPExtra["skills:plan"] = "h2" }), stored: prePolicy, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
		{name: "a pre-policy row whose command changed since drains", tp: mutate(func(tp *TemplateParams) { tp.Command = "run --other" }), stored: prePolicy, wantKind: intentDrainBegin, wantReason: decideDrainBegin + drainConfigDrift},
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
				w.Env = &reconcileEnv{Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}, SP: struct{ runtime.Provider }{runtime.NewFake()}}
				w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tc.tp}}}
				w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive}}
			})
			drift := slices.Contains([]string{decideDriftAttached, decideDriftPinned, decideDrainBegin + drainConfigDrift, decideDriftRepair, decideDriftRestart, decideDriftRelaunch}, it.Reason)
			if tc.wantKind == "" && drift || tc.wantKind != "" && (it.Kind != tc.wantKind || it.Reason != tc.wantReason) {
				t.Fatalf("decideRow = (%q, %q) %v, want (%q, %q)", it.Kind, it.Reason, it.Patch, tc.wantKind, tc.wantReason)
			}
		})
	}
	if started == storedBeforePolicy {
		t.Fatal("policyTP hashes as it did before the policy: the policy filters nothing")
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
