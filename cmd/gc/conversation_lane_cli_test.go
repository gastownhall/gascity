package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const cliLaneTestMessage = "check the deploy"

// cliLaneCity is a lane fixture whose city lists message under [ops]
// authoritative, with gc session nudge's seams pointed at it: the CLI's
// clock is the fixture's, its environment is env, and its ring is recorded.
type cliLaneCity struct {
	*laneFixture
	target nudgeTarget
	env    map[string]string
	// rings are the ring payloads, conv:op; ringReply and ringErr answer
	// them.
	rings     []string
	ringReply string
	ringErr   error
	// events orders the ring against stdout writes ("stdout", "ring").
	events []string
}

func newCLILaneCity(t *testing.T, opts ...laneFixtureOpt) *cliLaneCity {
	t.Helper()
	return newCLILaneCityOver(t, newLaneTestStore(), opts...)
}

func newCLILaneCityOver(t *testing.T, store beads.Store, opts ...laneFixtureOpt) *cliLaneCity {
	t.Helper()
	f := newLaneFixtureOver(t, store, opts...)
	f.cfg.Ops.Authoritative = []string{string(opjournal.KindMessage)}
	c := &cliLaneCity{laneFixture: f, env: map[string]string{}, ringReply: "ok"}
	oldClock, oldLookup, oldRing, oldOpen := cliOpClock, cliOpLookupEnv, cliLaneRing, openNudgeWorkStore
	t.Cleanup(func() {
		cliOpClock, cliOpLookupEnv, cliLaneRing, openNudgeWorkStore = oldClock, oldLookup, oldRing, oldOpen
	})
	// The legacy path queues a nudge through the city's nudges store. The
	// fixture's city is a bare directory, so the real opener resolves the
	// default bd provider and starts a dolt sql-server that outlives the test.
	// Every nudge store opened from here is the fixture's own (lane tests run
	// serially, as the seam requires).
	openNudgeWorkStore = func(string, string) (beads.Store, error) { return store, nil }
	cliOpClock = f.clk
	cliOpLookupEnv = func(k string) (string, bool) { v, ok := c.env[k]; return v, ok }
	// No controller answers by default: the mode is unknown.
	setLaneControllerMode(t, reconcilerLegacy, false)
	cliLaneRing = func(_, conv, opID string) (string, error) {
		c.rings = append(c.rings, conv+":"+opID)
		c.events = append(c.events, "ring")
		return c.ringReply, c.ringErr
	}
	info := f.rowInfo(t)
	c.target = resolveNudgeTargetFromSessionInfo(f.dir, f.cfg, info)
	return c
}

// eventWriter records a write into the city's event order.
type eventWriter struct {
	buf *bytes.Buffer
	c   *cliLaneCity
}

func (w eventWriter) Write(p []byte) (int, error) {
	w.c.events = append(w.c.events, "stdout")
	return w.buf.Write(p)
}

// nudge runs gc session nudge's delivery of msg under key.
func (c *cliLaneCity) nudge(mode nudgeDeliveryMode, key string, asJSON bool, msg string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeKeyed(c.target, c.store, c.sp, msg, mode, key, asJSON, eventWriter{buf: &stdout, c: c}, &stderr)
	return code, stdout.String(), stderr.String()
}

// ops lists every op row of the fixture's store.
func (c *cliLaneCity) ops(t *testing.T) []opjournal.Op {
	t.Helper()
	return cliOpRows(t, c.store)
}

func (c *cliLaneCity) only(t *testing.T) opjournal.Op {
	t.Helper()
	ops := c.ops(t)
	if len(ops) != 1 {
		t.Fatalf("op rows = %d (%+v), want 1", len(ops), ops)
	}
	return ops[0]
}

// busy makes the fake runtime look mid-turn under the quiescence rule.
func (c *cliLaneCity) busy() { c.sp.SetActivity(c.name, time.Now()) }

func (c *cliLaneCity) queuedItems(t *testing.T) int {
	t.Helper()
	pending, _, _, err := listQueuedNudgesForTarget(c.dir, c.target, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("listing queued nudges: %v", err)
	}
	return len(pending)
}

func TestCLILaneNudgePolicy(t *testing.T) {
	for mode, want := range map[nudgeDeliveryMode]opjournal.MessagePolicy{
		nudgeDeliveryWaitIdle:  {Role: opjournal.RolePrompt, When: opjournal.WhenBoundary, Wake: opjournal.WakeIfNeeded},
		nudgeDeliveryImmediate: {Role: opjournal.RolePrompt, When: opjournal.WhenSteer, Wake: opjournal.WakeIfNeeded, OnUnsupported: opjournal.FallbackBoundary},
		nudgeDeliveryQueue:     {Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly},
	} {
		got := cliLaneNudgePolicy(mode)
		if got.Message != want || got.Guarantee != opjournal.GuaranteeAtLeastOnce {
			t.Errorf("cliLaneNudgePolicy(%s) = %+v, want %+v at_least_once", mode, got, want)
		}
	}
}

// With message listed, a nudge is one lane op: no legacy op, no queue item,
// whatever ops.content says.
func TestSessionNudgeLaneWritesOneLaneOp(t *testing.T) {
	for _, gate := range []string{"", "auto", "require"} {
		for _, mode := range []nudgeDeliveryMode{nudgeDeliveryWaitIdle, nudgeDeliveryImmediate, nudgeDeliveryQueue} {
			t.Run(gate+"/"+string(mode), func(t *testing.T) {
				c := newCLILaneCity(t)
				if gate != "" {
					c.env["GC_OPS_CONTENT"] = gate
				}
				code, stdout, stderr := c.nudge(mode, "", true, cliLaneTestMessage)
				if code != 0 {
					t.Fatalf("nudge = %d; stderr=%s", code, stderr)
				}
				op := c.only(t)
				if op.Executor != opjournal.ExecutorLane || op.Kind != opjournal.KindMessage || op.Conversation != c.id || op.Payload != cliLaneTestMessage {
					t.Fatalf("op = %+v, want one lane message op carrying the nudge", op)
				}
				if op.Producer.Principal != legacyop.PrincipalOperator || op.Producer.Actor != opjournal.ActorOperator {
					t.Fatalf("producer = %+v, want the operator", op.Producer)
				}
				want := cliLaneNudgePolicy(mode)
				if m := op.Policy.Message; m.Role != want.Message.Role || m.When != want.Message.When || m.Wake != want.Message.Wake ||
					m.OnUnsupported != opjournal.FallbackBoundary || op.Policy.Guarantee != want.Guarantee {
					t.Fatalf("policy = %+v, want %+v", op.Policy, want)
				}
				if !strings.HasPrefix(op.ClientKey, "k-") {
					t.Fatalf("client key %q, want a minted key", op.ClientKey)
				}
				if n := c.queuedItems(t); n != 0 {
					t.Fatalf("%d queue items, want none", n)
				}
				got := cliOpJSON(t, []byte(stdout))
				if got["op_id"] != op.ID || got["key"] != op.ClientKey {
					t.Fatalf("JSON %v, want op_id %s and key %s", got, op.ID, op.ClientKey)
				}
				contentOpValidate(t, "schemas/session/nudge/result.schema.json", stdout)
			})
		}
	}
	t.Run("an agent's nudge is its conversation's, under its key", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.env["GC_SESSION_ID"] = "gc-caller"
		if code, _, stderr := c.nudge(nudgeDeliveryQueue, "k-agent-1", false, cliLaneTestMessage); code != 0 {
			t.Fatalf("nudge = %d; stderr=%s", code, stderr)
		}
		op := c.only(t)
		if op.Producer.Principal != "conversation:gc-caller" || op.Producer.Actor != opjournal.ActorAgent || op.ClientKey != "k-agent-1" {
			t.Fatalf("op = %+v", op)
		}
	})
}

