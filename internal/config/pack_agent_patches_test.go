package config

import "testing"

// #4525: [[patches.agent]] must target an imported agent's bare local
// name, not its binding-qualified name — even though pack-spec §2.5
// says imported agents are addressed by binding-qualified name
// everywhere else. When a pack author uses the qualified form here, the
// error should say so instead of leaving them to guess.
func TestApplyPackAgentPatchesQualifiedNameHint(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
	}
	patches := []AgentPatch{
		{Name: "roles.requirements-planner"},
	}

	err := applyPackAgentPatches(agents, patches)
	if err == nil {
		t.Fatal("expected error for qualified-name patch target, got nil")
	}

	const want = `patches.agent[0]: agent "roles.requirements-planner" not found in pack (patches match local names — did you mean "requirements-planner"?)`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestApplyPackAgentPatchesBareNameStillMatches(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
	}
	suspended := true
	patches := []AgentPatch{
		{Name: "requirements-planner", Suspended: &suspended},
	}

	if err := applyPackAgentPatches(agents, patches); err != nil {
		t.Fatalf("bare-name patch should match: %v", err)
	}
	if !agents[0].Suspended {
		t.Error("patch fields were not applied to the matched agent")
	}
}

func TestApplyPackAgentPatchesUnrelatedNameNoHint(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
	}
	patches := []AgentPatch{
		{Name: "totally-unknown"},
	}

	err := applyPackAgentPatches(agents, patches)
	if err == nil {
		t.Fatal("expected error for unmatched patch target, got nil")
	}

	const want = `patches.agent[0]: agent "totally-unknown" not found in pack`
	if err.Error() != want {
		t.Errorf("error = %q, want %q (no hint should be added when nothing qualifies)", err.Error(), want)
	}
}
