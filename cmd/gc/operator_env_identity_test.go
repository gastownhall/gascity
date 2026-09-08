package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

var operatorEnvStringMapType = reflect.TypeOf(map[string]string{})

// operatorAuthoredEnvIdentity reads the dedicated runtime.Config map without
// choosing its production name. Option A' settles the field's behavior while
// explicitly leaving its exact name to the implementation.
func operatorAuthoredEnvIdentity(t *testing.T, cfg runtime.Config) map[string]string {
	t.Helper()
	idx := operatorAuthoredEnvIdentityFieldIndex(t, cfg)
	value := reflect.ValueOf(cfg).Field(idx)
	if value.IsNil() {
		return nil
	}
	return value.Interface().(map[string]string)
}

func operatorAuthoredEnvIdentityFieldIndex(t *testing.T, cfg runtime.Config) int {
	t.Helper()
	typ := reflect.TypeOf(cfg)
	var candidates []int
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != operatorEnvStringMapType || field.Name == "Env" || field.Name == "FingerprintExtra" {
			continue
		}
		candidates = append(candidates, i)
		names = append(names, field.Name)
	}
	if len(candidates) != 1 {
		t.Fatalf("runtime.Config has config-authored environment identity candidates %v, want exactly one dedicated map[string]string field besides Env and FingerprintExtra", names)
	}
	return candidates[0]
}

func operatorAuthoredEnvOnlyCoreFingerprint(t *testing.T, cfg runtime.Config) string {
	t.Helper()
	env := operatorAuthoredEnvIdentity(t, cfg)
	isolated := runtime.Config{}
	idx := operatorAuthoredEnvIdentityFieldIndex(t, isolated)
	reflect.ValueOf(&isolated).Elem().Field(idx).Set(reflect.ValueOf(env))
	return runtime.CoreFingerprint(isolated)
}

