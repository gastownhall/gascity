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

func TestExpandAgentsCanonicalSingletonPoolUsesBaseName(t *testing.T) {
	agents := []config.Agent{
		{Name: "worker", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
	}
	result := ExpandAgents(agents, "city", "", nil)
	if len(result) != 1 {
		t.Fatalf("got %d agents, want 1", len(result))
	}
	if result[0].QualifiedName != "myrig/worker" {
		t.Errorf("name = %q, want myrig/worker", result[0].QualifiedName)
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

	a2.MinActiveSessions = intPtr(0)
	if got := PoolInstanceName("polecat", 1, a2); got != "polecat" {
		t.Errorf("canonical singleton pool: got %q, want polecat", got)
	}

	a3 := config.Agent{
		Name: "polecat", MaxActiveSessions: intPtr(2),
		NamepoolNames: []string{"alpha", "beta"},
	}
	if got := PoolInstanceName("polecat", 1, a3); got != "alpha" {
		t.Errorf("namepool: got %q, want alpha", got)
	}
}

// TestPoolMemberSlotInvertsPoolInstanceName pins PoolMemberSlot as the exact
// inverse of PoolInstanceName over in-bound slots, so the member names gc
// status displays and the names gc sling resolves cannot drift apart.
func TestPoolMemberSlotInvertsPoolInstanceName(t *testing.T) {
	roundTrip := []struct {
		name  string
		agent config.Agent
		slots int
	}{
		{
			name: "bounded namepool larger than max",
			agent: config.Agent{
				Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(2),
				NamepoolNames: []string{"furiosa", "rictus", "nux"},
			},
			slots: 2,
		},
		{
			name: "bounded namepool smaller than max",
			agent: config.Agent{
				Name: "polecat", Dir: "frontend", BindingName: "gastown", MaxActiveSessions: intPtr(4),
				NamepoolNames: []string{"furiosa", "rictus"},
			},
			slots: 4,
		},
		{
			name: "unlimited namepool",
			agent: config.Agent{
				Name: "polecat", NamepoolNames: []string{"furiosa", "rictus"},
			},
			slots: 12,
		},
		{
			name:  "numeric bounded",
			agent: config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)},
			slots: 3,
		},
		{
			name: "namepool singleton",
			agent: config.Agent{
				Name: "polecat", MaxActiveSessions: intPtr(1), MinActiveSessions: intPtr(0),
				NamepoolNames: []string{"furiosa"},
			},
			slots: 1,
		},
	}
	for _, tc := range roundTrip {
		t.Run("round trip/"+tc.name, func(t *testing.T) {
			for s := 1; s <= tc.slots; s++ {
				member := PoolInstanceName(tc.agent.Name, s, tc.agent)
				got, ok := PoolMemberSlot(tc.agent, member)
				if !ok || got != s {
					t.Errorf("PoolMemberSlot(%q) = (%d, %v), want (%d, true)", member, got, ok, s)
				}
			}
		})
	}

	namepool := config.Agent{
		Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(2),
		NamepoolNames: []string{"furiosa", "rictus", "nux"},
	}
	numeric := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(3)}
	rejects := []struct {
		name   string
		agent  config.Agent
		member string
	}{
		{"out-of-bound themed name", namepool, "nux"},
		{"name not in namepool", namepool, "ghost"},
		{"numeric slot that renders themed", namepool, "polecat-1"},
		{"numeric slot past max", namepool, "polecat-3"},
		{"zero slot", numeric, "polecat-0"},
		{"zero-padded slot", numeric, "polecat-01"},
		{"signed slot", numeric, "polecat-+1"},
		{"slot past max", numeric, "polecat-4"},
		{"bare pool name", numeric, "polecat"},
		{"qualified member", namepool, "frontend/furiosa"},
		{"max zero pool", config.Agent{
			Name: "polecat", MaxActiveSessions: intPtr(0), NamepoolNames: []string{"furiosa"},
		}, "furiosa"},
		{"max zero numeric pool", config.Agent{Name: "polecat", MaxActiveSessions: intPtr(0)}, "polecat-1"},
		{"canonical singleton pool", config.Agent{
			Name: "polecat", MaxActiveSessions: intPtr(1), MinActiveSessions: intPtr(0),
		}, "polecat-1"},
		{"canonical singleton pool base name", config.Agent{
			Name: "polecat", MaxActiveSessions: intPtr(1), MinActiveSessions: intPtr(0),
		}, "polecat"},
		{"named-session singleton", config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}, "mayor-1"},
	}
	for _, tc := range rejects {
		t.Run("rejects/"+tc.name, func(t *testing.T) {
			if got, ok := PoolMemberSlot(tc.agent, tc.member); ok {
				t.Errorf("PoolMemberSlot(%q) = (%d, true), want false", tc.member, got)
			}
		})
	}

	t.Run("duplicate namepool name resolves to first slot", func(t *testing.T) {
		dup := config.Agent{
			Name: "polecat", MaxActiveSessions: intPtr(3),
			NamepoolNames: []string{"furiosa", "rictus", "furiosa"},
		}
		if got, ok := PoolMemberSlot(dup, "furiosa"); !ok || got != 1 {
			t.Errorf("PoolMemberSlot(furiosa) = (%d, %v), want (1, true)", got, ok)
		}
	})
}
