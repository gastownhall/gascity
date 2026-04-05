package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPrefixedWorkQueryForProbe_UsesNamedSessionRuntimeName(t *testing.T) {
	cityPath := t.TempDir()
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

	command := prefixedWorkQueryForProbe(cfg, cityPath, "test-city", nil, nil, &cfg.Agents[0])
	// All agents now use metadata routing via gc.routed_to.
	if !strings.Contains(command, "gc.routed_to=demo/witness") {
		t.Fatalf("prefixedWorkQueryForProbe() = %q, want gc.routed_to=demo/witness", command)
	}
}
