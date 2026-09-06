package main

import (
	"testing"
	"time"
)

// TestNativeStorageOpenContextUsesFleetLoadBudget locks every cmd/gc fixture
// that opens native Dolt storage directly to one shared budget instead of
// each call site hardcoding its own. The prior per-call-site 15s budget
// (cmd_bd_test.go, cmd_mail_test.go) was sized for an unloaded host and
// timed out opening beads.OpenNativeStorage under make test-local-full-parallel's
// documented 40-job Dolt contention (ga-227zz7).
func TestNativeStorageOpenContextUsesFleetLoadBudget(t *testing.T) {
	ctx, cancel := nativeStorageOpenContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("nativeStorageOpenContext: context carries no deadline")
	}
	remaining := time.Until(deadline)

	const oldBudget = 15 * time.Second
	if remaining <= oldBudget {
		t.Fatalf("nativeStorageOpenContext budget = %s, want > %s (the old hardcoded per-call-site budget that flaked under fleet load)", remaining, oldBudget)
	}

	const tolerance = 2 * time.Second
	if diff := remaining - nativeStorageOpenTimeout; diff > tolerance || diff < -tolerance {
		t.Fatalf("nativeStorageOpenContext budget = %s, want ~%s (nativeStorageOpenTimeout)", remaining, nativeStorageOpenTimeout)
	}
}