// With the list empty, the nudge takes OJ-5's path: a legacy op under auto,
// none with the gate off, and no lane op either way.
func TestSessionNudgeListEmptyKeepsOJ5Path(t *testing.T) {
	for _, gate := range []string{"", "auto"} {
		t.Run("gate="+gate, func(t *testing.T) {
			c := newCLILaneCity(t)
			c.cfg.Ops.Authoritative = nil
			c.target = resolveNudgeTargetFromSessionInfo(c.dir, c.cfg, c.targetInfo(t))
			if gate != "" {
				c.env["GC_OPS_CONTENT"] = gate
			}
			code, stdout, stderr := c.nudge(nudgeDeliveryImmediate, "", false, cliLaneTestMessage)
			if code != 0 || stdout != "Nudged "+c.target.agentKey()+"\n" {
				t.Fatalf("nudge = %d, stdout %q; stderr=%s", code, stdout, stderr)
			}
			ops := c.ops(t)
			switch gate {
			case "":
				if len(ops) != 0 {
					t.Fatalf("op rows %+v, want none with the gate off", ops)
				}
			default:
				if len(ops) != 1 || ops[0].Executor != opjournal.ExecutorLegacyCLI {
					t.Fatalf("op rows %+v, want one legacy:cli op", ops)
				}
			}
			if len(c.rings) != 0 {
				t.Fatalf("rings %v on the legacy path", c.rings)
			}
		})
	}
}

func (c *cliLaneCity) targetInfo(t *testing.T) session.Info {
	t.Helper()
	info := c.rowInfo(t)
	return info
}

// A protocol-mode runtime no other host serves takes the legacy path; with a
// host stamp for the row's generation it takes the lane.
func TestSessionNudgeLaneRoutesUnservedSessionsLegacy(t *testing.T) {
	t.Run("unserved protocol runtime", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		prevPoller := startNudgePoller
		startNudgePoller = func(_, _, _ string) error { return nil }
		t.Cleanup(func() { startNudgePoller = prevPoller })
		asks := setLaneControllerMode(t, reconcilerLegacy, false)
		if code, _, stderr := c.nudge(nudgeDeliveryQueue, "", false, cliLaneTestMessage); code != 0 {
			t.Fatalf("nudge = %d; stderr=%s", code, stderr)
		}
		if len(asks.cities) != 0 {
			t.Fatalf("the legacy path asked the controller's mode %d times, want none", len(asks.cities))
		}
		for _, op := range c.ops(t) {
			if op.Executor == opjournal.ExecutorLane {
				t.Fatalf("lane op %+v for an unserved protocol runtime", op)
			}
		}
		pending, _, _, err := listQueuedNudgesForTarget(c.dir, c.target, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("listing queued nudges: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("the legacy path queued %d nudges, want 1", len(pending))
		}
		// The queue's shadow bead lands in the fixture's store, never in a
		// store a real provider would open (and start a server for).
		shadow, open, err := nudgeShadows(beads.NudgesStore{Store: c.store}).Find(pending[0].ID)
		if err != nil || !open || shadow.BeadID != pending[0].BeadID {
			t.Fatalf("queued nudge %s's shadow in the fixture store = %+v (open %v, err %v), want bead %q", pending[0].ID, shadow, open, err, pending[0].BeadID)
		}
	})
	t.Run("no session row", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.target.sessionID = ""
		handled, _ := deliverSessionNudgeLane(c.target, c.store, c.sp, cliLaneTestMessage, nudgeDeliveryQueue, "", false, &bytes.Buffer{}, &bytes.Buffer{})
		if handled {
			t.Fatal("a target with no session row took the lane")
		}
	})
	t.Run("protocol runtime a box host serves", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
		stampHost(t, c.laneFixture, c.laneStore(t), 1, opjournal.KindMessage)
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
		if code != 0 {
			t.Fatalf("nudge = %d; stderr=%s", code, stderr)
		}
		op := c.only(t)
		if op.Executor != opjournal.ExecutorLane || op.Status == opjournal.StatusClosed {
			t.Fatalf("op %+v, want an open lane op the box host delivers", op)
		}
		if !strings.Contains(stdout, "this process cannot type into the session") || len(c.nudges()) != 0 {
			t.Fatalf("stdout %q, %d deliveries; want queued not_live and none", stdout, len(c.nudges()))
		}
	})
}

// An idle runtime takes the nudge from the inline pass: verbatim, once, via
// live, under the CLI's lane.
func TestSessionNudgeLaneInlinePassDelivers(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		c := newCLILaneCity(t)
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-deliver-1", asJSON, cliLaneTestMessage)
		if code != 0 {
			t.Fatalf("nudge = %d; stderr=%s", code, stderr)
		}
		nudges := c.nudges()
		if len(nudges) != 1 || nudges[0].Message != cliLaneTestMessage {
			t.Fatalf("deliveries %+v, want the message verbatim once", nudges)
		}
		op := c.only(t)
		if op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != laneViaLive {
			t.Fatalf("op %+v, want succeeded via live", op)
		}
		byCLI := false
		for _, tr := range op.Transitions {
			if strings.HasPrefix(tr.By, "lane:"+cliLaneHostName+".") {
				byCLI = true
			}
			if strings.HasPrefix(tr.By, "lane:"+laneHostName) {
				t.Fatalf("transition %+v by the controller's lane", tr)
			}
		}
		if !byCLI {
			t.Fatalf("transitions %+v, want the CLI's lane", op.Transitions)
		}
		if asJSON {
			got := cliOpJSON(t, []byte(stdout))
			if got["outcome"] != "delivered" || got["queued"] != false || got["op_id"] != op.ID || got["key"] != "k-deliver-1" {
				t.Fatalf("JSON %v", got)
			}
			contentOpValidate(t, "schemas/session/nudge/result.schema.json", stdout)
		} else if stdout != "Nudged "+c.target.agentKey()+"\n" || strings.Contains(stdout, "k-deliver-1") {
			t.Fatalf("stdout %q", stdout)
		}
		if stderr != "" {
			t.Fatalf("stderr %q, want none on a delivered nudge", stderr)
		}
	}
}

