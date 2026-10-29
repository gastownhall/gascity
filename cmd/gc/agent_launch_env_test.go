package main

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
)

// AC-8: resolveTemplate's Env layers the controller's passthrough (its GC_*
// sweep included) < workspace < provider < agent < generated < upstream <
// pinned overlays, plus the session identity keys, and its OperatorEnv is the
// workspace, provider and agent layers alone, for rig-scoped, city-scoped,
// control-dispatcher and upstream-selecting agents. Every expected value is
// spelled out rather than derived from the shared launch-env builders, so a
// change to them that moves a layer or drops a key fails here.
func TestResolveTemplateEnvIsTheSharedLaunchEnvComposition(t *testing.T) {
	t.Setenv("LANG", "caller-lang")
	t.Setenv("GC_AC8_SWEPT", "swept")
	t.Setenv("GC_RIG", "stale-rig")
	t.Setenv("GC_CONTROLLER_TOKEN", "caller-token")
	cityDir := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[workspace.env]
LANG = "workspace"
L_WS_PROV = "workspace"
GC_CONTROLLER_TOKEN = "workspace-token"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = "`+rigRoot+`"

[upstreams.serving.env]
L_AGENT_UP = "upstream"
GT_ROOT = "upstream-root"
`+execenv.UsageMetricsDisableEnv+` = "upstream-metrics"

[[agent]]
name = "worker"
dir = "myrig"
provider = "stub"
max_active_sessions = 3

[agent.env]
L_PROV_AGENT = "agent"
GC_RIG = "agent-rig"

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

[agent.env]
L_AGENT_UP = "agent"

[providers.stub]
command = "/bin/echo"

[providers.stub.env]
L_WS_PROV = "provider"
L_PROV_AGENT = "provider"
`)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	layered := func(layers ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, layer := range layers {
			maps.Copy(out, layer)
		}
		return out
	}
	runtimeDir := citylayout.RuntimeDataDir(cityDir)
	cityTrace := citylayout.ControlDispatcherTraceDefaultPathForRuntimeDir(cityDir, runtimeDir)
	common := map[string]string{
		"LANG":                                "workspace",
		"L_WS_PROV":                           "provider",
		"GC_AC8_SWEPT":                        "swept",
		"GC_CONTROLLER_TOKEN":                 "",
		execenv.UsageMetricsDisableEnv:        execenv.UsageMetricsDisableValue,
		"GC_BIN":                              exe,
		"GC_CITY":                             cityDir,
		"GC_CITY_PATH":                        cityDir,
		"GC_CITY_RUNTIME_DIR":                 runtimeDir,
		"GC_SESSION_ORIGIN":                   "ephemeral",
		"GT_ROOT":                             cityDir,
		"L_PROV_AGENT":                        "provider",
		"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": cityTrace,
	}
	rigScope := map[string]string{
		"GC_RIG":              "myrig",
		"GC_RIG_ROOT":         rigRoot,
		"BEADS_DIR":           filepath.Join(rigRoot, ".beads"),
		"GC_BEADS_SCOPE_ROOT": rigRoot,
	}
	cityScope := map[string]string{
		"GC_RIG":              "",
		"GC_RIG_ROOT":         "",
		"BEADS_DIR":           "",
		"GC_BEADS_SCOPE_ROOT": cityDir,
	}
	operator := map[string]string{
		"LANG":                "workspace",
		"L_WS_PROV":           "provider",
		"GC_CONTROLLER_TOKEN": "",
		"L_PROV_AGENT":        "provider",
	}
	for _, tc := range []struct {
		name, template, agentName string
		want, wantOperator        map[string]string
		absent                    []string
	}{
		{
			name: "rig-scoped", template: "myrig/worker", agentName: "myrig/worker-1",
			want:         layered(common, rigScope, map[string]string{"L_PROV_AGENT": "agent"}),
			wantOperator: layered(operator, map[string]string{"L_PROV_AGENT": "agent", "GC_RIG": "agent-rig"}),
			absent:       []string{"L_AGENT_UP"},
		},
		{
			name: "city-scoped", template: "helper", agentName: "helper",
			want:         layered(common, cityScope),
			wantOperator: operator,
			absent:       []string{"L_AGENT_UP"},
		},
		{
			name: "control-dispatcher", template: config.ControlDispatcherAgentName, agentName: config.ControlDispatcherAgentName,
			want: layered(common, cityScope, map[string]string{
				"GC_CONTROL_DISPATCHER_TRACE_DEFAULT": citylayout.ControlDispatcherTraceDefaultPathForRuntimeDirAndName(cityDir, runtimeDir, config.ControlDispatcherAgentName),
			}),
			wantOperator: operator,
			absent:       []string{"L_AGENT_UP"},
		},
		{
			name: "upstream-selecting", template: "myrig/served", agentName: "myrig/served",
			want:         layered(common, rigScope, map[string]string{"L_AGENT_UP": "upstream", "GT_ROOT": "upstream-root"}),
			wantOperator: layered(operator, map[string]string{"L_AGENT_UP": "agent"}),
		},
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

			want := layered(tc.want, map[string]string{
				"GC_TEMPLATE":     tc.template,
				"GC_ALIAS":        tc.agentName,
				"GC_SESSION_NAME": tp.SessionName,
				"GC_AGENT":        tp.SessionName,
				"GC_DIR":          tp.WorkDir,
			})
			for key, wantValue := range want {
				if got, ok := tp.Env[key]; !ok || got != wantValue {
					t.Errorf("Env[%s] = %q (present %v), want %q", key, got, ok, wantValue)
				}
			}
			for _, key := range tc.absent {
				if got, ok := tp.Env[key]; ok {
					t.Errorf("Env[%s] = %q, want absent", key, got)
				}
			}
			if prefix := filepath.Dir(exe) + string(os.PathListSeparator); !strings.HasPrefix(tp.Env["PATH"], prefix) {
				t.Errorf("Env[PATH] = %q, want it to begin with GC_BIN's directory %q", tp.Env["PATH"], prefix)
			}
			if !maps.Equal(tp.OperatorEnv, tc.wantOperator) {
				t.Errorf("OperatorEnv = %v, want %v", tp.OperatorEnv, tc.wantOperator)
			}
		})
	}
}
