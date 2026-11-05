package main

import "testing"

// TestSplitStrictGatingDemoFailsUnderTheGate is a deliberate, temporary
// failure (ga-ylpbsy.94.2): it fails only under GC_TEST_SPLIT_STRICT=1, so
// the PR that makes the split-strict lane gating can show the lane's red
// verdict failing the required `bazel test (side-by-side)` check. The next
// commit reverts it.
func TestSplitStrictGatingDemoFailsUnderTheGate(t *testing.T) {
	if splitStrictSeam != nil {
		t.Fatal("deliberate split-strict-only failure: the gating demonstration for ga-ylpbsy.94.2")
	}
}