// The CLI's Take holds a 30 s lease.
func TestSessionNudgeLaneLeaseIsTheCLIs(t *testing.T) {
	withLaneFailpoint(t, "cli:take", true)
	c := newCLILaneCity(t)
	takeAt := c.clk.Now()
	if !crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-lease-1", false, cliLaneTestMessage) }) {
		t.Fatal("the take failpoint did not fire")
	}
	op := c.only(t)
	if op.Status != opjournal.StatusExecuting || !op.Lease.Deadline.Equal(takeAt.Add(cliLaneLeaseTTL)) {
		t.Fatalf("op %s with lease %+v, want executing until %s", op.Status, op.Lease, takeAt.Add(cliLaneLeaseTTL))
	}
	if !strings.Contains(op.Lease.Nonce, cliLaneHostName+".") {
		t.Fatalf("lease nonce %q, want the cli host's", op.Lease.Nonce)
	}
}

// A busy runtime leaves the op queued for the controller: exit 0, the
// mid-turn note, and a ring naming the op.
func TestSessionNudgeLaneBusyIsQueued(t *testing.T) {
	c := newCLILaneCity(t)
	c.busy()
	c.ringReply = "no-host"
	code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if len(c.nudges()) != 0 {
		t.Fatalf("deliveries %+v into a busy runtime", c.nudges())
	}
	want := "Queued nudge for " + c.target.agentKey() + " in the conversation lane (the session is mid-turn; the controller delivers it at the next idle boundary)\n"
	if stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	op := c.only(t)
	if len(c.rings) != 1 || c.rings[0] != c.id+":"+op.ID {
		t.Fatalf("rings %v, want one for the op", c.rings)
	}
	if !strings.Contains(stderr, "warning: no controller lane host answered (no-host); op "+op.ID) {
		t.Fatalf("stderr %q, want the no-host warning", stderr)
	}
	c.ringReply = "ok"
	code, stdout, _ = c.nudge(nudgeDeliveryWaitIdle, "", true, cliLaneTestMessage+" again")
	if got := cliOpJSON(t, []byte(stdout)); code != 0 || got["queued"] != true || got["outcome"] != "queued" {
		t.Fatalf("JSON %v (code %d)", got, code)
	}
}

// A session v2 asked to stop at its generation leaves the op queued for the
// controller (V2-18) while the controller's latched mode is v2 or unknown:
// the CLI holds no latch, so its inline pass reads the request by the mode
// the controller answers, live when none answers, and defers the content to
// the lane host.
func TestSessionNudgeLaneStopRequestedIsQueued(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  reconcilerMode
		known bool
	}{
		{"latched v2", reconcilerV2, true},
		// The real query answers an unknown mode as legacy, not known.
		{"mode unknown", reconcilerLegacy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSessionNudgeLaneStopRequestedIsQueued(t, tc.mode, tc.known)
		})
	}
}

func testSessionNudgeLaneStopRequestedIsQueued(t *testing.T, mode reconcilerMode, known bool) {
	t.Helper()
	c := newCLILaneCity(t, laneStopRequestedAt(1))
	asks := setLaneControllerMode(t, mode, known)
	code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if !slices.Equal(asks.cities, []string{c.target.cityPath}) || !slices.Equal(asks.legs, []time.Duration{cliReconcilerModeLeg}) {
		t.Fatalf("mode asks %+v; want one for the target's city %s with legs of %s", asks, c.target.cityPath, cliReconcilerModeLeg)
	}
	if noted := strings.Contains(stderr, "stop request is honored because the controller's session reconciler is unknown"); noted == known {
		t.Fatalf("stderr %q: unknown-mode note %v, want %v", stderr, noted, !known)
	}
	if len(c.nudges()) != 0 {
		t.Fatalf("deliveries %+v into a session v2 asked to stop", c.nudges())
	}
	want := "Queued nudge for " + c.target.agentKey() + " in the conversation lane (the session cannot take input now: stop_requested; the controller delivers it once that clears)\n"
	if stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	op := c.only(t)
	if op.Status != opjournal.StatusSubmitted || len(c.rings) != 1 || c.rings[0] != c.id+":"+op.ID {
		t.Fatalf("op %s, rings %v; want submitted and one ring for it", op.Status, c.rings)
	}
}

// A controller that latched legacy, as after a rollback, makes the CLI read
// a stale v2 stop request as residue at once (V2-18): the inline pass
// delivers, as legacy will never carry the request out.
func TestSessionNudgeLaneStopResidueUnderLatchedLegacyDelivers(t *testing.T) {
	c := newCLILaneCity(t, laneStopRequestedAt(1))
	asks := setLaneControllerMode(t, reconcilerLegacy, true)
	code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if !slices.Equal(asks.cities, []string{c.target.cityPath}) {
		t.Fatalf("mode asks %v; want one for the target's city %s", asks.cities, c.target.cityPath)
	}
	if nudges := c.nudges(); len(nudges) != 1 || nudges[0].Message != cliLaneTestMessage {
		t.Fatalf("deliveries %+v, want the message verbatim once", nudges)
	}
	if op := c.only(t); op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != laneViaLive {
		t.Fatalf("op %+v, want succeeded via live", op)
	}
	if stdout != "Nudged "+c.target.agentKey()+"\n" {
		t.Fatalf("stdout %q", stdout)
	}
}

// Queue mode proposes and rings: no pass, no runtime call, no note, and no
// ask for the controller's mode, which only a pass reads by.
func TestSessionNudgeLaneQueueModeRunsNoPass(t *testing.T) {
	c := newCLILaneCity(t)
	asks := setLaneControllerMode(t, reconcilerLegacy, false)
	calls := len(c.sp.Calls)
	code, stdout, stderr := c.nudge(nudgeDeliveryQueue, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if len(asks.cities) != 0 {
		t.Fatalf("queue mode asked the controller's mode %d times, want none", len(asks.cities))
	}
	if n := len(c.sp.Calls); n != calls {
		t.Fatalf("queue mode made %d runtime calls: %+v", n-calls, c.sp.Calls[calls:])
	}
	if want := "Queued nudge for " + c.target.agentKey() + " in the conversation lane\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	op := c.only(t)
	if op.Status != opjournal.StatusSubmitted || op.Policy.Message.Role != opjournal.RoleNotice || len(c.rings) != 1 {
		t.Fatalf("op %+v, rings %v", op, c.rings)
	}
}

// The ring comes after the Done and the output, once per lane-routed run,
// and never after a refusal.
func TestSessionNudgeLaneRingsAfterThePass(t *testing.T) {
	c := newCLILaneCity(t)
	var opAtRing opjournal.Op
	cliLaneRing = func(_, conv, opID string) (string, error) {
		c.rings = append(c.rings, conv+":"+opID)
		c.events = append(c.events, "ring")
		opAtRing = c.only(t)
		return "ok", nil
	}
	if code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage); code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if len(c.events) != 2 || c.events[0] != "stdout" || c.events[1] != "ring" {
		t.Fatalf("events %v, want stdout then one ring", c.events)
	}
	if opAtRing.Status != opjournal.StatusClosed {
		t.Fatalf("op %s at the ring, want closed: the ring follows the Done", opAtRing.Status)
	}
	c.events, c.rings = nil, nil
	if code, _, _ := c.nudge(nudgeDeliveryWaitIdle, " bad-key", false, cliLaneTestMessage); code != 1 {
		t.Fatalf("a bad key = %d, want refused", code)
	}
	if len(c.rings) != 0 {
		t.Fatalf("rings %v after a refusal", c.rings)
	}
}

