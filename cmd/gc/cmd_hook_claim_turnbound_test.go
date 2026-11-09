package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// turnBoundRoutedWork is one open, routed, claimable candidate: the shape every
// fence below must either claim (in a turn) or refuse (outside one).
const turnBoundRoutedWork = `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`

// turnBoundClaimRecorder captures every mutation a claim invocation attempted so
// a fence test can assert the store was never touched, which is the actual
// invariant — an exit code alone cannot tell a refusal from a claim that
// happened to fail.
type turnBoundClaimRecorder struct {
	claims        []string
	releases      []string
	drainAcked    bool
	stepsStarted  []string
	windowExpired []hookClaimWindowExpiry
	claimReleased []hookClaimReleaseRecord
	claimCtx      []context.Context
}

func (r *turnBoundClaimRecorder) ops(t *testing.T, output string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return output, nil },
		Claim: func(ctx context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			r.claims = append(r.claims, beadID)
			r.claimCtx = append(r.claimCtx, ctx)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		Release: func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
			r.releases = append(r.releases, beadID)
			return true, nil
		},
		DrainAck: func(io.Writer) error {
			r.drainAcked = true
			return nil
		},
		EmitExecutionStepStarted: func(step beads.Bead, _ string, _ []string, _ string) {
			r.stepsStarted = append(r.stepsStarted, step.ID)
		},
		EmitClaimWindowExpired: func(expiry hookClaimWindowExpiry) {
			r.windowExpired = append(r.windowExpired, expiry)
		},
		EmitClaimReleased: func(rec hookClaimReleaseRecord) {
			r.claimReleased = append(r.claimReleased, rec)
		},
		// A run-map publish on the claim hot path would touch the filesystem; the
		// fences under test never depend on it.
		PublishRunMap:     func(string, string, ...string) error { return nil },
		StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
	}
}

func decodeTurnBoundResult(t *testing.T, stdout string) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace([]byte(stdout)), &result); err != nil {
		t.Fatalf("stdout is not a JSON claim result: %v\n%s", err, stdout)
	}
	return result
}

// TestHookClaimRefusesCallbackLane pins F-A: a claim invoked from a provider
// CALLBACK lane mints nothing. A callback's result is never seen by the model,
// so a claim born there is parked by construction.
//
// The control below is the same store with the markers absent: it must claim.
// A broken fence flips only the first; a broken claim path flips only the
// second, so the pair cannot both pass on a no-op implementation.
func TestHookClaimRefusesCallbackLane(t *testing.T) {
	for _, marker := range []string{
		"GC_HOOK_CALLBACK_LANE=1",
		"GC_MANAGED_SESSION_HOOK=1",
		"GC_HOOK_EVENT_NAME=SessionStart",
		"GC_HOOK_EVENT_NAME=UserPromptSubmit",
	} {
		t.Run(marker, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				Env:          []string{marker},
				DrainAck:     true,
				JSON:         true,
			}, rec.ops(t, turnBoundRoutedWork), &stdout, &stderr)

			if code != 0 {
				t.Fatalf("code = %d, want 0 (a callback must not retry a refusal); stderr=%s", code, stderr.String())
			}
			if len(rec.claims) != 0 {
				t.Fatalf("claims = %v, want none: a callback lane must mint no claim", rec.claims)
			}
			result := decodeTurnBoundResult(t, stdout.String())
			if result.Action != "drain" || result.Reason != hookClaimReasonNonTurnContext {
				t.Fatalf("result = %+v, want action=drain reason=%s", result, hookClaimReasonNonTurnContext)
			}
			if rec.drainAcked || result.DrainAcknowledged {
				t.Fatalf("callback lane acknowledged the session's drain (acked=%v result=%+v)", rec.drainAcked, result)
			}
		})
	}
}

// TestHookClaimClaimsInsideATurn is the differently-failing control for
// TestHookClaimRefusesCallbackLane: identical store, no callback markers.
func TestHookClaimClaimsInsideATurn(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		Env:          []string{"GC_SESSION_ID=sess-1"},
		DrainAck:     true,
		JSON:         true,
	}, rec.ops(t, turnBoundRoutedWork), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(rec.claims) != 1 || rec.claims[0] != "work-1" {
		t.Fatalf("claims = %v, want [work-1]", rec.claims)
	}
	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.Reason != "claimed" {
		t.Fatalf("result = %+v, want action=work reason=claimed", result)
	}
}

// buildTurnBoundEnvProbe writes a stand-in for the gc executable that prints its
// own environment, so a test can observe exactly what gc hook run hands its
// child.
func buildTurnBoundEnvProbe(t *testing.T) string {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "gc-env-probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatalf("writing env probe: %v", err)
	}
	return probe
}

// TestHookRunExportsCallbackLaneMarker pins the other half of F-A: the managed
// callback wrapper marks its child, so the fence covers every argv a provider
// hook can carry — including `gc hook run -- hook --claim`, which no rendered
// lane uses today but nothing prevents an operator from writing.
func TestHookRunExportsCallbackLaneMarker(t *testing.T) {
	exe := buildTurnBoundEnvProbe(t)
	originalExecutable := hookRunExecutable
	t.Cleanup(func() { hookRunExecutable = originalExecutable })
	hookRunExecutable = func() (string, error) { return exe, nil }

	var stdout, stderr bytes.Buffer
	if code := cmdHookRun([]string{"probe"}, hookRunOptions{Timeout: 10 * time.Second}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdHookRun = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "GC_HOOK_CALLBACK_LANE=1") {
		t.Fatalf("gc hook run child env = %q, want GC_HOOK_CALLBACK_LANE=1", stdout.String())
	}
}

