package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestExpandAgentCommandTemplate_SubstitutesRig(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "myrig", Path: cityPath + "/myrig"}}
	agent := &config.Agent{Name: "ant", Dir: "myrig"}

	got := expandAgentCommandTemplate(cityPath, "test-city", agent, rigs, "work_query", "cmd {{.Rig}}/ant", nil)
	want := "cmd myrig/ant"
	if got != want {
		t.Fatalf("expandAgentCommandTemplate = %q, want %q", got, want)
	}
}

func TestExpandAgentCommandTemplate_SubstitutesAgentBase(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}

	got := expandAgentCommandTemplate(cityPath, "test-city", agent, nil, "work_query", "probe {{.AgentBase}}", nil)
	want := "probe worker"
	if got != want {
		t.Fatalf("expandAgentCommandTemplate = %q, want %q", got, want)
	}
}

func TestExpandAgentCommandTemplate_LiteralOnlyIsByteIdentical(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}

	cases := []string{
		"bd ready --metadata-field gc.routed_to=worker",
		"echo 1",
		"",
	}
	for _, cmd := range cases {
		got := expandAgentCommandTemplate(cityPath, "test-city", agent, nil, "work_query", cmd, nil)
		if got != cmd {
			t.Errorf("literal command mutated: got %q, want %q", got, cmd)
		}
	}
}

func TestExpandAgentCommandTemplate_ParseErrorLogsAndReturnsRaw(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	cmd := "cmd {{.Rig" // malformed

	var buf bytes.Buffer
	got := expandAgentCommandTemplate(cityPath, "test-city", agent, nil, "work_query", cmd, &buf)

	if got != cmd {
		t.Errorf("parse error: got %q, want raw %q", got, cmd)
	}
	if !strings.Contains(buf.String(), "expandAgentCommandTemplate") {
		t.Errorf("expected stderr log on parse error, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "worker") {
		t.Errorf("expected agent name in stderr log, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "work_query") {
		t.Errorf("expected field name in stderr log, got %q", buf.String())
	}
	if strings.Contains(buf.String(), cmd) {
		t.Errorf("expected stderr log to redact raw command, got %q", buf.String())
	}
}

func TestExpandAgentCommandTemplate_UnknownFieldLogsAndReturnsRaw(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	cmd := "cmd {{.NotAField}}"

	var buf bytes.Buffer
	got := expandAgentCommandTemplate(cityPath, "test-city", agent, nil, "work_query", cmd, &buf)

	if got != cmd {
		t.Errorf("unknown field: got %q, want raw %q", got, cmd)
	}
	if buf.Len() == 0 {
		t.Errorf("expected stderr log on missing key, got empty buffer")
	}
}

func TestExpandAgentCommandTemplate_UsesCityFallbackWhenNameUnset(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	agent := &config.Agent{Name: "worker"}

	got := expandAgentCommandTemplate(cityPath, "", agent, nil, "work_query", "cmd {{.CityName}}", nil)
	if got != "cmd demo-city" {
		t.Fatalf("expandAgentCommandTemplate() = %q, want %q", got, "cmd demo-city")
	}
}

func TestExpandAgentCommandTemplate_NilAgent(t *testing.T) {
	cityPath := t.TempDir()
	got := expandAgentCommandTemplate(cityPath, "test-city", nil, nil, "work_query", "cmd {{.Rig}}", nil)
	if got != "cmd {{.Rig}}" {
		t.Errorf("nil agent: got %q, want raw command unchanged", got)
	}
}

func TestExpandAgentCommandTemplate_NilStderrDoesNotPanic(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	// Parse error with nil stderr must not panic.
	got := expandAgentCommandTemplate(cityPath, "test-city", agent, nil, "work_query", "cmd {{.Rig", nil)
	if got != "cmd {{.Rig" {
		t.Errorf("nil stderr parse error: got %q", got)
	}
}
