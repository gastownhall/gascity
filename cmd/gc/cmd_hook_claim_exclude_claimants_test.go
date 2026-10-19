package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// gc.exclude_claimants keeps named identities from claiming a bead out of the
// pool it is routed to (review rechecks need a reviewer other than the one who
// gave the first verdict). The claim path enforces it in Go on every tier, so a
// custom work_query cannot bypass it; the default query also pre-filters it
// (internal/config/workquery_exclude_claimants_test.go) so an excluded head
// cannot starve allowed work behind a bounded read.

const (
	excludeTestPool     = "myrig/review"
	excludeTestReviewer = "myrig/review.one"
	excludeTestOther    = "myrig/review.two"
)

// excludeTestRow renders one work-query row routed to the review pool.
func excludeTestRow(id, status, assignee, exclude string) string {
	md := `"gc.routed_to":"` + excludeTestPool + `"`
	if exclude != "" {
		md += `,"` + beadmeta.ExcludeClaimantsMetadataKey + `":"` + exclude + `"`
	}
	return `{"id":"` + id + `","status":"` + status + `","issue_type":"task","assignee":"` + assignee + `","metadata":{` + md + `}}`
}

func excludeTestOptions(identity string) hookClaimOptions {
	return hookClaimOptions{
		Assignee:           identity,
		IdentityCandidates: hookClaimIdentityCandidates(identity),
		RouteTargets:       hookClaimRouteTargets(excludeTestPool),
		JSON:               true,
	}
}

// excludeTestRecorder stubs every store seam and records the claim mutations.
type excludeTestRecorder struct {
	claims   []string
	releases []string
	assigned []string
	// canonical, when set, is the bead the claim's canonical readback returns
	// for an id; otherwise the readback echoes the claim with no metadata.
	canonical  map[string]beads.Bead
	releaseErr error
}

func (r *excludeTestRecorder) ops(t *testing.T, output string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return output, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			r.claims = append(r.claims, id)
			if b, ok := r.canonical[id]; ok {
				b.Status = "in_progress"
				b.Assignee = assignee
				return b, true, nil
			}
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
		Release: func(_ context.Context, _ string, _ []string, id, _ string) (bool, error) {
			r.releases = append(r.releases, id)
			return r.releaseErr == nil, r.releaseErr
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, error) {
			if b, ok := r.canonical[id]; ok {
				return b, nil
			}
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, id, _ string) error {
			r.assigned = append(r.assigned, id)
			return nil
		},
		StampWorkMeta:            func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		StampSessionClaim:        func(string, string) error { return nil },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitClaimRejected:        func(string, string, string) {},
		EmitClaimReleased:        func(hookClaimReleaseRecord) {},
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		ConfirmBlocked:           func(context.Context, string, []string, string, string) (bool, error) { return false, nil },
	}
}

func decodeExcludeTestResult(t *testing.T, stdout string) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &result); err != nil {
		t.Fatalf("stdout is not one JSON result: %v\nraw: %s", err, stdout)
	}
	return result
}

// The reproducer: the same routed bead, excluded for one reviewer, polled by
// both reviewers in the pool. The excluded reviewer must not claim it and the
// other reviewer must.
func TestHookClaimExcludedReviewerSkipsAndEligibleReviewerClaims(t *testing.T) {
	output := `[` + excludeTestRow("proof-1", "open", "", excludeTestReviewer) + `]`

	excluded := &excludeTestRecorder{}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), excluded.ops(t, output), &stdout, &stderr)
	if len(excluded.claims) != 0 {
		t.Fatalf("excluded reviewer ran the claim CAS on %v; an excluded claimant must never reach the mutation", excluded.claims)
	}
	if got := decodeExcludeTestResult(t, stdout.String()); got.Action != "drain" || got.Reason != hookClaimReasonNoWork {
		t.Fatalf("excluded reviewer got action=%q reason=%q bead=%q, want drain/%s", got.Action, got.Reason, got.BeadID, hookClaimReasonNoWork)
	}

	eligible := &excludeTestRecorder{}
	stdout.Reset()
	stderr.Reset()
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestOther), eligible.ops(t, output), &stdout, &stderr)
	got := decodeExcludeTestResult(t, stdout.String())
	if got.Action != "work" || got.BeadID != "proof-1" || got.Assignee != excludeTestOther {
		t.Fatalf("eligible reviewer got action=%q bead=%q assignee=%q, want work proof-1 for %s (stderr %q)",
			got.Action, got.BeadID, got.Assignee, excludeTestOther, stderr.String())
	}
}

