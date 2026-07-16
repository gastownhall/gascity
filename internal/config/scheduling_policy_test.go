package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestEffectiveSchedulingPolicyDefaultsToPriorityFIFO(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent Agent
		want  beads.AdmissionPolicy
	}{
		{
			name:  "unset preserves #4322 priority-band behavior",
			agent: Agent{Name: "polecat"},
			want:  beads.PolicyPriorityFIFO,
		},
		{
			name:  "explicit priority_fifo",
			agent: Agent{Name: "polecat", SchedulingPolicy: "priority_fifo"},
			want:  beads.PolicyPriorityFIFO,
		},
		{
			name:  "explicit fifo",
			agent: Agent{Name: "polecat", SchedulingPolicy: "fifo"},
			want:  beads.PolicyFIFO,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.agent.EffectiveSchedulingPolicy(); got != tc.want {
				t.Fatalf("EffectiveSchedulingPolicy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateAgentsRejectsUnknownSchedulingPolicy(t *testing.T) {
	err := ValidateAgents([]Agent{{Name: "polecat", SchedulingPolicy: "lifo"}})
	if err == nil {
		t.Fatal("ValidateAgents accepted scheduling_policy=lifo, want fail-fast rejection")
	}
	for _, want := range []string{"polecat", "lifo", "priority_fifo", "fifo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestValidateAgentsAcceptsKnownSchedulingPolicies(t *testing.T) {
	for _, policy := range []string{"", "priority_fifo", "fifo"} {
		if err := ValidateAgents([]Agent{{Name: "polecat", SchedulingPolicy: policy}}); err != nil {
			t.Fatalf("ValidateAgents(scheduling_policy=%q) = %v, want nil", policy, err)
		}
	}
}

// A pack ships a default for a pool it owns; a city patch is the operator's
// authoritative override for a shared pool. This is the existing composition
// order (pack -> city patches -> rig overrides), asserted here so a future
// refactor cannot silently make a pack's value win over the operator's.
func TestCitySchedulingPolicyPatchOverridesPackValue(t *testing.T) {
	agent := Agent{Name: "polecat", SchedulingPolicy: "fifo"}
	operator := "priority_fifo"
	applyAgentPatchFields(&agent, &AgentPatch{SchedulingPolicy: &operator})

	if got := agent.EffectiveSchedulingPolicy(); got != beads.PolicyPriorityFIFO {
		t.Fatalf("city patch lost to pack value: got %q, want %q", got, beads.PolicyPriorityFIFO)
	}
}

func TestSchedulingPolicyRigOverrideReachesAgent(t *testing.T) {
	agent := Agent{Name: "polecat", SchedulingPolicy: "priority_fifo"}
	rigValue := "fifo"
	override := AgentOverride{SchedulingPolicy: &rigValue}
	applyAgentOverride(&agent, &override)

	if got := agent.EffectiveSchedulingPolicy(); got != beads.PolicyFIFO {
		t.Fatalf("rig override did not reach agent: got %q, want %q", got, beads.PolicyFIFO)
	}
}
