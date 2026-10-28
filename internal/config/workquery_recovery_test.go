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
		{"on_death", (&Agent{Name: "worker"}).EffectiveOnDeathFor(QueryTopology{})},
		{"on_boot", (&Agent{Name: "worker"}).EffectiveOnBootFor(QueryTopology{})},
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

// TestRecoveryHooksReportWhetherTheyAreGenerated pins OnDeathIsGenerated and
// OnBootIsGenerated to the resolver behind EffectiveOnDeath and
// EffectiveOnBoot: generated exactly when the agent sets no override.
func TestRecoveryHooksReportWhetherTheyAreGenerated(t *testing.T) {
	generated := &Agent{Name: "worker"}
	if !generated.OnDeathIsGenerated() || !generated.OnBootIsGenerated() {
		t.Fatal("an agent with no on_death or on_boot reports an override")
	}
	if generated.EffectiveOnDeath() != buildOnDeath(generated, QueryTopology{}) || generated.EffectiveOnBoot() != buildOnBoot(generated, QueryTopology{}) {
		t.Fatal("an agent with no override does not resolve to the generated hooks")
	}

	custom := &Agent{Name: "worker", OnDeath: "Write-Output death", OnBoot: "Write-Output boot"}
	if custom.OnDeathIsGenerated() || custom.OnBootIsGenerated() {
		t.Fatal("an agent with overrides reports generated hooks")
	}
	if custom.EffectiveOnDeath() != custom.OnDeath || custom.EffectiveOnBoot() != custom.OnBoot {
		t.Fatal("overrides do not resolve verbatim")
	}
}
