package rollout

import "testing"

// TestForTestDefaults proves ForTest with no options yields every gate's
// built-in default.
func TestForTestDefaults(t *testing.T) {
	t.Parallel()
	f := ForTest()
	if f.BeadsConditionalWrites() != Off {
		t.Errorf("default beads = %q, want off", f.BeadsConditionalWrites())
	}
	if !f.FormulaV2() {
		t.Errorf("default formula_v2 = false, want true")
	}
}

// TestForTestIsolationRequire and ...Off run in parallel with OPPOSITE overrides:
// if the seam held any process-scoped mutable state, one would observe the
// other's value. Repeated reads widen the interleave window. Passing under
// -race proves per-instance isolation.
func TestForTestIsolationRequire(t *testing.T) {
	t.Parallel()
	f := ForTest(WithBeadsConditionalWrites(Require), WithFormulaV2(false))
	for i := 0; i < 2000; i++ {
		if f.BeadsConditionalWrites() != Require || f.FormulaV2() {
			t.Fatalf("iter %d: got %q/%v, want require/false — cross-test leakage", i, f.BeadsConditionalWrites(), f.FormulaV2())
		}
	}
}

func TestForTestIsolationOff(t *testing.T) {
	t.Parallel()
	f := ForTest(WithBeadsConditionalWrites(Off), WithFormulaV2(true))
	for i := 0; i < 2000; i++ {
		if f.BeadsConditionalWrites() != Off || !f.FormulaV2() {
			t.Fatalf("iter %d: got %q/%v, want off/true — cross-test leakage", i, f.BeadsConditionalWrites(), f.FormulaV2())
		}
	}
}