// Every cause known before resolution is refused there, before a configured
// named session's row would materialize, with nothing written.
func TestSessionNudgeLaneRefusesBeforeResolving(t *testing.T) {
	for _, tc := range []struct {
		name    string
		store   func(c *cliLaneCity) beads.NudgesStore
		key     string
		payload string
		code    string
		text    string
	}{
		{name: "incapable store", store: func(*cliLaneCity) beads.NudgesStore {
			return beads.NudgesStore{Store: noBatchStore{beads.NewMemStore()}}
		}, payload: "hi", code: cliOpCodeUnavailable, text: "conversation lanes need guarded batches"},
		{name: "no nudge store", store: func(*cliLaneCity) beads.NudgesStore { return beads.NudgesStore{} }, payload: "hi", code: cliOpCodeUnavailable, text: "the nudge store did not open"},
		{name: "invalid key", key: " bad-key", payload: "hi", code: cliOpCodeInvalidKey, text: "invalid --key"},
		{name: "oversized payload", payload: strings.Repeat("x", opjournal.MaxInlinePayload+1), code: cliOpCodeUnavailable, text: "payload too large"},
		{name: "invalid utf-8", payload: "a\xffb", code: cliOpCodeUnavailable, text: "not valid UTF-8"},
		{name: "empty payload", payload: "", code: cliOpCodeUnavailable, text: "require a payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			store := beads.NudgesStore{Store: c.store}
			if tc.store != nil {
				store = tc.store(c)
			}
			var stdout, stderr bytes.Buffer
			req := sessionNudgeOpRequest(tc.payload, nudgeDeliveryWaitIdle, tc.key, true, &stdout, &stderr)
			before := len(c.ops(t))
			err := cliLaneNudgePreflight(c.dir, c.cfg, store, req)
			if !errors.Is(err, errMessageOpRefused) {
				t.Fatalf("preflight = %v, want refused", err)
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != tc.code {
				t.Fatalf("failure code %q, want %q", got, tc.code)
			}
			if !strings.Contains(stderr.String(), "gc session nudge: ") || !strings.Contains(stderr.String(), tc.text) {
				t.Fatalf("stderr %q, want %q", stderr.String(), tc.text)
			}
			if n := len(c.ops(t)); n != before {
				t.Fatalf("op rows %d, want %d: nothing written", n, before)
			}
		})
	}
	t.Run("a request that passes", func(t *testing.T) {
		c := newCLILaneCity(t)
		var out bytes.Buffer
		req := sessionNudgeOpRequest(strings.Repeat("x", opjournal.MaxInlinePayload), nudgeDeliveryWaitIdle, "k-ok", false, &out, &out)
		if err := cliLaneNudgePreflight(c.dir, c.cfg, beads.NudgesStore{Store: c.store}, req); err != nil || out.Len() != 0 {
			t.Fatalf("preflight = %v, output %q", err, out.String())
		}
	})
	t.Run("cliLaneExecutesMessage reads [ops] authoritative", func(t *testing.T) {
		cfg := &config.City{}
		cfg.Ops.Authoritative = []string{"message"}
		if !cliLaneExecutesMessage(cfg) || cliLaneExecutesMessage(&config.City{}) || cliLaneExecutesMessage(nil) {
			t.Fatal("cliLaneExecutesMessage does not read [ops] authoritative")
		}
	})
}

// A key that names an existing op replays it as §3.4 has it.
func TestSessionNudgeLaneKeyReplays(t *testing.T) {
	t.Run("a delivered op replays without a pass", func(t *testing.T) {
		c := newCLILaneCity(t)
		if code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-r1", false, cliLaneTestMessage); code != 0 {
			t.Fatalf("first = %d; %s", code, stderr)
		}
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-r1", true, cliLaneTestMessage)
		got := cliOpJSON(t, []byte(stdout))
		if code != 0 || got["replayed"] != true || got["outcome"] != "delivered" || !strings.Contains(stderr, "replayed op ") {
			t.Fatalf("rerun = %d, %v, stderr %q", code, got, stderr)
		}
		if n := len(c.nudges()); n != 1 {
			t.Fatalf("%d deliveries, want the first run's only", n)
		}
	})
	t.Run("an open op is replayed and its pass runs", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.busy()
		if code, _, _ := c.nudge(nudgeDeliveryWaitIdle, "k-r2", false, cliLaneTestMessage); code != 0 {
			t.Fatal("first run failed")
		}
		c.sp.SetActivity(c.name, time.Now().Add(-time.Minute))
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-r2", false, cliLaneTestMessage)
		if code != 0 || stdout != "Nudged "+c.target.agentKey()+"\n" || !strings.Contains(stderr, "replayed op ") {
			t.Fatalf("rerun = %d, %q, %q", code, stdout, stderr)
		}
		if n := len(c.nudges()); n != 1 {
			t.Fatalf("%d deliveries, want one", n)
		}
	})
	t.Run("a different payload under the key is a mismatch", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.nudge(nudgeDeliveryQueue, "k-r3", false, cliLaneTestMessage)
		code, _, stderr := c.nudge(nudgeDeliveryQueue, "k-r3", false, "something else")
		if code != 1 || !strings.Contains(stderr, "the key names a different request") || !strings.Contains(stderr, "(key k-r3)") {
			t.Fatalf("mismatch = %d, %q", code, stderr)
		}
	})
	t.Run("a failed op does not replay as success", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.nudge(nudgeDeliveryQueue, "k-r4", false, cliLaneTestMessage)
		op := c.only(t)
		st := c.laneStore(t)
		if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonExpiredBeforeStart}, "test"); err != nil {
			t.Fatalf("closing: %v", err)
		}
		code, _, stderr := c.nudge(nudgeDeliveryQueue, "k-r4", false, cliLaneTestMessage)
		if code != 1 || !strings.Contains(stderr, "closed failed{expired_before_start}; rerun without --key") {
			t.Fatalf("replay = %d, %q", code, stderr)
		}
	})
	// gc session submit records its op, legacy:cli under the operator,
	// with the policy a wait-idle lane nudge's request has, so the key it
	// used names that op.
	t.Run("a legacy op is never taken by the lane", func(t *testing.T) {
		c := newCLILaneCity(t)
		st := c.laneStore(t)
		legacy, _, err := st.Propose(opjournal.Request{
			Conversation: c.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyCLI, ClientKey: "k-r5",
			Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
			Payload:  cliLaneTestMessage, Policy: api.MessageOpPolicy(session.SubmitIntentDefault),
		})
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-r5", false, cliLaneTestMessage)
		if code != 1 || !strings.Contains(stderr, "in progress under its legacy path") {
			t.Fatalf("open legacy = %d, %q", code, stderr)
		}
		if _, err := st.Close(legacy.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacyQueued}}, "legacy:cli"); err != nil {
			t.Fatalf("closing: %v", err)
		}
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-r5", false, cliLaneTestMessage)
		if code != 0 || !strings.HasPrefix(stdout, "Queued nudge for ") || !strings.Contains(stderr, "replayed op "+legacy.ID) {
			t.Fatalf("closed legacy = %d, %q, %q", code, stdout, stderr)
		}
		if got := c.only(t); got.Executor != opjournal.ExecutorLegacyCLI {
			t.Fatalf("the legacy op was rewritten: %+v", got)
		}
	})
}

