package tmux

import (
	"reflect"
	"testing"
)

// Kills a method the seam-backed provider adds over the raw one: the
// runtime package's fake profile matches tmux against the raw provider
// (constructing the seam-backed one is a tmux test resource), which holds
// only while the seam-backed one has no method the raw one lacks.
func TestSeamBackedCarriesTheRawSurface(t *testing.T) {
	seam, raw := reflect.TypeFor[*seamBackedProvider](), reflect.TypeFor[*Provider]()
	for i := range seam.NumMethod() {
		if name := seam.Method(i).Name; !hasMethod(raw, name) {
			t.Errorf("the seam-backed provider has %s, which the raw provider lacks", name)
		}
	}
}

// Kills seam-backed capabilities that drift from the raw provider's, which
// the fake profile reports for tmux.
func TestSeamBackedCapabilitiesAreTheRawOnes(t *testing.T) {
	seam, ok := NewSeamBackedWithConfig(Config{}).(*seamBackedProvider)
	if !ok {
		t.Fatal("NewSeamBackedWithConfig builds no seam-backed provider")
	}
	if got, want := seam.Capabilities(), seam.Provider.Capabilities(); got != want {
		t.Fatalf("seam-backed capabilities %+v, raw %+v", got, want)
	}
}

func hasMethod(typ reflect.Type, name string) bool {
	_, ok := typ.MethodByName(name)
	return ok
}
