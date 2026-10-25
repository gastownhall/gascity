//go:build integration

package conversationlane_test

// X2, the kill -9 matrix (DES-O1 §7.7; G-A8's evidence): a lane host, the
// controller's (arm H) or gc session nudge's inline pass (arm C), dies by
// SIGKILL at each crash point (after Propose, after Take, after the effect,
// after Done) under at_least_once and at_most_once, over SQLite and over
// embedded Dolt, on a fake exec runtime that does not deduplicate. Every
// cell checks that no op is lost, that a duplicate follows only an injected
// crash and is recorded on the op, that at_most_once is never delivered
// again, that every delivery is its rendering byte for byte with no
// identifier in it, and that the failpoint fired where it should.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// x2Stages are the crash points, in pass order.
var x2Stages = []string{"propose", "take", "effect", "done"}

// x2Want is one cell's expected evidence for one op.
type x2Want struct {
	arm                      opjournal.Arm
	reason                   string
	attempt, maybe, failures int
}

// x2Delivery is one expected delivery: the ops it carries, in batch order,
// and whether it carries the repeat line.
type x2Delivery struct {
	ops  []opjournal.Op
	note bool
}

var (
	x2Succeeded       = x2Want{arm: opjournal.ArmSucceeded, attempt: 1}
	x2SucceededRetake = x2Want{arm: opjournal.ArmSucceeded, attempt: 2, maybe: 1, failures: 1}
	x2Ambiguous       = x2Want{arm: opjournal.ArmFailed, reason: opjournal.ReasonAmbiguousDelivery, attempt: 1, maybe: 1, failures: 1}
)

func storeArms() []storeArm { return []storeArm{armSQLite, armDolt} }

func TestX2LaneHostKillMatrix(t *testing.T) {
	t.Parallel() // after the tests that measure latency, which run alone
	for _, arm := range storeArms() {
		t.Run(string(arm), func(t *testing.T) {
			for _, stage := range x2Stages {
				for _, g := range []opjournal.Guarantee{opjournal.GuaranteeAtLeastOnce, opjournal.GuaranteeAtMostOnce} {
					t.Run(stage+"/"+string(g), func(t *testing.T) {
						parallelCity(t)
						runX2Host(t, arm, stage, g)
					})
				}
			}
		})
	}
}

