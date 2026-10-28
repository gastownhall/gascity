package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// TestReleaseOrphanedPoolAssignments_SweptClaimSurvivesHookHandOver is the
// end-to-end arm of issue #6362's fix: the seat hand-over moves a closed
// row's bead-ID claim to its successor, and the successor's very next
// `gc hook --claim` adopts the bead as existing_assignment — re-stamping the
// durable claim projection (gc.session_id, gc.session_name) onto it in one
// write while gc.claimed_at keeps its original value (write-once), and
// recording the bead in the successor's work_started cell as adopted. The
// base sweep releases the work instead, so the two sessions fight over a
// reopened bead (the bug).
func TestReleaseOrphanedPoolAssignments_SweptClaimSurvivesHookHandOver(t *testing.T) {
	cfg := seatHandoverConfig()
	store := beads.NewMemStore()
	// The successor's work_started write fences on the row's revision, so the
	// store must carry the conditional-writes mode a real open stamps on it.
	if err := beads.StampOpenedStore(store, "MemStore", gate.Auto, false, nil, nil); err != nil {
		t.Fatalf("stamp store: %v", err)
	}
	predecessor := seatRow(t, store, "predecessor", "worker-mc-old", "baker")
	successor, err := store.Create(beads.Bead{
		Title:  "successor worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":               "worker-mc-new",
			"template":                   "worker",
			"state":                      "awake",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "baker",
			"instance_token":             "tok-9",
			"generation":                 "9",
		},
	})
	if err != nil {
		t.Fatalf("create successor session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title:    "work claimed under the predecessor's bead ID",
		Type:     "task",
		Assignee: predecessor.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    "worker",
			beadmeta.SessionIDMetadataKey:   predecessor.ID,
			beadmeta.SessionNameMetadataKey: "worker-mc-old",
			beadmeta.ClaimedAtMetadataKey:   "2026-10-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	closed := "closed"
	if err := store.Update(predecessor.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close predecessor: %v", err)
	}
	// Refresh the snapshot after the writes: the sweep fences on it.
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work: %v", err)
	}

	// Phase 1: the dead-assignee sweep hands the claim to the seat successor
	// instead of reopening the work.
	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, cfg, "",
		sessionInfosFromBeads([]beads.Bead{successor}), []beads.Bead{work},
		nil, nil, nil, nil, nil, nil)
	if len(released) != 0 {
		t.Fatalf("released %v, want none: the closed row's seat has exactly one open successor, so the claim must move to it", released)
	}
	if got, err := store.Get(work.ID); err != nil || got.Assignee != successor.ID {
		t.Fatalf("work after sweep = (assignee %q, err %v), want the successor's bead ID %q", got.Assignee, err, successor.ID)
	}

	// Phase 2: the successor's first hook turn adopts the swept claim.
	env := []string{
		"GC_SESSION_ID=" + successor.ID,
		"GC_SESSION_NAME=worker-mc-new",
		"GC_INSTANCE_TOKEN=tok-9",
		"GC_RUNTIME_EPOCH=9",
		"GC_TEMPLATE=worker",
	}
	opts := hookClaimOptions{
		Assignee:           successor.ID,
		SessionID:          successor.ID,
		IdentityCandidates: []string{successor.ID, "worker-mc-new"},
		RouteTargets:       []string{"worker"},
		Env:                env,
		StartedFence:       hookStartedFenceOf(env),
		JSON:               true,
	}

	// The runner answers from the live store, as the work query does.
	runner := func(string, string) (string, error) {
		fresh, err := store.Get(work.ID)
		if err != nil {
			return "", err
		}
		meta, err := json.Marshal(fresh.Metadata)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`[{"id":%q,"status":%q,"assignee":%q,"metadata":%s}]`,
			fresh.ID, fresh.Status, fresh.Assignee, meta), nil
	}

	var stampPatch map[string]string
	var unrecorded []string
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Error("Claim must not be called: the swept claim is adopted as an existing assignment, not minted")
			return beads.Bead{}, false, nil
		},
		Release: func(context.Context, string, []string, string, string) (bool, error) {
			t.Error("Release must not be called on the adoption path")
			return false, nil
		},
		DrainAck:     func(io.Writer) error { return nil },
		DrainPending: func(string) (bool, error) { return false, nil },
		InputDone: func(context.Context, string, []string, beads.Bead, []string) (string, bool, error) {
			return "", false, nil
		},
		SkipDoneWorkflow: func(context.Context, string, []string, string) error {
			return nil
		},
		EmitClaimRejected:     func(string, string, string) {},
		ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
		ResolveSessionWorkDir: func(string) string { return "" },
		// StampWorkMeta mirrors production: one update writing the patch onto
		// the bead, captured so the patch itself can be asserted.
		StampWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string, patch map[string]string) error {
			stampPatch = make(map[string]string, len(patch))
			for k, v := range patch {
				stampPatch[k] = v
			}
			if len(patch) == 0 {
				return nil
			}
			fresh, err := store.Get(beadID)
			if err != nil {
				return err
			}
			merged := make(map[string]string, len(fresh.Metadata)+len(patch))
			for k, v := range fresh.Metadata {
				merged[k] = v
			}
			for k, v := range patch {
				merged[k] = v
			}
			return store.UpdateIfMatch(beadID, fresh.Revision, beads.UpdateOpts{Metadata: merged})
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
			return store.Get(beadID)
		},
		StoreRef: func(string) string { return "rig:local" },
		RecordWorkStarted: func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
			return recordWorkStarted(sessionFrontDoor(store), sessionID, fence, key, via,
				time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
		},
		ForgetWorkStarted: func(string, hookStartedKey) error { return nil },
		ReadWorkStarted: func(sessionID string) (string, error) {
			row, err := store.Get(sessionID)
			if err != nil {
				return "", err
			}
			return row.Metadata[session.WorkStartedKey], nil
		},
		EmitWorkStartedUnrecorded: func(_, _, beadID string, _ error) {
			unrecorded = append(unrecorded, beadID)
		},
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		StampSessionClaim:        func(string, string) error { return nil },
		EmitClaimReleased:        func(hookClaimReleaseRecord) {},
		PublishRunMap:            func(string, string, ...string) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/rig", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(unrecorded) != 0 {
		t.Fatalf("work started unrecorded for %v, want none: the successor's row is live and fenced; stderr=%s", unrecorded, stderr.String())
	}

	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != work.ID {
		t.Fatalf("result = %+v, want an existing_assignment receipt handing back %s", result, work.ID)
	}
	if result.Assignee != successor.ID {
		t.Fatalf("result assignee = %q, want the successor %q", result.Assignee, successor.ID)
	}

	// The adoption re-stamps the session identity and leaves the
	// write-once claim timestamp alone.
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:   successor.ID,
		beadmeta.SessionNameMetadataKey: "worker-mc-new",
	}
	if !reflect.DeepEqual(stampPatch, want) {
		t.Fatalf("stamp patch = %v, want %v and no gc.claimed_at (it is write-once and already set)", stampPatch, want)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-read work after hook: %v", err)
	}
	if got.Assignee != successor.ID || got.Status != "in_progress" {
		t.Fatalf("work after hook = assignee %q status %q, want %q/in_progress", got.Assignee, got.Status, successor.ID)
	}
	if claimed := got.Metadata[beadmeta.ClaimedAtMetadataKey]; claimed != "2026-10-01T00:00:00Z" {
		t.Fatalf("gc.claimed_at = %q, want the original value: the adoption must not re-stamp it", claimed)
	}
	if sid := got.Metadata[beadmeta.SessionIDMetadataKey]; sid != successor.ID {
		t.Fatalf("gc.session_id = %q, want the successor's bead ID %q", sid, successor.ID)
	}
	if name := got.Metadata[beadmeta.SessionNameMetadataKey]; name != "worker-mc-new" {
		t.Fatalf("gc.session_name = %q, want worker-mc-new", name)
	}

	// The successor's work_started cell records the adopted bead.
	row, err := store.Get(successor.ID)
	if err != nil {
		t.Fatalf("re-read successor row: %v", err)
	}
	cell, err := session.DecodeWorkStarted(row.Metadata[session.WorkStartedKey])
	if err != nil {
		t.Fatalf("decode work_started: %v", err)
	}
	if len(cell.Claims) != 1 || cell.Claims[0].Bead != work.ID ||
		cell.Claims[0].Via != session.WorkStartedViaAdopt || cell.Claims[0].Gen != 9 {
		t.Fatalf("work_started = %+v, want one adopted entry naming %s at generation 9", cell.Claims, work.ID)
	}
}
