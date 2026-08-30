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

// F1, the load-bearing pin. A seat whose session row says `draining` refuses the
// claim BEFORE the work query — before any read, before any mutation — and
// converts the refusal into the self-drain the row has been waiting for. The
// specimen (seat gcg-session-904dc4b6bb, parked since 14:28) spent three hours
// claiming and executing work while its row said draining, because the claim
// path read the drain state nowhere at all.
func TestHookClaimRefusesADrainingSessionBeforeAnyWorkQuery(t *testing.T) {
	e := newDrainPendingClaimEnv()
	e.probe.pending = true

	code := e.run(true)

	if code != 0 {
		t.Fatalf("code = %d, want 0 (drain acknowledged); stderr=%s", code, e.stderr.String())
	}
	if e.queries != 0 {
		t.Errorf("work query ran %d times; a draining seat must refuse before the query", e.queries)
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
