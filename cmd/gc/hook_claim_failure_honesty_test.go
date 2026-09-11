package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// The claim-read failure lanes (D4/D5). A failed read is not an idle store, and
// the whole defect class is that the two were indistinguishable: the hook exited
// silently, no lifecycle event named the cause, and — when the leg that failed
// was the one carrying the federated form — a surviving single-store leg could
// answer for it and mint a false no_work drain.

// failureHookHarness drives claimHookWorkWithRunner with a scripted per-leg
// runner so a row can state what one leg's error does to the whole invocation.
type failureHookHarness struct {
	// answers is consulted per (dir, attempt): the runner pops the next scripted
	// answer for that leg, repeating the last one forever after.
	answers map[string][]hookRunnerAnswer
	calls   map[string]int
	drained bool
	events  []events.Event
}

type hookRunnerAnswer struct {
	out string
	err error
}

func newFailureHookHarness() *failureHookHarness {
	return &failureHookHarness{answers: map[string][]hookRunnerAnswer{}, calls: map[string]int{}}
}

func (h *failureHookHarness) script(dir string, answers ...hookRunnerAnswer) {
	h.answers[dir] = answers
}

func (h *failureHookHarness) run(_, dir string, _ []string) (string, error) {
	n := h.calls[dir]
	h.calls[dir]++
	scripted := h.answers[dir]
	if len(scripted) == 0 {
		return "[]", nil
	}
	if n >= len(scripted) {
		n = len(scripted) - 1
	}
	return scripted[n].out, scripted[n].err
}

func (h *failureHookHarness) ops() hookClaimOps {
	return hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		DrainAck:                 func(io.Writer) error { h.drained = true; return nil },
		ResolveWorkBranch:        func(string) string { return "" },
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		EmitClaimRejected:        func(string, string, string) {},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
	}
}

func (h *failureHookHarness) emitFailure(command string, err error) {
	rec := events.NewFake()
	if emitWorkQueryFailure(rec, "gcs-1", "rig/worker", command, err) {
		h.events = append(h.events, rec.Events...)
	}
}

func failureClaimOptions() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "gc__worker-1",
		IdentityCandidates: []string{"gc__worker-1"},
		RouteTargets:       []string{"rig/worker"},
		Env:                []string{"GC_SESSION_ID=gcs-1", "GC_SESSION_NAME=gc__worker-1"},
		DrainAck:           true,
		JSON:               true,
	}
}

// routedRowJSON is one ready, unassigned, route-matched row as a work query
// would serve it.
func routedRowJSON(id string) string {
	rows, _ := json.Marshal([]beads.Bead{{
		ID:       id,
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "rig/worker"},
	}})
	return string(rows)
}

// inProgressRowJSON is a crash-recovery resume row: this session's own claim,
// already in progress. It is the row C7 says must survive a primary outage.
func inProgressRowJSON(id string) string {
	rows, _ := json.Marshal([]beads.Bead{{
		ID:       id,
		Status:   "in_progress",
		Type:     "task",
		Assignee: "gc__worker-1",
		Metadata: map[string]string{"gc.routed_to": "rig/worker"},
	}})
	return string(rows)
}

func runFailureClaim(t *testing.T, h *failureHookHarness, stores []hookStore) (hookClaimJSONResult, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("work-query", stores[0].dir, stores[0].env, stores, failureClaimOptions(), h.ops(),
		h.run, h.emitFailure, &stdout, &stderr)
	var result hookClaimJSONResult
	if trimmed := strings.TrimSpace(stdout.String()); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			t.Fatalf("decoding claim result %q: %v", trimmed, err)
		}
	}
	return result, code, stderr.String()
}

func withFastClaimRetries(t *testing.T) {
	t.Helper()
	prev := hookClaimQueryRetryInterval
	hookClaimQueryRetryInterval = time.Nanosecond
	t.Cleanup(func() { hookClaimQueryRetryInterval = prev })
}