// The CLI does not own an ACP session's input: no runtime observation, the
// not_live note, and no wake.
func TestSessionNudgeLaneACPIsNotLiveFromTheCLI(t *testing.T) {
	c := newCLILaneCity(t, func(meta map[string]string) { meta["transport"] = "acp" })
	calls := len(c.sp.Calls)
	code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; %s", code, stderr)
	}
	for _, call := range c.sp.Calls[calls:] {
		t.Fatalf("runtime call %+v from the CLI into an ACP session", call)
	}
	if !strings.Contains(stdout, "(this process cannot type into the session; the session's lane host delivers it)") {
		t.Fatalf("stdout %q", stdout)
	}
	if row := c.row(t); row.Metadata["wake_request"] != "" {
		t.Fatalf("the CLI requested a wake: %v", row.Metadata)
	}
}

// noBatchStore hides its store's guarded batches: a store that cannot host
// lanes.
type noBatchStore struct{ beads.Store }

// laneFailStore fails the op store's reads or the Done's guarded batch on
// demand.
type laneFailStore struct {
	laneTestStore
	failList bool
	failDone bool
	batches  int
}

func (s *laneFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.failList && q.Metadata["gc.op.open"] != "" {
		return nil, errors.New("store hiccup")
	}
	return s.laneTestStore.List(q)
}

// GuardedBatcherHandle fails every batch after the first (the Take) when
// failDone is set.
func (s *laneFailStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s, true }

func (s *laneFailStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	s.batches++
	if s.failDone && s.batches > 1 {
		// A definitive refusal ends the Done at once, as no retry can land it.
		return beads.GuardedResult{}, fmt.Errorf("store went away: %w", beads.ErrGuardedBatchUnsupported)
	}
	return s.MemStore.ApplyGuarded(ctx, b)
}

func TestSessionNudgeLanePassFailureKeepsTheOp(t *testing.T) {
	t.Run("before the take", func(t *testing.T) {
		store := &laneFailStore{laneTestStore: newLaneTestStore()}
		c := newCLILaneCityOver(t, store)
		store.failList = true
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
		if code != 0 || !strings.HasPrefix(stdout, "Queued nudge for ") {
			t.Fatalf("nudge = %d, %q; %s", code, stdout, stderr)
		}
		op := c.only(t)
		if op.Status != opjournal.StatusSubmitted || !strings.Contains(stderr, "warning: the inline delivery did not run") || !strings.Contains(stderr, "op "+op.ID+" waits for the controller") {
			t.Fatalf("op %s, stderr %q", op.Status, stderr)
		}
	})
	t.Run("at the done", func(t *testing.T) {
		store := &laneFailStore{laneTestStore: newLaneTestStore()}
		c := newCLILaneCityOver(t, store)
		store.failDone = true
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
		if code != 0 || !strings.HasPrefix(stdout, "Queued nudge for ") {
			t.Fatalf("nudge = %d, %q; %s", code, stdout, stderr)
		}
		op := c.only(t)
		if op.Status != opjournal.StatusExecuting || !strings.Contains(stderr, "warning: recording the delivery of op "+op.ID+" failed") || !strings.Contains(stderr, "it may repeat") {
			t.Fatalf("op %s, stderr %q", op.Status, stderr)
		}
	})
}

// cliLaneIdentifiers are the identifiers no delivered text may carry (I14).
func cliLaneIdentifiers(c *cliLaneCity, ops []opjournal.Op) []string {
	ids := []string{c.id, c.token, "#" + cliLaneHostName + ".", "#" + laneHostName + ".", "lane_seq", "attempt"}
	for _, op := range ops {
		ids = append(ids, op.ID, op.ClientKey, op.Producer.Principal)
		if op.DeliveryKey != "" {
			ids = append(ids, op.DeliveryKey, laneDeliveryKeyPrefix+op.DeliveryKey)
		}
		if !op.Lease.IsZero() {
			ids = append(ids, op.Lease.String(), op.Lease.Nonce)
		}
	}
	return ids
}

func assertNoLaneIdentifier(t *testing.T, text string, ids []string) {
	t.Helper()
	for _, id := range ids {
		if id != "" && id != legacyop.PrincipalOperator && strings.Contains(text, id) {
			t.Fatalf("delivered text %q carries identifier %q", text, id)
		}
	}
}

