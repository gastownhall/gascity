package agentutil

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

// TestLookupSessionNameReadsTheTemplateKeySessionRowsCarry pins the stored-name
// read against the row shape production stamps: session rows carry the
// gc:session label and the registry key "template"
// (internal/session/manager.go's create), never the "gc.session" label and
// "gc.template" key this lookup once read, which no writer stamps and which left
// the stored name unreachable and every caller naming sessions by the computed
// default.
func TestLookupSessionNameReadsTheTemplateKeySessionRowsCarry(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "frontend/worker",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession, "template:frontend/worker"},
		Metadata: map[string]string{"template": "frontend/worker", "session_name": "s-stored"},
	}); err != nil {
		t.Fatalf("creating the worker's session row: %v", err)
	}

	if got := LookupSessionName(beadstest.Sessions(store), "city", "frontend/worker", ""); got != "s-stored" {
		t.Errorf("LookupSessionName = %q, want the stored s-stored: the row has the shape production stamps (internal/session/manager.go), so the stored name is what routing must read", got)
	}
}

// TestLookupSessionNamePrefersTheAgentsOwnRow pins that the lookup does not
// answer with whichever row of the template lists first: an ad-hoc session
// of the template (gc session new) and the agent's pool capacity share its
// template metadata, but the agent's own row, matched by agent_name, answers.
// A template with only pool capacity falls back to the computed name.
func TestLookupSessionNamePrefersTheAgentsOwnRow(t *testing.T) {
	store := beads.NewMemStore()
	for _, row := range []beads.Bead{
		{Metadata: map[string]string{"session_name": "s-adhoc", "session_origin": "manual"}},
		{Metadata: map[string]string{"session_name": "s-pool", "pool_slot": "1"}},
		{Metadata: map[string]string{"session_name": "s-own", "agent_name": "frontend/worker"}},
	} {
		row.Title = "frontend/worker"
		row.Type = session.BeadType
		row.Labels = []string{session.LabelSession, "template:frontend/worker"}
		row.Metadata["template"] = "frontend/worker"
		if _, err := store.Create(row); err != nil {
			t.Fatalf("creating session row %s: %v", row.Metadata["session_name"], err)
		}
	}
	if got := LookupSessionName(beadstest.Sessions(store), "city", "frontend/worker", ""); got != "s-own" {
		t.Errorf("LookupSessionName = %q, want the agent's own row's s-own", got)
	}

	pool := beads.NewMemStore()
	if _, err := pool.Create(beads.Bead{
		Title:    "frontend/worker",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession, "template:frontend/worker"},
		Metadata: map[string]string{"template": "frontend/worker", "session_name": "s-pool", "pool_slot": "1"},
	}); err != nil {
		t.Fatalf("creating the pool row: %v", err)
	}
	if got, want := LookupSessionName(beadstest.Sessions(pool), "city", "frontend/worker", ""), "frontend--worker"; got != want {
		t.Errorf("LookupSessionName over only pool capacity = %q, want the computed %q", got, want)
	}
}
