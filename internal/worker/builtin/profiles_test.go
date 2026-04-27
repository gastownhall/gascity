package builtin

import (
	"testing"
)

func TestBuiltinProvidersAndOrder(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	if len(providers) != 11 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 11", len(providers))
	}
	if len(order) != 11 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 11", len(order))
	}

	for _, name := range order {
		spec, ok := providers[name]
		if !ok {
			t.Fatalf("BuiltinProviders() missing %q", name)
		}
		if spec.Command == "" {
			t.Fatalf("provider %q has empty Command", name)
		}
		if spec.DisplayName == "" {
			t.Fatalf("provider %q has empty DisplayName", name)
		}
	}
}

func TestBuiltinProvidersReturnClonedData(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()

	a["claude"] = BuiltinProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Fatal("BuiltinProviders() should return a cloned map")
	}

	claude := a["codex"]
	claude.ProcessNames[0] = "mutated"
	a["codex"] = claude
	if b["codex"].ProcessNames[0] == "mutated" {
		t.Fatal("BuiltinProviders() should clone nested slices")
	}
}
