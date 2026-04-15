package config

import "testing"

// TestApplyAgentPatch_Account verifies that a patch with Account set
// overwrites the agent's Account field.
func TestApplyAgentPatch_Account(t *testing.T) {
	agent := Agent{Name: "worker", Account: "old-account"}
	acct := "work1"
	patch := AgentPatch{
		Dir:     "",
		Name:    "worker",
		Account: &acct,
	}
	applyAgentPatchFields(&agent, &patch)
	if agent.Account != "work1" {
		t.Errorf("Account = %q, want %q", agent.Account, "work1")
	}
}

// TestApplyAgentPatch_Account_Nil verifies that a patch with nil Account
// leaves the agent's existing Account unchanged.
func TestApplyAgentPatch_Account_Nil(t *testing.T) {
	agent := Agent{Name: "worker", Account: "existing"}
	patch := AgentPatch{
		Dir:     "",
		Name:    "worker",
		Account: nil,
	}
	applyAgentPatchFields(&agent, &patch)
	if agent.Account != "existing" {
		t.Errorf("Account = %q, want %q (should be unchanged)", agent.Account, "existing")
	}
}

// TestApplyAgentOverride_Account verifies that an override with Account set
// applies the Account value to the agent.
func TestApplyAgentOverride_Account(t *testing.T) {
	agent := Agent{Name: "worker", Account: ""}
	acct := "work1"
	override := AgentOverride{
		Agent:   "worker",
		Account: &acct,
	}
	applyAgentOverride(&agent, &override)
	if agent.Account != "work1" {
		t.Errorf("Account = %q, want %q", agent.Account, "work1")
	}
}
