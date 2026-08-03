package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type publishRunMapSpy struct {
	calls  int
	runID  string
	beadID string
	keys   []string
	err    error
}

func (s *publishRunMapSpy) fn(runID, beadID string, keys ...string) error {
	s.calls++
	s.runID = runID
	s.beadID = beadID
	s.keys = append([]string(nil), keys...)
	return s.err
}

func claimOpsForRunMap(beadID string, claimedMeta map[string]string, spy *publishRunMapSpy) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"` + beadID + `","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: claimedMeta}, true, nil
		},
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		PublishRunMap:     spy.fn,
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env: []string{
			"GC_SESSION_NAME=worker-1",
			"GC_SESSION_ID=session-1",
			"BEADS_ACTOR=actor-1",
		},
		JSON: true,
	}
	return ops, opts
}

// TestDoHookClaimPublishesRunMapWithoutSessionBeadMutation pins the v1.3.5
// safety boundary. If session-1 disappears after the claim, a fuzzy bd update
// can otherwise resolve session-10 and corrupt it. Run-map publication retains
// correlation without issuing any post-claim bd mutation.
func TestDoHookClaimPublishesRunMapWithoutSessionBeadMutation(t *testing.T) {
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	var bdCalls int
	collisionMetadata := map[string]string{"sentinel": "unchanged"}
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			bdCalls++
			if len(args) >= 3 && args[0] == "update" && args[2] == "session-1" {
				collisionMetadata["gc.current_run_id"] = "root-safe"
			}
			return nil, nil
		}
	}

	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-safe", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-safe",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if bdCalls != 0 {
		t.Fatalf("post-claim bd mutation calls = %d, want 0", bdCalls)
	}
	if !reflect.DeepEqual(collisionMetadata, map[string]string{"sentinel": "unchanged"}) {
		t.Fatalf("prefix-colliding session metadata = %v, want sentinel only", collisionMetadata)
	}
	if spy.calls != 1 || spy.runID != "root-safe" || spy.beadID != "hw-safe" {
		t.Fatalf("run-map publish = %+v, want one root-safe/hw-safe publish", spy)
	}
	wantKeys := []string{"worker-1", "session-1", "actor-1"}
	if !reflect.DeepEqual(spy.keys, wantKeys) {
		t.Fatalf("run-map keys = %q, want %q", spy.keys, wantKeys)
	}
}

func TestDoHookClaimRunMapUsesBeadIDWithoutRunChain(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-standalone", map[string]string{
		"gc.routed_to": "worker",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.runID != "hw-standalone" {
		t.Fatalf("run-map publish = %+v, want standalone bead ID as run ID", spy)
	}
}

func TestDoHookClaimSkipsRunMapWithoutSessionID(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-nosess", map[string]string{"gc.routed_to": "worker"}, spy)
	opts.Env = []string{"GC_SESSION_NAME=worker-1", "BEADS_ACTOR=actor-1"}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("run-map calls = %d, want 0 without a session bead ID", spy.calls)
	}
}

func TestDoHookClaimRunMapFailureDoesNotFailClaim(t *testing.T) {
	spy := &publishRunMapSpy{err: errors.New("run-map unavailable")}
	ops, opts := claimOpsForRunMap("hw-err", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-err",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want bead hw-err reason claimed", result)
	}
	if !strings.Contains(stderr.String(), "publishing run-map for session session-1") {
		t.Fatalf("stderr missing best-effort run-map diagnostic: %s", stderr.String())
	}
}

func TestDoHookClaimPublishesRunMapOnExistingAssignment(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("unused", nil, spy)
	ops.Runner = func(string, string) (string, error) {
		return `[{"id":"hw-existing","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-existing"}}]`, nil
	}
	ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		t.Fatal("Claim must not run for an existing assignment")
		return beads.Bead{}, false, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.runID != "root-existing" || spy.beadID != "hw-existing" {
		t.Fatalf("run-map publish = %+v, want existing assignment mapping", spy)
	}
}