func TestSessionNudgeLaneDeliveredTextCarriesNoIdentifier(t *testing.T) {
	t.Run("prompt", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.env["GC_SESSION_ID"] = "gc-caller"
		c.nudge(nudgeDeliveryWaitIdle, "k-id-1", false, cliLaneTestMessage)
		ops := c.ops(t)
		want, err := opjournal.RenderDelivery(ops, opjournal.TextReminder, false)
		if err != nil {
			t.Fatal(err)
		}
		nudges := c.nudges()
		if len(nudges) != 1 || nudges[0].Message != want {
			t.Fatalf("deliveries %+v, want %q", nudges, want)
		}
		assertNoLaneIdentifier(t, nudges[0].Message, cliLaneIdentifiers(c, ops))
	})
	t.Run("queued notice delivered by the lane host", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.nudge(nudgeDeliveryQueue, "k-id-2", false, cliLaneTestMessage)
		st := c.laneStore(t)
		if _, err := c.pass(context.Background(), t, c.lane(t, st), st, nil); err != nil {
			t.Fatal(err)
		}
		ops := c.ops(t)
		want, _ := opjournal.RenderDelivery(ops, opjournal.TextReminder, false)
		nudges := c.nudges()
		if len(nudges) != 1 || nudges[0].Message != want || !strings.Contains(want, "[operator] "+cliLaneTestMessage) {
			t.Fatalf("deliveries %+v, want %q", nudges, want)
		}
		assertNoLaneIdentifier(t, nudges[0].Message, cliLaneIdentifiers(c, ops))
	})
	t.Run("redelivery after a crash", func(t *testing.T) {
		withLaneFailpoint(t, "cli:effect", true)
		c := newCLILaneCity(t)
		crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-id-3", false, cliLaneTestMessage) })
		c.clk.Advance(cliLaneLeaseTTL + opjournal.LeaseSkew + time.Second)
		st := c.laneStore(t)
		if _, err := c.pass(context.Background(), t, c.lane(t, st), st, nil); err != nil {
			t.Fatal(err)
		}
		ops := c.ops(t)
		want, _ := opjournal.RenderDelivery(ops, opjournal.TextReminder, true)
		nudges := c.nudges()
		if len(nudges) != 2 || nudges[1].Message != want || !strings.Contains(want, opjournal.RedeliveryNote) {
			t.Fatalf("deliveries %+v, want the redelivery %q", nudges, want)
		}
		for _, n := range nudges {
			assertNoLaneIdentifier(t, n.Message, cliLaneIdentifiers(c, ops))
		}
		if ops[0].Attempt != 2 || ops[0].MaybeDelivered != 1 || ops[0].Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("op %+v", ops[0])
		}
	})
}

// The extracted predicate and the planner notices' agree.
func TestLaneServesSessionMatchesServesNotice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol bool
		stamp    int64
		want     bool
	}{
		{name: "keystroke", want: true},
		{name: "protocol unserved", protocol: true},
		{name: "protocol served", protocol: true, stamp: 1, want: true},
		{name: "protocol stamped for another generation", protocol: true, stamp: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			if tc.protocol {
				f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			}
			st := f.laneStore(t)
			if tc.stamp > 0 {
				stampHost(t, f, st, tc.stamp, opjournal.KindMessage)
			}
			view, _ := f.view(t)
			got, err := laneServesSession(f.sp, st, f.name, view)
			if err != nil || got != tc.want {
				t.Fatalf("laneServesSession = %v, %v; want %v", got, err, tc.want)
			}
			h := newNoticeLaneHost(t, f, "message")
			notice, err := h.servesNotice(st, f.name, view)
			if err != nil || notice != got {
				t.Fatalf("servesNotice = %v, %v; laneServesSession %v", notice, err, got)
			}
		})
	}
}

// gc session nudge, with message listed, refuses a lane op it cannot write
// before the configured named session's row materializes: the lane
// preflight runs ahead of the resolution (resolveSessionNudgeTarget).
func TestCmdSessionNudgeLaneRefusesBeforeMaterializing(t *testing.T) {
	newCLIOpHarness(t, nil)
	dir := newCLIOpNamedCityWithoutRow(t)
	cityTOML := filepath.Join(dir, "city.toml")
	data, err := os.ReadFile(cityTOML)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	if err := os.WriteFile(cityTOML, append(data, []byte("\n[ops]\nauthoritative = [\"message\"]\n")...), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := cmdSessionNudge([]string{killPokeSessionIdentity, "hello"}, nudgeDeliveryWaitIdle, "", true, &stdout, &stderr)
	// The file store a test city opens applies guarded batches but names no
	// id prefix, so it cannot open as a lane store.
	if code != 1 || !strings.Contains(stderr.String(), "op journal cannot record this request (opening op store") {
		t.Fatalf("nudge = %d, stderr %q; want the lane refusal", code, stderr.String())
	}
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeUnavailable {
		t.Fatalf("failure code %q, want %s", got, cliOpCodeUnavailable)
	}
	if n := cliOpSessionRows(t, dir); n != 0 {
		t.Fatalf("session rows = %d, want none: the refusal comes before the resolution", n)
	}
}

// laneGetFailStore fails the next fails Gets of bead failID with failErr (a
// store hiccup when nil): a session or runtime row the in-run checks cannot
// read, and that a later read, the legacy path's included, would.
type laneGetFailStore struct {
	laneTestStore
	failID  string
	failErr error
	fails   int
}

func (s *laneGetFailStore) Get(id string) (beads.Bead, error) {
	if s.fails > 0 && id == s.failID {
		s.fails--
		if s.failErr != nil {
			return beads.Bead{}, s.failErr
		}
		return beads.Bead{}, errors.New("store hiccup")
	}
	return s.laneTestStore.Get(id)
}

// nudgeOver runs gc session nudge's delivery over store instead of the
// fixture's.
func (c *cliLaneCity) nudgeOver(store beads.Store, mode nudgeDeliveryMode, key string, asJSON bool, msg string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeKeyed(c.target, store, c.sp, msg, mode, key, asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Every refusal after the target resolved exits 1 with the failure envelope
// and writes nothing: no op, no queue item, no delivery. None falls back to
// the legacy path.
func TestSessionNudgeLaneRefusesInRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *cliLaneCity, store *laneGetFailStore) beads.Store
		text  string
	}{
		{name: "the sessions store cannot open as a lane store", setup: func(_ *testing.T, _ *cliLaneCity, store *laneGetFailStore) beads.Store {
			return noBatchStore{store}
		}, text: "op journal cannot record this request (opening lane store"},
		{name: "the session row cannot be read", setup: func(_ *testing.T, c *cliLaneCity, store *laneGetFailStore) beads.Store {
			store.failID, store.fails = c.id, 1
			return store
		}, text: "op journal cannot record this request (reading session "},
		{name: "the session row is absent", setup: func(_ *testing.T, c *cliLaneCity, store *laneGetFailStore) beads.Store {
			// Both of the view's reads miss the row.
			store.failID, store.failErr, store.fails = c.id, beads.ErrNotFound, 2
			return store
		}, text: "has no session row"},
		{name: "the runtime row cannot be read", setup: func(t *testing.T, c *cliLaneCity, store *laneGetFailStore) beads.Store {
			c.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
			id, err := opjournal.RuntimeRowID("gc", c.id)
			if err != nil {
				t.Fatal(err)
			}
			store.failID, store.fails = id, 1
			return store
		}, text: "runtime row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &laneGetFailStore{laneTestStore: newLaneTestStore()}
			c := newCLILaneCityOver(t, store)
			prevPoller := startNudgePoller
			startNudgePoller = func(_, _, _ string) error { return nil }
			t.Cleanup(func() { startNudgePoller = prevPoller })
			use := tc.setup(t, c, store)
			calls := len(c.nudges())
			code, stdout, stderr := c.nudgeOver(use, nudgeDeliveryWaitIdle, "k-in-run", true, cliLaneTestMessage)
			if code != 1 {
				t.Fatalf("nudge = %d, stdout %q, stderr %q; want refused", code, stdout, stderr)
			}
			if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeUnavailable {
				t.Fatalf("failure code %q, want %s", got, cliOpCodeUnavailable)
			}
			if !strings.Contains(stderr, tc.text) || !strings.Contains(stderr, "(key k-in-run)") {
				t.Fatalf("stderr %q, want %q and the key", stderr, tc.text)
			}
			store.fails = 0
			if ops := c.ops(t); len(ops) != 0 {
				t.Fatalf("op rows %+v, want none", ops)
			}
			if n := c.queuedItems(t); n != 0 {
				t.Fatalf("%d queue items, want none", n)
			}
			if n := len(c.nudges()); n != calls {
				t.Fatalf("%d deliveries, want none", n-calls)
			}
			if len(c.rings) != 0 {
				t.Fatalf("rings %v after a refusal", c.rings)
			}
		})
	}
}

