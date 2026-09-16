package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateAddsFilesystemSearchGuidanceToStartupPrompt(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	params := &agentBuildParams{
		cityName:        "city",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Provider: "claude"},
		providers:       map[string]config.ProviderSpec{"claude": {Command: "echo", PromptMode: "none"}},
		lookPath:        func(string) (string, error) { return "/bin/echo", nil },
		fs:              fsys.OSFS{},
		rigs:            []config.Rig{},
		beaconTime:      time.Unix(0, 0),
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		sessionProvider: "tmux",
	}
	agent := &config.Agent{
		Name:     "custom",
		Scope:    "city",
		Provider: "claude",
		WorkDir:  ".gc/agents/custom",
	}

	resolved, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if count := strings.Count(resolved.Prompt, formulaFilesystemSearchGuidance); count != 1 {
		t.Fatalf("filesystem search guidance count = %d, want 1:\n%s", count, resolved.Prompt)
	}
}