// TestDirectHookInvocationCarriesNoCallbackLaneMarker is the control for
// TestHookRunExportsCallbackLaneMarker: the marker must come from the wrapper,
// never from the ambient environment, or every turn would be fenced.
func TestDirectHookInvocationCarriesNoCallbackLaneMarker(t *testing.T) {
	if marker := hookClaimNonTurnMarker(nil); marker != "" {
		t.Fatalf("hookClaimNonTurnMarker(nil) = %q, want empty", marker)
	}
	turnEnv := []string{"GC_SESSION_ID=sess-1", "GC_SESSION_NAME=worker-1", "GC_ALIAS=worker-1"}
	if marker := hookClaimNonTurnMarker(turnEnv); marker != "" {
		t.Fatalf("hookClaimNonTurnMarker(turn env) = %q, want empty", marker)
	}
	// An explicitly disabled marker is not a callback lane either: a shell that
	// exports GC_HOOK_CALLBACK_LANE=0 must not fence its own turn.
	if marker := hookClaimNonTurnMarker([]string{"GC_HOOK_CALLBACK_LANE=0"}); marker != "" {
		t.Fatalf("hookClaimNonTurnMarker(disabled) = %q, want empty", marker)
	}
}

// TestHookClaimWindowExpiredRefusesFreshClaim pins F-B: a claim whose window is
// already spent when it reaches the mutation tier is a claim whose invoking turn
// is gone. It must not mint, and it must not write a drain record either — a
// spent window is not an idle store, and laundering it into no_work is exactly
// the "clean drain and killed command are indistinguishable" failure this fixes.
func TestHookClaimWindowExpiredRefusesFreshClaim(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	ops.InvokedAt = time.Now().Add(-90 * time.Second)
	ops.ClaimWindow = 45 * time.Second

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(rec.claims) != 0 {
		t.Fatalf("claims = %v, want none: the claim window was spent", rec.claims)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty: a spent window is not a drain record", stdout.String())
	}
	if rec.drainAcked {
		t.Fatal("drain acknowledged on a spent claim window")
	}
	if len(rec.windowExpired) != 1 {
		t.Fatalf("claim_window_expired events = %d, want 1", len(rec.windowExpired))
	}
	if got := rec.windowExpired[0].InvocationAge; got < 90*time.Second {
		t.Fatalf("event invocation age = %s, want >= 90s", got)
	}
	if !strings.Contains(stderr.String(), "claim window") {
		t.Fatalf("stderr = %q, want a named claim-window diagnostic", stderr.String())
	}
}

// TestHookClaimWindowUnspentStillClaims is control (a) for F-B: the fence must
// not be always-on. An honest claim well inside its window still mints.
func TestHookClaimWindowUnspentStillClaims(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	ops.InvokedAt = time.Now()
	ops.ClaimWindow = 45 * time.Second

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(rec.claims) != 1 {
		t.Fatalf("claims = %v, want exactly one", rec.claims)
	}
	if len(rec.windowExpired) != 0 {
		t.Fatalf("claim_window_expired fired inside the window: %+v", rec.windowExpired)
	}
}

// TestHookClaimWindowExemptsExistingAssignment is control (b) for F-B: adopting
// work this session ALREADY owns mints no new obligation, so a re-woken holder
// must still be served its own claim after the window is spent. Fencing it would
// re-open the parked-claim hole from the other side.
func TestHookClaimWindowExemptsExistingAssignment(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, `[{"id":"own-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`)
	ops.InvokedAt = time.Now().Add(-10 * time.Minute)
	ops.ClaimWindow = 45 * time.Second

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		JSON:               true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != "own-1" {
		t.Fatalf("result = %+v, want the session's own in_progress bead adopted", result)
	}
	if len(rec.windowExpired) != 0 {
		t.Fatalf("adoption fired claim_window_expired: %+v", rec.windowExpired)
	}
	if len(rec.releases) != 0 {
		t.Fatalf("adoption released its own claim: %v", rec.releases)
	}
}

// TestHookClaimSlowWorkQueryStillClaimsWithinDerivedWindow pins the reconciliation
// of the claim window with the raised work-query budget. A fresh routed claim whose
// federated work query legitimately runs long — 70s here, the measured loaded worst
// case and well past the old flat 45s window — must still CLAIM, not trip
// execution.claim_window_expired. Before hookClaimWindowDefault was derived from
// hookWorkQueryTimeout + hookClaimMutationTimeout, raising the query budget was
// inert on the --claim path: the read succeeded but the fence, anchored at
// invocation start and charged only 45s, refused the claim it had just found.
//
// InvokedAt and ClaimWindow are left unset so applyDefaults derives the window from
// the production hookClaimWindowDefault — the exact value under test. Under the old
// flat 45s default this claim was refused; the differently-failing control is
// TestHookClaimWindowExpiredRefusesFreshClaim, which still refuses a claim whose age
// exceeds an explicit window.
func TestHookClaimSlowWorkQueryStillClaimsWithinDerivedWindow(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	// A controlled clock rather than a real sleep: the work query "spends" 70s
	// before returning routed work. applyDefaults stamps InvokedAt at the pre-read
	// clock, so by the time the claim tier checks the window the invocation age is
	// 70s — past 45s, inside the derived budget.
	clk := time.Now()
	ops.Now = func() time.Time { return clk }
	ops.Runner = func(string, string) (string, error) {
		clk = clk.Add(70 * time.Second)
		return turnBoundRoutedWork, nil
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0: a slow-but-alive read must still claim; stderr=%s", code, stderr.String())
	}
	if len(rec.claims) != 1 || rec.claims[0] != "work-1" {
		t.Fatalf("claims = %v, want [work-1]: the raised work-query budget must reach the claim", rec.claims)
	}
	if len(rec.windowExpired) != 0 {
		t.Fatalf("claim_window_expired fired for a read inside the derived window: %+v", rec.windowExpired)
	}
	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.BeadID != "work-1" {
		t.Fatalf("result = %+v, want action=work bead=work-1", result)
	}
}