// A nudge whose op the pass closes without success is a failure: exit 1
// and op_failed, never "Nudged".
func TestSessionNudgeLaneClosedSessionFails(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		c := newCLILaneCity(t)
		if err := c.store.Close(c.id); err != nil {
			t.Fatalf("closing the session row: %v", err)
		}
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-closed", asJSON, cliLaneTestMessage)
		if code != 1 || strings.Contains(stdout, "Nudged") {
			t.Fatalf("nudge = %d, stdout %q; stderr %q; want a failure", code, stdout, stderr)
		}
		op := c.only(t)
		if op.Status != opjournal.StatusClosed || op.Outcome.Arm == opjournal.ArmSucceeded {
			t.Fatalf("op %+v, want closed without success", op)
		}
		if !strings.Contains(stderr, "op "+op.ID+" closed ") {
			t.Fatalf("stderr %q", stderr)
		}
		if asJSON {
			if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeFailed {
				t.Fatalf("failure code %q, want %s", got, cliOpCodeFailed)
			}
		}
		if len(c.nudges()) != 0 {
			t.Fatalf("deliveries %+v into a closed session", c.nudges())
		}
	}
}

// The ring stays silent for an op this run closed, whatever answers it.
func TestSessionNudgeLaneRingSilentForAClosedOp(t *testing.T) {
	c := newCLILaneCity(t)
	c.ringReply = "no-host"
	code, _, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
	if code != 0 || stderr != "" {
		t.Fatalf("nudge = %d, stderr %q; want delivered with no warning", code, stderr)
	}
	if len(c.rings) != 1 {
		t.Fatalf("rings %v, want one", c.rings)
	}
}

// Each wait the pass names has its note; an unnamed op or wait has none.
func TestCLILaneQueuedNote(t *testing.T) {
	ifNeeded := opjournal.Op{ID: "gc-op-1", Status: opjournal.StatusSubmitted, Policy: cliLaneNudgePolicy(nudgeDeliveryWaitIdle)}
	liveOnly := opjournal.Op{ID: "gc-op-1", Status: opjournal.StatusSubmitted, Policy: cliLaneNudgePolicy(nudgeDeliveryQueue)}
	executing := opjournal.Op{ID: "gc-op-1", Status: opjournal.StatusExecuting, Policy: cliLaneNudgePolicy(nudgeDeliveryWaitIdle)}
	wait := func(reason opjournal.WaitReason) opjournal.PassResult {
		return opjournal.PassResult{Waits: []opjournal.OpWait{{ID: "gc-op-2", Reason: opjournal.WaitBusy}, {ID: "gc-op-1", Reason: reason, Detail: "kill_pending"}}}
	}
	for _, tc := range []struct {
		name string
		op   opjournal.Op
		res  opjournal.PassResult
		want string
	}{
		{name: "busy", op: ifNeeded, res: wait(opjournal.WaitBusy), want: " (the session is mid-turn; the controller delivers it at the next idle boundary)"},
		{name: "boundary unknown", op: ifNeeded, res: wait(opjournal.WaitBoundaryUnknown), want: " (the session is mid-turn; the controller delivers it at the next idle boundary)"},
		{name: "pending interaction", op: ifNeeded, res: wait(opjournal.WaitPendingInteraction), want: " (the session is waiting on a permission prompt; the controller delivers it after the prompt is answered)"},
		{name: "not running, if_needed", op: ifNeeded, res: wait(opjournal.WaitNotRunning), want: " (the session is not running; the controller wakes it to deliver the nudge)"},
		{name: "dormant, if_needed", op: ifNeeded, res: wait(opjournal.WaitDormant), want: " (the session is not running; the controller wakes it to deliver the nudge)"},
		{name: "not running, live_only", op: liveOnly, res: wait(opjournal.WaitNotRunning), want: " (the session is not running; the nudge is delivered the next time it runs)"},
		{name: "no session, live_only", op: liveOnly, res: wait(opjournal.WaitNoSession), want: " (the session is not running; the nudge is delivered the next time it runs)"},
		{name: "content block", op: ifNeeded, res: wait(opjournal.WaitContentBlock), want: " (the session cannot take input now: kill_pending; the controller delivers it once that clears)"},
		{name: "content hold", op: ifNeeded, res: wait(opjournal.WaitContentHold), want: " (input to the session is on hold; the controller delivers it once the hold is lifted)"},
		{name: "not live", op: ifNeeded, res: wait(opjournal.WaitNotLive), want: " (this process cannot type into the session; the session's lane host delivers it)"},
		{name: "another delivery in flight", op: ifNeeded, res: wait(opjournal.WaitInFlight), want: " (another delivery to the session is in flight; the controller delivers this one after it)"},
		{name: "this op in flight", op: executing, res: wait(opjournal.WaitInFlight), want: " (a delivery of this nudge is in flight; if it does not finish, the controller delivers it again after its lease lapses)"},
		{name: "unavailable", op: ifNeeded, res: wait(opjournal.WaitUnavailable), want: " (the session's runtime did not answer; the controller delivers it once it does)"},
		{name: "incarnation", op: ifNeeded, res: wait(opjournal.WaitIncarnation), want: " (the session's runtime is being replaced; the controller delivers it to the new one)"},
		{name: "after earlier input", op: ifNeeded, res: wait(opjournal.WaitAfterPending), want: " (earlier input to the session goes first; the controller delivers it after that)"},
		{name: "a wait it does not name", op: ifNeeded, res: wait(opjournal.WaitReason("other")), want: ""},
		{name: "no wait for the op", op: ifNeeded, res: opjournal.PassResult{Waits: []opjournal.OpWait{{ID: "gc-op-2", Reason: opjournal.WaitBusy}}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cliLaneQueuedNote(tc.op, tc.res); got != tc.want {
				t.Fatalf("note %q, want %q", got, tc.want)
			}
		})
	}
}

