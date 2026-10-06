package config

import "testing"

// #4524: a pack's [agent_defaults] never applies to agents brought in by
// the pack's own [imports.*] -- applyInheritedPackAgentDefaults skips any
// agent with a non-empty BindingName. That's a defensible scoping choice
// (pack-spec §2.7 doesn't say either way), but it was silent: a pack author
// configuring agent_defaults.provider expecting it to cover imported roles
// gets no error and no warning, and every imported agent quietly runs on
// whatever provider it would have used anyway. This warns instead of
// changing the scoping.
func TestWarnUnusedPackAgentDefaultsForImportsProviderUnused(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
		{Name: "reviewer", BindingName: "roles"},
	}
	defaults := AgentDefaults{Provider: "cacc-sol"}

	warnings := warnUnusedPackAgentDefaultsForImports(agents, defaults)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want exactly 1", warnings)
	}
	const want = `agent_defaults currently does not apply to a pack's own [imports.*] agents (the loader scopes it to the pack's own agents/ and [[agent]] blocks; see pack-spec §2.7); provider unused by 2 imported agent(s)`
	if warnings[0] != want {
		t.Errorf("warning = %q, want %q", warnings[0], want)
	}
}

func TestWarnUnusedPackAgentDefaultsForImportsNoWarningWhenNoImports(t *testing.T) {
	agents := []Agent{
		{Name: "mayor"},
		{Name: "polecat"},
	}
	defaults := AgentDefaults{Provider: "cacc-sol"}

	warnings := warnUnusedPackAgentDefaultsForImports(agents, defaults)
	if warnings != nil {
		t.Errorf("warnings = %#v, want nil (no imported agents in scope)", warnings)
	}
}

func TestWarnUnusedPackAgentDefaultsForImportsNoWarningWhenImportAlreadyHasOwnProvider(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles", Provider: "already-set"},
	}
	defaults := AgentDefaults{Provider: "cacc-sol"}

	warnings := warnUnusedPackAgentDefaultsForImports(agents, defaults)
	if warnings != nil {
		t.Errorf("warnings = %#v, want nil (imported agent already has its own provider, agent_defaults not applying to it is expected, not a bug)", warnings)
	}
}

func TestWarnUnusedPackAgentDefaultsForImportsNoWarningWhenNoDefaultsConfigured(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
	}

	warnings := warnUnusedPackAgentDefaultsForImports(agents, AgentDefaults{})
	if warnings != nil {
		t.Errorf("warnings = %#v, want nil (pack declared no agent_defaults at all)", warnings)
	}
}

func TestWarnUnusedPackAgentDefaultsForImportsCombinesMultipleFields(t *testing.T) {
	agents := []Agent{
		{Name: "requirements-planner", BindingName: "roles"},
	}
	formula := "mol-do-work"
	defaults := AgentDefaults{Provider: "cacc-sol", DefaultSlingFormula: formula, AppendFragments: []string{"house-style"}}

	warnings := warnUnusedPackAgentDefaultsForImports(agents, defaults)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want exactly 1", warnings)
	}
	const want = `agent_defaults currently does not apply to a pack's own [imports.*] agents (the loader scopes it to the pack's own agents/ and [[agent]] blocks; see pack-spec §2.7); provider unused by 1 imported agent(s), default_sling_formula unused by 1 imported agent(s), append_fragments unused by 1 imported agent(s)`
	if warnings[0] != want {
		t.Errorf("warning = %q, want %q", warnings[0], want)
	}
}
