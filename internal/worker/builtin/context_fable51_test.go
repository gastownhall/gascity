package builtin

import "testing"

// Claude Fable 5.1 (claude-fable-5-1) gets both enum forms the other Claude
// generations have: the short alias operators pick from the select, and the
// canonical id they pin verbatim in agent.toml. Without the canonical entry a
// pinned "claude-fable-5-1" hits the ra-jbbv0 failure class (launch path
// silently emits no --model, named-session path hard-errors); the resolver
// side of that is asserted in internal/config/options_test.go.
func TestClaudeModelChoicesIncludeFable51(t *testing.T) {
	provider, ok := BuiltinProviders()["claude"]
	if !ok {
		t.Fatal("builtin claude provider missing")
	}
	byValue := make(map[string]BuiltinOptionChoice)
	for _, option := range provider.OptionsSchema {
		if option.Key != "model" {
			continue
		}
		for _, choice := range option.Choices {
			byValue[choice.Value] = choice
		}
	}
	for _, value := range []string{"fable-5-1", "claude-fable-5-1"} {
		choice, ok := byValue[value]
		if !ok {
			t.Fatalf("claude model choices missing %q", value)
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--model" || choice.FlagArgs[1] != "claude-fable-5-1" {
			t.Errorf("%s FlagArgs = %v, want [--model claude-fable-5-1]", value, choice.FlagArgs)
		}
		if len(choice.FlagAliases) != 1 || len(choice.FlagAliases[0]) != 2 ||
			choice.FlagAliases[0][0] != "-m" || choice.FlagAliases[0][1] != "claude-fable-5-1" {
			t.Errorf("%s FlagAliases = %v, want [[-m claude-fable-5-1]]", value, choice.FlagAliases)
		}
	}
}
