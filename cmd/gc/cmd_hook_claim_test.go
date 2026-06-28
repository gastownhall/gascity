package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestDoHookClaimUsesSelectedStoreContextForMutationAndContinuation(t *testing.T) {
	var claimedDir string
	var claimedEnv []string
	var listedDir string
	var listedEnv []string
	var assignedDir string
	var assignedEnv []string
	var assignedBead string

	storeDir := "rig-store"
	storeEnv := []string{"BEADS_DIR=rig-store", "GC_RIG_ROOT=rig-root"}
	candidates := []beads.Bead{{
		ID:       "bead-1",
		Status:   "open",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.run_target": "route-1", "gc.root_bead_id": "root-1", "gc.continuation_group": "group-a"},
	}}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedDir = dir
			claimedEnv = append([]string(nil), env...)
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: candidates[0].Metadata}, true, nil
		},
		ListContinuation: func(_ context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			listedDir = dir
			listedEnv = append([]string(nil), env...)
			if rootID != "root-1" || group != "group-a" {
				t.Fatalf("continuation lookup = (%q, %q), want (root-1, group-a)", rootID, group)
			}
			return []beads.Bead{{ID: "sib-1", Status: "open", Metadata: candidates[0].Metadata}}, nil
		},
		AssignContinuation: func(_ context.Context, dir string, env []string, beadID, assignee string) error {
			assignedDir = dir
			assignedEnv = append([]string(nil), env...)
			assignedBead = beadID
			if assignee != "worker-1" {
				t.Fatalf("assignee = %q, want worker-1", assignee)
			}
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", storeDir, hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		Env:                storeEnv,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedDir != storeDir {
		t.Fatalf("claimedDir = %q, want %q", claimedDir, storeDir)
	}
	if listedDir != storeDir {
		t.Fatalf("listedDir = %q, want %q", listedDir, storeDir)
	}
	if assignedDir != storeDir {
		t.Fatalf("assignedDir = %q, want %q", assignedDir, storeDir)
	}
	if !reflect.DeepEqual(claimedEnv, storeEnv) {
		t.Fatalf("claimedEnv = %#v, want %#v", claimedEnv, storeEnv)
	}
	if !reflect.DeepEqual(listedEnv, storeEnv) {
		t.Fatalf("listedEnv = %#v, want %#v", listedEnv, storeEnv)
	}
	if !reflect.DeepEqual(assignedEnv, storeEnv) {
		t.Fatalf("assignedEnv = %#v, want %#v", assignedEnv, storeEnv)
	}
	if assignedBead != "sib-1" {
		t.Fatalf("assignedBead = %q, want sib-1", assignedBead)
	}
}
