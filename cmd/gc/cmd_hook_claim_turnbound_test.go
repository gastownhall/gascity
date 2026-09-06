package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		ResolveWorkBranch: func(string) string { return "" },
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

// TestHookClaimWindowBoundsTheClaimWriteChild pins the ctx half of F-B: the
// claim-write child's own deadline is the REMAINING window, not the flat
// mutation timeout. Without it a claim started at second 44 of a 45s window
// keeps a bd subprocess alive for another 10s past the fence.
func TestHookClaimWindowBoundsTheClaimWriteChild(t *testing.T) {
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
	if remaining := time.Until(deadline); remaining > 2*time.Second {
		t.Fatalf("claim-write deadline = %s away, want <= the ~1s remaining window", remaining)
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
