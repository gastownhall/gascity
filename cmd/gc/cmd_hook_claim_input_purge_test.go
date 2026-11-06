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
	"github.com/gastownhall/gascity/internal/convoy"
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

// soleTrackedTarget purges seedDoneWorkflow's single tracked work bead and
// returns its id: the convoy is left holding a dangling tracks edge.
func soleTrackedTarget(t *testing.T, store *beads.MemStore, root beads.Bead) string {
	t.Helper()
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	members, err := convoy.Members(store, convoyID, true)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %v, %v; want exactly one tracked item", members, err)
	}
	target := members[0].ID
	if err := store.Delete(target); err != nil {
		t.Fatalf("purge tracked target: %v", err)
	}
	return target
}

// A convoy whose every tracked target was purged is provable at claim time:
// the dangling tracks edges are the evidence, and the workflow retires
// instead of serving its steps (#8729).
func TestWorkflowInputPurgedProvesAbsentTrackedTargets(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	target := soleTrackedTarget(t, store, root)

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if !verdict.Purged || verdict.RootID != root.ID {
		t.Fatalf("verdict = %+v, want purged with root %s (the tracked target %s is gone)", verdict, root.ID, target)
	}
}

// A purge can cascade the tracking edge away with the bead; the target stamps
// on the convoy are then the only record of what it tracked. A memberless
// convoy whose recorded targets are all absent still proves the purge.
func TestWorkflowInputPurgedProvesRecordedMemberlessConvoy(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	target := soleTrackedTarget(t, store, root)
	if err := convoy.UntrackItem(store, convoyID, target); err != nil {
		t.Fatalf("untrack purged target: %v", err)
	}
	if err := store.SetMetadata(convoyID, beadmeta.InputTargetIDMetadataKey, target); err != nil {
		t.Fatalf("stamp recorded target: %v", err)
	}

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if !verdict.Purged {
		t.Fatalf("purged = false (%s), want the recorded target to prove the purge", verdict.Diagnosis)
	}
}

// One surviving member blocks the proof: a purge is all-or-nothing over the
// tracked work, and a present member is the done-check's territory.
func TestWorkflowInputPurgedUnprovedWhileAnyMemberPresent(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	soleTrackedTarget(t, store, root) // purge the original member first
	second, err := store.Create(beads.Bead{Title: "second work"})
	if err != nil {
		t.Fatalf("create second member: %v", err)
	}
	if err := convoy.TrackItem(store, convoyID, second.ID); err != nil {
		t.Fatalf("track second member: %v", err)
	}

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if verdict.Purged {
		t.Fatal("purged = true while a tracked member survives, want unproved")
	}
}

// A convoy that tracks nothing and records no target has no evidence: no
// proof, and the candidate serves on exactly as before.
func TestWorkflowInputPurgedUnprovedWithoutTrackedWork(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	target := soleTrackedTarget(t, store, root)
	if err := convoy.UntrackItem(store, convoyID, target); err != nil {
		t.Fatalf("untrack: %v", err)
	}

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if verdict.Purged {
		t.Fatal("purged = true for a convoy with no tracked work, want unproved")
	}
}

