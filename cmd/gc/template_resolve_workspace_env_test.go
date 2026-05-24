package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateMergesWorkspaceEnv(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				"GC_TARGET_BRANCH": "boylec/develop",
				"FROM_WORKSPACE":   "ws",
			},
		},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "mayor"}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := tp.Env["GC_TARGET_BRANCH"]; got != "boylec/develop" {
		t.Errorf("GC_TARGET_BRANCH = %q, want %q", got, "boylec/develop")
	}
	if got := tp.Env["FROM_WORKSPACE"]; got != "ws" {
		t.Errorf("FROM_WORKSPACE = %q, want %q", got, "ws")
	}
}

func TestResolveTemplateAgentEnvWinsOverWorkspaceEnv(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")

	params := &agentBuildParams{
		cityName: "city",
		cityPath: cityPath,
		workspace: &config.Workspace{
			Provider: "test",
			Env: map[string]string{
				"GC_TARGET_BRANCH": "boylec/develop",
			},
		},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{
		Name: "mayor",
		Env:  map[string]string{"GC_TARGET_BRANCH": "boylec/special"},
	}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if got := tp.Env["GC_TARGET_BRANCH"]; got != "boylec/special" {
		t.Errorf("GC_TARGET_BRANCH = %q, want %q (agent env must override workspace env)", got, "boylec/special")
	}
}
