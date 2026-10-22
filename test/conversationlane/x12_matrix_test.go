//go:build integration

package conversationlane_test

// X12's kill matrix (DES-O0API §15 OJ-8, §8 of the OJ-8 design): every O0
// surface dies at each of its crash points, with and without a same-key
// retry, and each cell checks the op's one outcome, who closed it and
// when, how many times the legacy path ran and what it delivered.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// Surfaces a verb records on (legacyop.SurfaceAPI, SurfaceCLI).
const (
	surfaceAPI = "api"
	surfaceCLI = "cli"
)

// x12Verb is one request X12 sends.
type x12Verb struct {
	name    string
	surface string
	// endpoint is the API route's last element; cli the gc verb's words.
	endpoint string
	cli      []string
	// intent is a /submit's intent.
	intent string
	kind   opjournal.Kind
	// async verbs answer 202 and deliver on their own goroutine.
	async bool
	// shape names the P5 baseline of a message verb; "" for none.
	shape string
	// own verbs run on their own on_demand session; the rest on worker.
	own bool
	// suspended fixtures start suspended (a wake's).
	suspended bool
	// ctlDown runs the request with the controller down (submit's local
	// fallback).
	ctlDown bool
	// via is the op's via on a first execution that succeeds.
	via string
}

// x12Verbs are the matrix's verbs.
var x12Verbs = []x12Verb{
	{name: "submit", surface: surfaceAPI, endpoint: "submit", kind: opjournal.KindMessage, async: true, shape: "api-submit", via: "legacy.delivered"},
	{name: "submit-now", surface: surfaceAPI, endpoint: "submit", intent: "interrupt_now", kind: opjournal.KindMessage, async: true, shape: "api-submit-now", via: "legacy.delivered"},
	{name: "messages", surface: surfaceAPI, endpoint: "messages", kind: opjournal.KindMessage, async: true, shape: "api-messages", via: "legacy.delivered"},
	{name: "stop", surface: surfaceAPI, endpoint: "stop", kind: opjournal.KindInterrupt, own: true, via: "legacy"},
	{name: "wake", surface: surfaceAPI, endpoint: "wake", kind: opjournal.KindWake, own: true, suspended: true, via: "legacy"},
	{name: "suspend", surface: surfaceAPI, endpoint: "suspend", kind: opjournal.KindSuspend, own: true, via: "legacy"},
	{name: "kill", surface: surfaceAPI, endpoint: "kill", kind: opjournal.KindKill, own: true, via: "legacy"},
	{name: "reset", surface: surfaceAPI, endpoint: "reset", kind: opjournal.KindRestart, own: true, via: "legacy"},
	{name: "close", surface: surfaceAPI, endpoint: "close", kind: opjournal.KindClose, own: true, via: "legacy"},
	{name: "cli-wake", surface: surfaceCLI, cli: []string{"session", "wake"}, kind: opjournal.KindWake, own: true, suspended: true, via: "legacy"},
	{name: "cli-suspend", surface: surfaceCLI, cli: []string{"session", "suspend"}, kind: opjournal.KindSuspend, own: true, via: "legacy"},
	{name: "cli-kill", surface: surfaceCLI, cli: []string{"session", "kill"}, kind: opjournal.KindKill, own: true, via: "legacy"},
	{name: "cli-close", surface: surfaceCLI, cli: []string{"session", "close"}, kind: opjournal.KindClose, own: true, via: "legacy"},
	{name: "cli-reset", surface: surfaceCLI, cli: []string{"session", "reset"}, kind: opjournal.KindRestart, own: true, via: "legacy"},
	{name: "cli-nudge-wait", surface: surfaceCLI, cli: []string{"session", "nudge", "--delivery", "wait-idle"}, kind: opjournal.KindMessage, shape: "cli-nudge-wait", via: "legacy.queued"},
	{name: "cli-nudge-queue", surface: surfaceCLI, cli: []string{"session", "nudge", "--delivery", "queue"}, kind: opjournal.KindMessage, shape: "cli-nudge-queue", via: "legacy.queued"},
	{name: "cli-submit-local", surface: surfaceCLI, cli: []string{"session", "submit"}, kind: opjournal.KindMessage, shape: "cli-submit-local", ctlDown: true, via: "legacy.delivered"},
	{name: "cli-mail-notify", surface: surfaceCLI, cli: []string{"mail", "send", "--notify"}, kind: opjournal.KindMessage, shape: "cli-mail-notify", own: true, via: "legacy.queued"},
}