// A root whose input names a row that is not a convoy is a corrupt link: it
// proves nothing either way.
func TestWorkflowInputPurgedUnprovedWhenInputIsNotAConvoy(t *testing.T) {
	store := beads.NewMemStore()
	plain, err := store.Create(beads.Bead{Title: "not a convoy", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	root, err := store.Create(beads.Bead{Title: "mol", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		beadmeta.InputConvoyIDMetadataKey: plain.ID,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}

	verdict, err := workflowInputPurged(beads.HandlesFor(store), root)
	if err != nil {
		t.Fatalf("workflowInputPurged: %v", err)
	}
	if verdict.Purged {
		t.Fatal("purged = true for a non-convoy input row, want unproved")
	}
}

// An evidence read that fails is a failed proof, never absence: the probe
// errors rather than guessing, and the caller fails open (#8729).
func TestWorkflowInputPurgedErrorsOnFailingEvidenceRead(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open")
	probe := newPurgeProbeStore(store)
	probe.failGetIDs[root.Metadata[beadmeta.InputConvoyIDMetadataKey]] = true

	if _, err := workflowInputPurged(beads.HandlesFor(probe), root); err == nil {
		t.Fatal("workflowInputPurged err = nil on a failing evidence read, want the read error")
	}
}

// A probe that cannot run is a diagnosed failed proof, not a purge: the
// candidate is served untouched, the retirement never fires, and the failure
// lands on stderr for the operator.
func TestDoHookClaimServesWhenPurgeProbeErrors(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	output, err := json.Marshal([]beads.Bead{step})
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
		InputPurged: func(_ context.Context, _ string, _ []string, _ beads.Bead) (hookInputPurgeVerdict, error) {
			return hookInputPurgeVerdict{}, errors.New("frame unavailable")
		},
		RetirePurgedWorkflow: func(_ context.Context, _ string, _ []string, _ string) error {
			t.Fatal("RetirePurgedWorkflow called after the probe errored")
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if claimedID != step.ID {
		t.Errorf("claimed = %q, want %q: a failed proof must serve the candidate (stdout=%q)", claimedID, step.ID, stdout.String())
	}
	if !contains(stderr.String(), "probing whether") {
		t.Errorf("stderr = %q, want the failed proof diagnosed", stderr.String())
	}
	for _, id := range []string{root.ID, step.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != "open" {
			t.Errorf("%s status = %q, want open: a failed proof writes nothing", id, got.Status)
		}
	}
}

// A retirement that does not land leaves the workflow untouched and the
// candidate served: the close is the proof's consequence, never a leap.
func TestDoHookClaimServesWhenPurgeRetirementFails(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "open")
	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	output, err := json.Marshal([]beads.Bead{step})
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
		InputPurged: func(_ context.Context, _ string, _ []string, candidate beads.Bead) (hookInputPurgeVerdict, error) {
			return workflowInputPurged(beads.HandlesFor(store), candidate)
		},
		RetirePurgedWorkflow: func(_ context.Context, _ string, _ []string, _ string) error {
			return errors.New("store unavailable for close")
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if claimedID != step.ID {
		t.Errorf("claimed = %q, want %q: a failed retirement must serve the candidate (stdout=%q)", claimedID, step.ID, stdout.String())
	}
	got, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("step status = %q, want open: a failed retirement writes nothing", got.Status)
	}
	if !contains(stderr.String(), "retiring workflow") {
		t.Errorf("stderr = %q, want the failed retirement diagnosed", stderr.String())
	}
}

// An unproved shape rides the default probe wiring end to end: a workflow
// whose input still resolves is not the purge probe's business, and its step
// claims normally.
func TestDoHookClaimServesWhenPurgeUnproved(t *testing.T) {
	store, _, step := seedDoneWorkflow(t, "open") // member present: unproved
	output, err := json.Marshal([]beads.Bead{step})
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
		WorkflowStore: func(_ context.Context, _ string, _ []string, _, _ string) beads.Store { return store },
		DrainAck:      func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if claimedID != step.ID {
		t.Errorf("claimed = %q, want %q: an unproved purge must serve the candidate (stdout=%q stderr=%q)", claimedID, step.ID, stdout.String(), stderr.String())
	}
}

// A resolved input is the every-day case: the probe holds no evidence, so
// the frame — the complete read set the proof needs — never opens. Only a
// candidate with evidence worth proving pays for it (#8729).
func TestWorkflowInputPurgedOpensNoFrameWhenInputResolves(t *testing.T) {
	store, root, _ := seedDoneWorkflow(t, "open") // member present: resolved
	framesOpened := 0
	openFrame := func() (inputPurgeFrame, error) {
		framesOpened++
		return inputPurgeFrameForReaders(beads.HandlesFor(store).Live), nil
	}

	if _, err := workflowInputPurgedAcross(beads.HandlesFor(store), beads.HandlesFor(store), root, openFrame); err != nil {
		t.Fatalf("workflowInputPurgedAcross: %v", err)
	}
	plain, err := store.Create(beads.Bead{Title: "unlinked work"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := workflowInputPurgedAcross(beads.HandlesFor(store), beads.HandlesFor(store), plain, openFrame); err != nil {
		t.Fatalf("workflowInputPurgedAcross(plain): %v", err)
	}
	if framesOpened != 0 {
		t.Fatalf("frame opened %d time(s) for resolved inputs, want 0", framesOpened)
	}

	if err := store.Delete(root.Metadata[beadmeta.InputConvoyIDMetadataKey]); err != nil {
		t.Fatalf("purge convoy: %v", err)
	}
	verdict, err := workflowInputPurgedAcross(beads.HandlesFor(store), beads.HandlesFor(store), root, openFrame)
	if err != nil {
		t.Fatalf("workflowInputPurgedAcross(purged): %v", err)
	}
	if !verdict.Purged || framesOpened != 1 {
		t.Fatalf("verdict = %+v with %d frame(s), want purged over exactly one frame", verdict, framesOpened)
	}
}

// A done input stays the skip path's decision, never the purge probe's: the
// candidate is retired as skipped, and the probe is not even consulted.
func TestDoHookClaimSkipsDoneInputWithoutConsultingThePurgeProbe(t *testing.T) {
	store, root, step := seedDoneWorkflow(t, "closed") // member terminal: done
	output, err := json.Marshal([]beads.Bead{step})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
			t.Fatalf("unexpected claim of %s: a done input skips", beadID)
			return beads.Bead{}, false, nil
		},
		InputDone: func(_ context.Context, _ string, _ []string, candidate beads.Bead, ids []string) (string, bool, error) {
			return workflowInputDone(store, candidate, ids)
		},
		SkipDoneWorkflow: func(_ context.Context, _ string, _ []string, rootID string) error {
			return skipDoneWorkflow(store, rootID)
		},
		InputPurged: func(_ context.Context, _ string, _ []string, _ beads.Bead) (hookInputPurgeVerdict, error) {
			t.Fatal("InputPurged consulted for a done input; the skip decision is unchanged")
			return hookInputPurgeVerdict{}, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"rig/worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	got, err := store.Get(step.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.Status != "closed" || got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
		t.Errorf("step = %s/%s, want closed as skipped (stdout=%q stderr=%q)", got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], stdout.String(), stderr.String())
	}
	if root.ID != step.Metadata[beadmeta.RootBeadIDMetadataKey] {
		t.Fatalf("seed shape changed: step %s not under root %s", step.ID, root.ID)
	}
}
