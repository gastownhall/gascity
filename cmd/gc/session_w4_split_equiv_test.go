package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// WI-5 W4 identity-resolution-chain oracles. The reconciler's session-bead
// identity chain (sessionBeadQualifiedName, canonicalSessionIdentityWithConfig,
// existingPoolSlotWithConfig) moves onto session.Info in W4 so the raw
// resolveTemplateForSessionBead wrapper can be retired. Each Info form must be
// byte-identical to reading the raw session bead, across every agent shape (bare,
// instance-expanding pool, singleton) and every session-bead shape.

// w4OracleAgents returns the agent shapes the identity chain branches on: a
// default multi-session agent, an instance-expanding pool agent (numbered slots),
// and a singleton-pool agent (canonical identity, no slot synthesis).
func w4OracleAgents() []*config.Agent {
	bare := &config.Agent{Name: "worker"}
	pool := &config.Agent{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}
	singleton := &config.Agent{Name: "mayor", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}
	return []*config.Agent{bare, pool, singleton}
}

// w4OracleSessionBeads augments the shared oracleSessionBeadShapes with
// pool-slot-bearing beads that exercise the existingPoolSlotWithConfig branches
// (pool_slot with matching/mismatching agent_name and alias slots).
func w4OracleSessionBeads() []beads.Bead {
	mk := func(id string, m map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: m}
	}
	extra := []beads.Bead{
		mk("ga-slot", map[string]string{"template": "worker", "session_name": "worker-2", "pool_slot": "2", "agent_name": "worker-2"}),
		mk("ga-slot-alias", map[string]string{"template": "worker", "session_name": "worker-3", "pool_slot": "3", "alias": "worker-3"}),
		mk("ga-slot-mismatch", map[string]string{"template": "worker", "session_name": "worker-4", "pool_slot": "9", "agent_name": "worker-2"}),
		mk("ga-explicit", map[string]string{"template": "worker", "session_name": "chosen", "session_name_explicit": "true"}),
		// agent:<name> label fallback: no agent_name metadata, so the identity is
		// recovered from the agent: label (pins the label arm of
		// sessionBeadAgentName(Info) that the qualified-name/slot chains read).
		{
			ID: "ga-agentlabel", Type: session.BeadType, Status: "open",
			Labels:   []string{session.LabelSession, "agent:worker-7"},
			Metadata: map[string]string{"template": "worker", "session_name": "worker-7", "pool_slot": "7"},
		},
		// Legacy aliasless pooled bead: agent_name equals the agent's own qualified
		// name, with a session_name but no alias/explicit — sessionBeadQualifiedName
		// recovers session_name as the concrete identity via the
		// SupportsMultipleSessions legacy branch.
		mk("ga-legacy-aliasless", map[string]string{"template": "worker", "agent_name": "worker", "session_name": "worker-legacy"}),
	}
	return append(oracleSessionBeadShapes(), extra...)
}

// TestSessionBeadQualifiedNameInfoMatchesRaw proves the Info form of
// sessionBeadQualifiedName agrees with the raw-bead form across every agent and
// session-bead shape. This value seeds the identity used to resolve TemplateParams
// for a rediscovered session, so divergence would silently mis-key a session.
func TestSessionBeadQualifiedNameInfoMatchesRaw(t *testing.T) {
	rigs := []config.Rig{}
	for _, cfgAgent := range w4OracleAgents() {
		for _, sb := range w4OracleSessionBeads() {
			info := sessiontest.SeedBead(t, sb)
			got := sessionBeadQualifiedNameInfo("", cfgAgent, rigs, info)
			want := sessionBeadQualifiedName("", cfgAgent, rigs, sb)
			if got != want {
				t.Errorf("sessionBeadQualifiedName(agent=%s, %s): info=%q raw=%q", cfgAgent.Name, sb.ID, got, want)
			}
		}
	}
}

// TestExistingPoolSlotWithConfigInfoMatchesRaw proves the Info form of
// existingPoolSlotWithConfig agrees with the raw-bead form across every agent and
// session-bead shape (including slot-bearing pool beads), for both a real config
// and a nil config (the storedTemplateMatches short-circuit).
func TestExistingPoolSlotWithConfigInfoMatchesRaw(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		{Name: "mayor", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
	}}
	for _, cfgUnderTest := range []*config.City{cfg, nil} {
		for _, cfgAgent := range w4OracleAgents() {
			for _, sb := range w4OracleSessionBeads() {
				info := sessiontest.SeedBead(t, sb)
				got := existingPoolSlotWithConfigInfo(cfgUnderTest, cfgAgent, info)
				want := existingPoolSlotWithConfig(cfgUnderTest, cfgAgent, sb)
				if got != want {
					t.Errorf("existingPoolSlotWithConfig(cfg=%v, agent=%s, %s): info=%d raw=%d", cfgUnderTest != nil, cfgAgent.Name, sb.ID, got, want)
				}
			}
		}
	}
}

// TestCanonicalSessionIdentityWithConfigInfoMatchesRaw proves the Info form of
// canonicalSessionIdentityWithConfig returns the same (agent, qualifiedName) pair
// as the raw-bead form across every agent and session-bead shape. The returned
// *config.Agent is compared by value (DeepEqual) because the pool path
// deep-copies a slot-numbered agent.
func TestCanonicalSessionIdentityWithConfigInfoMatchesRaw(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{
		{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		{Name: "mayor", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)},
	}}
	for _, cfgAgent := range w4OracleAgents() {
		for _, sb := range w4OracleSessionBeads() {
			info := sessiontest.SeedBead(t, sb)
			gotAgent, gotQN := canonicalSessionIdentityWithConfigInfo(cfg, cfgAgent, info)
			wantAgent, wantQN := canonicalSessionIdentityWithConfig(cfg, cfgAgent, sb)
			if gotQN != wantQN {
				t.Errorf("canonicalSessionIdentityWithConfig qn(agent=%s, %s): info=%q raw=%q", cfgAgent.Name, sb.ID, gotQN, wantQN)
			}
			if !reflect.DeepEqual(gotAgent, wantAgent) {
				t.Errorf("canonicalSessionIdentityWithConfig agent(agent=%s, %s): info=%+v raw=%+v", cfgAgent.Name, sb.ID, gotAgent, wantAgent)
			}
		}
	}
}
