package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHaltClaimedHookBeadOnlyConsultsTheSeamWithTheSwitchOn(t *testing.T) {
	calls := 0
	ops := hookClaimOps{HaltClaimed: func(context.Context, string, []string, beads.Bead, string) (string, bool, error) {
		calls++
		return "gc-upstream", true, nil
	}}
	claimed := beads.Bead{ID: "gc-step", Assignee: "worker"}

	var stderr bytes.Buffer
	if haltClaimedHookBead(context.Background(), claimed, hookClaimOptions{}, ops, "", &stderr) {
		t.Fatal("switch off: the claimed bead must not be halted")
	}
	if calls != 0 {
		t.Fatalf("switch off: HaltClaimed called %d times, want 0", calls)
	}

	if !haltClaimedHookBead(context.Background(), claimed, hookClaimOptions{FailHalts: true}, ops, "", &stderr) {
		t.Fatal("switch on: the claimed bead must be halted")
	}
	if !strings.Contains(stderr.String(), "gc-step halted: it needs gc-upstream") {
		t.Fatalf("stderr = %q, want the halt named", stderr.String())
	}
}

func TestHaltClaimedHookBeadLeavesTheClaimOnError(t *testing.T) {
	ops := hookClaimOps{HaltClaimed: func(context.Context, string, []string, beads.Bead, string) (string, bool, error) {
		return "", false, context.DeadlineExceeded
	}}
	var stderr bytes.Buffer
	if haltClaimedHookBead(context.Background(), beads.Bead{ID: "gc-step"}, hookClaimOptions{FailHalts: true}, ops, "", &stderr) {
		t.Fatal("a failed check must leave the claim standing")
	}
}

func TestHookHaltClaimedWithBdStoreIgnoresABeadOutsideAWorkflow(t *testing.T) {
	haltedBy, halted, err := hookHaltClaimedWithBdStore(context.Background(), t.TempDir(), nil, beads.Bead{ID: "gc-loose"}, "worker")
	if err != nil || halted || haltedBy != "" {
		t.Fatalf("hookHaltClaimedWithBdStore = %q %v %v, want untouched", haltedBy, halted, err)
	}
}
