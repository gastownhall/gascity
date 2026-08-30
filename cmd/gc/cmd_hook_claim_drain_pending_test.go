package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// drainPendingProbe records the session ids a claim asked about and answers with
// a fixed verdict, so a test can prove both WHAT the fence asked and that it
// asked before anything else ran.
type drainPendingProbe struct {
	asked   []string
	pending bool
	err     error
}

func (p *drainPendingProbe) probe(sessionID string) (bool, error) {
	p.asked = append(p.asked, sessionID)
	return p.pending, p.err
}

// drainPendingClaimEnv is one claim invocation with every seam observable: the
// work query, the claim CAS, and the drain acknowledgement. The fence's whole
// contract is about which of these run, so each one records rather than acts.
type drainPendingClaimEnv struct {
	probe      *drainPendingProbe
	queries    int
	claimed    []string
	drainAcked bool
	stdout     bytes.Buffer
	stderr     bytes.Buffer
}

const drainPendingTestSessionID = "gcg-session-904dc4b6bb"

// newDrainPendingClaimEnv builds a claim whose store holds one unassigned,
// route-matched bead — i.e. a seat that WOULD claim if nothing fenced it. That
// is the specimen's situation: a busy repo is exactly where a draining seat
// postpones its own drain forever.
func newDrainPendingClaimEnv() *drainPendingClaimEnv {
	return &drainPendingClaimEnv{probe: &drainPendingProbe{}}
}