func resolveOperatorEnvTestAgent(t *testing.T, cityPath string, cfg *config.City, cfgAgent *config.Agent) TemplateParams {
	t.Helper()
	params := &agentBuildParams{
		city:       cfg,
		cityName:   cfg.EffectiveCityName(),
		cityPath:   cityPath,
		workspace:  &cfg.Workspace,
		agents:     cfg.Agents,
		providers:  cfg.Providers,
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       cfg.Rigs,
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	tp, err := resolveTemplate(params, cfgAgent, cfgAgent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", cfgAgent.QualifiedName(), err)
	}
	return tp
}

// TestResolveTemplateConfigAuthoredEnvIdentityAcrossRigProviderPaths covers
// every authored surface and both rig provider-selection paths. The final
// identity must use the same effective precedence as the process environment:
// workspace < provider < agent/rig patch.
func TestResolveTemplateConfigAuthoredEnvIdentityAcrossRigProviderPaths(t *testing.T) {
	cityPath := t.TempDir()
	packPath := filepath.Join(cityPath, "workers")
	defaultRigPath := filepath.Join(cityPath, "default-rig")
	patchedRigPath := filepath.Join(cityPath, "patched-rig")
	for _, dir := range []string{packPath, defaultRigPath, patchedRigPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	t.Setenv("OPERATOR_ENV_EXPANSION", "expanded-workspace")

	cityTOML := fmt.Sprintf(`[workspace]
name = "operator-env-city"
provider = "workspace-default"

[workspace.env]
WORKSPACE_ONLY = "$OPERATOR_ENV_EXPANSION"
CONFLICT = "workspace"

[beads]
provider = "file"

[providers.workspace-default]
command = "echo"
prompt_mode = "none"

[providers.workspace-default.env]
DEFAULT_PROVIDER_ONLY = "workspace-default-provider"
CONFLICT = "workspace-default-provider"

[providers.rig-selected]
command = "echo"
prompt_mode = "none"

[providers.rig-selected.env]
RIG_PROVIDER_ONLY = "rig-selected-provider"
CONFLICT = "rig-selected-provider"

[[rigs]]
name = "default"
path = %q

[rigs.imports.workers]
source = "./workers"

[[rigs]]
name = "patched"
path = %q

[rigs.imports.workers]
source = "./workers"

[[rigs.patches]]
agent = "worker"
provider = "rig-selected"

[rigs.patches.env]
RIG_PATCH_ONLY = "rig-patch"
CONFLICT = "rig-patch"
`, defaultRigPath, patchedRigPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	packTOML := `[pack]
name = "workers"
schema = 2

[[agent]]
name = "worker"
scope = "rig"

[agent.env]
AGENT_ONLY = "agent"
CONFLICT = "agent"
`
	if err := os.WriteFile(filepath.Join(packPath, "pack.toml"), []byte(packTOML), 0o644); err != nil {
		t.Fatalf("write worker pack: %v", err)
	}

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	seen := make(map[string]bool)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Name != "worker" {
			continue
		}
		var want map[string]string
		switch agent.Dir {
		case "default":
			want = map[string]string{
				"WORKSPACE_ONLY":        "expanded-workspace",
				"DEFAULT_PROVIDER_ONLY": "workspace-default-provider",
				"AGENT_ONLY":            "agent",
				"CONFLICT":              "agent",
			}
		case "patched":
			want = map[string]string{
				"WORKSPACE_ONLY":    "expanded-workspace",
				"RIG_PROVIDER_ONLY": "rig-selected-provider",
				"AGENT_ONLY":        "agent",
				"RIG_PATCH_ONLY":    "rig-patch",
				"CONFLICT":          "rig-patch",
			}
		default:
			continue
		}
		seen[agent.Dir] = true
		t.Run(agent.Dir, func(t *testing.T) {
			tp := resolveOperatorEnvTestAgent(t, cityPath, cfg, agent)
			runtimeCfg := templateParamsToConfig(tp)
			for key, value := range want {
				if got := runtimeCfg.Env[key]; got != value {
					t.Errorf("runtime Env[%s] = %q, want %q", key, got, value)
				}
			}
			identity := operatorAuthoredEnvIdentity(t, runtimeCfg)
			if !maps.Equal(identity, want) {
				t.Errorf("config-authored env identity = %#v, want exactly %#v", identity, want)
			}
		})
	}
	for _, rig := range []string{"default", "patched"} {
		if !seen[rig] {
			t.Errorf("rig-scoped worker for %q not resolved; agents=%#v", rig, cfg.Agents)
		}
	}
}

// TestResolveTemplateConfigAuthoredEnvIdentityExcludesRuntimeOnlyLayers locks
// the security and churn boundary: process passthrough, resolved upstream
// credentials, and generated per-session values may reach runtime Env, but
// they must not enter the dedicated authored identity map or its hash.
func TestResolveTemplateConfigAuthoredEnvIdentityExcludesRuntimeOnlyLayers(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	t.Setenv("OPENAI_API_KEY", "ambient-one")

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "operator-env-city",
			Provider: "test",
			Env:      map[string]string{"WORKSPACE_AUTHORED": "stable-workspace"},
		},
		Providers: map[string]config.ProviderSpec{
			"test": {
				Command:     "echo",
				PromptMode:  "none",
				Env:         map[string]string{"PROVIDER_AUTHORED": "stable-provider"},
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "RESOLVED_CREDENTIAL"},
			},
		},
		Upstreams: map[string]config.UpstreamSpec{
			"gateway": {APIKey: "credential-one"},
		},
		Agents: []config.Agent{{
			Name:     "worker",
			Provider: "test",
			Upstream: "gateway",
			Env:      map[string]string{"AGENT_AUTHORED": "stable-agent"},
		}},
	}

	firstTP := resolveOperatorEnvTestAgent(t, cityPath, cfg, &cfg.Agents[0])
	firstCfg := templateParamsToConfig(firstTP)
	if got := firstCfg.Env["OPENAI_API_KEY"]; got != "ambient-one" {
		t.Fatalf("test setup: passthrough OPENAI_API_KEY = %q, want ambient-one", got)
	}
	if got := firstCfg.Env["RESOLVED_CREDENTIAL"]; got != "credential-one" {
		t.Fatalf("test setup: resolved credential = %q, want credential-one", got)
	}

	t.Setenv("OPENAI_API_KEY", "ambient-two")
	cfg.Upstreams["gateway"] = config.UpstreamSpec{APIKey: "credential-two"}
	secondTP := resolveOperatorEnvTestAgent(t, cityPath, cfg, &cfg.Agents[0])
	secondCfg := templateParamsToConfig(secondTP)
	if got := secondCfg.Env["OPENAI_API_KEY"]; got != "ambient-two" {
		t.Fatalf("test setup: passthrough OPENAI_API_KEY = %q, want ambient-two", got)
	}
	if got := secondCfg.Env["RESOLVED_CREDENTIAL"]; got != "credential-two" {
		t.Fatalf("test setup: resolved credential = %q, want credential-two", got)
	}

	generatedTP := secondTP
	generatedTP.Env = maps.Clone(secondTP.Env)
	generatedTP.Env["GC_SESSION_ID"] = "different-generated-session-id"
	generatedCfg := templateParamsToConfig(generatedTP)

	wantIdentity := map[string]string{
		"WORKSPACE_AUTHORED": "stable-workspace",
		"PROVIDER_AUTHORED":  "stable-provider",
		"AGENT_AUTHORED":     "stable-agent",
	}
	for name, runtimeCfg := range map[string]runtime.Config{
		"first":                          firstCfg,
		"passthrough+credential changed": secondCfg,
		"generated env changed":          generatedCfg,
	} {
		identity := operatorAuthoredEnvIdentity(t, runtimeCfg)
		if !maps.Equal(identity, wantIdentity) {
			t.Errorf("%s config-authored env identity = %#v, want exactly %#v", name, identity, wantIdentity)
		}
		for _, forbidden := range []string{"OPENAI_API_KEY", "RESOLVED_CREDENTIAL", "GC_SESSION_ID"} {
			if _, ok := identity[forbidden]; ok {
				t.Errorf("%s config-authored env identity contains runtime-only %s", name, forbidden)
			}
		}
	}
	firstHash := operatorAuthoredEnvOnlyCoreFingerprint(t, firstCfg)
	if got := operatorAuthoredEnvOnlyCoreFingerprint(t, secondCfg); got != firstHash {
		t.Errorf("passthrough/credential-only change moved config-authored identity hash: got %q want %q", got, firstHash)
	}
	if got := operatorAuthoredEnvOnlyCoreFingerprint(t, generatedCfg); got != firstHash {
		t.Errorf("generated-agent-env-only change moved config-authored identity hash: got %q want %q", got, firstHash)
	}

	// The resolved identity is a value snapshot, not an alias back into any
	// caller-owned config map.
	firstIdentity := operatorAuthoredEnvIdentity(t, firstCfg)
	cfg.Workspace.Env["WORKSPACE_AUTHORED"] = "mutated-after-resolve"
	provider := cfg.Providers["test"]
	provider.Env["PROVIDER_AUTHORED"] = "mutated-after-resolve"
	cfg.Agents[0].Env["AGENT_AUTHORED"] = "mutated-after-resolve"
	if !maps.Equal(firstIdentity, wantIdentity) {
		t.Errorf("caller-owned config mutation changed resolved authored identity: got %#v want %#v", firstIdentity, wantIdentity)
	}
}

