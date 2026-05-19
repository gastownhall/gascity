package config

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func sidebarWakeModeForAgent(agent *Agent, namedSessionModes map[string]string) string {
	if agent == nil {
		return ""
	}
	if mode := namedSessionModes[agent.QualifiedName()]; mode != "" {
		return mode
	}
	if agent.Implicit || agent.MaxActiveSessions != nil && *agent.MaxActiveSessions > 1 {
		return "on_demand"
	}
	return "always"
}

func findAgentByNameDir(cfg *City, name, dir string) *Agent {
	if cfg == nil {
		return nil
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == name && cfg.Agents[i].Dir == dir {
			return &cfg.Agents[i]
		}
	}
	return nil
}

func TestLiveCity_AllAgentsHaveSidebarWakeContract(t *testing.T) {
	cityPath := "/data/projects/gc/city.toml"
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes(%q): %v", cityPath, err)
	}

	namedSessionModes := make(map[string]string, len(cfg.NamedSessions))
	for _, ns := range cfg.NamedSessions {
		namedSessionModes[ns.QualifiedName()] = ns.ModeOrDefault()
	}

	if len(cfg.Agents) == 0 {
		t.Fatal("cfg.Agents empty")
	}

	for _, agent := range cfg.Agents {
		qn := agent.QualifiedName()
		switch agent.Scope {
		case "city":
			if agent.Dir != "" {
				t.Fatalf("city agent %q has unexpected dir %q", qn, agent.Dir)
			}
		case "rig":
			if agent.Dir == "" {
				t.Fatalf("rig agent %q missing dir", qn)
			}
		case "":
			if !agent.Implicit {
				t.Fatalf("agent %q has empty scope but is not implicit", qn)
			}
		default:
			t.Fatalf("agent %q has unexpected scope %q", qn, agent.Scope)
		}
		mode := sidebarWakeModeForAgent(&agent, namedSessionModes)
		if mode == "" {
			t.Fatalf("agent %q missing named session mode", qn)
		}
	}

	if got := sidebarWakeModeForAgent(findAgentByNameDir(cfg, "dog", ""), namedSessionModes); got != "on_demand" {
		t.Fatalf("dog mode = %q, want on_demand", got)
	}
	if got := sidebarWakeModeForAgent(findAgentByNameDir(cfg, "polecat", "t3code"), namedSessionModes); got != "on_demand" {
		t.Fatalf("t3code/polecat mode = %q, want on_demand", got)
	}
	if got := sidebarWakeModeForAgent(findAgentByNameDir(cfg, "codex", ""), namedSessionModes); got != "on_demand" {
		t.Fatalf("codex mode = %q, want on_demand", got)
	}
	if got := sidebarWakeModeForAgent(findAgentByNameDir(cfg, "codex", "t3code"), namedSessionModes); got != "on_demand" {
		t.Fatalf("t3code/codex mode = %q, want on_demand", got)
	}

	if got := namedSessionModes["dog"]; got != "" {
		t.Fatalf("dog explicit mode = %q, want implicit default", got)
	}
	if got := namedSessionModes["t3code/polecat"]; got != "" {
		t.Fatalf("t3code/polecat explicit mode = %q, want pool default", got)
	}
	if got := namedSessionModes["codex"]; got != "" {
		t.Fatalf("codex explicit mode = %q, want implicit default", got)
	}
	if got := namedSessionModes["t3code/codex"]; got != "" {
		t.Fatalf("t3code/codex explicit mode = %q, want implicit default", got)
	}

	if filepath.Base(filepath.Dir(cityPath)) != "gc" {
		t.Fatalf("unexpected live city path %q", cityPath)
	}
}
