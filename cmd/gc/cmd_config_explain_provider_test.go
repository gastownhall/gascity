package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestRenderProviderExplainText_ShowsChainAndProvenance(t *testing.T) {
	b := "builtin:codex"
	city := map[string]config.ProviderSpec{
		"codex-max": {
			Base:          &b,
			Command:       "aimux",
			Args:          []string{"run", "codex"},
			ReadyDelayMs:  5000,
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	resolved, err := config.ResolveProviderChain("codex-max", city["codex-max"], city)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var out bytes.Buffer
	renderProviderExplainText(&out, resolved, "codex-max")
	got := out.String()

	if !strings.Contains(got, "Provider: codex-max") {
		t.Errorf("missing header: %s", got)
	}
	if !strings.Contains(got, "chain:") {
		t.Errorf("missing chain: %s", got)
	}
	if !strings.Contains(got, "builtin:codex") {
		t.Errorf("missing builtin:codex hop: %s", got)
	}
	if !strings.Contains(got, "# providers.codex-max") {
		t.Errorf("missing provenance annotation for leaf: %s", got)
	}
}

func TestRenderProviderExplainJSON_PayloadShape(t *testing.T) {
	b := "builtin:codex"
	city := map[string]config.ProviderSpec{
		"codex-max": {
			Base:         &b,
			Command:      "aimux",
			ReadyDelayMs: 5000,
			OptionDefaults: map[string]string{
				"effort": "xhigh",
			},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	resolved, err := config.ResolveProviderChain("codex-max", city["codex-max"], city)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if rc := renderProviderExplainJSON(resolved, "codex-max", &stdout, &stderr); rc != 0 {
		t.Fatalf("rc = %d, stderr=%s", rc, stderr.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json parse: %v — raw: %s", err, stdout.String())
	}

	if name, _ := payload["name"].(string); name != "codex-max" {
		t.Errorf("name = %v, want codex-max", payload["name"])
	}
	if payload["chain"] == nil {
		t.Errorf("chain missing: %v", payload)
	}
	prov, ok := payload["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance not a map: %T", payload["provenance"])
	}
	fieldLayer, ok := prov["field_layer"].(map[string]any)
	if !ok {
		t.Fatalf("field_layer not a map: %T", prov["field_layer"])
	}
	if got := fieldLayer["command"]; got != "providers.codex-max" {
		t.Errorf("field_layer.command = %v, want providers.codex-max", got)
	}
}
