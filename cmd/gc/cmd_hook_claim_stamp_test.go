package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// stampMetaSpy captures the (beadID, assignee, patch) a claim writes through the
// StampWorkMeta seam, and lets a test inject a write error to prove the stamp
// never fails the claim. The patch is copied so the assertion is stable.
type stampMetaSpy struct {
	calls    int
	beadID   string
	assignee string
	patch    map[string]string
	err      error
}

func (s *stampMetaSpy) fn(_ context.Context, _ string, _ []string, beadID, assignee string, patch map[string]string) error {
	s.calls++
	s.beadID, s.assignee = beadID, assignee
	s.patch = map[string]string{}
	for k, v := range patch {
		s.patch[k] = v
	}
	return s.err
}

// noopPublishRunMap keeps claim tests focused on work-bead identity stamping.
func noopPublishRunMap(string, string, ...string) error {
	return nil
}

// sessionClaimSpy captures the (sessionID, beadID) a claim records on the
// CLAIMING SESSION's bead through the StampSessionClaim seam, and lets a test
// inject an error to prove the stamp never fails the claim.
type sessionClaimSpy struct {
	calls     int
	sessionID string
	beadID    string
	// beadIDs records every bead id written through the seam in order, so a test
	// can assert a stamp is followed by a clear (empty id) rather than only seeing
	// the last write.
	beadIDs []string
	err     error
}

func (s *sessionClaimSpy) fn(sessionID, beadID string) error {
	s.calls++
	s.sessionID, s.beadID = sessionID, beadID
	s.beadIDs = append(s.beadIDs, beadID)
	return s.err
}

// noopStampWorkMeta suppresses the work-bead identity stamp so claim tests that
// don't assert on it stay hermetic — the default seam issues a real bd subprocess
// write, which fires now that a claim stamps session identity whenever GC_SESSION_ID
// is set.
func noopStampWorkMeta(context.Context, string, []string, string, string, map[string]string) error {
	return nil
}

// noopStampSessionClaim suppresses the session-bead claim back-channel stamp so
// claim tests that don't assert on it stay hermetic — the default seam resolves
// the city and opens the session store, which a test binary deliberately refuses.
func noopStampSessionClaim(string, string) error { return nil }

// poolClaimOps builds the seam for a pool slot claiming an unassigned,
// route-matched candidate: the runner yields it, Claim returns it owned by us,
// the branch resolver returns branch, and StampWorkMeta is captured by spy.
func poolClaimOps(runner string, claimedMeta map[string]string, branch string, spy *stampMetaSpy) hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return runner, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: claimedMeta}, true, nil
		},
		ResolveWorkBranch: func(string) string { return branch },
		StampWorkMeta:     spy.fn,
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, error) {
			meta := map[string]string{}
			for k, v := range claimedMeta {
				meta[k] = v
			}
			meta[beadmeta.SessionIDMetadataKey] = "mc-sess1"
			meta[beadmeta.SessionNameMetadataKey] = "gc__role-mc-sess1"
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: meta}, nil
		},
		PublishRunMap:     noopPublishRunMap,
		StampSessionClaim: noopStampSessionClaim,
	}
}

// poolClaimOpts is a pool slot's claim options: assignee is the pool session name
// and the env carries both GC_SESSION_ID (the session bead id) and
// GC_SESSION_NAME (the pool session name).
func poolClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "gc__role-mc-sess1",
		IdentityCandidates: []string{"gc__role-mc-sess1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_SESSION_ID=mc-sess1", "GC_SESSION_NAME=gc__role-mc-sess1"},
		JSON:               true,
	}
}

// TestDoHookClaimStampsSessionIdentity is the primary claim-time back-reference
// test: a fresh pool claim stamps gc.session_id + gc.session_name onto the work
// bead alongside gc.work_branch, in ONE patch. Fails before the fix, which stamped
// only the branch.
func TestDoHookClaimStampsSessionIdentity(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-pool","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-pool",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 {
		t.Fatalf("StampWorkMeta calls = %d, want 1", spy.calls)
	}
	want := map[string]string{
		beadmeta.WorkBranchMetadataKey:  "bd-hw-pool",
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("patch = %v, want %v", spy.patch, want)
	}
	if spy.beadID != "hw-pool" || spy.assignee != "gc__role-mc-sess1" {
		t.Fatalf("stamp target = bead %q assignee %q, want hw-pool / gc__role-mc-sess1", spy.beadID, spy.assignee)
	}
}

