package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPrefixedWorkQueryForProbe_UsesNamedSessionRuntimeName(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "witness",
			Dir:  "demo",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}

	command := prefixedWorkQueryForProbe(cfg, "test-city", nil, nil, &cfg.Agents[0])
	if !strings.Contains(command, "GC_SESSION_NAME='demo--witness'") && !strings.Contains(command, "GC_SESSION_NAME=demo--witness") {
		t.Fatalf("prefixedWorkQueryForProbe() = %q, want named-session GC_SESSION_NAME prefix", command)
	}
}