// An excluded head must not hide allowed work behind it in the same result.
func TestHookClaimSkipsExcludedHeadAndClaimsAllowedWorkBehindIt(t *testing.T) {
	output := `[` + excludeTestRow("recheck-1", "open", "", excludeTestReviewer) + `,` +
		excludeTestRow("fresh-2", "open", "", "") + `]`
	rec := &excludeTestRecorder{}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if len(rec.claims) != 1 || rec.claims[0] != "fresh-2" {
		t.Fatalf("claims = %v, want only fresh-2", rec.claims)
	}
	if got := decodeExcludeTestResult(t, stdout.String()); got.Action != "work" || got.BeadID != "fresh-2" {
		t.Fatalf("got action=%q bead=%q, want work fresh-2", got.Action, got.BeadID)
	}
}

// Exclusion is matched against GC_ALIAS and GC_AGENT as well as the session
// identities, so a claim recorded under the session id is still refused when
// the list names the seat's stable alias.
func TestHookClaimExclusionMatchesAliasAndAgentFromEnv(t *testing.T) {
	for _, key := range []string{"GC_ALIAS", "GC_AGENT"} {
		t.Run(key, func(t *testing.T) {
			output := `[` + excludeTestRow("proof-1", "open", "", excludeTestReviewer) + `]`
			opts := excludeTestOptions("gcs-session-1")
			opts.Env = []string{key + "=" + excludeTestReviewer}
			rec := &excludeTestRecorder{}
			var stdout, stderr bytes.Buffer
			doHookClaim("query", "/rig", opts, rec.ops(t, output), &stdout, &stderr)
			if len(rec.claims) != 0 {
				t.Fatalf("claims = %v, want none: %s=%s is excluded", rec.claims, key, excludeTestReviewer)
			}
		})
	}
}

// The query row is a projection; the canonical readback after the CAS is the
// fact. A claim whose canonical bead excludes this session is released, and
// the hook moves on to the next candidate.
func TestHookClaimReleasesClaimWhenCanonicalReadbackExcludes(t *testing.T) {
	output := `[` + excludeTestRow("stale-1", "open", "", "") + `,` + excludeTestRow("fresh-2", "open", "", "") + `]`
	rec := &excludeTestRecorder{canonical: map[string]beads.Bead{
		"stale-1": {ID: "stale-1", Type: "task", Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:         excludeTestPool,
			beadmeta.ExcludeClaimantsMetadataKey: excludeTestReviewer,
		}},
	}}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if len(rec.releases) != 1 || rec.releases[0] != "stale-1" {
		t.Fatalf("releases = %v, want stale-1 given back after its canonical readback excluded this session", rec.releases)
	}
	got := decodeExcludeTestResult(t, stdout.String())
	if got.Action != "work" || got.BeadID != "fresh-2" {
		t.Fatalf("got action=%q bead=%q, want work fresh-2 after releasing stale-1 (stderr %q)", got.Action, got.BeadID, stderr.String())
	}
	if !strings.Contains(stderr.String(), "stale-1") || !strings.Contains(stderr.String(), beadmeta.ExcludeClaimantsMetadataKey) {
		t.Fatalf("stderr = %q, want the release named with its bead and the exclusion key", stderr.String())
	}
}

// A release that fails leaves this session holding a bead it must not work.
// Claiming more work on top of that would hide the residue, so the hook stops.
func TestHookClaimFailsClosedWhenExcludedClaimCannotBeReleased(t *testing.T) {
	output := `[` + excludeTestRow("stale-1", "open", "", "") + `,` + excludeTestRow("fresh-2", "open", "", "") + `]`
	rec := &excludeTestRecorder{
		canonical: map[string]beads.Bead{
			"stale-1": {ID: "stale-1", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: excludeTestReviewer}},
		},
		releaseErr: errors.New("store unavailable"),
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit = %d, want 1 when an excluded claim could not be released", code)
	}
	if len(rec.claims) != 1 {
		t.Fatalf("claims = %v, want only stale-1: no further claim after a failed release", rec.claims)
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("stdout = %q, want no work receipt", stdout.String())
	}
	if !strings.Contains(stderr.String(), "store unavailable") {
		t.Fatalf("stderr = %q, want the release failure surfaced", stderr.String())
	}
}