// TestDoHookClaimStampsSessionIdentityOnAdoption covers the adoption path: a bead
// already in_progress and owned by this session (existing_assignment, no fresh
// Claim) still receives the session back-reference, since the stamp re-runs on
// every hook tick that adopts the bead.
func TestDoHookClaimStampsSessionIdentityOnAdoption(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"hw-adopt","status":"in_progress","assignee":"gc__role-mc-sess1","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Error("Claim must not be called on the existing-assignment path")
			return beads.Bead{}, false, nil
		},
		ResolveWorkBranch: func(string) string { return "" }, // no worktree
		StampWorkMeta:     spy.fn,
		PublishRunMap:     noopPublishRunMap,
		StampSessionClaim: noopStampSessionClaim,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v}", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimStampsSessionIdentityWithoutWorktree pins sessionVerify #1: when
// the worktree resolves no branch (no repo / detached HEAD), the session
// back-reference is STILL stamped — it must not be buried behind the branch
// early-return.
func TestDoHookClaimStampsSessionIdentityWithoutWorktree(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-nobranch","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"", // no branch
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{
		beadmeta.SessionIDMetadataKey:   "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (session id/name even with no worktree)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsStampWhenIdentityUnchanged pins the mandatory idempotence
// guard (sessionVerify #2): a candidate already carrying the current branch AND
// session identity produces NO write, so the per-tick adoption re-run does not
// flood bead.updated events.
func TestDoHookClaimSkipsStampWhenIdentityUnchanged(t *testing.T) {
	spy := &stampMetaSpy{}
	current := map[string]string{
		"gc.routed_to":    "worker",
		"gc.work_branch":  "bd-hw-idem",
		"gc.session_id":   "mc-sess1",
		"gc.session_name": "gc__role-mc-sess1",
	}
	ops := poolClaimOps(
		`[{"id":"hw-idem","status":"open","metadata":{"gc.routed_to":"worker","gc.work_branch":"bd-hw-idem","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1"}}]`,
		current,
		"bd-hw-idem",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("StampWorkMeta calls = %d, want 0 (branch + session identity already current)", spy.calls)
	}
}

// TestDoHookClaimStampsOnlyChangedIdentityKeys proves the patch is minimal: a
// candidate whose session identity is current but whose branch changed writes ONLY
// the branch, leaving the unchanged session keys out of the patch.
func TestDoHookClaimStampsOnlyChangedIdentityKeys(t *testing.T) {
	spy := &stampMetaSpy{}
	current := map[string]string{
		"gc.routed_to":    "worker",
		"gc.work_branch":  "bd-old",
		"gc.session_id":   "mc-sess1",
		"gc.session_name": "gc__role-mc-sess1",
	}
	ops := poolClaimOps(
		`[{"id":"hw-partial","status":"open","metadata":{"gc.routed_to":"worker","gc.work_branch":"bd-old","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1"}}]`,
		current,
		"bd-new",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-new"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (only the changed key)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsSessionIdentityForControlBead pins the control-bead edge
// policy (sessionVerify #3): a control-dispatcher session claiming a control bead
// (gc.kind in ControlKinds) must NOT acquire a session back-reference — control
// steps stay session-free by graphroute's design — while gc.work_branch is still
// stamped as before.
func TestDoHookClaimSkipsSessionIdentityForControlBead(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hc-check","status":"open","metadata":{"gc.routed_to":"worker","gc.kind":"check"}}]`,
		map[string]string{"gc.routed_to": "worker", "gc.kind": "check"},
		"bd-hc-check",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hc-check"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no session keys on a control bead)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimSkipsSessionIdentityWhenNoSessionID: a non-session run (no
// GC_SESSION_ID) has no session bead to reference, so neither session key is
// stamped even when GC_SESSION_NAME happens to be set.
func TestDoHookClaimSkipsSessionIdentityWhenNoSessionID(t *testing.T) {
	spy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-nosess","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-nosess",
		spy,
	)
	opts := poolClaimOpts()
	opts.Env = []string{"GC_SESSION_NAME=gc__role-mc-sess1"} // GC_SESSION_ID absent

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := map[string]string{beadmeta.WorkBranchMetadataKey: "bd-hw-nosess"}
	if spy.calls != 1 || !reflect.DeepEqual(spy.patch, want) {
		t.Fatalf("stamp = {calls:%d patch:%v}, want {1 %v} (no session id ⇒ no session keys)", spy.calls, spy.patch, want)
	}
}

// TestDoHookClaimIdentityStampFailureDoesNotFailClaim proves the stamp is
// best-effort: a failing StampWorkMeta logs to stderr but the claim still exits 0
// and reports the claimed bead id.
func TestDoHookClaimIdentityStampFailureDoesNotFailClaim(t *testing.T) {
	spy := &stampMetaSpy{err: errors.New("dolt boom")}
	ops := poolClaimOps(
		`[{"id":"hw-err","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-err",
		spy,
	)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stamp error must not fail the claim); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want bead hw-err reason claimed", result)
	}
}

func TestDoHookClaimEmitsStartedOnlyAfterDurableSessionReadback(t *testing.T) {
	spy := &stampMetaSpy{}
	meta := map[string]string{
		"gc.routed_to": "worker", beadmeta.RootBeadIDMetadataKey: "gcg-run",
		beadmeta.StepIDMetadataKey: "build", beadmeta.NativeStepDependenciesMetadataKey: `["prepare"]`,
	}
	ops := poolClaimOps(`[{"id":"gcg-attempt","status":"open","metadata":{"gc.routed_to":"worker"}}]`, meta, "", spy)
	var emitted []beads.Bead
	ops.EmitExecutionStepStarted = func(b beads.Bead, _ string, _ []string, _ string) { emitted = append(emitted, b) }
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d; stderr=%s", code, stderr.String())
	}
	if len(emitted) != 1 || emitted[0].ID != "gcg-attempt" || emitted[0].Metadata[beadmeta.SessionIDMetadataKey] != "mc-sess1" || emitted[0].Status != "in_progress" {
		t.Fatalf("started emission = %#v, want one durable in-progress session-stamped step", emitted)
	}

	spy.err = errors.New("stamp failed")
	emitted = nil
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("failed-stamp claim = %d; stderr=%s", code, stderr.String())
	}
	if len(emitted) != 0 {
		t.Fatalf("failed stamp emitted started event: %#v", emitted)
	}
}

// TestDoHookClaimAdoptionReconcilesDurableStartedFact covers the crash window
// between a durable claim-time identity stamp and event recording. A later hook
// tick adopts the same in-progress assignment, verifies its durable identity,
// and must re-emit the idempotent started fact rather than leave a permanent
// lifecycle gap.
func TestDoHookClaimAdoptionReconcilesDurableStartedFact(t *testing.T) {
	spy := &stampMetaSpy{}
	meta := map[string]string{
		"gc.routed_to": "worker", beadmeta.RootBeadIDMetadataKey: "gcg-run",
		beadmeta.StepIDMetadataKey: "build", beadmeta.SessionIDMetadataKey: "mc-sess1",
		beadmeta.SessionNameMetadataKey: "gc__role-mc-sess1",
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"gcg-attempt","status":"in_progress","assignee":"gc__role-mc-sess1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"gcg-run","gc.step_id":"build","gc.session_id":"mc-sess1","gc.session_name":"gc__role-mc-sess1"}}]`, nil
		},
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta:     spy.fn,
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: meta}, nil
		},
		PublishRunMap: noopPublishRunMap,
	}
	var emitted []beads.Bead
	ops.EmitExecutionStepStarted = func(b beads.Bead, _ string, _ []string, _ string) { emitted = append(emitted, b) }

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("StampWorkMeta calls = %d, want 0 for an already durable identity", spy.calls)
	}
	if len(emitted) != 1 || emitted[0].ID != "gcg-attempt" || emitted[0].Metadata[beadmeta.SessionIDMetadataKey] != "mc-sess1" {
		t.Fatalf("started emission = %#v, want durable adopted step", emitted)
	}
}

