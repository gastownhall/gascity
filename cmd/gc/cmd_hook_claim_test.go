package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHookClaimSessionStoreContextUsesCityScopeAfterRigClaim(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "demo")
	rigBeadsDir := filepath.Join(rigDir, ".beads")

	dir, env, err := hookClaimSessionStoreContext(context.Background(), []string{
		"GC_CITY_PATH=" + cityDir,
		"GC_CITY=" + cityDir,
		"GC_STORE_ROOT=" + rigDir,
		"GC_STORE_SCOPE=rig",
		"GC_RIG=demo",
		"GC_RIG_ROOT=" + rigDir,
		"BEADS_DIR=" + rigBeadsDir,
		"GC_DOLT_HOST=rig-dolt.example",
		"GC_DOLT_PORT=3307",
	})
	if err != nil {
		t.Fatalf("hookClaimSessionStoreContext: %v", err)
	}
	if dir != cityDir {
		t.Fatalf("dir = %q, want city dir %q", dir, cityDir)
	}

	got := envEntriesMap(env)
	for key, want := range map[string]string{
		"GC_CITY_PATH":   cityDir,
		"GC_STORE_ROOT":  cityDir,
		"GC_STORE_SCOPE": "city",
		"BEADS_DIR":      filepath.Join(cityDir, ".beads"),
		"GC_RIG":         "",
		"GC_RIG_ROOT":    "",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if got["GC_DOLT_HOST"] == "rig-dolt.example" || got["GC_DOLT_PORT"] == "3307" {
		t.Fatalf("rig Dolt endpoint leaked into city session store env: %#v", got)
	}
}

func TestHookClaimSessionStoreContextRejectsMissingCityPath(t *testing.T) {
	_, _, err := hookClaimSessionStoreContext(context.Background(), []string{
		"GC_RIG_ROOT=/city/rigs/demo",
		"BEADS_DIR=/city/rigs/demo/.beads",
	})
	if err == nil {
		t.Fatal("hookClaimSessionStoreContext succeeded without a city path")
	}
}

func TestHookRecordSessionPointersUsesCityStoreAfterRigClaim(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "demo")

	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	var capturedDir string
	var capturedEnv map[string]string
	var capturedName string
	var capturedArgs []string
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, env map[string]string) beads.CommandRunner {
		capturedEnv = env
		return func(dir, name string, args ...string) ([]byte, error) {
			capturedDir = dir
			capturedName = name
			capturedArgs = append([]string(nil), args...)
			return nil, nil
		}
	}

	err := hookRecordSessionPointersWithBdStore(
		context.Background(),
		rigDir,
		[]string{
			"GC_CITY_PATH=" + cityDir,
			"GC_STORE_ROOT=" + rigDir,
			"GC_STORE_SCOPE=rig",
			"GC_RIG=demo",
			"GC_RIG_ROOT=" + rigDir,
			"BEADS_DIR=" + filepath.Join(rigDir, ".beads"),
		},
		"worker-1", "session-1", "run-1", "step-1",
	)
	if err != nil {
		t.Fatalf("hookRecordSessionPointersWithBdStore: %v", err)
	}

	if capturedDir != cityDir {
		t.Fatalf("bd dir = %q, want %q", capturedDir, cityDir)
	}
	if capturedEnv["BEADS_DIR"] != filepath.Join(cityDir, ".beads") ||
		capturedEnv["GC_STORE_SCOPE"] != "city" || capturedEnv["GC_RIG_ROOT"] != "" {
		t.Fatalf("bd env did not select city scope: %#v", capturedEnv)
	}
	if capturedName != "bd" || len(capturedArgs) < 3 ||
		!reflect.DeepEqual(capturedArgs[:3], []string{"update", "--json", "session-1"}) {
		t.Fatalf("bd command = %q %#v, want bd update --json session-1", capturedName, capturedArgs)
	}
}

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

// TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt guards the
// widened-routed-tier fix: a routed tier's oldest candidate can be
// is_blocked (e.g. gated on a PR), and the hook must fall through to a
// Ready routed bead behind it rather than idle-exiting on the blocked head.
func TestDoHookClaimSkipsBlockedRoutedHeadAndClaimsReadyBehindIt(t *testing.T) {
	candidates := []beads.Bead{
		{ID: "blocked-head", Status: "open", IsBlocked: boolPtr(true), Metadata: map[string]string{"gc.routed_to": "route-1"}},
		{ID: "ready-behind", Status: "open", Metadata: map[string]string{"gc.routed_to": "route-1"}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimedBead string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedBead = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimedBead != "ready-behind" {
		t.Fatalf("claimedBead = %q, want ready-behind (blocked-head must be skipped)", claimedBead)
	}
}