// A ready bead pre-assigned to an excluded session is not promoted.
func TestHookClaimDoesNotPromoteExcludedReadyAssignment(t *testing.T) {
	output := `[` + excludeTestRow("pinned-1", "open", excludeTestReviewer, excludeTestReviewer) + `]`
	rec := &excludeTestRecorder{}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if len(rec.claims) != 0 {
		t.Fatalf("claims = %v, want none for an excluded ready assignment", rec.claims)
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("stdout = %q, want no work receipt", stdout.String())
	}
}

// A ready assignment whose canonical readback excludes this session is
// released after the promotion, like a fresh claim.
func TestHookClaimReleasesReadyAssignmentWhenCanonicalReadbackExcludes(t *testing.T) {
	output := `[` + excludeTestRow("pinned-1", "open", excludeTestReviewer, "") + `]`
	rec := &excludeTestRecorder{canonical: map[string]beads.Bead{
		"pinned-1": {ID: "pinned-1", Metadata: map[string]string{beadmeta.ExcludeClaimantsMetadataKey: excludeTestReviewer}},
	}}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if len(rec.releases) != 1 || rec.releases[0] != "pinned-1" {
		t.Fatalf("releases = %v, want pinned-1", rec.releases)
	}
	if strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("stdout = %q, want no work receipt", stdout.String())
	}
}

// An in_progress bead held by an excluded session is not adopted.
func TestHookClaimDoesNotAdoptExcludedExistingAssignment(t *testing.T) {
	output := `[` + excludeTestRow("held-1", "in_progress", excludeTestReviewer, excludeTestReviewer) + `,` +
		excludeTestRow("fresh-2", "open", "", "") + `]`
	rec := &excludeTestRecorder{}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	got := decodeExcludeTestResult(t, stdout.String())
	if got.Reason == "existing_assignment" || got.BeadID == "held-1" {
		t.Fatalf("got reason=%q bead=%q, want the excluded in_progress bead not adopted", got.Reason, got.BeadID)
	}
	if got.Action != "work" || got.BeadID != "fresh-2" {
		t.Fatalf("got action=%q bead=%q, want work fresh-2 behind the refused adoption", got.Action, got.BeadID)
	}
}

// Adoption certifies against the canonical store; a canonical exclusion the
// query row did not carry refuses the adoption.
func TestHookClaimAdoptionRefusedWhenCanonicalReadbackExcludes(t *testing.T) {
	output := `[` + excludeTestRow("held-1", "in_progress", excludeTestReviewer, "") + `]`
	rec := &excludeTestRecorder{canonical: map[string]beads.Bead{
		"held-1": {ID: "held-1", Status: "in_progress", Assignee: excludeTestReviewer, Metadata: map[string]string{
			beadmeta.ExcludeClaimantsMetadataKey: excludeTestReviewer,
		}},
	}}
	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", excludeTestOptions(excludeTestReviewer), rec.ops(t, output), &stdout, &stderr)

	if strings.Contains(stdout.String(), `"reason":"existing_assignment"`) {
		t.Fatalf("stdout = %q, want no adoption of a bead whose canonical metadata excludes this session", stdout.String())
	}
	if !strings.Contains(stderr.String(), beadmeta.ExcludeClaimantsMetadataKey) {
		t.Fatalf("stderr = %q, want the refusal to name the exclusion", stderr.String())
	}
}