// TestDoHookClaimStampsCurrentClaimOnSession is the primary claim back-channel
// test: a fresh pool claim records the claimed bead id on the CLAIMING SESSION's
// own bead, so the step's shell can name the bead it is running. Fails before the
// fix, where the claimed id existed nowhere the session could read it.
func TestDoHookClaimStampsCurrentClaimOnSession(t *testing.T) {
	sessSpy := &sessionClaimSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-pool","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-pool",
		&stampMetaSpy{},
	)
	ops.StampSessionClaim = sessSpy.fn

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if sessSpy.calls != 1 || sessSpy.sessionID != "mc-sess1" || sessSpy.beadID != "hw-pool" {
		t.Fatalf("session claim stamp = {calls:%d session:%q bead:%q}, want {1 mc-sess1 hw-pool}",
			sessSpy.calls, sessSpy.sessionID, sessSpy.beadID)
	}
}

// TestDoHookClaimStampsCurrentClaimOnAdoption covers the two non-fresh terminal
// reasons: an already-owned in_progress bead (existing_assignment) and an open
// bead already assigned to this session (ready_assignment) must record the claim
// too — a session that recycles mid-step re-derives its bead id from the stamp,
// so it cannot be written only on the fresh-claim branch.
func TestDoHookClaimStampsCurrentClaimOnAdoption(t *testing.T) {
	for _, tc := range []struct {
		name, row, wantBead string
	}{
		{
			name:     "existing_assignment",
			row:      `[{"id":"hw-adopt","status":"in_progress","assignee":"gc__role-mc-sess1","metadata":{"gc.routed_to":"worker"}}]`,
			wantBead: "hw-adopt",
		},
		{
			name:     "ready_assignment",
			row:      `[{"id":"hw-ready","status":"open","assignee":"gc__role-mc-sess1","metadata":{"gc.routed_to":"worker"}}]`,
			wantBead: "hw-ready",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessSpy := &sessionClaimSpy{}
			ops := hookClaimOps{
				Runner: func(string, string) (string, error) { return tc.row, nil },
				Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
					return beads.Bead{
						ID: id, Status: "in_progress", Assignee: assignee,
						Metadata: map[string]string{"gc.routed_to": "worker"},
					}, true, nil
				},
				ResolveWorkBranch: func(string) string { return "" },
				StampWorkMeta:     noopStampWorkMeta,
				PublishRunMap:     noopPublishRunMap,
				StampSessionClaim: sessSpy.fn,
			}
			var stdout, stderr bytes.Buffer
			if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
				t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
			}
			if sessSpy.calls != 1 || sessSpy.beadID != tc.wantBead {
				t.Fatalf("session claim stamp = {calls:%d bead:%q}, want {1 %s}", sessSpy.calls, sessSpy.beadID, tc.wantBead)
			}
		})
	}
}