// x12LoadOnlyVerbs are the load's message shapes the matrix does not run.
var x12LoadOnlyVerbs = []x12Verb{
	{name: "submit-followup", surface: surfaceAPI, endpoint: "submit", intent: "follow_up", kind: opjournal.KindMessage, async: true, shape: "api-submit-followup", via: "legacy.delivered"},
	{name: "cli-nudge-immediate", surface: surfaceCLI, cli: []string{"session", "nudge", "--delivery", "immediate"}, kind: opjournal.KindMessage, shape: "cli-nudge-immediate", via: "legacy.delivered"},
}

func verbByName(name string) x12Verb {
	for _, v := range append(append([]x12Verb(nil), x12Verbs...), x12LoadOnlyVerbs...) {
		if v.name == name {
			return v
		}
	}
	panic("no X12 verb " + name)
}

// message reports whether the verb delivers text.
func (v x12Verb) message() bool { return v.kind == opjournal.KindMessage }

// cliArgs are a CLI verb's arguments for s, key and payload. A mail
// notify's payload is its subject; its key is its message's.
func (v x12Verb) cliArgs(s x12Session, key, payload string) []string {
	args := append([]string(nil), v.cli...)
	args = append(args, "--json")
	switch {
	case v.name == "cli-mail-notify":
		return append(args, "-s", payload, s.name, "x12 mail body")
	case v.message():
		if key != "" {
			args = append(args, "--key", key)
		}
		return append(args, s.id, payload)
	default:
		if key != "" {
			args = append(args, "--key", key)
		}
		return append(args, s.id)
	}
}

// x12Points are each surface's crash points.
func (v x12Verb) points() []string {
	switch {
	case v.surface == surfaceCLI:
		return []string{"k1", "k3", "k4"}
	case v.async:
		return []string{"k1", "k2", "k3", "k4", "k5"}
	default:
		return []string{"k1", "k3", "k4", "k5"}
	}
}

// retryArms are a verb's retry arms: a mail notify's key is its message's,
// which no rerun reuses.
func (v x12Verb) retryArms() []bool {
	if v.name == "cli-mail-notify" {
		return []bool{false}
	}
	return []bool{true, false}
}

// effected reports whether the first execution's effect landed at point.
func effected(point string) bool { return point == "k3" || point == "k4" || point == "k5" }

// x12CellSpec is one cell to run.
type x12CellSpec struct {
	verb  x12Verb
	point string
	retry bool
}

func (s x12CellSpec) name() string {
	arm := "none"
	if s.retry {
		arm = "key"
	}
	return s.verb.name + "/" + s.point + "/" + arm
}

// x12GatingCells is the gating tier's subset, per mode.
func x12GatingCells() [][]x12CellSpec {
	v := verbByName
	return [][]x12CellSpec{
		{ // API async
			{v("submit"), "k1", true},
			{v("submit"), "k2", true},
			{v("submit"), "k3", true},
			{v("submit"), "k4", true},
			{v("submit"), "k5", true},
			{v("submit"), "k4", false},
			{v("messages"), "k3", true},
		},
		{ // API sync
			{v("stop"), "k1", true},
			{v("stop"), "k3", true},
			{v("stop"), "k4", true},
			{v("stop"), "k5", true},
			{v("suspend"), "k3", true},
		},
		{ // CLI
			{v("cli-nudge-wait"), "k1", true},
			{v("cli-nudge-wait"), "k3", true},
			{v("cli-nudge-wait"), "k4", true},
			{v("cli-kill"), "k4", true},
		},
	}
}

// x12EvidenceCells is every cell, grouped into the cities that run them.
func x12EvidenceCells() [][]x12CellSpec {
	groups := map[string][]x12CellSpec{}
	order := []string{"api-async", "api-sync-a", "api-sync-b", "cli"}
	for _, v := range x12Verbs {
		g := "cli"
		switch {
		case v.surface == surfaceAPI && v.async:
			g = "api-async"
		case v.surface == surfaceAPI && (v.name == "stop" || v.name == "wake" || v.name == "suspend"):
			g = "api-sync-a"
		case v.surface == surfaceAPI:
			g = "api-sync-b"
		}
		for _, p := range v.points() {
			for _, retry := range v.retryArms() {
				groups[g] = append(groups[g], x12CellSpec{v, p, retry})
			}
		}
	}
	out := make([][]x12CellSpec, 0, len(order))
	for _, g := range order {
		out = append(out, groups[g])
	}
	return out
}

