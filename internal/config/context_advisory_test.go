package config

import "testing"

func TestResolveContextAdvisoryPerAgentOverridesGlobal(t *testing.T) {
	builtin := DefaultContextAdvisory()
	global := &ContextAdvisory{WindowTokens: contextAdvisoryPtr(1_000_000)}
	perAgent := &ContextAdvisory{Tiers: []ContextAdvisoryTier{{
		Threshold: contextAdvisoryPtr(75),
		Message:   contextAdvisoryPtr("Agent {{.Pct}}%"),
	}}}

	policy := ResolveContextAdvisory(&builtin, global, perAgent)
	if !policy.Enabled || policy.WindowTokens != 1_000_000 {
		t.Fatalf("policy = %+v, want enabled policy with global window", policy)
	}
	if len(policy.Tiers) != 1 || policy.Tiers[0].Threshold != 75 {
		t.Fatalf("tiers = %+v, want per-agent tier", policy.Tiers)
	}
}

func TestDefaultContextAdvisoryPreservesThresholdBoundaries(t *testing.T) {
	builtin := DefaultContextAdvisory()
	policy := ResolveContextAdvisory(&builtin)
	for _, test := range []struct {
		pct  float64
		want int
		ok   bool
	}{{59.9, 0, false}, {60, 60, true}, {80, 60, true}, {80.1, 80, true}} {
		tier, ok := policy.SelectTier(test.pct)
		if ok != test.ok || (ok && tier.Threshold != test.want) {
			t.Errorf("SelectTier(%v) = (%+v, %v), want threshold %d, found %v", test.pct, tier, ok, test.want, test.ok)
		}
	}
}

func TestParseContextAdvisoryAtGlobalAndAgentScope(t *testing.T) {
	cfg, err := Parse([]byte(`
[agent_defaults.context_advisory]
window_tokens = 500000
[[agent_defaults.context_advisory.tiers]]
threshold = 70
message = "global {{.Pct}}"

[[agent]]
name = "worker"
[agent.context_advisory]
enabled = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.AgentDefaults.ContextAdvisory == nil || cfg.AgentDefaults.ContextAdvisory.WindowTokens == nil || *cfg.AgentDefaults.ContextAdvisory.WindowTokens != 500000 {
		t.Fatalf("global advisory = %#v", cfg.AgentDefaults.ContextAdvisory)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].ContextAdvisory == nil || cfg.Agents[0].ContextAdvisory.Enabled == nil || *cfg.Agents[0].ContextAdvisory.Enabled {
		t.Fatalf("agent advisory = %#v", cfg.Agents)
	}
}

func TestMergeAgentDefaultsContextAdvisoryPreservesUnsetFields(t *testing.T) {
	base := AgentDefaults{ContextAdvisory: &ContextAdvisory{Enabled: contextAdvisoryPtr(true), WindowTokens: contextAdvisoryPtr(1_000_000)}}
	mergeAgentDefaults(&base, AgentDefaults{ContextAdvisory: &ContextAdvisory{WindowTokens: contextAdvisoryPtr(500_000)}}, "fragment", nil)
	if base.ContextAdvisory == nil || base.ContextAdvisory.Enabled == nil || !*base.ContextAdvisory.Enabled || base.ContextAdvisory.WindowTokens == nil || *base.ContextAdvisory.WindowTokens != 500_000 {
		t.Fatalf("merged advisory = %#v", base.ContextAdvisory)
	}
}

func TestParseRejectsInvalidContextAdvisory(t *testing.T) {
	_, err := Parse([]byte(`
[agent_defaults.context_advisory]
[[agent_defaults.context_advisory.tiers]]
threshold = 101
message = "too late"
`))
	if err == nil {
		t.Fatal("Parse accepted an invalid context_advisory threshold")
	}
}
