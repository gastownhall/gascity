package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestExpandProbeCommandTemplate_SubstitutesRig(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "myrig", Path: cityPath + "/myrig"}}
	agent := &config.Agent{Name: "ant", Dir: "myrig"}

	got := expandProbeCommandTemplate(cityPath, "test-city", agent, rigs, "cmd {{.Rig}}/ant", nil)
	want := "cmd myrig/ant"
	if got != want {
		t.Fatalf("expandProbeCommandTemplate = %q, want %q", got, want)
	}
}

func TestExpandProbeCommandTemplate_SubstitutesAgentBase(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}

	got := expandProbeCommandTemplate(cityPath, "test-city", agent, nil, "probe {{.AgentBase}}", nil)
	want := "probe worker"
	if got != want {
		t.Fatalf("expandProbeCommandTemplate = %q, want %q", got, want)
	}
}

func TestExpandProbeCommandTemplate_LiteralOnlyIsByteIdentical(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}

	cases := []string{
		"bd ready --metadata-field gc.routed_to=worker",
		"echo 1",
		"",
	}
	for _, cmd := range cases {
		got := expandProbeCommandTemplate(cityPath, "test-city", agent, nil, cmd, nil)
		if got != cmd {
			t.Errorf("literal command mutated: got %q, want %q", got, cmd)
		}
	}
}

func TestExpandProbeCommandTemplate_ParseErrorLogsAndReturnsRaw(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	cmd := "cmd {{.Rig" // malformed

	var buf bytes.Buffer
	got := expandProbeCommandTemplate(cityPath, "test-city", agent, nil, cmd, &buf)

	if got != cmd {
		t.Errorf("parse error: got %q, want raw %q", got, cmd)
	}
	if !strings.Contains(buf.String(), "expandProbeCommandTemplate") {
		t.Errorf("expected stderr log on parse error, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "worker") {
		t.Errorf("expected agent name in stderr log, got %q", buf.String())
	}
}

func TestExpandProbeCommandTemplate_UnknownFieldLogsAndReturnsRaw(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	cmd := "cmd {{.NotAField}}"

	var buf bytes.Buffer
	got := expandProbeCommandTemplate(cityPath, "test-city", agent, nil, cmd, &buf)

	if got != cmd {
		t.Errorf("unknown field: got %q, want raw %q", got, cmd)
	}
	if buf.Len() == 0 {
		t.Errorf("expected stderr log on missing key, got empty buffer")
	}
}

func TestExpandProbeCommandTemplate_NilAgent(t *testing.T) {
	cityPath := t.TempDir()
	got := expandProbeCommandTemplate(cityPath, "test-city", nil, nil, "cmd {{.Rig}}", nil)
	if got != "cmd {{.Rig}}" {
		t.Errorf("nil agent: got %q, want raw command unchanged", got)
	}
}

func TestExpandProbeCommandTemplate_NilStderrDoesNotPanic(t *testing.T) {
	cityPath := t.TempDir()
	agent := &config.Agent{Name: "worker"}
	// Parse error with nil stderr must not panic.
	got := expandProbeCommandTemplate(cityPath, "test-city", agent, nil, "cmd {{.Rig", nil)
	if got != "cmd {{.Rig" {
		t.Errorf("nil stderr parse error: got %q", got)
	}
}