func TestX12KillMatrix(t *testing.T) {
	t.Parallel() // after the load, which measures latency alone
	arm := x12Backend(t)
	baselines := x12Baselines(t)
	groups := x12GatingCells()
	tier := "gating"
	if x12Evidence() {
		groups, tier = x12EvidenceCells(), "evidence"
	}
	for _, mode := range x12Modes {
		for gi, cells := range groups {
			t.Run(fmt.Sprintf("%s/%s/city%d", arm, mode, gi+1), func(t *testing.T) {
				if x12Evidence() {
					t.Parallel()
				} else {
					parallelCity(t)
				}
				runX12City(t, arm, mode, tier, cells, baselines)
			})
		}
	}
}

// runX12City runs cells in order in one city, then, in the evidence tier,
// waits out every op's finish_by and checks P2.
func runX12City(t *testing.T, arm storeArm, mode, tier string, specs []x12CellSpec, baselines map[string]x12Shape) {
	own := 0
	for _, s := range specs {
		if s.verb.own {
			own++
		}
	}
	c := newX12City(t, arm, mode, own, false)
	c.start("")
	var cells []*x12Cell
	for i, s := range specs {
		cell := &x12Cell{
			spec: s, tier: tier, backend: string(arm), mode: mode,
			key:     fmt.Sprintf("x12-%s-%02d", strings.ReplaceAll(s.name(), "/", "-"), i),
			payload: fmt.Sprintf("x12 %s %s %s %s %s alpha", arm, mode, strings.ReplaceAll(s.verb.name, "-", " "), s.point, retryWord(s.retry)),
		}
		ok := t.Run(s.name(), func(t *testing.T) {
			cell.t = t
			c.t = t
			defer func() { c.t = t }()
			c.runCell(cell, baselines)
		})
		c.t = t
		cells = append(cells, cell)
		if !ok {
			// A cell that failed may leave the controller down or armed:
			// bring an unarmed one back for the next cell.
			c.stop()
			c.start("")
		}
	}
	rec := func(line, format string, args ...any) {
		t.Errorf("[%s] "+format, append([]any{line}, args...)...)
		x12Report.problem(tier, string(arm), mode, line, fmt.Sprintf(format, args...))
	}
	c.assertNoOpIdentifier(rec)
	if x12Evidence() {
		c.finalChecks(cells, rec)
	}
	for _, cell := range cells {
		x12Report.addCell(cell)
	}
}

func retryWord(retry bool) string {
	if retry {
		return "r"
	}
	return "n"
}

// x12Cell is one cell's record.
type x12Cell struct {
	t                    *testing.T
	spec                 x12CellSpec
	tier, backend, mode  string
	key, payload         string
	sess                 x12Session
	off                  offsets
	orig, retry          x12Answer
	origOut, retryOut    x12Outcome
	opID                 string
	op                   opjournal.Op
	executions           int
	deliveries           int
	effects              map[string]int
	problems             []x12Problem
	killedRow            beads.Bead
	finishBy             time.Time
	evidenceRerun        *x12Answer
	finalOutcome, closer string
}

// x12Problem is one failed check, under its pass line.
type x12Problem struct{ line, text string }

// fail records a failed check under line and fails the cell.
func (cell *x12Cell) fail(line, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	cell.problems = append(cell.problems, x12Problem{line: line, text: msg})
	cell.t.Errorf("[%s] %s: %s", line, cell.spec.name(), msg)
}

