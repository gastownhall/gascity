package testenv_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/testenv"
)

// TestRolloutEnvOverridesAreLeakVectors: every rollout gate's env override must
// be scrubbed by testenv, so a live shell export cannot leak into a test and flip
// a gate. It lives here (not in internal/rollout) because testenv owns
// LeakVectorVars and the stray-import lint forbids non-testenv test files from
// importing internal/testenv; the testenv package dir is exempt from that lint.
func TestRolloutEnvOverridesAreLeakVectors(t *testing.T) {
	t.Parallel()
	leak := map[string]bool{}
	for _, v := range testenv.LeakVectorVars {
		leak[v] = true
	}
	checked := 0
	for _, s := range rollout.Specs() {
		if s.EnvOverride == "" {
			continue
		}
		checked++
		if !leak[s.EnvOverride] {
			t.Errorf("%s: EnvOverride %q is not in testenv.LeakVectorVars; a stray shell value could flip the gate during tests", s.Key, s.EnvOverride)
		}
	}
	if checked == 0 {
		t.Fatal("no rollout gate declared an EnvOverride — the leak-vector coverage check is vacuous")
	}
}
