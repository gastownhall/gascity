package config

import (
	"strings"
	"testing"
)

func TestGuardedRigDispatcherWarnings(t *testing.T) {
	dispatcher := func(dir string) Agent {
		return Agent{Name: ControlDispatcherAgentName, Dir: dir, StartCommand: ControlDispatcherStartCommandFor("{{.Agent}}")}
	}
	cfg := &City{
		Rigs: []Rig{
			{Name: "beta", Federation: RigFederationRig},
			{Name: "gamma", Federation: RigFederationRig},
			{Name: "alpha"},
		},
		Agents: []Agent{dispatcher(""), dispatcher("gamma")},
	}
	got := GuardedRigDispatcherWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], `rig "beta"`) {
		t.Fatalf("warnings = %q, want one for beta", got)
	}
	if GuardedRigDispatcherWarnings(nil) != nil {
		t.Fatal("nil config warned")
	}
}