// TestHookClaimWindowDefaultCoversWorkQueryBudget guards the derivation itself: the
// default claim window must leave a full claim-mutation margin after an honest
// full-budget work query, or the fence would again refuse a claim the raised query
// just surfaced. Pinning the relation (not the literal 160s) keeps it correct if
// either budget is retuned.
func TestHookClaimWindowDefaultCoversWorkQueryBudget(t *testing.T) {
	if hookClaimWindowDefault < hookWorkQueryTimeout+hookClaimMutationTimeout {
		t.Fatalf("hookClaimWindowDefault = %s, want >= hookWorkQueryTimeout(%s) + hookClaimMutationTimeout(%s); "+
			"a smaller window refuses a claim the full-budget work query just found",
			hookClaimWindowDefault, hookWorkQueryTimeout, hookClaimMutationTimeout)
	}
}

// TestHookClaimWriteChildGetsReservedBudgetAtWindowEdge pins the reserved claim
// budget: a claim that reaches the CAS inside its window gets the FULL mutation
// budget, never the sliver of window the read left over.
//
// The old contract bounded the claim-write child by the REMAINING window, so a
// federated read that finished a few milliseconds before the window closed
// handed the CAS an already-expired context. bd then failed the claim with
// `claiming bead "<id>": timed out after 0s (caller deadline)`, the tier skipped
// it as an errored claim, and the seat drained claims_errored with its routed
// work still unclaimed (maintainer-city, 2026-09-22/23). The claim budget is now
// separate from the read budget; the straddle fence still bounds how late the
// CAS may land (window + reserve).
func TestHookClaimWriteChildGetsReservedBudgetAtWindowEdge(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	ops.InvokedAt = time.Now().Add(-44 * time.Second)
	ops.ClaimWindow = 45 * time.Second

	var stdout, stderr bytes.Buffer
	doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if len(rec.claimCtx) != 1 {
		t.Fatalf("claim contexts = %d, want 1", len(rec.claimCtx))
	}
	deadline, ok := rec.claimCtx[0].Deadline()
	if !ok {
		t.Fatal("claim ran with no deadline")
	}
	if remaining := time.Until(deadline); remaining < hookClaimMutationTimeout-2*time.Second {
		t.Fatalf("claim-write deadline = %s away, want the reserved mutation budget (~%s): the read must not eat the claim's time", remaining, hookClaimMutationTimeout)
	}
	if remaining := time.Until(deadline); remaining > hookClaimMutationTimeout+time.Second {
		t.Fatalf("claim-write deadline = %s away, want it bounded by the mutation budget %s", remaining, hookClaimMutationTimeout)
	}
}

// TestHookClaimFoundBeadStaysClaimableWhenReadSpendsTheWindow reproduces the
// claims_errored specimen end to end: the work query returns routed work with
// 1ms of the claim window left. The claim op behaves like BdStore.Claim — it
// fails with the caller-deadline error when its context has no time left — so
// under the old leftover-window budget the only candidate errored and the seat
// drained claims_errored. With the reserved budget the found bead is claimed.
func TestHookClaimFoundBeadStaysClaimableWhenReadSpendsTheWindow(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	clk := time.Now()
	ops.Now = func() time.Time { return clk }
	ops.InvokedAt = clk
	ops.ClaimWindow = 240 * time.Second // the maintainer city's GC_HOOK_CLAIM_WINDOW
	ops.Runner = func(string, string) (string, error) {
		// A slow federated read that returns with 1ms of window left.
		clk = clk.Add(ops.ClaimWindow - time.Millisecond)
		return turnBoundRoutedWork, nil
	}
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		// A healthy CAS takes a moment of real time; one that starts with an
		// all-but-expired context is refused by the bd runner exactly as in the
		// supervisor evidence.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < time.Second {
			return beads.Bead{}, false, errors.New(`claiming bead "` + beadID + `": timed out after 0s (caller deadline)`)
		}
		return baseClaim(ctx, dir, env, beadID, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		DrainAck:     true,
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0: the found bead must be claimable; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	result := decodeTurnBoundResult(t, stdout.String())
	if result.Action != "work" || result.BeadID != "work-1" {
		t.Fatalf("result = %+v, want action=work bead=work-1 (not a %s drain)", result, hookClaimReasonClaimsErrored)
	}
	if len(rec.releases) != 0 {
		t.Fatalf("releases = %v, want none: the claim landed inside window+reserve", rec.releases)
	}
}

