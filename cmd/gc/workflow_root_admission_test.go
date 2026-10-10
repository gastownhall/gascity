package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestWorkflowRootAdmissionAtClaimBoundary(t *testing.T) {
	root := beads.Bead{ID: "root", Status: "open", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.workflow_expanded": "true", "gc.routed_to": "worker",
	}}
	child := beads.Bead{ID: "step", Status: "open", Metadata: map[string]string{
		"gc.root_bead_id": "root", "gc.routed_to": "worker",
	}}
	opts := hookClaimOptions{Assignee: "second", IdentityCandidates: []string{"second"}, RouteTargets: []string{"worker"}}
	var attempts []string
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			attempts = append(attempts, id)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: "first"}, false, nil
		},
		EmitClaimRejected: func(string, string, string) {},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstEligibleHookCandidate([]beads.Bead{child, root}, opts, ops, "unused", &stdout, &stderr)
	if result.terminal || len(attempts) != 1 || attempts[0] != "step" {
		t.Fatalf("lost child claim fell through to root: result=%+v attempts=%v", result, attempts)
	}
	if demandRowServable(root) || hookCandidateVisible(root, opts.IdentityCandidates, opts.RouteTargets) {
		t.Fatal("expanded root is still fresh demand or visible work")
	}
	root.Assignee = "second"
	root.Status = "in_progress"
	if !hookCandidateVisible(root, opts.IdentityCandidates, opts.RouteTargets) {
		t.Fatal("existing owner lost its anchor")
	}
	if _, _, ok := hookClaimExistingAssignment([]beads.Bead{root}, opts); !ok {
		t.Fatal("existing assignment must remain adoptable")
	}
	root.Assignee = "first"
	if hookCandidateReclaimEligible(root, opts.RouteTargets, time.Now()) {
		t.Fatal("stale-owner reclaim bypasses root admission")
	}
	root.Assignee = ""
	delete(root.Metadata, "gc.workflow_expanded")
	root.Metadata[beadmeta.NativeStepDependenciesMetadataKey] = "[]"
	if !hookCandidateClaimable(root, opts.RouteTargets, time.Now()) || !demandRowServable(root) {
		t.Fatal("root-only launch must remain claimable and count as demand")
	}
}

func TestTopologyInfrastructureRefusedAtHookAdmissionBoundaries(t *testing.T) {
	opts := hookClaimOptions{
		Assignee:           "worker-session",
		IdentityCandidates: []string{"worker-session"},
		RouteTargets:       []string{"worker"},
	}
	cases := []struct {
		name     string
		metadata map[string]string
	}{
		{"formula spec", map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindSpec,
			beadmeta.RoutedToMetadataKey: "worker",
		}},
		{"workflow scope", map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindScope,
			beadmeta.RoutedToMetadataKey: "worker",
		}},
		{"unstamped graph latch", map[string]string{
			beadmeta.KindMetadataKey:                   beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey:        beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:               "worker",
			beadmeta.NativeStepDependenciesMetadataKey: `["workflow-finalize"]`,
		}},
		{"legacy graph latch", map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.RoutedToMetadataKey:        "worker",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := beads.Bead{ID: "candidate", Status: "open", Metadata: tc.metadata}
			if hookCandidateClaimable(candidate, opts.RouteTargets, time.Now()) {
				t.Fatal("fresh claim admitted topology infrastructure")
			}
			if hookCandidateVisible(candidate, opts.IdentityCandidates, opts.RouteTargets) {
				t.Fatal("visibility admitted topology infrastructure")
			}
			candidate.Assignee = "stale-session"
			if hookCandidateReclaimEligible(candidate, opts.RouteTargets, time.Now()) {
				t.Fatal("stale reclaim admitted topology infrastructure")
			}
		})
	}

	for _, kind := range []string{beadmeta.KindSpec, beadmeta.KindScope} {
		candidate := beads.Bead{ID: kind, Status: "in_progress", Assignee: opts.Assignee, Metadata: map[string]string{
			beadmeta.KindMetadataKey: kind,
		}}
		if hookCandidateVisible(candidate, opts.IdentityCandidates, opts.RouteTargets) {
			t.Fatalf("owned %s remained visible", kind)
		}
		if _, _, ok := hookClaimExistingAssignment([]beads.Bead{candidate}, opts); ok {
			t.Fatalf("owned %s remained adoptable", kind)
		}
	}
}