// The notes reach the output from a real pass: a permission prompt and a
// stopped runtime.
func TestSessionNudgeLaneQueuedNotesFromThePass(t *testing.T) {
	t.Run("pending interaction", func(t *testing.T) {
		c := newCLILaneCity(t)
		c.sp.SetPendingInteraction(c.name, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
		if code != 0 || !strings.Contains(stdout, "(the session is waiting on a permission prompt;") {
			t.Fatalf("nudge = %d, stdout %q; stderr %q", code, stdout, stderr)
		}
	})
	t.Run("not running, if_needed", func(t *testing.T) {
		c := newCLILaneCity(t)
		if err := c.sp.Stop(c.name); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
		if code != 0 || !strings.Contains(stdout, "(the session is not running; the controller wakes it to deliver the nudge)") {
			t.Fatalf("nudge = %d, stdout %q; stderr %q", code, stdout, stderr)
		}
	})
}

// A pass that fails before its Take, over an op another lane holds, does
// not claim this run's delivery failed to record; a rerun while that lane
// holds the op notes the op's own delivery in flight.
func TestSessionNudgeLaneHeldOpAttribution(t *testing.T) {
	withLaneFailpoint(t, "cli:take", true)
	store := &laneFailStore{laneTestStore: newLaneTestStore()}
	c := newCLILaneCityOver(t, store)
	if !crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-held", false, cliLaneTestMessage) }) {
		t.Fatal("the take failpoint did not fire")
	}
	if op := c.only(t); op.Status != opjournal.StatusExecuting {
		t.Fatalf("op %s, want executing under the first run's lease", op.Status)
	}
	withLaneFailpoint(t, "", false)
	t.Run("a pass error before the take", func(t *testing.T) {
		store.failList = true
		defer func() { store.failList = false }()
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-held", false, cliLaneTestMessage)
		if code != 0 || !strings.HasPrefix(stdout, "Queued nudge for ") {
			t.Fatalf("nudge = %d, %q; %s", code, stdout, stderr)
		}
		if strings.Contains(stderr, "recording the delivery") || strings.Contains(stderr, "may repeat") {
			t.Fatalf("stderr %q blames this run for a Take it never made", stderr)
		}
		if !strings.Contains(stderr, "warning: the inline delivery did not run") || !strings.Contains(stderr, "another lane host holds it") {
			t.Fatalf("stderr %q", stderr)
		}
	})
	t.Run("a rerun while the op is in flight", func(t *testing.T) {
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-held", false, cliLaneTestMessage)
		if code != 0 || !strings.Contains(stdout, "(a delivery of this nudge is in flight;") {
			t.Fatalf("nudge = %d, stdout %q; stderr %q", code, stdout, stderr)
		}
	})
}

// contractBreakingFake acks a fenced nudge with a receipt whose message id
// is longer than the journal stores.
type contractBreakingFake struct{ runtime.FullFake }

func (f contractBreakingFake) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	if err := f.FullFake.NudgeFenced(ctx, name, content, n); err != nil {
		return err
	}
	if n.Receipt != nil {
		n.Receipt.MessageID = strings.Repeat("m", 300)
	}
	return nil
}

// A Done that landed with the runtime's answer out of contract reports the
// delivery and warns of the breach: the op closed succeeded with the message
// id dropped, so the breach is neither dropped nor blamed on recording the
// delivery.
func TestSessionNudgeLaneContractBreachIsWarned(t *testing.T) {
	c := newCLILaneCity(t)
	var stdout, stderr bytes.Buffer
	sp := contractBreakingFake{runtime.FullFake{Fake: c.sp}}
	code := deliverSessionNudgeKeyed(c.target, c.store, sp, cliLaneTestMessage, nudgeDeliveryWaitIdle, "k-contract", false, &stdout, &stderr)
	if code != 0 || stdout.String() != "Nudged "+c.target.agentKey()+"\n" || len(c.nudges()) != 1 {
		t.Fatalf("nudge = %d, stdout %q, %d deliveries; stderr %q; want delivered once", code, stdout.String(), len(c.nudges()), stderr.String())
	}
	op := c.only(t)
	if op.Outcome.Arm != opjournal.ArmSucceeded || op.Outcome.Degradation != opjournal.DegradationExecutorContract || op.Outcome.Result.Delivery.MessageID != "" {
		t.Fatalf("op outcome %+v, want succeeded{executor_contract} with the message id dropped", op.Outcome)
	}
	want := "warning: the runtime's answer to the delivery of op " + op.ID + " broke the executor contract"
	if got := stderr.String(); !strings.Contains(got, want) || !strings.Contains(got, "delivery message id") || strings.Contains(got, "recording the delivery") {
		t.Fatalf("stderr %q, want %q naming the message id", got, want)
	}
	// --key's rerun reports the first run's outcome, the breach included.
	t.Run("a same-key rerun repeats the warning", func(t *testing.T) {
		code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-contract", false, cliLaneTestMessage)
		if code != 0 || stdout != "Nudged "+c.target.agentKey()+"\n" || len(c.nudges()) != 1 {
			t.Fatalf("rerun = %d, stdout %q, %d deliveries; stderr %q; want the delivery replayed", code, stdout, len(c.nudges()), stderr)
		}
		if !strings.Contains(stderr, "replayed op "+op.ID) || !strings.Contains(stderr, want+" (the op records degradation executor_contract)") {
			t.Fatalf("stderr %q, want the replay and %q", stderr, want)
		}
	})
}

// An inline delivery the runtime could not confirm may have reached the
// session: the Done releases the op for a redelivery marked as a possible
// repeat, and the nudge, reported queued, warns of it. A delivery the
// runtime reports it never sent is released too, but nothing may repeat.
func TestSessionNudgeLaneUnconfirmedDeliveryIsWarned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		warns bool
	}{
		{name: "unconfirmed", err: fmt.Errorf("%w: submit Enter not confirmed", runtime.ErrNudgeDeliveryUnconfirmed), warns: true},
		{name: "not sent", err: fmt.Errorf("%w: pane gone", runtime.ErrNudgeNotSent)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLILaneCity(t)
			c.sp.NudgeErrors = map[string]error{c.name: tc.err}
			code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "", false, cliLaneTestMessage)
			if code != 0 || stdout != "Queued nudge for "+c.target.agentKey()+" in the conversation lane\n" || len(c.nudges()) != 1 {
				t.Fatalf("nudge = %d, stdout %q, %d deliveries; stderr %q; want one attempt, queued", code, stdout, len(c.nudges()), stderr)
			}
			op := c.only(t)
			if op.Status != opjournal.StatusAccepted || op.Attempt != 1 {
				t.Fatalf("op %s attempt %d, want released after attempt 1", op.Status, op.Attempt)
			}
			warned := strings.Contains(stderr, "warning: the delivery of op "+op.ID+" may have reached the session but was not confirmed ("+op.LastError+"); the controller delivers it again after its retry backoff, marked as a possible repeat")
			if maybe := op.MaybeDelivered == op.Attempt; warned != tc.warns || maybe != tc.warns {
				t.Fatalf("warned %v, maybe delivered %v (last error %q), want %v; stderr %q", warned, maybe, op.LastError, tc.warns, stderr)
			}
		})
	}
}
