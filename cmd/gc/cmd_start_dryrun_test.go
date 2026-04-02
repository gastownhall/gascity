package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPrintDryRunPreview(t *testing.T) {
	ds := map[string]TemplateParams{
		"mayor":         {SessionName: "mayor", TemplateName: "mayor", Command: "echo hello"},
		"hw--polecat-1": {SessionName: "hw--polecat-1", TemplateName: "hw/polecat-1", Command: "echo hello"},
		"hw--polecat-2": {SessionName: "hw--polecat-2", TemplateName: "hw/polecat-2", Command: "echo hello"},
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "hw", MaxActiveSessions: intPtr(1)},
			{Name: "worker", Suspended: true},
		},
	}

	var stdout bytes.Buffer
	printDryRunPreview(ds, cfg, "test", &stdout)
	out := stdout.String()

	if !strings.Contains(out, "3 agent(s) would start") {
		t.Errorf("should report 3 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "mayor") {
		t.Errorf("should list mayor, got:\n%s", out)
	}
	if !strings.Contains(out, "hw/polecat-1") {
		t.Errorf("should list hw/polecat-1, got:\n%s", out)
	}
	if !strings.Contains(out, "1 agent(s) suspended") {
		t.Errorf("should mention 1 suspended, got:\n%s", out)
	}
	if !strings.Contains(out, "No side effects executed (--dry-run).") {
		t.Errorf("should show dry-run footer, got:\n%s", out)
	}
}

func TestPrintDryRunPreviewEmpty(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "empty"},
	}

	var stdout bytes.Buffer
	printDryRunPreview(nil, cfg, "empty", &stdout)
	out := stdout.String()

	if !strings.Contains(out, "0 agent(s) would start") {
		t.Errorf("should report 0 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "(no agents to start)") {
		t.Errorf("should show empty message, got:\n%s", out)
	}
}

func TestStartDryRunFlagExists(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStartCmd(&stdout, &stderr)
	f := cmd.Flags().Lookup("dry-run")
	if f == nil {
		t.Fatal("missing --dry-run flag")
	}
	if f.Shorthand != "n" {
		t.Errorf("--dry-run shorthand = %q, want %q", f.Shorthand, "n")
	}
}
