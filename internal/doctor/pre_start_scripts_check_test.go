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

func TestPreStartScriptsCheck_CityRoot_ScriptExists(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "scripts", "worktree-setup.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "builder",
				SourceDir: pack,
				PreStart:  []string{"{{.CityRoot}}/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}}"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "{{.ConfigDir}}") || !strings.Contains(r.Message, "{{.CityRoot}}") {
		t.Errorf("message = %q; want both supported templates mentioned", r.Message)
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

func TestPreStartScriptsCheck_CityRoot_ScriptMissing(t *testing.T) {
	cityPath := t.TempDir()
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "builder",
				SourceDir: pack,
				PreStart:  []string{"{{.CityRoot}}/scripts/worktree-setup.sh args"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(r.Details), r.Details)
	}
	want := `agent "builder": pre_start script "scripts/worktree-setup.sh" not found`
	if r.Details[0] != want {
		t.Errorf("detail = %q; want %q", r.Details[0], want)
	}
	if !strings.Contains(r.FixHint, "<city>/scripts/") || !strings.Contains(r.FixHint, "pack") {
		t.Errorf("FixHint = %q; want city and pack repair options", r.FixHint)
	}
}

func TestPreStartScriptsCheck_BothTemplates_OneMissing(t *testing.T) {
	cityPath := t.TempDir()
	packA := t.TempDir()
	if err := os.MkdirAll(filepath.Join(packA, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packA, "scripts", "A.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	packB := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "agent-a", SourceDir: packA, PreStart: []string{"{{.ConfigDir}}/scripts/A.sh"}},
			{Name: "agent-b", SourceDir: packB, PreStart: []string{"{{.CityRoot}}/scripts/B.sh"}},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if len(r.Details) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(r.Details), r.Details)
	}
	if !strings.Contains(r.Details[0], "agent-b") || !strings.Contains(r.Details[0], "scripts/B.sh") {
		t.Errorf("detail = %q; want second agent and CityRoot missing script", r.Details[0])
	}
	if strings.Contains(r.Details[0], "agent-a") {
		t.Errorf("detail = %q; ConfigDir script that exists should not be reported", r.Details[0])
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

func TestPreStartScriptsCheck_CityRoot_OtherTemplateInPath(t *testing.T) {
	cityPath := t.TempDir()
	pack := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:      "agent",
				SourceDir: pack,
				PreStart:  []string{"{{.CityRoot}}/scripts/{{.AgentBase}}/setup.sh"},
			},
		},
	}
	c := NewPreStartScriptsCheck(cfg)
	r := c.Run(&CheckContext{CityPath: cityPath})
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

func TestResolvePreStartScript_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		sourceDir string
		cityPath  string
		wantOK    bool
		wantPath  string
	}{
		{
			name:      "config dir",
			cmd:       "{{.ConfigDir}}/scripts/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    true,
			wantPath:  "/p/a/scripts/x.sh",
		},
		{
			name:      "city root",
			cmd:       "{{.CityRoot}}/scripts/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    true,
			wantPath:  "/c/scripts/x.sh",
		},
		{
			name:      "city root with trailing runtime template arg",
			cmd:       "{{.CityRoot}}/scripts/x.sh {{.RigRoot}}",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    true,
			wantPath:  "/c/scripts/x.sh",
		},
		{
			name:      "both templates substituted",
			cmd:       "{{.ConfigDir}}/scripts/{{.CityRoot}}/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    true,
			wantPath:  "/p/a/scripts/c/x.sh",
		},
		{
			name:      "rig root skipped",
			cmd:       "{{.RigRoot}}/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    false,
		},
		{
			name:      "relative skipped",
			cmd:       "relative/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    false,
		},
		{
			name:      "unresolved first token skipped",
			cmd:       "{{.CityRoot}}/{{.AgentBase}}/x.sh",
			sourceDir: "/p/a",
			cityPath:  "/c",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolvePreStartScript(tt.cmd, tt.sourceDir, tt.cityPath)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v; path = %q", ok, tt.wantOK, got)
			}
			if got != tt.wantPath {
				t.Errorf("path = %q, want %q", got, tt.wantPath)
			}
		})
	}
}