// TestClaimQueryFailureIsRetriedThenReportedNotDrained is D4's core row: an
// ordinary non-zero work-query exit is a failed read. It must be retried, must
// be recorded on the bus, and — if it persists — must exit non-zero with NO
// drain result and NO drain-ack, so the demand-spawned seat is retained rather
// than converted into a false idle.
func TestClaimQueryFailureIsRetriedThenReportedNotDrained(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")})
	stores := []hookStore{{dir: "/rig", env: []string{"BEADS_DIR=/rig"}}}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a persistent claim-read failure", code)
	}
	if result.Action != "" {
		t.Fatalf("result = %+v, want NO drain result written for a failed read", result)
	}
	if h.drained {
		t.Fatal("drain was acknowledged for a failed read; the seat must be retained")
	}
	if want := 1 + hookClaimQueryRetryAttempts; h.calls["/rig"] != want {
		t.Fatalf("primary-leg reads = %d, want %d (one attempt plus %d bounded retries)", h.calls["/rig"], want, hookClaimQueryRetryAttempts)
	}
	if len(h.events) == 0 {
		t.Fatal("no session.work_query_failed event recorded for an ordinary query failure")
	}
	if h.events[0].Type != events.SessionWorkQueryFailed {
		t.Fatalf("event type = %q, want %q", h.events[0].Type, events.SessionWorkQueryFailed)
	}
	if !strings.Contains(stderr, "exit status 1") {
		t.Fatalf("stderr = %q, want the underlying failure named", stderr)
	}
}

// The retry earns its keep: a read that recovers mid-retry claims, so a
// transient store fault costs seconds instead of a parked seat.
func TestClaimQueryFailureRecoveredMidRetryClaims(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig",
		hookRunnerAnswer{err: errors.New("bd ready: exit status 1")},
		hookRunnerAnswer{out: routedRowJSON("wb-1")},
	)
	stores := []hookStore{{dir: "/rig", env: []string{"BEADS_DIR=/rig"}}}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 after a mid-retry recovery; stderr=%s", code, stderr)
	}
	if result.Action != "work" || result.BeadID != "wb-1" {
		t.Fatalf("result = %+v, want the recovered row claimed", result)
	}
}

// Control: a genuinely EMPTY read is not a failure. It drains no_work, acks, and
// records nothing — the correct-pull outcome that must stay distinguishable from
// the failure above (drain+ack vs exit 1).
func TestClaimQueryEmptyStillDrainsWithoutAnEvent(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{out: "[]"})
	stores := []hookStore{{dir: "/rig", env: []string{"BEADS_DIR=/rig"}}}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 || result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("result = %+v code = %d, want a no_work drain; stderr=%s", result, code, stderr)
	}
	if !h.drained {
		t.Fatal("an idle store must still acknowledge drain")
	}
	if len(h.events) != 0 {
		t.Fatalf("events = %v, want none for a genuinely empty read", h.events)
	}
	if h.calls["/rig"] != 1 {
		t.Fatalf("reads = %d, want exactly 1 (emptiness is not retried)", h.calls["/rig"])
	}
}

// Control: an EXTRA leg's error is best-effort discovery and must not abort the
// invocation — the pre-existing contract, unchanged.
func TestClaimQueryExtraLegErrorDoesNotAbort(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{out: routedRowJSON("wb-1")})
	h.script("/city", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")})
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 || result.BeadID != "wb-1" {
		t.Fatalf("result = %+v code = %d, want the primary's work claimed; stderr=%s", result, code, stderr)
	}
	if len(h.events) != 0 {
		t.Fatalf("events = %v, want none for a best-effort extra-leg error", h.events)
	}
}

