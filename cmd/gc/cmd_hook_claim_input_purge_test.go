package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// purgeProbeStore wraps a store so a test can inject read faults and count
// reads on the legs a purge proof runs over.
type purgeProbeStore struct {
	beads.Store
	failGetIDs map[string]bool
	gets       map[string]int
}

func newPurgeProbeStore(store beads.Store) *purgeProbeStore {
	return &purgeProbeStore{Store: store, failGetIDs: map[string]bool{}, gets: map[string]int{}}
}

func (s *purgeProbeStore) Get(id string) (beads.Bead, error) {
	s.gets[id]++
	if s.failGetIDs[id] {
		return beads.Bead{}, fmt.Errorf("store unavailable for %s", id)
	}
	return s.Store.Get(id)
}

// A workflow whose input convoy was purged is provable at claim time: the
// convoy row is absent over a complete frame, so nothing legitimate can still
// resolve it (#8729).
func TestWorkflowInputPurgedProvesAbsentConvoy(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged(root): %v", err)
	}
	if !verdict.Purged {
		t.Fatalf("purged = false (%s), want proved over the one-store frame", verdict.Diagnosis)
	}
	if verdict.RootID != root.ID {
		t.Fatalf("rootID = %q, want %q", verdict.RootID, root.ID)
	}

	// The same verdict must hold when the candidate is a step, not the root.
	step := beads.Bead{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	}
	verdict, err = workflowInputPurged(beads.HandlesFor(store), step)
	if err != nil {
		t.Fatalf("workflowInputPurged(step): %v", err)
	}
	if !verdict.Purged || verdict.RootID != root.ID {
		t.Fatalf("step verdict = %+v, want purged with root %s", verdict, root.ID)
	}
}

// A convoy that is still present is not this probe's shape: the done-check
// owns present convoys, and the target-absence proof (a later arm of #8729)
// has its own evidence standard. No proof, no error.
func TestWorkflowInputPurgedUnprovedWhileConvoyPresent(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if verdict.Purged {
		t.Fatalf("purged = true with the convoy row present, want unproved (%s)", verdict.Diagnosis)
	}
}