// runCell runs one cell (the OJ-8 design §8.2) and checks it (§8.4).
func (c *x12City) runCell(cell *x12Cell, baselines map[string]x12Shape) {
	t := cell.t
	v := cell.spec.verb
	point := cell.spec.point
	if v.own {
		cell.sess = c.materialize(v.suspended)
	} else {
		cell.sess = c.worker()
	}
	if v.name == "cli-mail-notify" {
		cell.key = "" // the cause key mail:<message id>, read back below
	}
	degradeBefore := 0
	if point == "k5" {
		degradeBefore = c.degradeEvents("close", "")
	}
	fp := fmt.Sprintf("%s:%s:%s", v.surface, point, v.kind)
	var armed *ctlProc
	var extra []string
	switch {
	case v.surface == surfaceAPI:
		armed = c.restart(fp)
	case v.ctlDown:
		c.stop()
	}
	if v.surface == surfaceCLI {
		extra = []string{opsFailpointEnv + "=" + fp}
	}
	cell.off = c.offsets()
	answers := make(chan x12Answer, 1)
	go func() { answers <- c.send(v, cell.sess, cell.key, cell.payload, extra) }()

	switch point {
	case "k1", "k3", "k4":
		if armed != nil {
			c.waitKilledBySignal(armed, 2*time.Minute)
			cell.orig = <-answers
		} else {
			cell.orig = <-answers
			assertKilledByFailpointOf(t, opsFailpointEnv, cell.orig.state, cell.orig.body+cell.orig.stderr, fp)
		}
	case "k2":
		cell.orig = <-answers
		if !cell.orig.ok() || cell.orig.status != 202 {
			t.Fatalf("k2: the request answered %s, want its 202", cell.orig)
		}
		c.waitLogLine(armed, "op "+cell.orig.requestID+" parked after its answer, before its effect", 30*time.Second)
		c.killController()
	case "k5":
		cell.orig = <-answers
		if !cell.orig.ok() {
			t.Fatalf("k5: the request answered %s, want its success", cell.orig)
		}
		opID := cell.orig.opID
		if v.async {
			cell.origOut = c.pollOutcome(cell.orig.requestID, cell.orig.cursor)
			if cell.origOut.state != "succeeded" && cell.origOut.state != "failed" {
				t.Fatalf("k5: outcome %+v, want a terminal one", cell.origOut)
			}
			if cell.origOut.opStatus == string(opjournal.StatusClosed) {
				cell.fail("harness", "k5: the terminal event carries a closed op; the injected close fault did not hold it open")
			}
		} else if cell.orig.opStatus == string(opjournal.StatusClosed) {
			cell.fail("harness", "k5: the answer carries a closed op; the injected close fault did not hold it open")
		}
		c.waitLogLine(armed, "op "+opID+": every close of it fails from now on", 30*time.Second)
		deadline := time.Now().Add(30 * time.Second)
		for c.degradeEvents("close", "") <= degradeBefore && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if n := c.degradeEvents("close", "") - degradeBefore; n != 1 {
			cell.fail("harness", "k5: %d ops.recording_degraded{surface: api, stage: close} events, want 1", n)
		}
		c.killController()
	}
	if v.surface == surfaceAPI {
		c.start("")
	}
	if v.async && (point == "k2" || point == "k4") && cell.orig.requestID != "" {
		cell.origOut = c.outcome(cell.orig.requestID, cell.orig.cursor)
	}
	if v.name == "cli-mail-notify" {
		cell.key = "mail:" + c.mailMessageID(cell.payload)
	}
	cell.opID = operatorOpID(t, cell.sess.id, cell.key)

	// Settle what the first execution set going before the retry reads it.
	if effected(point) {
		switch v.endpoint + strings.Join(v.cli, " ") {
		case "reset", "session reset":
			c.waitRow(cell.sess.id, "restarted", 2*time.Minute, func(b beads.Bead) bool { return generation(b) > cell.sess.gen && c.rowRunning(b) })
		case "wake", "session wake":
			c.waitRow(cell.sess.id, "running", 2*time.Minute, c.rowRunning)
		case "kill", "session kill":
			time.Sleep(3 * time.Second)
			cell.killedRow = c.sessionRow(cell.sess.id)
		}
	}
	if cell.spec.retry {
		// A local fallback's rerun runs with the controller still down: up,
		// gc session submit would go through the API.
		cell.retry = c.send(v, cell.sess, cell.key, cell.payload, nil)
		if v.async && cell.retry.ok() {
			cell.retryOut = c.pollOutcome(cell.retry.requestID, cell.retry.cursor)
		}
		if v.async && cell.retry.ok() && cell.retryOut.state == "pending" {
			cell.fail("P2", "the retry's outcome is still pending after %s", x12OutcomeTimeout)
		}
	}
	if v.ctlDown {
		c.start("")
	}
	c.checkCell(cell, baselines)
}