// TestHookClaimUnwindsOnResultDeliveryFailure pins F-C: a claim whose result
// cannot leave the process (EPIPE — the provider closed the tool pipe) is a
// claim nobody received. Release it rather than park it.
func TestHookClaimUnwindsOnResultDeliveryFailure(t *testing.T) {
	for _, jsonOut := range []bool{true, false} {
		name := "json"
		if !jsonOut {
			name = "plain"
		}
		t.Run(name, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			var stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				JSON:         jsonOut,
			}, rec.ops(t, turnBoundRoutedWork), brokenPipeWriter{}, &stderr)

			if code != 1 {
				t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
			}
			if len(rec.claims) != 1 {
				t.Fatalf("claims = %v, want exactly one", rec.claims)
			}
			if len(rec.releases) != 1 || rec.releases[0] != "work-1" {
				t.Fatalf("releases = %v, want [work-1]: an undeliverable claim must be released", rec.releases)
			}
			if len(rec.claimReleased) != 1 || rec.claimReleased[0].BeadID != "work-1" {
				t.Fatalf("bead.claim_released events = %+v, want one for work-1", rec.claimReleased)
			}
		})
	}
}

// TestHookClaimUnwoundClaimDoesNotEnqueueContinuationNudge pins that the
// continuation nudge follows a DELIVERED claim result. When the result cannot
// leave the process the claim is released (F-C), and a nudge for a claim the
// session no longer holds would wake it for work it does not own.
func TestHookClaimUnwoundClaimDoesNotEnqueueContinuationNudge(t *testing.T) {
	var enqueued, assigned, released []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), []string{"sib-1"}, &enqueued)
	ops.AssignContinuation = func(_ context.Context, _ string, _ []string, beadID, _ string) error {
		assigned = append(assigned, beadID)
		return nil
	}
	ops.Release = func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
		released = append(released, beadID)
		return true, nil
	}
	ops.EmitClaimReleased = func(hookClaimReleaseRecord) {}

	var stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"route-1"},
		JSON:         true,
	}, ops, brokenPipeWriter{}, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if got := strings.Join(assigned, ","); got != "sib-1" {
		t.Fatalf("assigned siblings = %q, want sib-1: without it the nudge gate is shut and this test proves nothing", got)
	}
	if got := strings.Join(released, ","); got != "root-1" {
		t.Fatalf("released = %q, want root-1: the claim must have been unwound", got)
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge enqueued for %v, want none for an unwound claim", enqueued)
	}
}

// TestHookClaimKeepsDeliveredClaim is the control for F-C: a fence that released
// on SUCCESS would destroy every claim the fleet makes, and only this direction
// catches it.
func TestHookClaimKeepsDeliveredClaim(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, rec.ops(t, turnBoundRoutedWork), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(rec.releases) != 0 {
		t.Fatalf("releases = %v, want none on a delivered claim", rec.releases)
	}
	if len(rec.claimReleased) != 0 {
		t.Fatalf("bead.claim_released emitted on a delivered claim: %+v", rec.claimReleased)
	}
}