// A candidate with no workflow ancestry is never the probe's shape.
func TestWorkflowInputPurgedUnprovedWithoutWorkflow(t *testing.T) {
	store := beads.NewMemStore()
	plain, err := store.Create(beads.Bead{Title: "plain work"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	verdict, err := workflowInputPurged(beads.HandlesFor(store), plain)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if verdict.Purged {
		t.Fatal("purged = true for a bead with no workflow ancestry, want false")
	}
}

// The frame is the proof's completeness contract: over it, every evidence id
// must be absent from EVERY leg, live.
func TestInputPurgeFrameProvesAbsence(t *testing.T) {
	legA, legB := newPurgeProbeStore(beads.NewMemStore()), newPurgeProbeStore(beads.NewMemStore())
	frame := inputPurgeFrameForReaders(beads.HandlesFor(legA).Live, beads.HandlesFor(legB).Live)

	proved, diag, err := frame.proveAbsent([]string{"gc-1", "gc-2"})
	if err != nil {
		t.Fatalf("proveAbsent: %v", err)
	}
	if !proved {
		t.Fatalf("proved = false (%s), want absence over both legs", diag)
	}
	if legA.gets["gc-1"] != 1 || legB.gets["gc-1"] != 1 {
		t.Fatalf("reads = a:%d b:%d, want each leg read once per evidence id", legA.gets["gc-1"], legB.gets["gc-1"])
	}
}

// A row any leg still holds blocks the proof: absence must hold everywhere.
func TestInputPurgeFrameFoundRowBlocksProof(t *testing.T) {
	legA, legB := beads.NewMemStore(), beads.NewMemStore()
	if _, err := legB.Create(beads.Bead{Title: "the row"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	frame := inputPurgeFrameForReaders(beads.HandlesFor(legA).Live, beads.HandlesFor(legB).Live)

	proved, diag, err := frame.proveAbsent([]string{"gc-1"})
	if err != nil {
		t.Fatalf("proveAbsent: %v", err)
	}
	if proved {
		t.Fatal("proved = true while a leg holds the row, want blocked")
	}
	if diag == "" {
		t.Fatal("diagnosis empty for a blocked proof, want the reason")
	}
}

// A leg that cannot answer blocks the proof: an unreadable store is not
// absence (#8729's evidence standard).
func TestInputPurgeFrameLegErrorBlocksProof(t *testing.T) {
	legA := newPurgeProbeStore(beads.NewMemStore())
	legA.failGetIDs["gc-1"] = true
	frame := inputPurgeFrameForReaders(beads.HandlesFor(legA).Live)

	if _, _, err := frame.proveAbsent([]string{"gc-1"}); err == nil {
		t.Fatal("proveAbsent err = nil on a failing leg, want the read error")
	}
}

// The city frame refuses the shapes absence cannot be proved over: a refused
// split and a prefix fault both degrade the frame itself, while a complete
// topology grounds one live reader per leg.
func TestInputPurgeFrameRefusesIncompleteCityTopology(t *testing.T) {
	if err := inputPurgeFrameRefusal(errors.New("storage: split not converged"), nil); err == nil {
		t.Fatal("refused topology grounded a frame, want refusal")
	}

	faulted := storeref.Topology{
		Work: storeref.Leg{Ref: "city:test", Store: beads.NewMemStore()},
		Rigs: []storeref.Leg{{
			Ref:    "rig:alpha",
			Store:  prefixDeclaringStore{Store: beads.NewMemStore(), prefix: "ga-9-"},
			Prefix: "ga-7-",
		}},
	}
	if err := inputPurgeFrameRefusal(nil, faulted.PrefixFaults()); err == nil {
		t.Fatal("prefix-faulted topology grounded a frame, want refusal")
	}

	complete := storeref.Topology{
		Work: storeref.Leg{Ref: "city:test", Store: beads.NewMemStore()},
		Rigs: []storeref.Leg{{Ref: "rig:alpha", Store: beads.NewMemStore()}},
	}
	if err := inputPurgeFrameRefusal(complete.Refused, complete.PrefixFaults()); err != nil {
		t.Fatalf("complete topology refused: %v", err)
	}
	readers := []beads.LiveReader{beads.HandlesFor(complete.Work.Store).Live}
	for _, leg := range complete.Rigs {
		readers = append(readers, beads.HandlesFor(leg.Store).Live)
	}
	if frame := inputPurgeFrameForReaders(readers...); len(frame.legs) != 2 {
		t.Fatalf("frame legs = %d, want work + rig", len(frame.legs))
	}
}

// The purge disposition is the one a seat would write after burning its claim
// cycle, applied once: root and non-terminal steps close as
// fail/hard/input_purged, and a terminal step keeps its own disposition.
func TestRetirePurgedWorkflowClosesRootAndOpenSteps(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	inFlight, err := store.Create(beads.Bead{
		Title:    "in-flight step",
		Status:   "in_progress",
		Assignee: "worker-9",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create in-flight step: %v", err)
	}
	done, err := store.Create(beads.Bead{
		Title:    "done step",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create done step: %v", err)
	}
	if err := store.Close(done.ID); err != nil {
		t.Fatalf("close done step: %v", err)
	}
	if err := store.SetMetadata(done.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("stamp done step outcome: %v", err)
	}

	if err := retirePurgedWorkflow(beads.HandlesFor(store), root.ID); err != nil {
		t.Fatalf("retirePurgedWorkflow: %v", err)
	}

	for _, id := range []string{root.ID, step.ID, inFlight.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
			t.Errorf("%s gc.outcome = %q, want fail", id, got.Metadata[beadmeta.OutcomeMetadataKey])
		}
		if got.Metadata[beadmeta.FailureClassMetadataKey] != beadmeta.FailureClassHard {
			t.Errorf("%s gc.failure_class = %q, want hard", id, got.Metadata[beadmeta.FailureClassMetadataKey])
		}
		if got.Metadata[beadmeta.FailureReasonMetadataKey] != beadmeta.FailureReasonInputPurged {
			t.Errorf("%s gc.failure_reason = %q, want input_purged", id, got.Metadata[beadmeta.FailureReasonMetadataKey])
		}
		if got.Metadata["close_reason"] != hookRetirePurgedWorkflowCloseReason {
			t.Errorf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], hookRetirePurgedWorkflowCloseReason)
		}
	}

	kept, err := store.Get(done.ID)
	if err != nil {
		t.Fatalf("get done step: %v", err)
	}
	if kept.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Errorf("terminal step gc.outcome = %q, want its own pass disposition retained", kept.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if kept.Metadata[beadmeta.FailureReasonMetadataKey] == beadmeta.FailureReasonInputPurged {
		t.Error("terminal step was re-stamped by purge retirement, want untouched")
	}
}

// End to end over the claim filter: a purged-input workflow's step is retired
// instead of served, while an unrelated candidate in the same query is still
// claimed — retiring one workflow must not idle the seat (#8729).
func TestDoHookClaimRetiresWorkflowWhoseInputConvoyWasPurged(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	other, err := store.Create(beads.Bead{
		Title:    "unrelated work",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	})
	if err != nil {
		t.Fatalf("create unrelated work: %v", err)
	}
	output, err := json.Marshal([]beads.Bead{step, other})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	claimedID := ""
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			claimedID = beadID
			return beads.Bead{ID: beadID}, true, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(store, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(store, rootID)
		},
		WorkflowStore: func(_ context.Context, _ string, _ []string, _, _ string) beads.Store { return store },
		DrainAck:      func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if claimedID != other.ID {
		t.Errorf("claimed = %q, want %q (the purged workflow's step must not be served; stdout=%q stderr=%q)", claimedID, other.ID, stdout.String(), stderr.String())
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("%s status = %q, want closed: purged-input workflow left in the ready pool", id, got.Status)
		}
	}
}

// The per-seat treadmill shape for a purged input: the seat already holds an
// in_progress step of a workflow whose input was purged. The hook must retire
// the workflow instead of handing the step back as existing_assignment.
func TestDoHookClaimRetiresPurgedWorkflowHeldAssignment(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	if err := store.Update(step.ID, beads.UpdateOpts{Status: strPtr("in_progress"), Assignee: strPtr("worker-1")}); err != nil {
		t.Fatalf("hold step: %v", err)
	}
	output, err := json.Marshal([]beads.Bead{step})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("unexpected claim of %s", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(store, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(store, rootID)
		},
		WorkflowStore: func(_ context.Context, _ string, _ []string, _, _ string) beads.Store { return store },
		DrainAck:      func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if stdout := stdout.String(); !containsJSONField(stdout, "reason", "no_work") {
		t.Errorf("stdout = %q, want the no-work drain after retiring the held step", stdout)
	}
	got, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("held step status = %q, want closed with the purged workflow", got.Status)
	}
}

// containsJSONField reports whether the hook's JSON output line carries
// key:value, tolerating the surrounding fields of the result envelope.
func containsJSONField(out, key, value string) bool {
	return bytes.Contains([]byte(out), []byte(`"`+key+`":"`+value+`"`))
}