// TestReconcileSessionBeads_OperatorAuthoredEnvDriftRelaunchesNamedSession is
// the alive named-session contract for Option A'. An authored-env-only change
// must take exactly one warm-box Relaunch, preserve the durable conversation,
// and avoid every full-reset signal.
func TestReconcileSessionBeads_OperatorAuthoredEnvDriftRelaunchesNamedSession(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	one := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "operator-env-city", Provider: "test"},
		Providers: map[string]config.ProviderSpec{
			"test": {
				Command:       "claude",
				PromptMode:    "none",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		},
		Agents: []config.Agent{{
			Name:              "worker",
			Env:               map[string]string{"OPERATOR_FLAG": "new"},
			MaxActiveSessions: &one,
		}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	oldAgent := cfg.Agents[0]
	oldAgent.Env = map[string]string{"OPERATOR_FLAG": "old"}
	oldTP := resolveOperatorEnvTestAgent(t, cityPath, cfg, &oldAgent)
	currentTP := resolveOperatorEnvTestAgent(t, cityPath, cfg, &cfg.Agents[0])

	sessionName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "worker")
	for _, tp := range []*TemplateParams{&oldTP, &currentTP} {
		tp.SessionName = sessionName
		tp.ConfiguredNamedIdentity = "worker"
		tp.ConfiguredNamedMode = "always"
	}

	env := newReconcilerTestEnv()
	env.cfg = cfg
	env.desiredState[sessionName] = currentTP
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: currentTP.Command}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "warm-conversation",
	})
	info := env.sessionInfo(session.ID)
	oldRuntimeCfg := sessionCoreConfigForHashInfo(oldTP, info)
	currentRuntimeCfg := sessionCoreConfigForHashInfo(currentTP, info)
	if oldRuntimeCfg.Env["OPERATOR_FLAG"] != "old" || currentRuntimeCfg.Env["OPERATOR_FLAG"] != "new" {
		t.Fatalf("test setup: operator env did not resolve old->new: old=%q new=%q", oldRuntimeCfg.Env["OPERATOR_FLAG"], currentRuntimeCfg.Env["OPERATOR_FLAG"])
	}
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(oldRuntimeCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldRuntimeCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldRuntimeCfg),
		"started_live_hash":      runtime.LiveFingerprint(oldRuntimeCfg),
	})

	startsBefore := env.sp.CountCalls("Start", sessionName)
	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", sessionName); got != 1 {
		t.Fatalf("Relaunch calls = %d, want exactly 1 for operator-authored-env-only drift; stderr=%s", got, env.stderr.String())
	}
	if got := env.sp.CountCalls("Stop", sessionName); got != 0 {
		t.Errorf("Stop calls = %d, want 0 (operator env is Launch-tier, not a full reset)", got)
	}
	if got := env.sp.CountCalls("Start", sessionName); got != startsBefore {
		t.Errorf("Start calls = %d, want %d (warm-box relaunch must not reprovision)", got, startsBefore)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("unexpected drain for operator-authored env drift: reason=%q", ds.reason)
	}
	relaunchCfg := env.sp.LastRelaunchConfig(sessionName)
	if relaunchCfg == nil {
		t.Fatal("no Relaunch config recorded")
	} else if got := relaunchCfg.Env["OPERATOR_FLAG"]; got != "new" {
		t.Errorf("Relaunch Env[OPERATOR_FLAG] = %q, want new", got)
	}
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session after relaunch: %v", err)
	}
	if got := after.Metadata["session_key"]; got != "warm-conversation" {
		t.Errorf("session_key after relaunch = %q, want preserved warm-conversation", got)
	}
	if got := after.Metadata["generation"]; got != session.Metadata["generation"] {
		t.Errorf("generation after relaunch = %q, want unchanged %q", got, session.Metadata["generation"])
	}

	env.reconcile([]beads.Bead{after})
	if got := env.sp.CountCalls("Relaunch", sessionName); got != 1 {
		t.Errorf("Relaunch calls after unchanged second tick = %d, want still 1", got)
	}
	afterSecondTick, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session after second tick: %v", err)
	}
	if got := afterSecondTick.Metadata["session_key"]; got != "warm-conversation" {
		t.Errorf("session_key after second tick = %q, want preserved warm-conversation", got)
	}
}
