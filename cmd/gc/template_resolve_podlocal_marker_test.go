package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionk8s "github.com/gastownhall/gascity/internal/runtime/k8s"
)

// TestResolveTemplateClearsK8sPodSessionMarker pins the template resolver's
// side of the pod-marker contract: the marker is provider-owned, written only
// by the k8s pod builder, so no config-authored layer — workspace env, the
// provider's env, agent env — may smuggle a value into a session template.
// The resolver pins the key empty after the last writer (the same
// final-pinning the controller-only keys get), and an ambient process env
// copy is overridden the same way.
func TestResolveTemplateClearsK8sPodSessionMarker(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	// An ambient copy reaches the resolver through passthrough; it must not
	// survive either.
	t.Setenv(sessionk8s.PodSessionEnv, "ambient-copy")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		city:     &config.City{},
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				sessionk8s.PodSessionEnv: "workspace-forged",
			},
		},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none", Env: map[string]string{sessionk8s.PodSessionEnv: "provider-forged"}}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{
		Name: "reviewer",
		Env: map[string]string{
			sessionk8s.PodSessionEnv: "agent-forged",
		},
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	val, ok := tp.Env[sessionk8s.PodSessionEnv]
	if !ok {
		t.Fatalf("Env omits %s; want present and empty so no inherited copy survives", sessionk8s.PodSessionEnv)
	} else if val != "" {
		t.Fatalf("Env[%s] = %q, want empty: config-authored values must not overwrite the pin", sessionk8s.PodSessionEnv, val)
	}
}

// TestApplyTemplateProgramStatusClearsK8sPodSessionMarker pins the second
// late env writer: applyTemplateProgramStatus copies a provider's program
// status env over the template env after resolveTemplate's pins, so it must
// re-pin the pod marker the way it re-pins the usage-metrics opt-out and the
// controller-only keys — or a config-authored program-status key could carry
// a forged marker onto the launch.
func TestApplyTemplateProgramStatusClearsK8sPodSessionMarker(t *testing.T) {
	tp := TemplateParams{ProgramStatusEnv: map[string]string{sessionk8s.PodSessionEnv: "program-status-copy"}}
	cfg := &runtime.Config{Env: map[string]string{sessionk8s.PodSessionEnv: "template-leftover"}}
	applyTemplateProgramStatus(cfg, tp)
	if val, ok := cfg.Env[sessionk8s.PodSessionEnv]; !ok || val != "" {
		t.Fatalf("cfg.Env[%s] = %q (present %t), want present and empty after applyTemplateProgramStatus", sessionk8s.PodSessionEnv, val, ok)
	}
}
