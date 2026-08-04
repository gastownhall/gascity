package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateRefusesModelOnlySchemaThatDropsPermissionPolicy(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	base := "builtin:claude"
	providers := map[string]config.ProviderSpec{
		"claude-managed": {
			Base: &base,
			OptionDefaults: map[string]string{
				"model": "opus",
			},
			OptionsSchema: []config.ProviderOption{{
				Key: "model",
				Choices: []config.OptionChoice{{
					Value:    "opus",
					FlagArgs: []string{"--model", "claude-opus-5"},
				}},
			}},
		},
	}
	city := &config.City{
		Workspace: config.Workspace{Provider: "claude-managed"},
		Providers: providers,
	}
	params := &agentBuildParams{
		city:       city,
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &city.Workspace,
		providers:  providers,
		lookPath:   func(string) (string, error) { return "/usr/bin/claude", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	agent := &config.Agent{Name: "worker", Provider: "claude-managed", Scope: "city"}

	_, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err == nil {
		t.Fatal("resolveTemplate() error = nil, want missing permission-policy refusal")
	}
	if !strings.Contains(err.Error(), "permission policy") {
		t.Fatalf("resolveTemplate() error = %q, want permission-policy diagnostic", err)
	}
}