// mailMessageID is the id of the mail message whose subject is subject.
func (c *x12City) mailMessageID(subject string) string {
	c.t.Helper()
	id, ok := c.mailMessageIDs()[subject]
	if !ok {
		c.t.Fatalf("no mail message with subject %q", subject)
	}
	return id
}

// mailMessageIDs maps every mail message's subject to its id.
func (c *x12City) mailMessageIDs() map[string]string {
	c.t.Helper()
	out := map[string]string{}
	c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
		// Mail messages are ephemeral: they live in the wisps tier.
		rows, err := raw.List(beads.ListQuery{Type: "message", IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			return err
		}
		for _, b := range rows {
			out[b.Title] = b.ID
		}
		return nil
	})
	return out
}

// checkCell runs §8.4's immediate checks.
func (c *x12City) checkCell(cell *x12Cell, baselines map[string]x12Shape) {
	v := cell.spec.verb
	point := cell.spec.point
	retry := cell.spec.retry

	// I4: one op under the key, the derived one.
	n := 0
	for _, op := range c.allOps() {
		if op.Conversation == cell.sess.id && op.ClientKey == cell.key {
			n++
		}
	}
	op, found := c.opByID(cell.opID)
	if !found || n != 1 {
		cell.fail("I4", "%d ops under key %q, op %s found %v; want exactly the derived one", n, cell.key, cell.opID, found)
		return
	}

	// P1: in require, an answered request names an op that exists.
	if cell.mode == "require" {
		for _, a := range []x12Answer{cell.orig, cell.retry} {
			if !a.ok() {
				continue
			}
			if v.surface == surfaceAPI && (a.opID == "" || a.opID != cell.opID) {
				cell.fail("P1", "require answered %s naming op %q, want %s", a, a.opID, cell.opID)
			}
			if v.async && a.requestID != a.opID {
				cell.fail("P1", "require answered request_id %q for op %q", a.requestID, a.opID)
			}
		}
	}

	// The expected op and answers (§8.3).
	exec := 0
	if effected(point) {
		exec = 1
	}
	adopts := v.surface == surfaceAPI && retry
	if adopts && point != "k4" {
		exec++ // the adopted re-execution
	}
	cell.executions = exec
	by, closes, _ := closer(op)
	cell.op = op
	cell.closer = by
	cell.finalOutcome = outcomeText(op)
	cell.finishBy = op.FinishBy
	switch {
	case point == "k4":
		// The first execution's close landed: the op is closed at attempt
		// 0 by its own surface, whatever the arm.
		wantBy := closerAPI
		if v.surface == surfaceCLI {
			wantBy = closerCLI
		}
		if op.Status != opjournal.StatusClosed || op.Attempt != 0 || by != wantBy || closes != 1 {
			cell.fail("P2", "op %s %s attempt %d closed by %q (%d closes); want closed once at attempt 0 by %s", op.ID, outcomeText(op), op.Attempt, by, closes, wantBy)
		}
		c.checkFirstOutcome(cell, op)
		if retry {
			if !cell.retry.ok() || !cell.retry.replayed {
				cell.fail("P2", "the K4 retry answered %s, want a replay", cell.retry)
			}
			if v.async && cell.retryOut.state != "succeeded" {
				cell.fail("P2", "the K4 replay's outcome is %+v, want its re-emitted success", cell.retryOut)
			}
		}
	case adopts:
		// The retry adopts the orphan and runs it again.
		if op.Status != opjournal.StatusClosed || op.Attempt != 1 || by != closerAPI || closes != 1 {
			cell.fail("P2", "op %s %s attempt %d closed by %q (%d closes); want closed once at attempt 1 by %s", op.ID, outcomeText(op), op.Attempt, by, closes, closerAPI)
		}
		if cell.retry.replayed {
			cell.fail("P2", "the retry answered a replay %s, want a fresh execution", cell.retry)
		}
		if v.surface == surfaceAPI && cell.retry.attempt != 1 && cell.retry.answered && cell.retry.ok() {
			cell.fail("P2", "the retry's op is at attempt %d, want 1", cell.retry.attempt)
		}
		c.checkAdoptedOutcome(cell, op)
	case v.surface == surfaceCLI && retry:
		// The CLI never adopts: a rerun within the bound is busy.
		if op.Status == opjournal.StatusClosed || op.Attempt != 0 {
			cell.fail("P2", "op %s %s attempt %d after a CLI crash at %s, want it open at attempt 0", op.ID, outcomeText(op), op.Attempt, point)
		}
		if cell.retry.answered && (cell.retry.status != 1 || cell.retry.code != "op_in_progress") {
			cell.fail("P2", "the rerun answered %s (code %q), want exit 1 op_in_progress", cell.retry, cell.retry.code)
		}
		if !strings.Contains(cell.retry.stderr, op.ID) || !strings.Contains(cell.retry.stderr, cell.key) {
			cell.fail("P2", "the rerun's refusal does not name the op and the key: %s", cell.retry.stderr)
		}
	default:
		// No retry: the orphan waits for the sweeper at attempt 0.
		if op.Status == opjournal.StatusClosed || op.Attempt != 0 {
			cell.fail("P2", "op %s %s attempt %d with no retry after %s, want it open at attempt 0 until the sweeper", op.ID, outcomeText(op), op.Attempt, point)
		}
	}

	// P3 and P5: the effects.
	c.checkEffects(cell, baselines)
}

