package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// workflowRootCandidates returns a slice of candidates suitable for testing
// the hook-claim-continuation-nudge path: a workflow root that routes to
// route-1 and carries a continuation group so preassignHookContinuationGroup
// will assign siblings.
func workflowRootCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.run_target":         "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// stepBeadCandidates returns candidates whose gc.kind is "task" (a step
// bead), which must NOT trigger the hook-claim-continuation nudge even when
// siblings are assigned. Step beads are routed via gc.routed_to (not
// gc.run_target), so we set that for route matching.
func stepBeadCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "task",
			"gc.routed_to":          "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// buildContinuationNudgeOps returns a hookClaimOps that captures nudge
// enqueue calls. siblingIDs controls what ListContinuation returns.
func buildContinuationNudgeOps(candidates []beads.Bead, siblingIDs []string, enqueued *[]string) hookClaimOps {
	siblings := make([]beads.Bead, 0, len(siblingIDs))
	for _, id := range siblingIDs {
		siblings = append(siblings, beads.Bead{
			ID:     id,
			Status: "open",
			Metadata: map[string]string{
				"gc.kind":               "workflow",
				"gc.run_target":         "route-1",
				"gc.root_bead_id":       "root-1",
				"gc.continuation_group": "group-a",
			},
		})
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			out, _ := json.Marshal(candidates)
			return string(out), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			meta := candidates[0].Metadata
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: meta}, true, nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return siblings, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error {
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
		EnqueueContinuationNudge: func(assignee string) {
			*enqueued = append(*enqueued, assignee)
		},
	}
}

// TestHookClaimWorkflowRootEnqueuesContinuationNudge verifies that
// claiming a workflow root that pre-assigns at least one sibling enqueues a
// hook-claim-continuation nudge for the claiming session name.
func TestHookClaimWorkflowRootEnqueuesContinuationNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), []string{"sib-1", "sib-2"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 1 || enqueued[0] != "gascity/worker/slot-0" {
		t.Fatalf("continuation nudge enqueued for %v, want [gascity/worker/slot-0]", enqueued)
	}
}

// TestHookClaimStepBeadDoesNotEnqueueContinuationNudge verifies that claiming a
// step bead (gc.kind="task") does NOT enqueue a hook-claim-continuation
// nudge even when siblings are pre-assigned. The nudge is only needed for
// self-propelling pool workflow roots; step claims happen while the session
// is already executing and will naturally poll.
func TestHookClaimStepBeadDoesNotEnqueueContinuationNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(stepBeadCandidates(), []string{"sib-1"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued for a step-bead claim, got %v", enqueued)
	}
}

// TestHookClaimExistingAssignmentDoesNotEnqueueContinuationNudge verifies that
// re-finding a workflow root this session already holds does NOT enqueue a
// second nudge. The first claim pre-assigned the continuation siblings and
// nudged once; on the re-find they already carry an assignee, so
// preassignHookContinuationGroup assigns nothing and the nudge gate stays shut.
func TestHookClaimExistingAssignmentDoesNotEnqueueContinuationNudge(t *testing.T) {
	const slot = "gascity/worker/slot-0"
	var enqueued []string
	held := workflowRootCandidates()
	held[0].Status = "in_progress"
	held[0].Assignee = slot
	ops := buildContinuationNudgeOps(held, nil, &enqueued)
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return []beads.Bead{{ID: "sib-1", Status: "open", Assignee: slot, Metadata: held[0].Metadata}}, nil
	}
	ops.AssignContinuation = func(_ context.Context, _ string, _ []string, beadID, _ string) error {
		t.Errorf("a re-find must not re-assign continuation sibling %s", beadID)
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           slot,
		IdentityCandidates: []string{slot},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s", err, stdout.String())
	}
	if result.Reason != "existing_assignment" {
		t.Fatalf("reason = %q, want existing_assignment: the re-find path was not exercised", result.Reason)
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued again on a re-find, got %v", enqueued)
	}
}

// TestHookClaimZeroContinuationDoesNotEnqueueContinuationNudge verifies that claiming a
// workflow root that has NO unassigned siblings does NOT enqueue a nudge.
// An empty continuation means the session has no further work queued and
// should not be immediately propelled.
func TestHookClaimZeroContinuationDoesNotEnqueueContinuationNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), nil /* no siblings */, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued when no siblings assigned, got %v", enqueued)
	}
}

