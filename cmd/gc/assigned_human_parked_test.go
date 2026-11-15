package main

// bd's human label parks a bead for a person. On the assignee-scoped SERVE
// paths it is parked exactly like a dispatch hold: a worker cannot advance a
// bead that waits on a person, and re-serving its own in_progress assignment
// every tick starves the ready work behind it. These tests pin that for the
// hook seam, the claim path and the real generated work query end to end, and
// pin the near-miss labels that must keep serving.

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestFilterUnreadyHookCandidatesDropsHumanLabeledBeads: the one seam every
// hook path runs through strips a human-labeled candidate beside the holds.
func TestFilterUnreadyHookCandidatesDropsHumanLabeledBeads(t *testing.T) {
	in := `[
		{"id":"human","status":"in_progress","labels":["` + beadmeta.HumanLabel + `","mysql-cutover"]},
		{"id":"human-and-hold","status":"open","labels":["` + beadmeta.HumanLabel + `","` + beadmeta.HoldMayorLabel + `"]},
		{"id":"held","status":"open","labels":["` + beadmeta.HoldExternalLabel + `"]},
		{"id":"plain","status":"open","labels":["mysql-cutover"]}
	]`
	got := filterUnreadyHookCandidates(in, time.Now())
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("filtered output is not a JSON array: %v (output %q)", err, got)
	}
	if len(rows) != 1 || rows[0]["id"] != "plain" {
		t.Fatalf("filterUnreadyHookCandidates kept %s, want only \"plain\": a human-labeled bead is parked", got)
	}
}

// TestFilterUnreadyHookCandidatesKeepsHumanNearMisses is the negative control:
// the match is the exact label, so look-alikes and a nil label list still serve.
func TestFilterUnreadyHookCandidatesKeepsHumanNearMisses(t *testing.T) {
	in := `[
		{"id":"needs-human","status":"open","labels":["needs-human"]},
		{"id":"human-hold","status":"open","labels":["human-hold"]},
		{"id":"mpr-human-hold","status":"open","labels":["mpr-human-hold"]},
		{"id":"needs-mayor","status":"open","labels":["needs-mayor"]},
		{"id":"capital","status":"open","labels":["Human"]},
		{"id":"nulllabels","status":"open","labels":null}
	]`
	got := filterUnreadyHookCandidates(in, time.Now())
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("filtered output is not a JSON array: %v (output %q)", err, got)
	}
	if len(rows) != 6 {
		t.Fatalf("filterUnreadyHookCandidates kept %d rows, want all 6 near-misses served: %s", len(rows), got)
	}
}

// TestHookClaimDoesNotServeHumanLabeledExistingAssignment is the claim-path
// twin of TestHookClaimDoesNotServeHeldExistingAssignment.
func TestHookClaimDoesNotServeHumanLabeledExistingAssignment(t *testing.T) {
	const parkedID = "gas-human"
	runner := func(string, string) (string, error) {
		return `[{"id":"` + parkedID + `","status":"in_progress","issue_type":"task","assignee":"` +
			holdTestIdentity + `","labels":["` + beadmeta.HumanLabel + `","mysql-cutover"]}]`, nil
	}
	ops := hookClaimOps{
		Runner: runner,
		Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
			t.Fatalf("store.Claim called for human-labeled bead %q", id)
			return beads.Bead{}, false, nil
		},
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", holdTestClaimOptions(), ops, &stdout, &stderr)

	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("want action=drain reason=%s for a hook holding only a human-labeled bead, got action=%q reason=%q bead=%q",
			hookClaimReasonNoWork, result.Action, result.Reason, result.BeadID)
	}
}

// TestAssignedHumanInProgressFallsThroughToRoutedStep is the end-to-end
// signal. Agent A owns X (in_progress, labeled human) and a ready routed Y
// exists: the real generated work query, fed to the real claim path, answers
// Y and never X. With the label removed X is answered again, so the fixture
// is honest and crash recovery is intact.
func TestAssignedHumanInProgressFallsThroughToRoutedStep(t *testing.T) {
	identities := map[string]string{
		"GC_SESSION_ID":   assignedHoldSession,
		"GC_SESSION_NAME": assignedHoldSessionName,
		"GC_ALIAS":        assignedHoldAlias,
	}
	for _, federated := range []bool{false, true} {
		for identityName, identity := range identities {
			t.Run("federated="+strconv.FormatBool(federated)+"/"+identityName, func(t *testing.T) {
				out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{
					assignedHoldDecoy("in_progress", identity, beadmeta.HumanLabel),
					assignedHoldRoutedStep(),
				})
				res, claimed := assignedHoldClaim(t, out)
				assertAssignedHoldRoutedStepClaimed(t, out, res, claimed)
			})
		}
	}
}

// TestAssignedHumanInProgressRemovingLabelServesItAgain is the other half of
// the end-to-end signal.
func TestAssignedHumanInProgressRemovingLabelServesItAgain(t *testing.T) {
	for _, federated := range []bool{false, true} {
		t.Run("federated="+strconv.FormatBool(federated), func(t *testing.T) {
			decoy := assignedHoldDecoy("in_progress", assignedHoldAlias, beadmeta.HumanLabel)
			decoy.Labels = nil
			out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{decoy, assignedHoldRoutedStep()})
			res, claimed := assignedHoldClaim(t, out)
			if res.Action != "work" || res.BeadID != assignedHoldDecoyID || len(claimed) != 0 {
				t.Fatalf("unlabeled in_progress assignment not re-served: action=%s bead=%q claimed=%v out=%q", res.Action, res.BeadID, claimed, out)
			}
		})
	}
}

// TestAssignedHumanNearMissInProgressStillServes: look-alike labels are
// ordinary work, so the owner is still handed its in_progress bead.
func TestAssignedHumanNearMissInProgressStillServes(t *testing.T) {
	for _, label := range []string{"needs-human", "human-hold", "mpr-human-hold", "needs-mayor"} {
		for _, federated := range []bool{false, true} {
			t.Run(label+"/federated="+strconv.FormatBool(federated), func(t *testing.T) {
				out, _ := assignedHoldRunWorkQuery(t, assignedHoldTopology(federated), []assignedHoldRow{
					assignedHoldDecoy("in_progress", assignedHoldAlias, label),
					assignedHoldRoutedStep(),
				})
				res, _ := assignedHoldClaim(t, out)
				if res.Action != "work" || res.BeadID != assignedHoldDecoyID {
					t.Fatalf("near-miss label %q parked the bead: action=%s bead=%q out=%q", label, res.Action, res.BeadID, out)
				}
			})
		}
	}
}
