package config

import (
	"strings"
	"testing"
)

// TestDefaultRecoveryHooksSurfaceFailures pins the recovery-diagnostic contract
// on the DEFAULT on_death/on_boot hooks. The exact generated shell is locked
// byte-for-byte by the golden fixtures (TestWorkQueryGolden); this test asserts
// the three load-bearing properties by name so a refactor that keeps the golden
// superficially plausible but breaks the contract still fails:
//
//   - the hook emits the RecoveryHookMarker the controller callers filter on;
//   - a failed bd write's stderr is CAPTURED (`2>&1 >/dev/null`), not discarded;
//   - the capture rides an `if ! err=$(...)` guard so the pipeline still exits 0
//     even on failure — otherwise shellCommand's cmd.Output() discards the
//     diagnostic on its error return and the whole feature silently regresses.
//
// It does not exec the shell (that would add a tracked subprocess call site);
// the golden fixtures are the executable-shape pin.
func TestDefaultRecoveryHooksSurfaceFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"on_death", (&Agent{Name: "worker"}).EffectiveOnDeathForBeads(BeadsConfig{})},
		{"on_boot", (&Agent{Name: "worker"}).EffectiveOnBootForBeads(BeadsConfig{})},
	} {
		if !strings.Contains(tc.script, RecoveryHookMarker) {
			t.Errorf("%s hook does not emit the %q recovery marker:\n%s", tc.name, RecoveryHookMarker, tc.script)
		}
		if !strings.Contains(tc.script, "2>&1 >/dev/null") {
			t.Errorf("%s hook does not capture a failed bd write's stderr (want `2>&1 >/dev/null`):\n%s", tc.name, tc.script)
		}
		if !strings.Contains(tc.script, "if ! err=$(") {
			t.Errorf("%s hook does not use the exit-0-preserving `if ! err=$(...)` capture guard:\n%s", tc.name, tc.script)
		}
	}
}