// checkFirstOutcome checks a first execution's closed outcome.
func (c *x12City) checkFirstOutcome(cell *x12Cell, op opjournal.Op) {
	v := cell.spec.verb
	arm, _ := op.Outcome.Effective()
	if arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != v.via {
		cell.fail("P2", "op %s closed %s, want succeeded via %s", op.ID, outcomeText(op), v.via)
	}
}

// checkAdoptedOutcome checks an adopted re-execution's outcome and answer.
func (c *x12City) checkAdoptedOutcome(cell *x12Cell, op opjournal.Op) {
	v := cell.spec.verb
	arm, reason := op.Outcome.Effective()
	if v.name == "kill" && effected(cell.spec.point) {
		// The re-execution answers by the session's state at the retry.
		row := cell.killedRow
		running := c.rowRunning(row) && generation(row) > cell.sess.gen
		switch {
		case running && (arm != opjournal.ArmFailed || reason != "refused:target_moved" || cell.retry.status != 409):
			cell.fail("P2", "kill re-execution on a restarted session closed %s answering %d, want failed{refused:target_moved} 409", outcomeText(op), cell.retry.status)
		case !running && (arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != v.via || cell.retry.status != 200):
			// X12 finding (OJ-8 design F18 predicted skipped{not_needed}):
			// the operator kill of a row already asleep at the op's
			// generation runs again and succeeds, 200, the class the
			// client saw first.
			cell.fail("P2", "kill re-execution on a killed session closed %s answering %d, want succeeded via %s 200", outcomeText(op), cell.retry.status, v.via)
		}
		return
	}
	if arm != opjournal.ArmSucceeded || op.Outcome.Result.Via != v.via {
		cell.fail("P2", "the adopted op closed %s, want succeeded via %s", outcomeText(op), v.via)
	}
	if !cell.retry.ok() {
		cell.fail("P2", "the adopting retry answered %s, want success", cell.retry)
	}
	if v.async && cell.retryOut.state != "succeeded" {
		cell.fail("P2", "the adopting retry's outcome is %+v, want succeeded", cell.retryOut)
	}
}