// TestHookClaimStraddleUnwinds pins the straddle case the babysit evidence
// showed: the CAS is STARTED inside the window and LANDS outside it, because the
// bd child carries its own ceiling. A claim that lands after its invoking turn's
// deadline is the same parked claim by another route, so it self-releases.
func TestHookClaimStraddleUnwinds(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	// A controlled clock rather than a real sleep: the property under test is
	// "the CAS returned after the window closed", and sleeping to produce it
	// would only make the test slower and racier than the fact it is pinning.
	clock := time.Now()
	ops.Now = func() time.Time { return clock }
	ops.InvokedAt = clock
	ops.ClaimWindow = 40 * time.Second
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		// The CAS commits, but only after the invoking process's window is spent —
		// the bd child carries its own ceiling, so this is the straddle.
		clock = clock.Add(80 * time.Second)
		return baseClaim(ctx, dir, env, beadID, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(rec.claims) != 1 {
		t.Fatalf("claims = %v, want exactly one", rec.claims)
	}
	if len(rec.releases) != 1 || rec.releases[0] != "work-1" {
		t.Fatalf("releases = %v, want [work-1]: a straddling claim must self-release", rec.releases)
	}
	if len(rec.claimReleased) != 1 {
		t.Fatalf("bead.claim_released events = %+v, want one", rec.claimReleased)
	}
}

// TestHookClaimSlowButInsideWindowStands is the control for the straddle: a slow
// claim that still lands INSIDE its window is a healthy claim and must stand.
func TestHookClaimSlowButInsideWindowStands(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	clock := time.Now()
	ops.Now = func() time.Time { return clock }
	ops.InvokedAt = clock
	ops.ClaimWindow = 30 * time.Second
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		// Slow, but still comfortably inside the window.
		clock = clock.Add(5 * time.Second)
		return baseClaim(ctx, dir, env, beadID, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(rec.releases) != 0 {
		t.Fatalf("releases = %v, want none: the claim landed inside its window", rec.releases)
	}
}

// TestHookClaimLandingInsideReserveStands pins the other edge of the reserved
// claim budget: a CAS that STARTS just inside the window and lands a few seconds
// past it — inside window + hookClaimMutationTimeout — is a healthy claim, not a
// straddle. Under the old window-only landing check it was released, so the
// found bead was claimed and immediately given back.
func TestHookClaimLandingInsideReserveStands(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	clock := time.Now()
	ops.Now = func() time.Time { return clock }
	ops.InvokedAt = clock.Add(-(40*time.Second - time.Millisecond))
	ops.ClaimWindow = 40 * time.Second
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		clock = clock.Add(hookClaimMutationTimeout / 2)
		return baseClaim(ctx, dir, env, beadID, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(rec.releases) != 0 {
		t.Fatalf("releases = %v, want none: the claim landed inside its reserved budget", rec.releases)
	}
}

// brokenPipeWriter is a stdout whose reader has gone: the orphaned tool call's
// signature. Every write fails with EPIPE, exactly as the provider's closed tool
// pipe does.
type brokenPipeWriter struct{}

func (brokenPipeWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }

// TestHookClaimUnwindFailureIsSurfaced pins that a release which itself fails is
// reported rather than swallowed: the claim is still parked, and a silent
// success here would hide the one residue the operator must chase.
func TestHookClaimUnwindFailureIsSurfaced(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	ops.Release = func(context.Context, string, []string, string, string) (bool, error) {
		return false, errors.New("binding refused the release")
	}

	var stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, brokenPipeWriter{}, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "binding refused the release") {
		t.Fatalf("stderr = %q, want the release failure surfaced", stderr.String())
	}
	if len(rec.claimReleased) != 0 {
		t.Fatalf("bead.claim_released emitted for a release that did not happen: %+v", rec.claimReleased)
	}
}

// TestHookClaimClearsSessionCurrentClaimOnUndeliveredUnwind pins the inverse the
// claim back-channel was missing: when a MINTED claim stamps the session bead's
// current_claim_bead_id and then its result cannot be delivered (F-C, EPIPE), the
// undelivered-claim unwind must clear that stamp as well as release the work bead.
// Without the clear a later `gc hook current --id-only` hands the shell a bead the
// session no longer owns, and `gc bd close $BEAD_ID` closes another session's
// in-progress work — the exact "close somebody else's bead" hazard the stamp exists
// to prevent. The clear routes through the same StampSessionClaim seam as the stamp
// (an empty id), and it must land BEFORE the release so a freed bead is never both
// claimable elsewhere and still named here.
func TestHookClaimClearsSessionCurrentClaimOnUndeliveredUnwind(t *testing.T) {
	for _, jsonOut := range []bool{true, false} {
		name := "json"
		if !jsonOut {
			name = "plain"
		}
		t.Run(name, func(t *testing.T) {
			rec := &turnBoundClaimRecorder{}
			ops := rec.ops(t, turnBoundRoutedWork)
			// A session-run claim (GC_SESSION_ID set) reaches the identity readback;
			// stub it so the test stays hermetic (the default seam issues a real bd
			// store read). This test asserts only the session-claim back-channel.
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, error) {
				return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.session_id": "mc-sess1"}}, nil
			}
			sessSpy := &sessionClaimSpy{}
			ops.StampSessionClaim = sessSpy.fn
			// The release override proves clear-before-release: by the time the work
			// bead is given back, the stamp must already have been cleared.
			ops.Release = func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
				if len(sessSpy.beadIDs) != 2 || sessSpy.beadIDs[1] != "" {
					t.Errorf("release ran before the session-claim clear; seam writes so far = %v", sessSpy.beadIDs)
				}
				rec.releases = append(rec.releases, beadID)
				return true, nil
			}

			var stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				Env:          []string{"GC_SESSION_ID=mc-sess1"},
				JSON:         jsonOut,
			}, ops, brokenPipeWriter{}, &stderr)

			if code != 1 {
				t.Fatalf("code = %d, want 1; stderr=%s", code, stderr.String())
			}
			if len(rec.releases) != 1 || rec.releases[0] != "work-1" {
				t.Fatalf("releases = %v, want [work-1]", rec.releases)
			}
			// Exactly two seam writes, in order: the stamp of the minted bead, then
			// the clear (empty id) on the unwind.
			if len(sessSpy.beadIDs) != 2 || sessSpy.beadIDs[0] != "work-1" || sessSpy.beadIDs[1] != "" {
				t.Fatalf("session-claim seam writes = %v, want [work-1 \"\"] (stamp then clear)", sessSpy.beadIDs)
			}
			if sessSpy.sessionID != "mc-sess1" {
				t.Fatalf("session-claim seam sessionID = %q, want mc-sess1", sessSpy.sessionID)
			}
		})
	}
}

// TestHookClaimStraddleLeavesSessionClaimUntouched is the negative control for the
// undelivered-unwind clear: the straddle path (F-B) returns BEFORE the stamp, so it
// neither stamps nor clears the session-claim seam. The clear added for F-C must not
// leak onto this path, where blanking the seam could drop a stamp a different,
// still-owned bead legitimately left on the session.
func TestHookClaimStraddleLeavesSessionClaimUntouched(t *testing.T) {
	rec := &turnBoundClaimRecorder{}
	ops := rec.ops(t, turnBoundRoutedWork)
	sessSpy := &sessionClaimSpy{}
	ops.StampSessionClaim = sessSpy.fn
	clock := time.Now()
	ops.Now = func() time.Time { return clock }
	ops.InvokedAt = clock
	ops.ClaimWindow = 40 * time.Second
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		// The CAS commits after the invoking process's window is spent — the straddle.
		clock = clock.Add(80 * time.Second)
		return baseClaim(ctx, dir, env, beadID, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		Env:          []string{"GC_SESSION_ID=mc-sess1"},
		JSON:         true,
	}, ops, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(rec.releases) != 1 || rec.releases[0] != "work-1" {
		t.Fatalf("releases = %v, want [work-1]: a straddling claim must self-release", rec.releases)
	}
	if len(sessSpy.beadIDs) != 0 {
		t.Fatalf("session-claim seam writes = %v, want none: the straddle path stamps nothing, so it must clear nothing", sessSpy.beadIDs)
	}
}