// TestDoHookClaimStampsCurrentClaimForControlBead pins the deliberate divergence
// from the work-bead session back-reference: a control bead never acquires
// gc.session_id (control steps stay session-free by graphroute's design), but the
// SESSION still records which bead it claimed — a control-dispatcher session needs
// to name its own bead exactly as much as any worker does.
func TestDoHookClaimStampsCurrentClaimForControlBead(t *testing.T) {
	sessSpy := &sessionClaimSpy{}
	metaSpy := &stampMetaSpy{}
	ops := poolClaimOps(
		`[{"id":"hc-check","status":"open","metadata":{"gc.routed_to":"worker","gc.kind":"check"}}]`,
		map[string]string{"gc.routed_to": "worker", "gc.kind": "check"},
		"bd-hc-check",
		metaSpy,
	)
	ops.StampSessionClaim = sessSpy.fn

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if _, ok := metaSpy.patch[beadmeta.SessionIDMetadataKey]; ok {
		t.Errorf("control bead acquired %s, want the work bead to stay session-free", beadmeta.SessionIDMetadataKey)
	}
	if sessSpy.calls != 1 || sessSpy.beadID != "hc-check" {
		t.Fatalf("session claim stamp = {calls:%d bead:%q}, want {1 hc-check}", sessSpy.calls, sessSpy.beadID)
	}
}

// TestDoHookClaimSkipsCurrentClaimWhenNoSessionID: a non-session run has no
// session bead to stamp, so the back-channel write is not attempted at all
// (rather than issued against an empty id).
func TestDoHookClaimSkipsCurrentClaimWhenNoSessionID(t *testing.T) {
	sessSpy := &sessionClaimSpy{}
	ops := poolClaimOps(
		`[{"id":"hw-nosess","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-nosess",
		&stampMetaSpy{},
	)
	ops.StampSessionClaim = sessSpy.fn
	opts := poolClaimOpts()
	opts.Env = []string{"GC_SESSION_NAME=gc__role-mc-sess1"} // GC_SESSION_ID absent

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if sessSpy.calls != 0 {
		t.Fatalf("session claim stamp calls = %d, want 0 (no session id ⇒ no session bead)", sessSpy.calls)
	}
}

// TestDoHookClaimCurrentClaimStampFailureDoesNotFailClaim proves the back-channel
// stamp is best-effort: a failing write is reported on stderr but the claim still
// exits 0 and reports the claimed bead. The loud refusal for a step that cannot
// name its bead belongs at the point of use, not on the claim.
func TestDoHookClaimCurrentClaimStampFailureDoesNotFailClaim(t *testing.T) {
	sessSpy := &sessionClaimSpy{err: errors.New("dolt boom")}
	ops := poolClaimOps(
		`[{"id":"hw-err","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
		map[string]string{"gc.routed_to": "worker"},
		"bd-hw-err",
		&stampMetaSpy{},
	)
	ops.StampSessionClaim = sessSpy.fn

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", poolClaimOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0 (stamp error must not fail the claim); stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" {
		t.Fatalf("claim result = %+v, want bead hw-err", result)
	}
	if !strings.Contains(stderr.String(), "recording current claim hw-err on session mc-sess1") {
		t.Errorf("stderr = %q, want the failed back-channel write surfaced", stderr.String())
	}
}
