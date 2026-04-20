package agentutil

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type partialSessionLister struct {
	running []string
	err     error
}

func (p partialSessionLister) ListRunning(prefix string) ([]string, error) {
	var filtered []string
	for _, name := range p.running {
		if len(prefix) == 0 || len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			filtered = append(filtered, name)
		}
	}
	return filtered, p.err
}

func TestExpandAgentsFixedAgent(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 {
		t.Fatalf("got %d agents, want 1", len(result))
	}
	if result[0].QualifiedName != "mayor" {
		t.Errorf("name = %q, want mayor", result[0].QualifiedName)
	}
	if result[0].Pool != "" {
		t.Errorf("pool = %q, want empty", result[0].Pool)
	}
}

func TestExpandAgentsBoundedPool(t *testing.T) {
	agents := []config.Agent{
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(3)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 3 {
		t.Fatalf("got %d agents, want 3", len(result))
	}
	if result[0].QualifiedName != "myrig/polecat-1" {
		t.Errorf("[0] name = %q, want myrig/polecat-1", result[0].QualifiedName)
	}
	if result[0].Pool != "myrig/polecat" {
		t.Errorf("[0] pool = %q, want myrig/polecat", result[0].Pool)
	}
	if result[2].QualifiedName != "myrig/polecat-3" {
		t.Errorf("[2] name = %q, want myrig/polecat-3", result[2].QualifiedName)
	}
}

func TestExpandAgentsNamepool(t *testing.T) {
	agents := []config.Agent{
		{
			Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2),
			NamepoolNames: []string{"alpha", "beta"},
		},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 2 {
		t.Fatalf("got %d agents, want 2", len(result))
	}
	if result[0].QualifiedName != "myrig/alpha" {
		t.Errorf("[0] = %q, want myrig/alpha", result[0].QualifiedName)
	}
	if result[1].QualifiedName != "myrig/beta" {
		t.Errorf("[1] = %q, want myrig/beta", result[1].QualifiedName)
	}
}

func TestExpandAgentsMixed(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1)},
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 3 {
		t.Fatalf("got %d agents, want 3 (1 fixed + 2 pool)", len(result))
	}
}

func TestExpandAgentsSuspended(t *testing.T) {
	agents := []config.Agent{
		{Name: "mayor", MaxActiveSessions: intPtr(1), Suspended: true},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 || !result[0].Suspended {
		t.Error("expected suspended agent")
	}
}

func TestExpandAgentsUnlimitedPoolFailsClosedOnPartialListResults(t *testing.T) {
	agents := []config.Agent{
		{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(-1)},
	}
	sp := partialSessionLister{
		running: []string{"myrig--polecat-1", "myrig--polecat-2"},
		err:     &runtime.PartialListError{Err: errors.New("remote backend down")},
	}

	result := ExpandAgents(agents, "city", "", sp)
	if len(result) != 0 {
		t.Fatalf("got %d agents, want fail-closed empty result on partial list", len(result))
	}
}

func TestPoolInstanceName(t *testing.T) {
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}
	if got := PoolInstanceName("polecat", 2, a); got != "polecat-2" {
		t.Errorf("got %q, want polecat-2", got)
	}

	a2 := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(1)}
	if got := PoolInstanceName("polecat", 1, a2); got != "polecat" {
		t.Errorf("single instance: got %q, want polecat", got)
	}

	a3 := config.Agent{
		Name: "polecat", MaxActiveSessions: intPtr(2),
		NamepoolNames: []string{"alpha", "beta"},
	}
	if got := PoolInstanceName("polecat", 1, a3); got != "alpha" {
		t.Errorf("namepool: got %q, want alpha", got)
	}
}