func (e *drainPendingClaimEnv) ops() hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			e.queries++
			return `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		DrainPending: e.probe.probe,
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			e.claimed = append(e.claimed, beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		DrainAck: func(io.Writer) error {
			e.drainAcked = true
			return nil
		},
	}
}

func (e *drainPendingClaimEnv) opts(drainAck bool) hookClaimOptions {
	return hookClaimOptions{
		Assignee:     "worker-1",
		SessionID:    drainPendingTestSessionID,
		RouteTargets: []string{"worker"},
		Env:          []string{"GC_SESSION_ID=" + drainPendingTestSessionID},
		DrainAck:     drainAck,
		JSON:         true,
	}
}

func (e *drainPendingClaimEnv) run(drainAck bool) int {
	return doHookClaim("query", "/rig", e.opts(drainAck), e.ops(), &e.stdout, &e.stderr)
}

func (e *drainPendingClaimEnv) result(t *testing.T) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(e.stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, e.stdout.String())
	}
	return result
}

// F1, the load-bearing pin. A seat whose session row says `draining` refuses
// the claim before any MUTATION — no claim CAS, no adoption, no stamp — and
// converts the refusal into the self-drain the row has been waiting for. The
// specimen (seat gcg-session-904dc4b6bb, parked since 14:28) spent three hours
// claiming and executing work while its row said draining, because the claim
// path read the drain state nowhere at all.
//
// The queries==0 assertion below is a property of THIS entry point, where the
// runner is called inline. Production reaches tryHookClaim through
// claimHookWorkWithRunner, which has already run the federated query to select
// a store — so read the assertion as "the fence did not need the query", not as
// a claim that production never pays for one.
func TestHookClaimRefusesADrainingSessionBeforeAnyMutation(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true

	code := e.run(true)

	if code != 0 {
		t.Fatalf("code = %d, want 0 (drain acknowledged); stderr=%s", code, e.stderr.String())
	}
	if e.queries != 0 {
		t.Errorf("work query ran %d times; the fence must not need the query to answer", e.queries)
	}
	if len(e.claimed) != 0 {
		t.Errorf("claim mutations = %v, want none", e.claimed)
	}
	if !e.drainAcked {
		t.Error("drain not acknowledged; the refusal exists to convert the wedge into a self-drain")
	}
	if got := strings.Join(e.probe.asked, ","); got != drainPendingTestSessionID {
		t.Errorf("probe asked about %q, want the session id %q", got, drainPendingTestSessionID)
	}

	result := e.result(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonDrainPending {
		t.Errorf("result = %+v, want action=drain reason=%s", result, hookClaimReasonDrainPending)
	}
	if !result.DrainAcknowledged {
		t.Error("result.DrainAcknowledged = false after a consumed --drain-ack")
	}
	if !result.OK || result.SchemaVersion != "1" || result.Command != hookClaimCommandName {
		t.Errorf("result envelope = %+v, want the shared schema-v1 hook envelope", result)
	}
	if result.BeadID != "" || result.Assignee != "" {
		t.Errorf("result names work (%q/%q); a refusal claims nothing", result.BeadID, result.Assignee)
	}
}

// The 6z contract: an adopted pane's environment may not carry the identity that
// `gc runtime drain-ack` binds through when called bare, so the refusal must
// name the EXPLICIT-argument form. A reminder the agent cannot act on is not a
// reminder.
func TestHookClaimDrainPendingRefusalNamesTheExplicitAckCommand(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true

	e.run(true)

	want := "gc runtime drain-ack " + drainPendingTestSessionID
	if !strings.Contains(e.stderr.String(), want) {
		t.Errorf("stderr = %q, want the explicit-arg command %q", e.stderr.String(), want)
	}
}

// Control. The fence must cost a not-draining seat nothing: it claims exactly
// the work it claimed before, and never acknowledges a drain nobody signaled.
func TestHookClaimNotDrainingSessionClaimsAsBefore(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = false

	code := e.run(true)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if e.queries != 1 {
		t.Errorf("work query ran %d times, want 1", e.queries)
	}
	if got := strings.Join(e.claimed, ","); got != "work-1" {
		t.Fatalf("claim mutations = %q, want work-1", got)
	}
	if e.drainAcked {
		t.Error("drain acknowledged for a healthy seat")
	}
	if result := e.result(t); result.Action != "work" {
		t.Errorf("result = %+v, want a work result", result)
	}
}

// Fail-open pin. The probe is one sqlite read on the agent-turn path, and a
// store hiccup there must not idle every healthy seat in the city — the drain
// lanes remain the backstop exactly as they are today. A fail-CLOSED probe would
// convert one store flap into a city-wide work stoppage, which is strictly worse
// than the wedge this fence exists to close.
func TestHookClaimDrainPendingProbeErrorFailsOpen(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.err = errors.New("session store unavailable")

	code := e.run(true)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if e.queries != 1 {
		t.Errorf("work query ran %d times, want 1 (fail open)", e.queries)
	}
	if got := strings.Join(e.claimed, ","); got != "work-1" {
		t.Fatalf("claim mutations = %q, want work-1 (fail open)", got)
	}
	if e.drainAcked {
		t.Error("drain acknowledged on a probe error")
	}
	if !strings.Contains(e.stderr.String(), "drain-pending probe") ||
		!strings.Contains(e.stderr.String(), "session store unavailable") {
		t.Errorf("stderr = %q, want the probe fault named without refusing the claim", e.stderr.String())
	}
}

// Adoption-ordering pin. The fence runs before the work query, which also puts
// it before hookClaimExistingAssignment. That is deliberate: letting a draining
// seat ADOPT its own already-in_progress bead re-parks the identical wedge — the
// seat resumes work instead of draining, which is exactly how the specimen spent
// three hours "finishing" a queue that never emptied. A draining seat's
// in-flight work belongs to the dead-assignee and reopen lanes AFTER the drain
// completes, not to the seat that is supposed to be leaving.
func TestHookClaimRefusesADrainingSessionHoldingAnExistingAssignment(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true
	ops := e.ops()
	ops.Runner = func(string, string) (string, error) {
		e.queries++
		return `[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`, nil
	}
	opts := e.opts(true)
	opts.IdentityCandidates = []string{"worker-1"}

	code := doHookClaim("query", "/rig", opts, ops, &e.stdout, &e.stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if e.queries != 0 {
		t.Errorf("work query ran %d times; adoption must not outrun the drain fence", e.queries)
	}
	result := e.result(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonDrainPending {
		t.Fatalf("result = %+v, want the drain refusal, not an adopted assignment", result)
	}
}

// Exit contract. Without --drain-ack the refusal is still terminal and still
// carries the schema-backed drain record, but it exits 1 — parity with every
// other writeHookClaimDrain caller, so a wrapper that never acknowledges a drain
// does not read the refusal as success.
func TestHookClaimDrainPendingWithoutDrainAckExitsOne(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true

	code := e.run(false)

	if code != 1 {
		t.Fatalf("code = %d, want 1 without --drain-ack; stderr=%s", code, e.stderr.String())
	}
	if e.drainAcked {
		t.Error("drain acknowledged without --drain-ack")
	}
	result := e.result(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonDrainPending {
		t.Errorf("result = %+v, want action=drain reason=%s", result, hookClaimReasonDrainPending)
	}
	if result.DrainAcknowledged {
		t.Error("result.DrainAcknowledged = true without --drain-ack")
	}
}

// Failing open must not be SILENT.
//
// The same agent-side store fault that blinds this probe also fails open the
// runtime-identity fence, so a persistent one switches both drain fences off
// for every seat in the city while the reconciler keeps marking rows draining —
// the exact wedge this series closes, restored fleet-wide. stderr inside an
// agent pane is not an operator signal; this event is the only thing off-pane
// that says the fence is inert.
func TestHookClaimDrainPendingProbeErrorEmitsTheFenceUnavailableEvent(t *testing.T) {
	type emission struct {
		sessionID string
		template  string
		err       error
	}
	var emitted []emission
	old := hookEmitDrainFenceUnavailable
	hookEmitDrainFenceUnavailable = func(_ io.Writer, sessionID, template string, err error) {
		emitted = append(emitted, emission{sessionID, template, err})
	}
	t.Cleanup(func() { hookEmitDrainFenceUnavailable = old })

	e := newDrainPendingClaimEnv()
	e.probe.err = errors.New("session store unavailable")
	opts := e.opts(true)
	opts.Env = append(opts.Env, "GC_TEMPLATE=beads--gc__run-operator")

	doHookClaim("query", "/rig", opts, e.ops(), &e.stdout, &e.stderr)

	if len(emitted) != 1 {
		t.Fatalf("emitted %d fence-unavailable events, want 1", len(emitted))
	}
	if emitted[0].sessionID != drainPendingTestSessionID {
		t.Errorf("event session = %q, want %q", emitted[0].sessionID, drainPendingTestSessionID)
	}
	if emitted[0].template != "beads--gc__run-operator" {
		t.Errorf("event template = %q, want the seat's template", emitted[0].template)
	}
	if emitted[0].err == nil || !strings.Contains(emitted[0].err.Error(), "session store unavailable") {
		t.Errorf("event err = %v, want the probe fault carried", emitted[0].err)
	}
}

// The control the emission pin needs: a fence that ANSWERED emits nothing, so
// the event means "inert" and not merely "ran".
func TestHookClaimDrainPendingEmitsNoEventWhenTheProbeAnswers(t *testing.T) {
	var emitted int
	old := hookEmitDrainFenceUnavailable
	hookEmitDrainFenceUnavailable = func(io.Writer, string, string, error) { emitted++ }
	t.Cleanup(func() { hookEmitDrainFenceUnavailable = old })

	for _, pending := range []bool{true, false} {
		e := newDrainPendingClaimEnv()
		e.probe.pending = pending
		e.run(true)
	}
	if emitted != 0 {
		t.Fatalf("emitted %d events for probes that answered, want 0", emitted)
	}
}

// A failed acknowledgement must still produce a drain RECORD.
//
// writeHookClaimDrain used to return 1 before writing the JSON line whenever
// the ack errored, so a --json caller got no action at all — the bare-exit-1
// shape a startup wrapper retries forever. That turned a recoverable ack fault
// into an infinite refusal loop on exactly the seat that was trying to leave.
// The seat still exits non-zero (nothing was acknowledged), but its consumer
// now learns that the answer was "drain", not "command failed".
func TestHookClaimDrainPendingWritesTheDrainRecordEvenWhenTheAckFails(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true
	ops := e.ops()
	ops.DrainAck = func(io.Writer) error { return errors.New("drain-ack refused") }

	code := doHookClaim("query", "/rig", e.opts(true), ops, &e.stdout, &e.stderr)

	if code != 1 {
		t.Fatalf("code = %d, want 1; a failed ack is not a completed drain", code)
	}
	result := e.result(t)
	if result.Action != "drain" || result.Reason != hookClaimReasonDrainPending {
		t.Fatalf("result = %+v, want the drain record despite the ack failure", result)
	}
	if result.DrainAcknowledged {
		t.Error("result.DrainAcknowledged = true after a failed ack")
	}
	if !strings.Contains(e.stderr.String(), "drain-ack refused") {
		t.Errorf("stderr = %q, want the ack fault reported", e.stderr.String())
	}
}

// An un-keyed invocation has no row to read, so the fence is a no-op rather than
// a refusal. This is the same shape as the runtime-identity fence's
// un-fenceable arm: a missing identity is not evidence of a drain.
func TestHookClaimDrainPendingSkipsWhenNoSessionIDIsKeyed(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true
	opts := e.opts(true)
	opts.Env = nil

	code := doHookClaim("query", "/rig", opts, e.ops(), &e.stdout, &e.stderr)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if len(e.probe.asked) != 0 {
		t.Errorf("probe asked %v, want no probe without a session id", e.probe.asked)
	}
	if got := strings.Join(e.claimed, ","); got != "work-1" {
		t.Fatalf("claim mutations = %q, want work-1", got)
	}
}
