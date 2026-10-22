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
