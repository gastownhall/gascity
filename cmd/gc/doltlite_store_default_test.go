//go:build !gascity_doltlite_lib

package main

import "testing"

func TestDefaultBuildDoesNotEnableDoltliteRuntime(t *testing.T) {
	t.Setenv("GC_NATIVE_DOLTLITE_BEADS", "true")
	store, ok := openOptimizedDoltliteStore(t.TempDir(), nil)
	if ok {
		t.Fatalf("openOptimizedDoltliteStore ok = true, want false")
	}
	if store != nil {
		t.Fatalf("openOptimizedDoltliteStore store = %#v, want nil", store)
	}
}
