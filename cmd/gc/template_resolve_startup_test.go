package main

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
)

func TestTemplateParamsToConfig_IncludesStartupEnvelope(t *testing.T) {
	tp := TemplateParams{
		Command:         "codex --dangerously-bypass-approvals-and-sandbox",
		SessionName:     "gascity-startup-envelope-test",
		TemplateName:    "gascity/codex",
		InstanceName:    "gascity/codex",
		RigName:         "gascity",
		RigRoot:         "/data/projects/gascity",
		WorkDir:         "/data/projects/gascity",
		SessionOverride: "exec:/data/projects/gc/scripts/gc-session-t3",
		Prompt:          "startup prompt body",
		Env: map[string]string{
			"GC_AGENT":        "gascity/codex",
			"GC_PROVIDER":     "codex",
			"GC_TEMPLATE":     "gascity/codex",
			"GC_CITY_PATH":    "/data/projects/gc",
			"GC_RIG":          "gascity",
			"GC_SESSION_NAME": "gascity-startup-envelope-test",
		},
		Hints: agent.StartupHints{Nudge: "do the assigned work now"},
	}

	cfg := templateParamsToConfig(tp)
	if len(cfg.StartupEnvelope) == 0 {
		t.Fatal("StartupEnvelope is empty")
	}

	var got map[string]any
	if err := json.Unmarshal(cfg.StartupEnvelope, &got); err != nil {
		t.Fatalf("unmarshal StartupEnvelope: %v", err)
	}

	gc, _ := got["gc"].(map[string]any)
	runtime, _ := got["runtime"].(map[string]any)
	startup, _ := got["startup"].(map[string]any)
	resume, _ := got["resume"].(map[string]any)

	if gc["agent"] != "gascity/codex" {
		t.Fatalf("gc.agent = %#v, want %q", gc["agent"], "gascity/codex")
	}
	if gc["template"] != "gascity/codex" {
		t.Fatalf("gc.template = %#v, want %q", gc["template"], "gascity/codex")
	}
	if runtime["provider"] != "codex" {
		t.Fatalf("runtime.provider = %#v, want %q", runtime["provider"], "codex")
	}
	if runtime["model"] != "gpt-5-codex" {
		t.Fatalf("runtime.model = %#v, want %q", runtime["model"], "gpt-5-codex")
	}
	if runtime["workDir"] != "/data/projects/gascity" {
		t.Fatalf("runtime.workDir = %#v, want %q", runtime["workDir"], "/data/projects/gascity")
	}
	if startup["initialNudge"] != "do the assigned work now" {
		t.Fatalf("startup.initialNudge = %#v, want %q", startup["initialNudge"], "do the assigned work now")
	}
	if startup["startupPrompt"] != "startup prompt body" {
		t.Fatalf("startup.startupPrompt = %#v, want %q", startup["startupPrompt"], "startup prompt body")
	}
	if resume["requiredThreadProvider"] != "codex" {
		t.Fatalf("resume.requiredThreadProvider = %#v, want %q", resume["requiredThreadProvider"], "codex")
	}
}