// TestHookContinuationNudgeEnqueueClosesOnlyOpenedHandle exercises the
// production EnqueueContinuationNudge helper against a relocated-shape seam:
// the nudges-class store and the handle the call opened are distinct. The
// helper must queue the nudge under the assignee, resolve the assignee (which
// may be an alias or session bead ID, not the runtime session name) to the
// session bead in the session class (the opened work store at the default
// backend), fence the item to that session, start the poller for the real
// runtime session name unless the transport is ACP, and close only the handle
// it opened — closing the class store would close the storage routes' shared
// engine. Replaces package seams; must stay serial.
func TestHookContinuationNudgeEnqueueClosesOnlyOpenedHandle(t *testing.T) {
	const runtimeName = "rt-slot-0"
	const alias = "gascity/worker/slot-0"
	tests := []struct {
		name        string
		sessionName string
		transport   string
		// assignee returns the claim assignee given the session bead ID.
		assignee    func(id string) string
		wantSession string
		wantPoller  bool
	}{
		{
			name:        "assignee is session name",
			sessionName: alias,
			assignee:    func(string) string { return alias },
			wantSession: alias,
			wantPoller:  true,
		},
		{
			name:        "assignee is session alias",
			sessionName: runtimeName,
			assignee:    func(string) string { return alias },
			wantSession: runtimeName,
			wantPoller:  true,
		},
		{
			name:        "assignee is session bead ID",
			sessionName: runtimeName,
			assignee:    func(id string) string { return id },
			wantSession: runtimeName,
			wantPoller:  true,
		},
		{
			name:        "acp transport skips poller",
			sessionName: runtimeName,
			transport:   "acp",
			assignee:    func(string) string { return alias },
			wantPoller:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile(city.toml): %v", err)
			}
			t.Setenv("GC_CITY", cityDir)
			resetCLIStorageRoutes(t)

			var classCloses, openedCloses, opens int
			classStore := &countingNudgeStore{MemStore: beads.NewMemStore(), closes: &classCloses}
			openedStore := &countingNudgeStore{MemStore: beads.NewMemStore(), closes: &openedCloses}
			meta := map[string]string{
				"session_name":       tc.sessionName,
				"alias":              alias,
				"continuation_epoch": "3",
			}
			if tc.transport != "" {
				meta["transport"] = tc.transport
			}
			sessionBead, err := openedStore.Create(beads.Bead{
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel},
				Metadata: meta,
			})
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			assignee := tc.assignee(sessionBead.ID)
			prevOpen := openOwnedNudgeBeadStore
			openOwnedNudgeBeadStore = func(string) (beads.NudgesStore, beads.Store) {
				opens++
				return beads.NudgesStore{Store: classStore}, openedStore
			}
			t.Cleanup(func() { openOwnedNudgeBeadStore = prevOpen })
			var pollerSessions []string
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, sessionName string) error {
				pollerSessions = append(pollerSessions, sessionName)
				return nil
			}
			t.Cleanup(func() { startNudgePoller = prevPoller })

			hookContinuationNudgeEnqueue(assignee)

			state, err := nudgequeue.LoadState(cityDir)
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			if len(state.Pending) != 1 {
				t.Fatalf("pending = %#v, want exactly one continuation nudge", state.Pending)
			}
			item := state.Pending[0]
			if item.Agent != assignee {
				t.Fatalf("queued nudge Agent = %q, want %q", item.Agent, assignee)
			}
			if item.Source != "hook-claim-continuation" {
				t.Fatalf("queued nudge Source = %q, want hook-claim-continuation", item.Source)
			}
			if item.SessionID != sessionBead.ID || item.ContinuationEpoch != "3" {
				t.Fatalf("fence = (%q, %q), want (%q, 3): the fence must come from the session class", item.SessionID, item.ContinuationEpoch, sessionBead.ID)
			}
			if tc.wantPoller {
				if len(pollerSessions) != 1 || pollerSessions[0] != tc.wantSession {
					t.Fatalf("startNudgePoller sessions = %v, want [%s]", pollerSessions, tc.wantSession)
				}
			} else if len(pollerSessions) != 0 {
				t.Fatalf("startNudgePoller sessions = %v, want none for acp transport", pollerSessions)
			}
			if opens != 1 {
				t.Fatalf("opens = %d, want 1", opens)
			}
			if openedCloses != 1 {
				t.Fatalf("opened handle closes = %d, want 1", openedCloses)
			}
			if classCloses != 0 {
				t.Fatalf("nudges-class store closes = %d, want 0: only the opened handle may be closed", classCloses)
			}
		})
	}
}
