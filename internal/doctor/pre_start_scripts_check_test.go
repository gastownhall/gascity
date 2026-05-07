package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPreStartScriptsCheck_NoCfg(t *testing.T) {
	c := NewPreStartScriptsCheck(nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
}

func TestPreStartScriptsCheck_NoAgents(t *testing.T) {
	c := NewPreStartScriptsCheck(&config.City{})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
}

func TestPreStartScriptsCheck_ScriptExists(t *testing.T) {
	pack := t.TempDir()
	if err := os.MkdirAll(filepath.Join(pack, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "scripts", "setup.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "builder",
				SourceDir: pack,
				PreStart:  []string{"{{.ConfigDir}}/scripts/setup.sh {{.RigRoot}} {{.WorkDir}}"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestPreStartScriptsCheck_ScriptMissing(t *testing.T) {
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "wren-runner",
				SourceDir: pack,
				PreStart:  []string{"{{.ConfigDir}}/scripts/missing.sh args"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "wren-runner") || !strings.Contains(r.Details[0], "scripts/missing.sh") {
		t.Errorf("detail = %q; want to mention agent + missing path", r.Details[0])
	}
	if r.FixHint == "" {
		t.Error("expected FixHint to be set on warning")
	}
}

func TestPreStartScriptsCheck_InlineAgentSkipped(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "inline",
				SourceDir: "",
				PreStart:  []string{"{{.ConfigDir}}/scripts/whatever.sh"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("inline agents should be skipped; status = %d, msg = %s", r.Status, r.Message)
	}
}

func TestPreStartScriptsCheck_NoConfigDirReference(t *testing.T) {
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "agent",
				SourceDir: pack,
				PreStart:  []string{"mkdir -p {{.WorkDir}}/foo", "echo hello"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("commands without {{.ConfigDir}} should be skipped; status = %d, details = %v", r.Status, r.Details)
	}
}

func TestPreStartScriptsCheck_OtherTemplateInScriptPath(t *testing.T) {
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "agent",
				SourceDir: pack,
				PreStart:  []string{"{{.ConfigDir}}/{{.AgentBase}}/foo.sh"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("script paths with unresolved templates should be skipped; status = %d, details = %v", r.Status, r.Details)
	}
}

func TestPreStartScriptsCheck_MultipleAgentsSortedOutput(t *testing.T) {
	packA := t.TempDir()
	if err := os.WriteFile(filepath.Join(packA, "ok.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	packB := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "agent-a", SourceDir: packA, PreStart: []string{"{{.ConfigDir}}/ok.sh"}},
			{Name: "agent-b", SourceDir: packB, PreStart: []string{"{{.ConfigDir}}/missing-z.sh", "{{.ConfigDir}}/missing-a.sh"}},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) != 2 {
		t.Fatalf("expected 2 issues, got %d: %v", len(r.Details), r.Details)
	}
	for i := 1; i < len(r.Details); i++ {
		if r.Details[i-1] > r.Details[i] {
			t.Errorf("details not sorted: %v", r.Details)
		}
	}
}

func TestPreStartScriptsCheck_RelativeScriptPathSkipped(t *testing.T) {
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "agent",
				SourceDir: pack,
				PreStart:  []string{"scripts/setup.sh"}, // no ConfigDir, relative — runtime resolves CWD
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("relative paths without {{.ConfigDir}} should be skipped; status = %d, details = %v", r.Status, r.Details)
	}
}

func TestPreStartScriptsCheck_QualifiedNameInDetail(t *testing.T) {
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:        "runner",
				BindingName: "wren",
				SourceDir:   pack,
				PreStart:    []string{"{{.ConfigDir}}/scripts/missing.sh"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(r.Details))
	}
	want := "wren.runner"
	if !strings.Contains(r.Details[0], want) {
		t.Errorf("detail = %q; want it to contain qualified name %q", r.Details[0], want)
	}
}