// landingClaimRecorder is the turnbound recorder plus the seams a tier-level
// landing row needs: the claim runs through the REAL bd adapter
// (hookClaimWithBdStore) over a fake bd runner, so what lands in the fences is
// the adapter's own CAS/readback sequence, not a stubbed verdict.
type landingClaimRecorder struct {
	releases      []string
	drainAcked    bool
	windowExpired []hookClaimWindowExpiry
	claimReleased []hookClaimReleaseRecord
}

func (r *landingClaimRecorder) ops(t *testing.T, output string, clock *time.Time) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return output, nil },
		// The production adapter: a fake bd runner serves its subprocesses.
		Claim: hookClaimWithBdStore,
		Release: func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
			r.releases = append(r.releases, beadID)
			return true, nil
		},
		DrainAck: func(io.Writer) error {
			r.drainAcked = true
			return nil
		},
		EmitClaimWindowExpired: func(expiry hookClaimWindowExpiry) {
			r.windowExpired = append(r.windowExpired, expiry)
		},
		EmitClaimReleased: func(rec hookClaimReleaseRecord) {
			r.claimReleased = append(r.claimReleased, rec)
		},
		StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		PublishRunMap:     func(string, string, ...string) error { return nil },
		ListContinuation:  func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return nil, nil },
		StoreRef:          func(string) string { return "rig:landing" },
		ReadWorkStarted:   noStartedClaims,
		Now:               func() time.Time { return *clock },
		InvokedAt:         *clock,
		ClaimWindow:       30 * time.Second,
	}
}

// landingBdTimeline is the fake clock's budget for each bd leg of a claim:
// casLanding is how much fake time the claim CAS consumes before it commits,
// and readback is how much the canonical show consumes after it.
type landingBdTimeline struct {
	casLanding    time.Duration
	readback      time.Duration
	readbackFails bool
}

