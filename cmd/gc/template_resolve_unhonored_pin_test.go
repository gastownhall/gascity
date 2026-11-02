package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveTemplateWarnsOnUnhonoredPin covers a CLOSED option: an
// unrecognized permission_mode cannot be rendered, so launch must warn rather
// than drop the flag and leave the agent on the CLI default (ga-fyh).
func TestResolveTemplateWarnsOnUnhonoredPin(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "boomfartville",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "boomfartville"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/grok", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}
	agent := &config.Agent{
		Name:     "gorkcats",
		Provider: "grok",
		OptionDefaults: map[string]string{
			"model":           "grok-4.6",
			"effort":          "high",
			"permission_mode": "not-a-permission-mode",
		},
	}

	tp, err := resolveTemplate(params, agent, "gascity/gasburger.gorkcats", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if strings.Contains(tp.Command, "not-a-permission-mode") {
		t.Fatalf("command honored unknown permission_mode: %q", tp.Command)
	}
	got := stderr.String()
	for _, want := range []string{
		"WARNING:",
		`agent "gascity/gasburger.gorkcats"`,
		`permission_mode="not-a-permission-mode"`,
		"flag omitted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr %q missing %q", got, want)
		}
	}
}

// TestResolveTemplateHonorsUnheardOfModelPin is the systemic fix: the catalog
// will always trail the providers' model releases, so an id it has never seen
// must still reach the CLI. Dropping it is what left refinery and gorkcats
// running unpinned (ga-fyh).
func TestResolveTemplateHonorsUnheardOfModelPin(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "boomfartville",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "boomfartville"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/grok", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}
	agent := &config.Agent{
		Name:     "gorkcats",
		Provider: "grok",
		OptionDefaults: map[string]string{
			"model":           "grok-4.9-unreleased",
			"permission_mode": "unrestricted",
		},
	}

	tp, err := resolveTemplate(params, agent, "gascity/gasburger.gorkcats", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !strings.Contains(tp.Command, "--model grok-4.9-unreleased") {
		t.Fatalf("command dropped unheard-of model pin: %q", tp.Command)
	}
	if strings.Contains(stderr.String(), "WARNING:") {
		t.Fatalf("honored pin warned: %q", stderr.String())
	}
}

func TestResolveTemplateHonorsGrok46Pin(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "boomfartville",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "boomfartville"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/grok", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}
	agent := &config.Agent{
		Name:     "gorkcats",
		Provider: "grok",
		OptionDefaults: map[string]string{
			"model":           "grok-4.6",
			"effort":          "high",
			"permission_mode": "unrestricted",
		},
	}

	tp, err := resolveTemplate(params, agent, "gascity/gasburger.gorkcats", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !strings.Contains(tp.Command, "--model grok-4.6") {
		t.Fatalf("command missing grok-4.6 model flag: %q", tp.Command)
	}
	if !strings.Contains(tp.Command, "--effort high") {
		t.Fatalf("command missing effort flag: %q", tp.Command)
	}
	if strings.Contains(stderr.String(), "WARNING:") {
		t.Fatalf("valid grok-4.6 pin warned: %q", stderr.String())
	}
}
