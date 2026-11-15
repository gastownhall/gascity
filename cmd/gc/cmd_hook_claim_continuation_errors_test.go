package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// These cases drive the production enqueue after doHookClaim delivers its
// result. Claim persistence uses the existing hook seams; config loading,
// city resolution and queue persistence are real. Stay serial.
func TestHookClaimContinuationFailureWarnsWithoutLosingDeliveredWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault string
		cause string
	}{
		{name: "city cannot resolve", fault: "city", cause: "not-a-city"},
		{name: "config cannot load", fault: "config", cause: "missing-fragment.toml"},
		{name: "queue cannot persist", fault: "queue", cause: "state.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			cityTOML := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n"
			if tc.fault == "config" {
				cityTOML = "include = [\"missing-fragment.toml\"]\n" + cityTOML
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.fault == "queue" {
				// A directory where the queue file belongs fails under root too.
				if err := os.MkdirAll(nudgequeue.StatePath(cityDir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fault == "city" {
				cityDir = filepath.Join(cityDir, "not-a-city")
			}
			prevCity, prevRig := cityFlag, rigFlag
			cityFlag, rigFlag = cityDir, ""
			t.Cleanup(func() { cityFlag, rigFlag = prevCity, prevRig })
			resetCLIStorageRoutes(t)

			store := beads.NewMemStore()
			prevOpen, prevPoller := openOwnedNudgeBeadStore, startNudgePoller
			t.Cleanup(func() { openOwnedNudgeBeadStore, startNudgePoller = prevOpen, prevPoller })
			openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
				return beads.NudgesStore{Store: store}, store
			}
			pollers := 0
			startNudgePoller = func(_, _, _ string) error {
				pollers++
				return nil
			}

			var unused []string
			ops := buildContinuationNudgeOps(workflowRootCandidates(), []string{"sib-1"}, &unused)
			ops.StampWorkMeta = noopStampWorkMeta
			ops.StampSessionClaim = noopStampSessionClaim
			ops.PublishRunMap = noopPublishRunMap
			ops.EnqueueContinuationNudge = hookContinuationNudgeEnqueue
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", cityDir, hookClaimOptions{
				Assignee:           "worker-session",
				IdentityCandidates: []string{"worker-session"},
				RouteTargets:       []string{"route-1"},
				JSON:               true,
			}, ops, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("claim exit = %d, want delivered work despite nudge failure; stderr=%s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("claim output is not JSON: %v; stdout=%s", err, stdout.String())
			}
			if !result.OK || result.Action != "work" || result.BeadID != "root-1" || len(result.ContinuationAssigned) != 1 {
				t.Errorf("delivered result = %+v, want root-1 with its assigned continuation", result)
			}
			warning := stderr.String()
			if !strings.Contains(warning, "continuation") || !strings.Contains(warning, "root-1") || !strings.Contains(warning, tc.cause) {
				t.Errorf("stderr = %q, want a continuation warning for root-1 preserving %q", warning, tc.cause)
			}
			if strings.Count(strings.ToLower(warning), "warning:") != 1 {
				t.Errorf("stderr = %q, want one warning for the failed continuation", warning)
			}
			if pollers != 0 {
				t.Errorf("started %d pollers after a failed continuation, want none", pollers)
			}
			if tc.fault != "queue" {
				state, err := nudgequeue.LoadState(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(state.Pending) != 0 {
					t.Errorf("queued %d nudges after city/config failure, want none", len(state.Pending))
				}
			}
		})
	}
}