// installLandingBdRunner replaces the bd runner seam for the duration of one
// tier row and returns the bd argv log, so a row can count the CAS legs it
// caused.
func installLandingBdRunner(t *testing.T, clock *time.Time, tl landingBdTimeline) *[][]string {
	t.Helper()
	var calls [][]string
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(context.Context, map[string]string) beads.CommandRunner {
		return func(_ string, _ string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			switch {
			case len(args) >= 4 && args[0] == "update" && args[2] == "--claim":
				*clock = clock.Add(tl.casLanding)
				return []byte(fmt.Sprintf(`[{"id":%q,"status":"in_progress","assignee":"worker-1"}]`, args[1])), nil
			case len(args) >= 3 && args[0] == "show" && args[1] == "--json":
				*clock = clock.Add(tl.readback)
				if tl.readbackFails {
					return nil, errors.New("canonical read failed")
				}
				return []byte(fmt.Sprintf(`[{"id":%q,"status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, args[2])), nil
			default:
				t.Fatalf("unexpected bd args: %#v", args)
				return nil, nil
			}
		}
	}
	return &calls
}

// TestHookClaimTiersKeepTheCASLandingAcrossCanonicalReadback pins #6984's
// timing half in both claim tiers: the straddle fence measures the instant the
// claim CAS returned, not the canonical readback that follows it. A CAS that
// lands inside the window plus its reserved mutation budget is a healthy claim
// even when the canonical readback afterwards crosses that boundary; fencing
// on the post-readback clock released it, and the readback's cost grew further
// once it got its own budget. Both tiers are driven through the production bd
// adapter over a fake bd runner and a controlled clock.
func TestHookClaimTiersKeepTheCASLandingAcrossCanonicalReadback(t *testing.T) {
	const window = 30 * time.Second
	reserve := hookClaimMutationTimeout
	for _, tier := range []struct {
		name    string
		row     string
		wantWhy string
	}{
		{
			name:    "ready assignment",
			row:     `[{"id":"work-1","status":"open","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`,
			wantWhy: "ready_assignment",
		},
		{
			name:    "fresh candidate",
			row:     `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`,
			wantWhy: "claimed",
		},
	} {
		t.Run(tier.name, func(t *testing.T) {
			clock := time.Now()
			rec := &landingClaimRecorder{}
			ops := rec.ops(t, tier.row, &clock)
			// The CAS lands one second inside window + reserve; the canonical
			// readback then spends five more seconds, crossing the boundary.
			bdCalls := installLandingBdRunner(t, &clock, landingBdTimeline{
				casLanding: window + reserve - time.Second,
				readback:   5 * time.Second,
			})

			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				DrainAck:     true,
				JSON:         true,
			}, ops, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("code = %d, want 0; a CAS that landed inside window+reserve is a healthy claim, not a straddle; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			if len(rec.releases) != 0 {
				t.Fatalf("releases = %v, want none: the claim landed in time and must be delivered", rec.releases)
			}
			if rec.drainAcked {
				t.Fatal("the drain was acknowledged on a delivered claim")
			}
			result := decodeTurnBoundResult(t, stdout.String())
			if result.Action != "work" || result.Reason != tier.wantWhy || result.BeadID != "work-1" {
				t.Fatalf("result = %+v, want the work receipt (reason %s)", result, tier.wantWhy)
			}
			// The CAS ran once, its canonical readback once, and nothing else
			// was claimed or probed afterwards.
			if len(*bdCalls) != 2 {
				t.Fatalf("bd calls = %#v, want one claim CAS and one canonical readback", *bdCalls)
			}
		})
	}
	// The fence's two edges. A CAS that lands EXACTLY at window+reserve is
	// inside the strict greater-than fence and stands; one that lands a
	// second past it is the straddle the reserve exists to catch, and still
	// takes the unwind now that the readback no longer spends the fence's
	// clock.
	for _, row := range []struct {
		name       string
		casLanding time.Duration
		wantKept   bool
	}{
		{name: "exact boundary lands at window+reserve", casLanding: window + reserve, wantKept: true},
		{name: "genuinely late CAS still unwinds", casLanding: window + reserve + time.Second, wantKept: false},
	} {
		t.Run(row.name, func(t *testing.T) {
			clock := time.Now()
			rec := &landingClaimRecorder{}
			ops := rec.ops(t, turnBoundRoutedWork, &clock)
			installLandingBdRunner(t, &clock, landingBdTimeline{
				casLanding: row.casLanding,
				readback:   5 * time.Second,
			})

			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{
				Assignee:     "worker-1",
				RouteTargets: []string{"worker"},
				DrainAck:     true,
				JSON:         true,
			}, ops, &stdout, &stderr)

			if row.wantKept {
				if code != 0 {
					t.Fatalf("code = %d, want 0; a CAS at exactly window+reserve is not past the strict fence; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
				}
				if len(rec.releases) != 0 {
					t.Fatalf("releases = %v, want none: the boundary CAS must stand", rec.releases)
				}
				return
			}
			if code != 1 {
				t.Fatalf("code = %d, want 1; a CAS past window+reserve must take the straddle unwind; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			if len(rec.releases) != 1 || rec.releases[0] != "work-1" {
				t.Fatalf("releases = %v, want [work-1]: a late CAS must self-release", rec.releases)
			}
			if len(rec.windowExpired) != 0 {
				t.Fatalf("claim_window_expired = %+v, want none: the late-CAS unwind reports the landing, not a spent window", rec.windowExpired)
			}
			if rec.drainAcked {
				t.Fatal("the drain was acknowledged on a straddled claim")
			}
		})
	}
	// A committed CAS whose canonical readback then fails stops the tier with
	// the committed-claim diagnostic: no second candidate is tried even though
	// one is available, nothing is released, and no idle drain is written.
	t.Run("committed CAS with failed readback stops the tier", func(t *testing.T) {
		clock := time.Now()
		rec := &landingClaimRecorder{}
		// Two candidates, so "stops without another candidate" is observable.
		ops := rec.ops(t, `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}]`, &clock)
		bdCalls := installLandingBdRunner(t, &clock, landingBdTimeline{readbackFails: true})

		var stdout, stderr bytes.Buffer
		code := doHookClaim("query", "/rig", hookClaimOptions{
			Assignee:     "worker-1",
			RouteTargets: []string{"worker"},
			DrainAck:     true,
			JSON:         true,
		}, ops, &stdout, &stderr)

		if code != 1 {
			t.Fatalf("code = %d, want 1; a committed claim whose readback failed must stop the tier; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "claimed work-1 but loading canonical bead failed") {
			t.Fatalf("stderr = %q, want the committed-claim diagnostic", stderr.String())
		}
		if len(rec.releases) != 0 {
			t.Fatalf("releases = %v, want none: canonical state is unknown, so the claim is left parked", rec.releases)
		}
		if rec.drainAcked {
			t.Fatal("the drain was acknowledged on a committed claim: an unknown-ownership stop is not idle")
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want no work receipt on an unreadable claim", stdout.String())
		}
		// One CAS, one failed readback — the second candidate was never tried.
		if len(*bdCalls) != 2 {
			t.Fatalf("bd calls = %#v, want one claim CAS and one failed readback, no second candidate", *bdCalls)
		}
	})
}

// TestHookClaimLandingRecorder pins the CAS-landing recorder's own contract
// (#6984), unit-level: the classifier records the instant the CAS committed —
// before the projection identity check and the canonical readback, so a claim
// rejected as foreign still records — errors and conflicts never record, a
// context without a recorder is safe, a non-recording op falls back to the
// tier's immediate post-return clock sample, and two candidates in one tier
// never share a recorder: a rejected early candidate must not lend its landing
// to a genuinely late one.
func TestHookClaimLandingRecorder(t *testing.T) {
	t.Run("recording precedes the identity check and the reload", func(t *testing.T) {
		start := time.Now()
		clock := &start
		ctx, landing := withHookClaimLanding(context.Background(), func() time.Time { return *clock })
		// The CAS commits at t0 and its projection names another seat, so the
		// classifier returns a rejected claim — but the landing is already
		// recorded by then, before the identity check that rejects it: that
		// ordering is what keeps the fence's clock from being spent by anything
		// after the CAS.
		_, ok, err := hookClaimThroughStore(ctx, "work-1", "worker-1",
			func() (beads.Bead, bool, error) {
				return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "someone-else"}, true, nil
			},
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil },
			func(string) (beads.Bead, error) {
				t.Fatal("the reload must not run: the identity check rejects the claim first")
				return beads.Bead{}, nil
			})
		if err != nil || ok {
			t.Fatalf("fixture: a foreign projection is a rejected claim (ok=false, nil error), got (ok=%v err=%v)", ok, err)
		}
		if got := landing.landingTime(); !got.Equal(start) {
			t.Fatalf("landing = %v, want the CAS instant %v: the landing must be recorded before the reload spends the clock", got, start)
		}
	})

	t.Run("errors and conflicts never record", func(t *testing.T) {
		start := time.Now()
		clock := &start
		for _, tc := range []struct {
			name  string
			claim func() (beads.Bead, bool, error)
		}{
			{name: "claim error", claim: func() (beads.Bead, bool, error) {
				return beads.Bead{}, false, errors.New("store down")
			}},
			{name: "lost race", claim: func() (beads.Bead, bool, error) {
				return beads.Bead{ID: "work-1", Assignee: "other"}, false, nil
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx, landing := withHookClaimLanding(context.Background(), func() time.Time { return *clock })
				_, _, _ = hookClaimThroughStore(ctx, "work-1", "worker-1", tc.claim,
					func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil },
					func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil })
				if !landing.landed.IsZero() {
					t.Fatalf("landing = %v, want none: a failed CAS is not a landing", landing.landed)
				}
			})
		}
	})

	t.Run("a context without a recorder is safe", func(_ *testing.T) {
		// A plain classifier caller (the class binding until AC-5) must not
		// record, and must not panic.
		recordHookClaimLanding(context.Background())
		_, _, _ = hookClaimThroughStore(context.Background(), "work-1", "worker-1",
			func() (beads.Bead, bool, error) {
				return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "worker-1"}, true, nil
			},
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil },
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil })
	})

	t.Run("a non-recording op falls back to the post-return clock", func(t *testing.T) {
		start := time.Now()
		clock := start
		// A tier whose op never derived the recorder context still gets a
		// landing: the immediate post-return sample, the pre-recorder behavior.
		rec := &hookClaimLandingRecorder{clock: func() time.Time { return clock }}
		clock = clock.Add(2 * time.Second)
		if got := rec.landingTime(); !got.Equal(start.Add(2 * time.Second)) {
			t.Fatalf("fallback landing = %v, want the clock at call time %v", got, start.Add(2*time.Second))
		}
	})

	t.Run("each candidate records on its own", func(t *testing.T) {
		// One tier, two candidates, one clock, exactly the derivation the tier
		// performs per candidate. The first CAS commits an EARLY landing but its
		// projection names another seat, so the tier skips it and continues; the
		// second CAS commits late. The recorders must be fresh per candidate: a
		// shared one would still hold the first stamp and wrongly stand the
		// second, genuinely late claim.
		start := time.Now()
		clock := start
		window := 30 * time.Second
		parent := context.Background()

		// Candidate one: the classifier records its landing (recording precedes
		// the identity check), then the projection names another seat, so the
		// tier sees a lost race and moves on.
		ctxOne, landingOne := withHookClaimLanding(parent, func() time.Time { return clock })
		_, ok, err := hookClaimThroughStore(ctxOne, "work-1", "worker-1",
			func() (beads.Bead, bool, error) {
				clock = clock.Add(window + hookClaimMutationTimeout - 5*time.Second)
				return beads.Bead{ID: "work-1", Status: "in_progress", Assignee: "other"}, true, nil
			},
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil },
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-1"}, nil })
		if err != nil || ok {
			t.Fatalf("candidate one = (ok=%v err=%v), want a lost race it skipped", ok, err)
		}
		if landingOne.landingTime().Equal(start) {
			t.Fatal("fixture: candidate one's landing must be recorded for this row to mean anything")
		}

		// Candidate two, exactly as the tier derives it: a FRESH recorder. Its
		// CAS commits past the fence; its own landing must show that, not the
		// early stamp candidate one left behind.
		ctxTwo, landingTwo := withHookClaimLanding(parent, func() time.Time { return clock })
		_, ok, err = hookClaimThroughStore(ctxTwo, "work-2", "worker-1",
			func() (beads.Bead, bool, error) {
				// Lands a second past the fence: candidate one's stamp was 5s
				// inside it, so only its own recorder can answer correctly.
				clock = clock.Add(6 * time.Second)
				return beads.Bead{ID: "work-2", Status: "in_progress", Assignee: "worker-1"}, true, nil
			},
			func(string) (beads.Bead, error) { return beads.Bead{ID: "work-2"}, nil },
			func(string) (beads.Bead, error) {
				return beads.Bead{ID: "work-2", Status: "in_progress", Assignee: "worker-1"}, nil
			})
		if err != nil || !ok {
			t.Fatalf("candidate two = (ok=%v err=%v), want it claimed", ok, err)
		}
		ops := hookClaimOps{InvokedAt: start, ClaimWindow: window}
		if !ops.claimLandingWindowSpent(landingTwo.landingTime()) {
			t.Fatalf("candidate two landed at %v, want it past the fence: a fresh recorder must record its own CAS instant, not reuse candidate one's", landingTwo.landingTime())
		}
		if ops.claimLandingWindowSpent(landingOne.landingTime()) {
			t.Fatal("fixture: candidate one's landing is inside the fence, so only a shared recorder could stand candidate two")
		}
	})
}
