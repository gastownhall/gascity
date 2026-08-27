package main

import (
	goruntime "runtime"
	"testing"
)

// TestConfigureSupervisorRuntime pins the GC_PPROF gate in both directions: an
// operator who sets GC_PPROF=1 to diagnose a live memory problem must keep a
// usable /debug/pprof/heap, and everyone else must not pay for a profile
// nothing reads.
func TestConfigureSupervisorRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pprof   string
		wantOff bool
	}{
		{"unset disables heap profiling", "", true},
		{"GC_PPROF=1 preserves heap profiling", "1", false},
		{"GC_PPROF=true is not an opt-in", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := goruntime.MemProfileRate
			t.Cleanup(func() { goruntime.MemProfileRate = orig })
			goruntime.MemProfileRate = 512 * 1024
			t.Setenv("GC_PPROF", tc.pprof)

			configureSupervisorRuntime()

			if tc.wantOff && goruntime.MemProfileRate != 0 {
				t.Errorf("MemProfileRate = %d, want 0", goruntime.MemProfileRate)
			}
			if !tc.wantOff && goruntime.MemProfileRate == 0 {
				t.Error("MemProfileRate = 0, want heap profiling left enabled under GC_PPROF=1")
			}
		})
	}
}