// TestFederatedPrimaryFailureIsTerminalNotADowngrade is D5: only the primary leg
// runs the federated form, so when it errors the surviving extras are
// structurally blind to the binding. Their answer must not become the
// invocation's authoritative one — least of all a nil-error empty, which would
// mint a false no_work drain-ack and reap a seat whose demand still exists.
func TestFederatedPrimaryFailureIsTerminalNotADowngrade(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")})
	h.script("/city", hookRunnerAnswer{out: "[]"}) // blind extra: sees nothing
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 1 {
		t.Fatalf("exit = %d, want 1: a blind leg must not answer for a failed federated read; stderr=%s", code, stderr)
	}
	if result.Action == "drain" {
		t.Fatalf("result = %+v, want NO drain — this is the false no_work the downgrade produced", result)
	}
	if h.drained {
		t.Fatal("drain acknowledged after a federated-primary failure")
	}
}

// Same shape, but the blind extra holds unrelated ROUTED work. Claiming it would
// be acting on a partial view of the federation the primary was carrying.
func TestFederatedPrimaryFailureDoesNotClaimFromABlindLeg(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")})
	h.script("/city", hookRunnerAnswer{out: routedRowJSON("unrelated-1")})
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, _ := runFailureClaim(t, h, stores)

	if code != 1 || result.BeadID != "" {
		t.Fatalf("result = %+v code = %d, want no claim from a leg blind to the federation", result, code)
	}
}

// C7 pin: the terminal rule must not regress crash recovery. An extra leg's
// in-progress row is THIS session's own resume work — already claimed, and no
// federated view can change that verdict — so it is served even while the
// primary is down. Anything else from a blind leg is not.
func TestFederatedPrimaryFailureStillResumesOwnInProgressWork(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")})
	h.script("/city", hookRunnerAnswer{out: inProgressRowJSON("resume-1")})
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 {
		t.Fatalf("exit = %d, want 0: crash-recovery resume must survive a primary outage; stderr=%s", code, stderr)
	}
	if result.BeadID != "resume-1" || result.Reason != "existing_assignment" {
		t.Fatalf("result = %+v, want the session's own in-progress row resumed", result)
	}
}

// Control for the row above: with a HEALTHY primary, the extras' single-store
// answers are accepted exactly as they are today.
func TestHealthyPrimaryStillAcceptsExtraLegWork(t *testing.T) {
	withFastClaimRetries(t)
	h := newFailureHookHarness()
	h.script("/rig", hookRunnerAnswer{out: "[]"})
	h.script("/city", hookRunnerAnswer{out: routedRowJSON("city-1")})
	stores := []hookStore{
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}},
		{dir: "/city", env: []string{"BEADS_DIR=/city"}, command: "bd ready --json"},
	}

	result, code, stderr := runFailureClaim(t, h, stores)

	if code != 0 || result.BeadID != "city-1" {
		t.Fatalf("result = %+v code = %d, want the extra leg's work claimed; stderr=%s", result, code, stderr)
	}
}

// TestWorkQueryFailureEventsCoverOrdinaryErrors pins the widening itself: kills
// and timeouts kept their reasons, and an ordinary non-zero exit — previously
// classified un-recordable and therefore invisible — is now a lifecycle fact.
func TestWorkQueryFailureEventsCoverOrdinaryErrors(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		want       bool
		wantReason string
	}{
		{name: "nil", err: nil, want: false},
		{name: "killed", err: errors.New("signal: killed"), want: true, wantReason: "work query killed"},
		{name: "timeout", err: errors.New("running work query \"x\": timed out after 60s"), want: true, wantReason: "work query timed out"},
		{name: "ordinary", err: errors.New("bd ready: exit status 1"), want: true, wantReason: "work query failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := events.NewFake()
			got := emitWorkQueryFailure(rec, "gcs-1", "rig/worker", "cmd", tt.err)
			if got != tt.want {
				t.Fatalf("emitWorkQueryFailure recorded = %v, want %v", got, tt.want)
			}
			if !tt.want {
				if len(rec.Events) != 0 {
					t.Fatalf("events = %v, want none", rec.Events)
				}
				return
			}
			if len(rec.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(rec.Events))
			}
			if !strings.Contains(rec.Events[0].Message, tt.wantReason) {
				t.Fatalf("event message = %q, want it to name %q", rec.Events[0].Message, tt.wantReason)
			}
		})
	}
}