// checkEffects is P3 (duplicates only in the declared window, effects =
// executions × the shape's signature) and P5 (each delivery is off's text).
func (c *x12City) checkEffects(cell *x12Cell, baselines map[string]x12Shape) {
	v := cell.spec.verb
	exec := cell.executions
	if exec > 1 && !(v.surface == surfaceAPI && cell.spec.retry && (cell.spec.point == "k3" || cell.spec.point == "k5")) {
		cell.fail("P3", "%d executions outside the declared window (API K3/K5 with a retry)", exec)
	}
	matches := func(d delivery) bool {
		if d.Session != cell.sess.name {
			return false
		}
		if v.shape == "cli-mail-notify" {
			return true
		}
		return strings.Contains(d.Text, cell.payload)
	}
	count := func() []delivery {
		var out []delivery
		for _, d := range c.deliveries()[cell.off.deliveries:] {
			if matches(d) {
				out = append(out, d)
			}
		}
		return out
	}
	var got []delivery
	if v.message() {
		deadline := time.Now().Add(x12EffectWait)
		for got = count(); len(got) < exec && time.Now().Before(deadline); got = count() {
			time.Sleep(250 * time.Millisecond)
		}
	}
	time.Sleep(settleWindow)
	got = count()
	cell.deliveries = len(got)
	cell.effects = effectCounts(c.invocations(), cell.off.invocations, cell.sess.name)
	switch {
	case v.message():
		sh, ok := baselines[v.shape]
		if !ok {
			cell.fail("harness", "no off baseline for shape %s", v.shape)
			return
		}
		if len(got) != exec*sh.deliveries {
			cell.fail("P3", "%d deliveries for %d executions, want %d", len(got), exec, exec*sh.deliveries)
		}
		if want := exec * sh.effects["interrupt"]; v.name == "submit-now" && cell.effects["interrupt"] != want {
			cell.fail("P3", "%d interrupts for %d executions, want %d", cell.effects["interrupt"], exec, want)
		}
		for _, d := range got {
			if want := sh.expected(cell.payload); d.Text != want {
				cell.fail("P5", "delivery %d = %q, want off's %q", d.Seq, d.Text, want)
			}
		}
	case v.kind == opjournal.KindInterrupt:
		sh := baselines["api-stop"]
		if want := exec * sh.effects["interrupt"]; cell.effects["interrupt"] != want {
			cell.fail("P3", "%d interrupts for %d executions, want %d", cell.effects["interrupt"], exec, want)
		}
	}
}

// finalChecks is the evidence tier's end of a city: wait until every op is
// closed or the last finish_by plus x12FinalWait has passed, rerun the CLI
// cells whose op a rerun found busy, then check P2 on every op.
func (c *x12City) finalChecks(cells []*x12Cell, rec func(line, format string, args ...any)) {
	c.t.Helper()
	var last time.Time
	for _, op := range c.allOps() {
		if op.FinishBy.After(last) {
			last = op.FinishBy
		}
	}
	deadline := last.Add(x12FinalWait)
	for {
		open := 0
		for _, op := range c.allOps() {
			if op.Status != opjournal.StatusClosed {
				open++
			}
		}
		if open == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	for _, cell := range cells {
		s := cell.spec
		if s.verb.surface != surfaceCLI || !s.retry || s.point == "k4" {
			continue
		}
		cell.t = c.t
		if s.verb.ctlDown {
			c.stop()
		} else if c.ctl == nil || c.ctl.exited() {
			c.start("")
		}
		a := c.send(s.verb, cell.sess, cell.key, cell.payload, nil)
		cell.evidenceRerun = &a
		if a.status != 1 || a.code != "op_already_failed" {
			cell.fail("P2", "the rerun after finish_by answered %s (code %q), want exit 1 op_already_failed", a, a.code)
		}
	}
	byID := map[string]*x12Cell{}
	for _, cell := range cells {
		byID[cell.opID] = cell
	}
	for _, op := range c.allOps() {
		by, closes, at := closer(op)
		cell := byID[op.ID]
		switch {
		case op.Status != opjournal.StatusClosed || closes != 1:
			rec("P2", "op %s is %s with %d closing transitions, want closed once", op.ID, op.Status, closes)
		case at.After(op.FinishBy.Add(x12FinishGrace)):
			rec("P2", "op %s closed at %s, more than %s past its finish_by %s", op.ID, at.Format(time.RFC3339), x12FinishGrace, op.FinishBy.Format(time.RFC3339))
		}
		if cell == nil {
			continue
		}
		cell.op, cell.closer, cell.finalOutcome = op, by, outcomeText(op)
		s := cell.spec
		orphan := s.point != "k4" && !(s.verb.surface == surfaceAPI && s.retry)
		if !orphan {
			continue
		}
		arm, reason := op.Outcome.Effective()
		if arm != opjournal.ArmFailed || reason != opjournal.ReasonDeadlineInFlight || op.Attempt != 0 {
			rec("P2", "%s: op %s closed %s at attempt %d, want failed{deadline_in_flight} at attempt 0", s.name(), op.ID, outcomeText(op), op.Attempt)
		}
		if by != closerSweep && !(s.verb.surface == surfaceCLI && by == closerCLI) {
			rec("P2", "%s: op %s closed by %q, want the sweeper", s.name(), op.ID, by)
		}
	}
}
