package main

import (
	"encoding/json"
	"testing"
)

func TestBuildT3BridgeStartupEnvelope_UsesTemplateForGroupingAgent(t *testing.T) {
	tp := TemplateParams{
		TemplateName:             "t3code/polecat",
		InstanceName:             "t3code/polecat-1",
		SessionName:              "t3code--polecat-1",
		EffectiveSessionProvider: "t3bridge",
		WorkDir:                  "/data/projects/gc/.gc/worktrees/t3code/polecat/furiosa",
		Command:                  "codex",
		Env: map[string]string{
			"GC_CITY_PATH":    "/data/projects/gc",
			"GC_PROVIDER":     "codex",
			"GC_AGENT":        "t3code/polecat-1",
			"GC_TEMPLATE":     "t3code/polecat",
			"GC_SESSION_NAME": "t3code--polecat-1",
		},
	}

	raw := buildT3BridgeStartupEnvelope(tp, "prime")
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	gc, ok := envelope["gc"].(map[string]any)
	if !ok {
		t.Fatalf("gc section missing: %#v", envelope["gc"])
	}
	if got := gc["agent"]; got != "t3code/polecat" {
		t.Fatalf("gc.agent = %#v, want t3code/polecat", got)
	}
	if got := gc["template"]; got != "t3code/polecat" {
		t.Fatalf("gc.template = %#v, want t3code/polecat", got)
	}
	if got := gc["sessionName"]; got != "t3code--polecat-1" {
		t.Fatalf("gc.sessionName = %#v, want t3code--polecat-1", got)
	}
}

func TestBuildT3BridgeStartupEnvelope_NamedSessionPublishesTemplatePatchIdentity(t *testing.T) {
	tp := TemplateParams{
		TemplateName:             "crew",
		InstanceName:             "t3code/gastown.crew",
		Alias:                    "t3code/gastown.crew",
		SessionName:              "t3code--gastown__crew",
		EffectiveSessionProvider: "t3bridge",
		WorkDir:                  "/data/projects/gc/.gc/worktrees/t3code/crew/gastown.crew",
		Command:                  "codex",
		Env: map[string]string{
			"GC_CITY_PATH":    "/data/projects/gc",
			"GC_PROVIDER":     "codex",
			"GC_AGENT":        "t3code/gastown.crew",
			"GC_TEMPLATE":     "crew",
			"GC_SESSION_NAME": "t3code--gastown__crew",
		},
	}

	raw := buildT3BridgeStartupEnvelope(tp, "prime")
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	gc, ok := envelope["gc"].(map[string]any)
	if !ok {
		t.Fatalf("gc section missing: %#v", envelope["gc"])
	}
	if got := gc["template"]; got != "crew" {
		t.Fatalf("gc.template = %#v, want crew", got)
	}
}