// runX2Host is one arm-H cell: the controller's lane host dies at stage.
func runX2Host(t *testing.T, arm storeArm, stage string, g opjournal.Guarantee) {
	c := newLaneCity(t, arm, activityNone)
	payload := fmt.Sprintf("x2 host %s %s alpha", stage, guaranteeWord(g))
	key := "x2-h-" + stage
	var killed *ctlProc
	if stage == "propose" {
		c.startController("")
		c.killController()
	} else {
		killed = c.startController("ctl:" + stage)
	}
	var id string
	switch g {
	case opjournal.GuaranteeAtLeastOnce:
		// The nudge's own op write rings the lane host before the CLI
		// reads the op back to report it, so a host that is up serves
		// the op at once and, on Dolt, can close it before that read
		// lands. Hold the session busy until the nudge returns: the
		// host waits for the boundary and delivers after.
		if killed != nil {
			c.setActivityMode(activityLastNudge)
			c.setActivity(time.Now().Add(time.Hour))
		}
		r := c.nudge("queue", key, payload)
		if killed != nil {
			c.setActivityMode(activityNone)
		}
		if r.code() != 0 || r.stdout != "Queued nudge for "+sessionName+" in the conversation lane\n" {
			t.Fatalf("nudge = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		id = c.cliOpID(key)
	default:
		id = c.propose(noticeRequest("gc:x2-producer", key, payload, g), stage != "propose").ID
	}
	if killed != nil {
		c.waitKilledBySignal(killed, time.Minute)
	}
	c.startController("")
	timeout := time.Minute
	if stage == "take" || stage == "effect" {
		timeout = ctlLeaseRecovery + 90*time.Second
	}
	op := c.waitClosed(timeout, id)[0]
	time.Sleep(settleWindow)
	op = c.get(op.ID)

	want, deliveries := x2Succeeded, []x2Delivery{{ops: []opjournal.Op{op}}}
	switch {
	case stage == "take" && g == opjournal.GuaranteeAtMostOnce:
		want, deliveries = x2Ambiguous, nil
	case stage == "take":
		want, deliveries = x2SucceededRetake, []x2Delivery{{ops: []opjournal.Op{op}, note: true}}
	case stage == "effect" && g == opjournal.GuaranteeAtMostOnce:
		want = x2Ambiguous
	case stage == "effect":
		want, deliveries = x2SucceededRetake, []x2Delivery{{ops: []opjournal.Op{op}}, {ops: []opjournal.Op{op}, note: true}}
	}
	c.assertX2(map[string]x2Want{op.ID: want}, []string{payload}, deliveries)
}

func TestX2CLIKillMatrix(t *testing.T) {
	t.Parallel() // after the tests that measure latency, which run alone
	for _, arm := range storeArms() {
		t.Run(string(arm), func(t *testing.T) {
			for _, tc := range []struct {
				name, stage  string
				rerun, mixed bool
			}{
				{name: "propose/host", stage: "propose"},
				{name: "propose/rerun", stage: "propose", rerun: true},
				{name: "take", stage: "take"},
				{name: "effect", stage: "effect"},
				{name: "done", stage: "done", rerun: true},
				{name: "mixed-take", stage: "take", mixed: true},
				{name: "mixed-effect", stage: "effect", mixed: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					parallelCity(t)
					runX2CLI(t, arm, tc.stage, tc.rerun, tc.mixed)
				})
			}
		})
	}
}

// runX2CLI is one arm-C cell: gc session nudge's inline pass dies at stage
// while the controller is down.
func runX2CLI(t *testing.T, arm storeArm, stage string, rerun, mixed bool) {
	c := newLaneCity(t, arm, activityNone)
	c.startController("")
	c.killController()
	variant := stage
	if mixed {
		variant = "mixed " + stage
	}
	if rerun {
		variant += " rerun"
	}
	payload := fmt.Sprintf("x2 cli %s alpha", variant)
	key := "x2-c-" + strings.ReplaceAll(variant, " ", "-")
	var notice opjournal.Op
	payloads := []string{payload}
	if mixed {
		noticePayload := fmt.Sprintf("x2 cli %s beta", variant)
		payloads = append(payloads, noticePayload)
		notice = c.propose(noticeRequest("gc:x2-producer", key+"-n", noticePayload, opjournal.GuaranteeAtMostOnce), false)
	}
	failpoint := "cli:" + stage
	r := c.nudge("wait-idle", key, payload, failpointEnv+"="+failpoint)
	assertKilledByFailpoint(t, r.state, r.stdout+r.stderr, failpoint)
	id := c.cliOpID(key)
	if rerun {
		before := len(c.deliveries())
		r := c.nudge("wait-idle", key, payload)
		if r.code() != 0 || r.stdout != "Nudged "+sessionName+"\n" || !strings.Contains(r.stderr, "replayed op "+id) {
			t.Fatalf("rerun = %d, %q; stderr %s", r.code(), r.stdout, r.stderr)
		}
		if stage == "done" && len(c.deliveries()) != before {
			t.Fatalf("the rerun of a delivered nudge delivered again: %+v", c.deliveries())
		}
	}
	c.startController("")
	ids := []string{id}
	if mixed {
		ids = append(ids, notice.ID)
	}
	timeout := time.Minute
	if stage == "take" || stage == "effect" {
		timeout = cliLeaseRecovery + 90*time.Second
	}
	c.waitClosed(timeout, ids...)
	time.Sleep(settleWindow)
	prompt := c.get(id)

	want := map[string]x2Want{prompt.ID: x2Succeeded}
	deliveries := []x2Delivery{{ops: []opjournal.Op{prompt}}}
	switch {
	case mixed && stage == "take":
		notice = c.get(notice.ID)
		want = map[string]x2Want{prompt.ID: x2SucceededRetake, notice.ID: x2Ambiguous}
		deliveries = []x2Delivery{{ops: []opjournal.Op{prompt}, note: true}}
	case mixed:
		notice = c.get(notice.ID)
		want = map[string]x2Want{prompt.ID: x2SucceededRetake, notice.ID: x2Ambiguous}
		deliveries = []x2Delivery{{ops: []opjournal.Op{notice, prompt}}, {ops: []opjournal.Op{prompt}, note: true}}
	case stage == "take":
		want[prompt.ID] = x2SucceededRetake
		deliveries = []x2Delivery{{ops: []opjournal.Op{prompt}, note: true}}
	case stage == "effect":
		want[prompt.ID] = x2SucceededRetake
		deliveries = []x2Delivery{{ops: []opjournal.Op{prompt}}, {ops: []opjournal.Op{prompt}, note: true}}
	}
	c.assertX2(want, payloads, deliveries)
}

func guaranteeWord(g opjournal.Guarantee) string {
	if g == opjournal.GuaranteeAtMostOnce {
		return "once"
	}
	return "least"
}

// assertX2 is the pass line of one cell: every op closed with one typed
// outcome as want says, the case's deliveries exactly as deliveries render,
// a repeat only where an op records one, and no identifier in any text.
func (c *laneCity) assertX2(want map[string]x2Want, payloads []string, deliveries []x2Delivery) {
	c.t.Helper()
	ops := c.ops()
	if len(ops) != len(want) {
		c.t.Fatalf("%d ops in the conversation, want %d: %+v", len(ops), len(want), ops)
	}
	for _, op := range ops {
		w, ok := want[op.ID]
		if !ok {
			c.t.Fatalf("unexpected op %+v", op)
		}
		closes := 0
		for _, tr := range op.Transitions {
			if tr.Status == opjournal.StatusClosed {
				closes++
			}
		}
		if op.Status != opjournal.StatusClosed || closes != 1 {
			c.t.Fatalf("op %s is %s with %d closing transitions, want closed once", op.ID, op.Status, closes)
		}
		if op.Outcome.Arm != w.arm || op.Outcome.Reason != w.reason || op.Attempt != w.attempt || op.MaybeDelivered != w.maybe || op.Failures != w.failures {
			c.t.Fatalf("op %s closed %s{%s} attempt %d maybe %d failures %d; want %s{%s} %d/%d/%d",
				op.ID, op.Outcome.Arm, op.Outcome.Reason, op.Attempt, op.MaybeDelivered, op.Failures, w.arm, w.reason, w.attempt, w.maybe, w.failures)
		}
		if w.arm == opjournal.ArmSucceeded {
			r := op.Outcome.Result
			if r.Delivery.Ack != opjournal.AckDelivered || r.Via != "live" || op.Outcome.Degradation != opjournal.DegradationUnfencedDelivery {
				c.t.Fatalf("op %s result %+v degradation %q; want delivered via live, unfenced", op.ID, r, op.Outcome.Degradation)
			}
		}
	}
	var got []delivery
	for _, d := range c.deliveries() {
		if slices.ContainsFunc(payloads, func(p string) bool { return strings.Contains(d.Text, p) }) {
			got = append(got, d)
		}
	}
	if len(got) != len(deliveries) {
		c.t.Fatalf("%d deliveries of the case, want %d: %+v", len(got), len(deliveries), got)
	}
	for i, d := range got {
		if wantText := render(c.t, deliveries[i].note, deliveries[i].ops...); d.Text != wantText {
			c.t.Fatalf("delivery %d = %q, want %q", i, d.Text, wantText)
		}
		if deliveries[i].note {
			for _, op := range deliveries[i].ops {
				if op.MaybeDelivered == 0 {
					c.t.Fatalf("delivery %d repeats op %s, which records no maybe_delivered", i, op.ID)
				}
			}
		}
	}
	c.assertNoIdentifier(ops)
}
