package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// The fence's other door.
//
// F-D guards `gc hook --claim`, but the fleet's dominant workflow does not
// re-enter through it. Every workflows-pack prompt's post-close lifecycle says:
// after closing a bead run plain `gc hook`, and if it returns work carrying the
// same gc.root_bead_id / gc.continuation_group, continue it. No --claim is
// specified, because the continuation sibling was PREASSIGNED to this session at
// claim time — it is already open with this seat as assignee, so discovery lists
// it for a draining seat exactly as it does for a healthy one.
//
// A draining adopt-pr seat therefore closed step N, discovered step N+1, and
// walked into a multi-hour chain without ever touching the fence built to stop
// it — the specimen's "postpones its own drain indefinitely" shape, arriving
// through the pack's own documented instructions.

// preassignedSiblingQuery is the continuation sibling a post-close `gc hook`
// finds: open, already assigned to this seat, same root and group.
const preassignedSiblingQuery = `[{"id":"work-2","status":"open","assignee":"worker-1",` +
	`"metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-1","gc.continuation_group":"review"}}]`

type discoveryDrainEnv struct {
	probe   *drainPendingProbe
	queries int
	stdout  bytes.Buffer
	stderr  bytes.Buffer
}

func newDiscoveryDrainEnv() *discoveryDrainEnv {
	return &discoveryDrainEnv{probe: &drainPendingProbe{}}
}

func (e *discoveryDrainEnv) runner() WorkQueryRunner {
	return func(string, string) (string, error) {
		e.queries++
		return preassignedSiblingQuery, nil
	}
}

func (e *discoveryDrainEnv) run(jsonOut bool) int {
	opts := hookClaimOptions{
		Env:  []string{"GC_SESSION_ID=" + drainPendingTestSessionID, "GC_TEMPLATE=beads--gc__run-operator"},
		JSON: jsonOut,
	}
	ops := hookClaimOps{DrainPending: e.probe.probe}
	return doHookDiscovery("bd ready", "/rig", false, opts, ops, e.runner(), &e.stdout, &e.stderr, hookVisibility{})
}

// A draining seat's plain hook must not hand it the next link in the chain.
func TestHookDiscoveryRefusesToServeWorkToADrainingSeat(t *testing.T) {
	e := newDiscoveryDrainEnv()
	e.probe.pending = true

	code := e.run(false)

	// Exit 1 with no work IS the discovery no-work contract, and the packs'
	// post-close block already routes that answer to `gc runtime drain-ack`.
	// Reusing it means no prompt changes are needed to make the seat leave.
	if code != 1 {
		t.Fatalf("code = %d, want 1 (the no-work answer the packs act on); stderr=%s", code, e.stderr.String())
	}
	if strings.Contains(e.stdout.String(), "work-2") {
		t.Fatalf("stdout = %q, want the preassigned continuation sibling withheld", e.stdout.String())
	}
	if e.queries != 0 {
		t.Errorf("work query ran %d times; the fence answers without it", e.queries)
	}
	if !strings.Contains(e.stderr.String(), "drain pending") {
		t.Errorf("stderr = %q, want the drain named", e.stderr.String())
	}
	if !strings.Contains(e.stderr.String(), "gc runtime drain-ack "+drainPendingTestSessionID) {
		t.Errorf("stderr = %q, want the explicit-arg ack command", e.stderr.String())
	}
}

// The --json caller gets the same schema-backed drain record the claim path
// emits, so one consumer can read both doors.
func TestHookDiscoveryDrainPendingWritesTheDrainRecord(t *testing.T) {
	e := newDiscoveryDrainEnv()
	e.probe.pending = true

	code := e.run(true)

	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(e.stdout.Bytes()), &result); err != nil {
		t.Fatalf("stdout is not a JSON result: %v\n%s", err, e.stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonDrainPending {
		t.Fatalf("result = %+v, want action=drain reason=%s", result, hookClaimReasonDrainPending)
	}
}

// Control: a healthy seat's discovery is untouched — same work, same exit code.
func TestHookDiscoveryServesWorkToAHealthySeat(t *testing.T) {
	e := newDiscoveryDrainEnv()
	e.probe.pending = false

	code := e.run(false)

	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "work-2") {
		t.Fatalf("stdout = %q, want the discovered work", e.stdout.String())
	}
	if e.queries != 1 {
		t.Errorf("work query ran %d times, want 1", e.queries)
	}
}

// Fail-open, with the same off-pane signal the claim door emits. A blind probe
// must not stop a healthy fleet discovering work, and must not do it quietly.
func TestHookDiscoveryProbeErrorFailsOpenAndEmits(t *testing.T) {
	var emitted int
	old := hookEmitDrainFenceUnavailable
	hookEmitDrainFenceUnavailable = func(io.Writer, string, string, error) { emitted++ }
	t.Cleanup(func() { hookEmitDrainFenceUnavailable = old })

	e := newDiscoveryDrainEnv()
	e.probe.err = errors.New("session store unavailable")

	code := e.run(false)

	if code != 0 {
		t.Fatalf("code = %d, want 0 (fail open); stderr=%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "work-2") {
		t.Fatalf("stdout = %q, want discovery to proceed on a probe error", e.stdout.String())
	}
	if emitted != 1 {
		t.Fatalf("emitted %d fence-unavailable events, want 1", emitted)
	}
}

// An --inject invocation is not a discovery answer at all (doHook returns 0
// without reading anything), so the fence has nothing to refuse and must not
// pay for a probe.
func TestHookDiscoveryInjectSkipsTheFence(t *testing.T) {
	e := newDiscoveryDrainEnv()
	e.probe.pending = true

	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=" + drainPendingTestSessionID}}
	ops := hookClaimOps{DrainPending: e.probe.probe}
	code := doHookDiscovery("bd ready", "/rig", true, opts, ops, e.runner(), &e.stdout, &e.stderr, hookVisibility{})

	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if len(e.probe.asked) != 0 {
		t.Fatalf("probe asked %v on an inject invocation, want none", e.probe.asked)
	}
}
