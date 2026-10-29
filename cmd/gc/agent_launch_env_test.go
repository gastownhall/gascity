package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// sessionIdentityEnvKeys are the launch env keys resolveTemplate adds on top
// of the shared generated layer.
var sessionIdentityEnvKeys = []string{"GC_SESSION_NAME", "GC_SESSION_ID", "GC_TEMPLATE", "GC_SESSION_ORIGIN", "GC_AGENT", "GC_ALIAS"}

// AC-8: resolveTemplate's Env and OperatorEnv are exactly the shared builders'
// composition plus the session identity keys, for rig-scoped, city-scoped,
// control-dispatcher and upstream-selecting agents.
func TestResolveTemplateEnvIsTheSharedLaunchEnvComposition(t *testing.T) {
	t.Setenv("GC_CONTROLLER_TOKEN", "caller-token")
	cityDir := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[workspace.env]
WS_ENV = "workspace"
GC_CONTROLLER_TOKEN = "workspace-token"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = "`+rigRoot+`"

[upstreams.serving.env]
UP_ENV = "upstream"

[[agent]]
name = "worker"
dir = "myrig"
provider = "stub"
max_active_sessions = 3

[agent.env]
AGENT_ENV = "agent"

[[agent]]
name = "helper"
provider = "stub"

[[agent]]
name = "`+config.ControlDispatcherAgentName+`"
provider = "stub"

[[agent]]
name = "served"
dir = "myrig"
provider = "stub"
upstream = "serving"

[providers.stub]
command = "/bin/echo"

[providers.stub.env]
PROVIDER_ENV = "provider"
`)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	for _, tc := range []struct{ name, template, agentName string }{
		{"rig-scoped", "myrig/worker", "myrig/worker-1"},
		{"city-scoped", "helper", "helper"},
		{"control-dispatcher", config.ControlDispatcherAgentName, config.ControlDispatcherAgentName},
		{"upstream-selecting", "myrig/served", "myrig/served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentCfg, ok := resolveAgentIdentity(cfg, tc.template, "")
			if !ok {
				t.Fatalf("no agent for template %q", tc.template)
			}
			bp := newReadOnlyAgentBuildParams(cfg.EffectiveCityName(), cityDir, cfg, nil, time.Now(), io.Discard)
			tp, err := resolveTemplate(bp, &agentCfg, tc.agentName, nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}

			in := agentLaunchEnvInput{
				CityPath:      cityDir,
				Rigs:          cfg.Rigs,
				Workspace:     &cfg.Workspace,
				Upstreams:     cfg.Upstreams,
				Agent:         &agentCfg,
				Resolved:      tp.ResolvedProvider,
				QualifiedName: tc.agentName,
				WorkDir:       tp.WorkDir,
				Rig:           tp.RigName,
				RigRoot:       tp.RigRoot,
			}
			scope, err := agentScopeLaunchEnv(in)
			if err != nil {
				t.Fatalf("agentScopeLaunchEnv: %v", err)
			}
			for _, key := range sessionIdentityEnvKeys {
				if _, ok := scope[key]; ok {
					t.Errorf("agentScopeLaunchEnv sets session identity key %s", key)
				}
			}
			generated := maps.Clone(scope)
			for _, key := range sessionIdentityEnvKeys {
				generated[key] = tp.Env[key]
			}
			env, operatorEnv, err := composeAgentLaunchEnv(passthroughEnv(), generated, in)
			if err != nil {
				t.Fatalf("composeAgentLaunchEnv: %v", err)
			}
			if !maps.Equal(env, tp.Env) {
				t.Errorf("composed env differs from resolveTemplate's:\n got %v\nwant %v", env, tp.Env)
			}
			if !maps.Equal(operatorEnv, tp.OperatorEnv) {
				t.Errorf("composed operator env = %v, want resolveTemplate's %v", operatorEnv, tp.OperatorEnv)
			}
		})
	}
}