// Continuation pre-assignment does not pin a sibling to a session it excludes;
// the sibling stays open and routed for an eligible seat.
func TestPreassignHookContinuationGroupSkipsExcludedSibling(t *testing.T) {
	rec := &excludeTestRecorder{}
	ops := rec.ops(t, "[]")
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return []beads.Bead{
			{ID: "step-2", Status: "open", Metadata: map[string]string{
				beadmeta.RoutedToMetadataKey:         excludeTestPool,
				beadmeta.ExcludeClaimantsMetadataKey: excludeTestReviewer,
			}},
			{ID: "step-3", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: excludeTestPool}},
		}, nil
	}
	claimed := beads.Bead{ID: "step-1", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey:        "root-1",
		beadmeta.ContinuationGroupMetadataKey: "review",
	}}
	assigned, err := preassignHookContinuationGroup(claimed, excludeTestOptions(excludeTestReviewer), ops, ".")
	if err != nil {
		t.Fatalf("preassignHookContinuationGroup() error = %v", err)
	}
	if len(assigned) != 1 || assigned[0] != "step-3" {
		t.Fatalf("assigned = %v, want only step-3: step-2 excludes %s", assigned, excludeTestReviewer)
	}
}

// Plain `gc hook` does not display work the claim would refuse.
func TestFilterForeignHookCandidatesHidesExcludedWork(t *testing.T) {
	output := `[` + excludeTestRow("recheck-1", "open", "", excludeTestReviewer) + `,` +
		excludeTestRow("fresh-2", "open", "", "") + `]`
	got := filterForeignHookCandidates(output, hookVisibility{
		Identities:   []string{excludeTestReviewer},
		RouteTargets: []string{excludeTestPool},
	})
	if strings.Contains(got, "recheck-1") || !strings.Contains(got, "fresh-2") {
		t.Fatalf("filterForeignHookCandidates() = %s, want recheck-1 hidden and fresh-2 kept", got)
	}
}

// A claim released because its canonical readback excludes this session drops
// any work_started entry for the bead before the release, as the unwinds do,
// and is never recorded as started: a released claim must not read as started
// work the drain-ack fence holds or the drain-ack release keeps. The entry
// here is left by an earlier hand-over of the same bead. Kills no removal and
// the removal after the release.
func TestHookClaimExcludedReadbackForgetsStartedBeforeRelease(t *testing.T) {
	row := newStartedRow(t, map[string]string{
		session.WorkStartedKey: startedCell(session.WorkStartedEntry{Store: startedStoreRef, Bead: "work-1"}),
	})
	l := &startedLog{}
	ops := l.ops(startedRoutedWork, row, nil)
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		l.add("claim:" + beadID)
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{
			beadmeta.ExcludeClaimantsMetadataKey: assignee,
		}}, true, nil
	}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)

	forget, release := l.index("forget:work-1"), l.index("release:work-1")
	if forget < 0 || release < 0 || forget >= release {
		t.Fatalf("effects %v; want forget, then release", l.log)
	}
	if l.index("record:") >= 0 || l.index("step_started:") >= 0 {
		t.Fatalf("effects %v; want an excluded claim never recorded as started", l.log)
	}
	if row.cell(t).Names(startedStoreRef, "work-1", 7) {
		t.Fatalf("work_started %+v still names the released claim", row.cell(t))
	}
	if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Action != "drain" {
		t.Fatalf("result %+v, stderr %q; want the drain after the only candidate was released", got, stderr.String())
	}
}

// An exclusion does not take back a claim this session already started: the
// drain-ack release keeps started work whatever its metadata says, so the
// drain-ack fence still holds it and the session finishes it. A fence that
// skipped it would answer a drain whose ack the release guard then refuses.
// Kills an exclusion check in the fence.
func TestHookDrainAckFenceHoldsAnExcludedStartedClaim(t *testing.T) {
	got, l, stderr := drainAckFenceRun(t, startedCell(session.WorkStartedEntry{Store: startedStoreRef, Bead: "p-1"}), func(id string) (beads.Bead, error) {
		return beads.Bead{ID: id, Status: "in_progress", Assignee: "worker-1", Metadata: map[string]string{
			beadmeta.ExcludeClaimantsMetadataKey: "worker-1",
		}}, nil
	})
	if got.Action != "work" || got.Reason != hookClaimWorkReasonStartedClaimHeld || got.BeadID != "p-1" || l.acked {
		t.Fatalf("result %+v, acked %v, stderr %q; want started_claim_held for p-1 and no ack", got, l.acked, stderr)
	}
}
