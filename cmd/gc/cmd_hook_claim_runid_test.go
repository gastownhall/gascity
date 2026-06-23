package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// recordRunIDSpy captures the (assignee, sessionBeadID, runID) a claim records,
// and lets a test inject a write error to prove the decoration never fails the
// claim. assignee is captured to pin actor parity with the work_branch stamp.
type recordRunIDSpy struct {
	calls    int
	assignee string
	session  string
	runID    string
	err      error
}

func (s *recordRunIDSpy) fn(_ context.Context, _ string, _ []string, assignee, sessionBeadID, runID string) error {
	s.calls++
	s.assignee, s.session, s.runID = assignee, sessionBeadID, runID
	return s.err
}

// claimOpsForRunID builds the minimal seam for driving a successful fresh claim:
// a routed/open candidate, a Claim that returns it owned by us, the work-branch
// stamp suppressed, and the RecordRunID spy wired in.
func claimOpsForRunID(beadID string, claimedMeta map[string]string, spy *recordRunIDSpy) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"` + beadID + `","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: claimedMeta}, true, nil
		},
		ResolveWorkBranch: func(string) string { return "" }, // suppress work_branch stamp
		RecordRunID:       spy.fn,
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		JSON:               true,
	}
	return ops, opts
}

// TestDoHookClaimRecordsRunIDFromRunChain: a claimed run bead stamps the session
// bead with the run root resolved from its metadata chain (gc.root_bead_id here).
func TestDoHookClaimRecordsRunIDFromRunChain(t *testing.T) {
	spy := &recordRunIDSpy{}
	ops, opts := claimOpsForRunID("hw-run", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-R1",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.session != "sess-1" || spy.runID != "root-R1" {
		t.Fatalf("record = {calls:%d session:%q runID:%q}, want {1 sess-1 root-R1}", spy.calls, spy.session, spy.runID)
	}
	if spy.assignee != "worker-1" {
		t.Fatalf("record assignee = %q, want worker-1 (actor parity with the work_branch stamp)", spy.assignee)
	}
}

// TestDoHookClaimRecordsRunIDFromOwnIDWhenNoRunChain is the no-run-id edge: a
// worker grabbing work outside any run (no chain) resolves to the bead's OWN id
// — a standalone unit is its own run, never misattributed to a prior run on the
// reused session bead.
func TestDoHookClaimRecordsRunIDFromOwnIDWhenNoRunChain(t *testing.T) {
	spy := &recordRunIDSpy{}
	ops, opts := claimOpsForRunID("hw-standalone", map[string]string{
		"gc.routed_to": "worker",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.session != "sess-1" || spy.runID != "hw-standalone" {
		t.Fatalf("record = {calls:%d session:%q runID:%q}, want {1 sess-1 hw-standalone}", spy.calls, spy.session, spy.runID)
	}
}

// TestDoHookClaimSkipsRunIDWhenNoSessionID: a non-session run (no GC_SESSION_ID)
// has no session bead to stamp, so the record is skipped entirely.
func TestDoHookClaimSkipsRunIDWhenNoSessionID(t *testing.T) {
	spy := &recordRunIDSpy{}
	ops, opts := claimOpsForRunID("hw-nosess", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-R1",
	}, spy)
	opts.Env = []string{"GC_ALIAS=worker-1"} // GC_SESSION_ID absent

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("record calls = %d, want 0 (no session bead to stamp)", spy.calls)
	}
}

// TestDoHookClaimRunIDRecordFailureDoesNotFailClaim: a failing run_id write is
// best-effort decoration — it logs to stderr but the claim still succeeds and the
// claimed bead id is still reported on stdout.
func TestDoHookClaimRunIDRecordFailureDoesNotFailClaim(t *testing.T) {
	spy := &recordRunIDSpy{err: errors.New("dolt boom")}
	ops, opts := claimOpsForRunID("hw-err", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-R1",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (record error must not fail the claim); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want bead hw-err reason claimed", result)
	}
	if !strings.Contains(stderr.String(), "recording run_id on session bead sess-1") {
		t.Fatalf("stderr missing best-effort log line; got: %s", stderr.String())
	}
}

// TestDoHookClaimRecordsRunIDOnExistingAssignment pins the run-chain projection
// for the existing-assignment path: when gc hook --claim resumes a bead already
// in_progress and owned by this session (no fresh Claim call), the run id is still
// resolved from the candidate's metadata chain (gc.root_bead_id), not the bead's
// own id. This guards against a future work-query projection that thins candidate
// metadata silently switching the recorded value.
func TestDoHookClaimRecordsRunIDOnExistingAssignment(t *testing.T) {
	spy := &recordRunIDSpy{}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"hw-existing","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-R2"}}]`, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Error("Claim must not be called on the existing-assignment path")
			return beads.Bead{}, false, nil
		},
		ResolveWorkBranch: func(string) string { return "" }, // suppress work_branch stamp
		RecordRunID:       spy.fn,
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=sess-1"},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.session != "sess-1" || spy.runID != "root-R2" {
		t.Fatalf("record = {calls:%d session:%q runID:%q}, want {1 sess-1 root-R2}", spy.calls, spy.session, spy.runID)
	}
	if spy.assignee != "worker-1" {
		t.Fatalf("record assignee = %q, want worker-1 (actor parity with the work_branch stamp)", spy.assignee)
	}
}
