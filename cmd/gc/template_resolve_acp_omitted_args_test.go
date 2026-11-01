package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func acpOmittedArgsBuildParams(t *testing.T, providers map[string]config.ProviderSpec, stderr *bytes.Buffer) *agentBuildParams {
	t.Helper()
	return &agentBuildParams{
		fs:         fsys.OSFS{},
		cityName:   "acpville",
		cityPath:   t.TempDir(),
		workspace:  &config.Workspace{Name: "acpville"},
		providers:  providers,
		lookPath:   func(name string) (string, error) { return "/usr/bin/" + name, nil },
		beaconTime: testBeaconTime,
		beadNames:  make(map[string]string),
		stderr:     stderr,
	}
}

func opencodeWrapperProviders() map[string]config.ProviderSpec {
	base := "builtin:opencode"
	return map[string]config.ProviderSpec{
		"opencode-local": {Base: &base, Args: []string{"-m", "acme/model-x"}},
	}
}

// TestResolveTemplateOpenCodeWrapperArgsUseCLITransport is issue #1504: a
// provider extending builtin:opencode with its own args must launch on the
// CLI transport with those args, on both the template and the cache path,
// rather than as "opencode acp" with the args silently dropped.
func TestResolveTemplateOpenCodeWrapperArgsUseCLITransport(t *testing.T) {
	providers := opencodeWrapperProviders()
	var stderr bytes.Buffer
	params := acpOmittedArgsBuildParams(t, providers, &stderr)
	agent := &config.Agent{Name: "worker", Provider: "opencode-local"}

	tp, err := resolveTemplate(params, agent, "rig/worker", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.IsACP {
		t.Fatalf("tp.IsACP = true, want the CLI transport (command %q)", tp.Command)
	}
	if !strings.HasPrefix(tp.Command, "opencode -m acme/model-x") {
		t.Fatalf("command = %q, want it to start with %q", tp.Command, "opencode -m acme/model-x")
	}
	for _, field := range strings.Fields(tp.Command) {
		if field == "acp" {
			t.Fatalf("command = %q, want no acp token", tp.Command)
		}
	}

	cfg := &config.City{Providers: providers}
	if err := config.BuildResolvedProviderCache(cfg); err != nil {
		t.Fatalf("BuildResolvedProviderCache: %v", err)
	}
	if got := agentSessionCreateTransport(cfg, *agent); got == config.SessionTransportACP {
		t.Fatalf("agentSessionCreateTransport = %q, want the CLI transport", got)
	}
}

// TestResolveTemplateOpenCodeWrapperACPArgsKeepACPTransport covers wrappers
// whose ACP launch drops nothing the CLI launch carries: they stay on ACP on
// the template and on both cache-backed transport helpers, and launch with
// the effective acp_args. workspace.start_command replaces the wrapper's args
// on both launches, so the helpers must not read a cache entry that still
// records them as omitted.
func TestResolveTemplateOpenCodeWrapperACPArgsKeepACPTransport(t *testing.T) {
	base := "builtin:opencode"
	middle := "provider:middle"
	cases := []struct {
		name         string
		provider     string
		providers    map[string]config.ProviderSpec
		workspaceCmd string
		wantLaunch   string
	}{
		{
			name:       "no args",
			provider:   "opencode-local",
			providers:  map[string]config.ProviderSpec{"opencode-local": {Base: &base}},
			wantLaunch: "opencode acp",
		},
		{
			name:       "curated model args",
			provider:   "opencode-local",
			providers:  map[string]config.ProviderSpec{"opencode-local": {Base: &base, Args: []string{"-m", "opencode/big-pickle"}}},
			wantLaunch: "opencode acp --model opencode/big-pickle",
		},
		{
			name:         "workspace start_command over wrapper args",
			provider:     "opencode-local",
			providers:    opencodeWrapperProviders(),
			workspaceCmd: "my-agent --serve",
			wantLaunch:   "my-agent --serve acp",
		},
		{
			name:     "args and acp_args on one layer",
			provider: "opencode-local",
			providers: map[string]config.ProviderSpec{"opencode-local": {
				Base:    &base,
				Args:    []string{"-m", "acme/model-x"},
				ACPArgs: []string{"acp", "--port", "4096"},
			}},
			wantLaunch: "opencode acp --port 4096",
		},
		{
			name:     "leaf acp_args over middle args",
			provider: "leaf",
			providers: map[string]config.ProviderSpec{
				"middle": {Base: &base, Args: []string{"-m", "acme/model-x"}},
				"leaf":   {Base: &middle, ACPArgs: []string{"acp", "--port", "4096"}},
			},
			wantLaunch: "opencode acp --port 4096",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			params := acpOmittedArgsBuildParams(t, tc.providers, &stderr)
			params.workspace.StartCommand = tc.workspaceCmd
			agent := &config.Agent{Name: "worker", Provider: tc.provider}
			tp, err := resolveTemplate(params, agent, "rig/worker", nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if !tp.IsACP {
				t.Fatalf("tp.IsACP = false, want ACP (command %q)", tp.Command)
			}
			if tp.Command != tc.wantLaunch {
				t.Fatalf("command = %q, want %q", tp.Command, tc.wantLaunch)
			}
			cfg := &config.City{Workspace: *params.workspace, Providers: tc.providers}
			if err := config.BuildResolvedProviderCache(cfg); err != nil {
				t.Fatalf("BuildResolvedProviderCache: %v", err)
			}
			if got := agentSessionCreateTransport(cfg, *agent); got != config.SessionTransportACP {
				t.Fatalf("agentSessionCreateTransport = %q, want %q", got, config.SessionTransportACP)
			}
			if !providerSessionCreateUsesACP(cfg, tc.provider) {
				t.Fatalf("providerSessionCreateUsesACP(%q) = false, want true", tc.provider)
			}
		})
	}
}

// TestResolveTemplateWarnsWhenACPDropsArgs covers ACP launches that still
// drop declared args: an explicit session = "acp" on a wrapper whose args the
// ACP launch omits, and agent-level args under a provider with acp_args. Each
// prints one warning line naming the launch; a wrapper whose acp_args cover
// its args prints none, and neither do schema-managed flags, which the ACP
// launch carries as option defaults.
func TestResolveTemplateWarnsWhenACPDropsArgs(t *testing.T) {
	base := "builtin:opencode"
	cases := []struct {
		name       string
		providers  map[string]config.ProviderSpec
		agent      config.Agent
		wantWarn   bool
		wantLaunch string
	}{
		{
			name:      "explicit acp on wrapper with args",
			providers: opencodeWrapperProviders(),
			agent:     config.Agent{Name: "worker", Provider: "opencode-local", Session: "acp"},
			wantWarn:  true,
		},
		{
			name:      "agent args under builtin opencode",
			providers: config.BuiltinProviders(),
			agent:     config.Agent{Name: "worker", Provider: "opencode", Args: []string{"-m", "acme/model-x"}},
			wantWarn:  true,
		},
		{
			name:      "explicit acp on wrapper without args",
			providers: map[string]config.ProviderSpec{"opencode-local": {Base: &base}},
			agent:     config.Agent{Name: "worker", Provider: "opencode-local", Session: "acp"},
		},
		{
			name: "explicit acp on wrapper with args and acp_args",
			providers: map[string]config.ProviderSpec{"opencode-local": {
				Base:    &base,
				Args:    []string{"-m", "acme/model-x"},
				ACPArgs: []string{"acp", "--port", "4096"},
			}},
			agent: config.Agent{Name: "worker", Provider: "opencode-local", Session: "acp"},
		},
		{
			name:       "explicit acp on wrapper with curated model args",
			providers:  map[string]config.ProviderSpec{"opencode-local": {Base: &base, Args: []string{"-m", "opencode/big-pickle"}}},
			agent:      config.Agent{Name: "worker", Provider: "opencode-local", Session: "acp"},
			wantLaunch: "opencode acp --model opencode/big-pickle",
		},
		{
			name:       "agent curated model args under builtin opencode",
			providers:  config.BuiltinProviders(),
			agent:      config.Agent{Name: "worker", Provider: "opencode", Args: []string{"-m", "opencode/big-pickle"}},
			wantLaunch: "opencode acp --model opencode/big-pickle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			params := acpOmittedArgsBuildParams(t, tc.providers, &stderr)
			agent := tc.agent
			tp, err := resolveTemplate(params, &agent, "rig/worker", nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if !tp.IsACP {
				t.Fatalf("tp.IsACP = false, want ACP (command %q)", tp.Command)
			}
			var warnings []string
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.Contains(line, "acp_args") {
					warnings = append(warnings, line)
				}
			}
			if !tc.wantWarn {
				if len(warnings) != 0 {
					t.Fatalf("stderr warnings = %q, want none", warnings)
				}
				if tc.wantLaunch != "" && tp.Command != tc.wantLaunch {
					t.Fatalf("command = %q, want %q", tp.Command, tc.wantLaunch)
				}
				return
			}
			if tp.Command != "opencode acp" {
				t.Fatalf("command = %q, want %q", tp.Command, "opencode acp")
			}
			if len(warnings) != 1 {
				t.Fatalf("stderr warnings = %q, want exactly one line", warnings)
			}
			for _, want := range []string{
				`agent "rig/worker"`,
				`provider "` + agent.Provider + `"`,
				`ACP launch "opencode acp"`,
				"omits args -m acme/model-x;",
				"acp_args",
				`session = "tmux"`,
			} {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning %q missing %q", warnings[0], want)
				}
			}
		})
	}
}
