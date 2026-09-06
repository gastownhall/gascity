package config

import (
	"reflect"
	"testing"
)

// TestBuiltinProviderMatchesMapForm guards the single-preset accessor against
// drifting from BuiltinProviders(): every preset must round-trip identically
// through both, and an unknown name must miss.
func TestBuiltinProviderMatchesMapForm(t *testing.T) {
	all := BuiltinProviders()
	if len(all) == 0 {
		t.Fatal("no builtin providers")
	}
	for name, want := range all {
		got, ok := BuiltinProvider(name)
		if !ok {
			t.Fatalf("BuiltinProvider(%q) missing", name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("BuiltinProvider(%q) differs from BuiltinProviders()[%q]", name, name)
		}
	}
	if _, ok := BuiltinProvider("no-such-provider"); ok {
		t.Error("BuiltinProvider(unknown) = ok, want miss")
	}
}
