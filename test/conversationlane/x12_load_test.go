//go:build integration

package conversationlane_test

// X12's fault-free load (the OJ-8 design §9): one city per backend runs a
// request mix in rounds off, auto, require, off, auto, require (embedded
// Dolt and the gating tier: the first three; the gating tier with a small
// mix), restarts its controller after
// each recorded round and replays every request whose op closed. It asserts P4 (the op rows and the product's own replay
// projection equal what the client saw), P1 and P5 over every request, and
// measures R1 (refusals and unrecorded requests), R2 (latency on the 202
// path) and R3 (op-row growth).

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// x12StateWait bounds a load's wait for a session to reach a state; embedded
// Dolt is slow to start one.
const x12StateWait = 5 * time.Minute

// x12LoadMix is one round's request counts.
type x12LoadMix struct {
	submit, followUp, submitNow, messages, stop int
	cycles                                      int
	nudgeWait, nudgeImmediate, nudgeQueue       int
	mail, cliSubmit                             int
	provoked                                    []string
}

// The provoked causes R1 classifies.
const (
	provokeUnknown    = "unknown-session"
	provokeEmpty      = "empty-message"
	provokeKeyReuse   = "key-reuse"
	provokeTooLarge   = "payload-too-large"
	provokeBadKey     = "invalid-key"
	provokeNoRow      = "unmaterialized-named"
	provokeCLITooLong = "cli-oversized-nudge"
)

// x12LoadMixFor is the gating or the evidence mix.
func x12LoadMixFor() x12LoadMix {
	if x12Evidence() {
		var provoked []string
		for _, p := range []struct {
			cause string
			n     int
		}{{provokeUnknown, 10}, {provokeEmpty, 5}, {provokeKeyReuse, 5}, {provokeTooLarge, 5}, {provokeBadKey, 5}, {provokeNoRow, 5}, {provokeCLITooLong, 1}} {
			for range p.n {
				provoked = append(provoked, p.cause)
			}
		}
		return x12LoadMix{
			submit: 50, followUp: 10, submitNow: 10, messages: 50, stop: 20, cycles: 6,
			nudgeWait: 30, nudgeImmediate: 10, nudgeQueue: 10, mail: 10, cliSubmit: 20, provoked: provoked,
		}
	}
	return x12LoadMix{submit: 10, messages: 10, stop: 5, cycles: 2, nudgeWait: 5, provoked: []string{provokeUnknown, provokeEmpty, provokeKeyReuse}}
}

// sessionsNeeded is how many on_demand sessions one round uses.
func (m x12LoadMix) sessionsNeeded() int {
	n := m.cycles + m.mail
	for _, p := range m.provoked {
		if p == provokeNoRow {
			n++
		}
	}
	return n
}

// x12LoadReq is one request of the load.
type x12LoadReq struct {
	round   int
	mode    string
	verb    x12Verb
	sess    x12Session
	target  string // the target the request names, when not the session id
	key     string
	payload string
	cause   string // a provoked cause, "" for a well-formed request
	orig    x12Answer
	origOut x12Outcome
	// class is the client's original outcome class.
	class  string
	opID   string
	op     opjournal.Op
	hasOp  bool
	replay x12Answer
	repOut x12Outcome
	// replayClass is the replay's projection class.
	replayClass string
}

func TestX12FaultFreeLoad(t *testing.T) {
	if !x12Evidence() {
		// The gating tier reports latency without asserting it, so its
		// small load runs beside the parallel tests; the evidence's runs
		// alone, before the matrix's cities start.
		t.Parallel()
	}
	arm := x12Backend(t)
	baselines := x12Baselines(t)
	mix := x12LoadMixFor()
	tier := "gating"
	if x12Evidence() {
		tier = "evidence"
	}
	// The evidence runs each mode twice on SQLite. Embedded Dolt runs each
	// once: a recorded round of the evidence mix takes over an hour there
	// (2.5-28 s per recorded API request in the evidence run), so six
	// rounds outlast any reasonable test timeout.
	modes := []string{"off", "auto", "require"}
	if x12Evidence() && arm == armSQLite {
		modes = append(modes, modes...)
	}
	c := newX12City(t, arm, "off", mix.sessionsNeeded()*len(modes), false)
	c.start("")
	record := x12LoadRecord{Tier: tier, Backend: string(arm), R1: map[string]map[string]int{}}
	fail := func(line, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		record.Problem = append(record.Problem, "["+line+"] "+msg)
		t.Errorf("[%s] %s", line, msg)
	}
	defer func() {
		if t.Failed() && len(record.Problem) == 0 {
			record.Problem = append(record.Problem, "[harness] "+t.Name()+" failed outside every recorded check; its log says why")
		}
		x12Report.addLoad(record)
	}()
	var all []*x12LoadReq
	latency := map[string][]time.Duration{}
	// R3 is measured over the recorded rounds: the op rows the store gained
	// from the first one's start to the last one's end, and each round's
	// physical growth, so a recorded round's can be set against the off
	// round before it (the same mix, recording nothing).
	opsBefore := -1
	var rounds []x12RoundGrowth
	for round, mode := range modes {
		c.setMode(mode)
		c.restart("")
		if mode != "off" && opsBefore < 0 {
			opsBefore = len(c.allOps())
		}
		start := c.bindingSize()
		reqs := c.loadRound(round, mode, mix)
		for _, r := range reqs {
			if r.cause == "" && r.verb.async {
				latency[mode+" "+r.verb.endpoint] = append(latency[mode+" "+r.verb.endpoint], r.orig.latency)
			}
		}
		c.waitLoadSettled(reqs, fail)
		if mode != "off" {
			// B: a restart empties the LRU and the in-flight registry, so
			// phase C replays through the op rows.
			c.restart("")
			c.replayRound(reqs, fail)
		}
		rounds = append(rounds, x12RoundGrowth{Mode: mode, Bytes: c.bindingSize() - start})
		all = append(all, reqs...)
	}
	newRows := len(c.allOps()) - opsBefore

	// P4: every closed op of a well-formed recorded request; a mail
	// notify has no replay surface (its key is its message's), so it is
	// counted apart.
	p4 := x12P4{}
	for _, r := range all {
		if r.mode == "off" || r.cause != "" || !r.hasOp {
			continue
		}
		if r.verb.name == "cli-mail-notify" {
			p4.MailNotifyExcluded++
			continue
		}
		if r.op.Status != opjournal.StatusClosed {
			p4.NotReplay++
			if !retryableError(r) {
				p4.OpenUnexplained = append(p4.OpenUnexplained, fmt.Sprintf("%s %s %s: op %s still open after the client saw %s (outcome %q)", r.mode, r.verb.name, r.opID, r.op.Status, r.orig, r.origOut.state))
			}
			continue
		}
		p4.Requests++
		rowClass := opClass(r.op, r.verb.message())
		by, _, _ := closer(r.op)
		if r.class == rowClass && r.replayClass == rowClass {
			p4.Equal++
		} else {
			item := fmt.Sprintf("%s %s %s: client %s, op %s (%s by %s), replay %s", r.mode, r.verb.name, r.opID, r.class, rowClass, outcomeText(r.op), by, r.replayClass)
			if by == closerSweep {
				p4.Late = append(p4.Late, item)
			} else {
				p4.Other = append(p4.Other, item)
			}
		}
		if r.origOut.code != "" && r.repOut.code != "" && r.origOut.code != r.repOut.code {
			p4.CodeDiffs = append(p4.CodeDiffs, fmt.Sprintf("%s: %s then %s (rule 7)", r.opID, r.origOut.code, r.repOut.code))
		}
	}
	if p4.Requests > 0 {
		p4.EqualShare = float64(p4.Equal) / float64(p4.Requests)
	}
	threshold := 0.999
	if !x12Evidence() {
		threshold = 1
	}
	if p4.Requests == 0 || p4.EqualShare < threshold || len(p4.Other) > 0 {
		fail("P4", "%d of %d requests equal (%.4f, want >= %.3f), %d other mismatches: %v", p4.Equal, p4.Requests, p4.EqualShare, threshold, len(p4.Other), p4.Other)
	}
	if len(p4.OpenUnexplained) > 0 {
		fail("P4", "%d ops open with no retryable error to explain them: %v", len(p4.OpenUnexplained), p4.OpenUnexplained)
	}
	x12Report.eval("P4", p4.Requests+p4.NotReplay)
	record.P4 = p4

	// P1 and P5 over every request; R1.
	x12Report.eval("R1", len(all))
	for _, r := range all {
		c.checkLoadRequest(r, baselines, fail)
		surface := r.verb.surface
		k := r.mode + " " + surface
		if record.R1[k] == nil {
			record.R1[k] = map[string]int{}
		}
		record.R1[k][r1Class(r)]++
	}
	c.assertNoOpIdentifier(func(line, format string, args ...any) { fail(line, format, args...) })

	// R2.
	for _, key := range sortedKeys(latency) {
		mode, path, _ := strings.Cut(key, " ")
		record.R2 = append(record.R2, latencyRow(string(arm), mode, path, latency[key]))
	}
	for _, path := range []string{"submit", "messages"} {
		off := latency["off "+path]
		for _, mode := range []string{"auto", "require"} {
			on := latency[mode+" "+path]
			if len(off) == 0 || len(on) == 0 {
				continue
			}
			lo, ln := latencyRow("", "", "", off), latencyRow("", "", "", on)
			record.R2 = append(record.R2, x12Latency{Backend: string(arm), Mode: mode + "-off", Path: path, N: len(on), P50: ln.P50 - lo.P50, P95: ln.P95 - lo.P95, P99: ln.P99 - lo.P99, Max: ln.Max - lo.Max})
		}
	}

	// R3.
	x12Report.eval("R3", 1)
	record.R3 = c.growth(all, newRows, rounds)
	if record.R3.Recorded != record.R3.NewRows {
		fail("R3", "%d recorded requests, but the store gained %d op rows over the recorded rounds; want one each", record.R3.Recorded, record.R3.NewRows)
	}
}

// retryableError reports whether r's client saw the retryable error after
// which the API leaves a client-keyed op open for a same-key retry (DES-O0API
// D10): a sync verb's error answer, or an async verb's failed outcome. The
// CLI closes every op it records.
func retryableError(r *x12LoadReq) bool {
	switch {
	case r.verb.surface != surfaceAPI:
		return false
	case r.verb.async:
		return r.orig.ok() && r.origOut.state == "failed"
	default:
		return r.orig.answered && !r.orig.ok()
	}
}

// x12RoundGrowth is one load round's physical growth of the sessions
// binding, from its start to the end of its replays.
type x12RoundGrowth struct {
	Mode  string `json:"mode"`
	Bytes int64  `json:"bytes"`
}

// loadRound runs one round's phase A.
func (c *x12City) loadRound(round int, mode string, mix x12LoadMix) []*x12LoadReq {
	c.t.Helper()
	w := c.worker()
	var reqs []*x12LoadReq
	n := 0
	add := func(v x12Verb, s x12Session, cause string) *x12LoadReq {
		n++
		r := &x12LoadReq{
			round: round, mode: mode, verb: v, sess: s, cause: cause,
			key:     fmt.Sprintf("x12-load-%d-%03d", round, n),
			payload: fmt.Sprintf("x12 load %s %s %d %d alpha", mode, strings.ReplaceAll(v.name, "-", " "), round, n),
		}
		reqs = append(reqs, r)
		return r
	}
	run := func(r *x12LoadReq) {
		target := r.sess
		if r.target != "" {
			target.id = r.target
		}
		r.orig = c.send(r.verb, target, r.key, r.payload, nil)
		if !r.verb.async {
			r.class = clientClass(r.verb, r.orig, r.origOut)
		}
	}
	for _, spec := range []struct {
		verb string
		n    int
	}{
		{"submit", mix.submit},
		{"submit-followup", mix.followUp},
		{"submit-now", mix.submitNow},
		{"messages", mix.messages},
		{"cli-nudge-wait", mix.nudgeWait},
		{"cli-nudge-immediate", mix.nudgeImmediate},
		{"cli-nudge-queue", mix.nudgeQueue},
	} {
		for range spec.n {
			run(add(verbByName(spec.verb), w, ""))
		}
	}
	for range mix.cliSubmit {
		r := add(verbByName("cli-submit-api"), w, "")
		r.key = "" // the CLI mints the API's key
		run(r)
	}
	for range mix.stop {
		run(add(verbByName("stop"), w, ""))
	}
	for range mix.mail {
		rcpt := c.materialize(false)
		r := add(verbByName("cli-mail-notify"), rcpt, "")
		r.key = ""
		run(r)
	}
	for i := range mix.cycles {
		prefix := ""
		if i%2 == 1 {
			prefix = "cli-"
		}
		name := c.sessions[c.used]
		c.used++
		// The first wake names the configured session, which materializes
		// its row; every later request names the row's bead id.
		first := add(verbByName(prefix+"wake"), x12Session{name: name, id: name}, "")
		run(first)
		row, ok := c.sessionByName(name)
		if !ok {
			c.t.Fatalf("the cycle's wake did not materialize %s: %s", name, first.orig)
		}
		s := x12Session{name: name, id: row.ID, gen: generation(row)}
		first.sess = s
		first.target = name
		c.waitRow(s.id, "running", x12StateWait, c.rowRunning)
		for _, step := range []string{"suspend", "wake", "reset", "kill", "close"} {
			before := c.sessionRow(s.id)
			run(add(verbByName(prefix+step), s, ""))
			switch step {
			case "suspend":
				c.waitRow(s.id, "suspended", x12StateWait, c.rowStopped)
			case "wake":
				c.waitRow(s.id, "running", x12StateWait, c.rowRunning)
			case "reset":
				c.waitRow(s.id, "restarted", x12StateWait, func(b beads.Bead) bool { return c.rowRunning(b) && generation(b) > generation(before) })
			case "kill":
				time.Sleep(3 * time.Second)
			}
		}
	}
	for _, cause := range mix.provoked {
		switch cause {
		case provokeUnknown:
			r := add(verbByName("submit"), x12Session{name: "none", id: "gcg-x12-nosuch"}, cause)
			run(r)
		case provokeEmpty:
			r := add(verbByName("submit"), w, cause)
			r.payload = ""
			run(r)
		case provokeKeyReuse:
			first := add(verbByName("submit"), w, "")
			run(first)
			r := add(verbByName("submit"), w, cause)
			r.key = first.key
			run(r)
		case provokeTooLarge:
			r := add(verbByName("submit"), w, cause)
			r.payload = "x12 large " + strings.Repeat("y", opjournal.MaxInlinePayload)
			run(r)
		case provokeBadKey:
			r := add(verbByName("submit"), w, cause)
			// Over the journal's 512-byte key limit, and unique: the
			// Idempotency-Key LRU refuses a key reused with another body.
			r.key = fmt.Sprintf("%s-%03d", strings.Repeat("k", 513), n)
			run(r)
		case provokeNoRow:
			name := c.sessions[c.used]
			c.used++
			r := add(verbByName("submit"), x12Session{name: name, id: name}, cause)
			run(r)
		case provokeCLITooLong:
			r := add(verbByName("cli-nudge-wait"), w, cause)
			r.payload = "x12 large " + strings.Repeat("z", opjournal.MaxInlinePayload)
			run(r)
		}
	}
	// The async requests' outcomes, polled once every request is sent, so
	// the sends are not paced by the polls.
	var mail map[string]string
	for _, r := range reqs {
		if r.verb.name == "cli-mail-notify" {
			mail = c.mailMessageIDs()
			break
		}
	}
	for _, r := range reqs {
		if r.verb.async {
			if r.orig.ok() {
				r.origOut = c.pollOutcome(r.orig.requestID, r.orig.cursor)
			}
			r.class = clientClass(r.verb, r.orig, r.origOut)
		}
		if r.key == "" && r.verb.name == "cli-mail-notify" {
			id, ok := mail[r.payload]
			if !ok {
				c.t.Fatalf("no mail message with subject %q", r.payload)
			}
			r.key = "mail:" + id
		}
	}
	return reqs
}

// clientClass is a request's outcome class as its client saw it.
func clientClass(v x12Verb, a x12Answer, out x12Outcome) string {
	switch {
	case !a.answered:
		return "no-answer"
	case v.async && a.ok():
		if out.state == "succeeded" || out.state == "failed" {
			return out.class
		}
		return "outcome-" + out.state
	case a.ok() && v.message():
		return cliMessageClass(a)
	case a.ok():
		return classSucceeded
	default:
		return classFailed
	}
}

// cliMessageClass is a CLI message verb's success class: delivered or
// queued, as its --json result says.
func cliMessageClass(a x12Answer) string {
	if a.queued == nil {
		return "succeeded-without-queued-flag"
	}
	return deliveredClass(*a.queued)
}

// waitLoadSettled waits until every recorded request's op that should close
// is closed (all but those a retryable error leaves open until their
// finish_by) and the deliveries stop, then reads each request's op. An op
// still open at the deadline fails P4.
func (c *x12City) waitLoadSettled(reqs []*x12LoadReq, fail func(line, format string, args ...any)) {
	c.t.Helper()
	for _, r := range reqs {
		r.opID = c.loadOpID(r)
	}
	// One store read per poll: an embedded-Dolt handle is slow to open.
	var ops map[string]opjournal.Op
	deadline := time.Now().Add(x12StateWait)
	for {
		ops = c.opsByID()
		var open []string
		for _, r := range reqs {
			if op, ok := ops[r.opID]; ok && op.Status != opjournal.StatusClosed && !retryableError(r) {
				open = append(open, r.verb.name+" "+r.opID)
			}
		}
		if len(open) == 0 {
			break
		}
		if time.Now().After(deadline) {
			fail("P4", "%d ops still open %s after their round: %v", len(open), x12StateWait, open)
			break
		}
		time.Sleep(time.Second)
	}
	for last := -1; last != len(c.deliveries()); {
		last = len(c.deliveries())
		time.Sleep(settleWindow)
	}
	ops = c.opsByID()
	for _, r := range reqs {
		if r.opID != "" {
			r.op, r.hasOp = ops[r.opID]
		}
	}
}

// loadOpID is the id of r's op: the answer's, else the one its key derives.
func (c *x12City) loadOpID(r *x12LoadReq) string {
	if r.orig.opID != "" {
		return r.orig.opID
	}
	switch r.cause {
	case provokeUnknown, provokeEmpty, provokeKeyReuse:
		// Refused at the front door: a reused key's op is the first
		// request's.
		return ""
	}
	if r.key == "" || r.sess.id == "" {
		return ""
	}
	conv := r.sess.id
	if r.cause == provokeNoRow {
		row, ok := c.sessionByName(r.sess.name)
		if !ok {
			return ""
		}
		conv = row.ID
	}
	if opjournal.ValidateClientKey(r.key) != nil {
		return ""
	}
	return operatorOpID(c.t, conv, r.key)
}

// replayRound is phase C: replay every request whose op closed, same key,
// endpoint and target, and check the replay is marked and does nothing.
func (c *x12City) replayRound(reqs []*x12LoadReq, fail func(line, format string, args ...any)) {
	c.t.Helper()
	before := c.offsets()
	for _, r := range reqs {
		if !r.hasOp || r.cause != "" || r.op.Status != opjournal.StatusClosed {
			continue
		}
		target := r.sess
		if r.target != "" {
			// The replay names the bead id: by name, a closed row's replay
			// is refused, or reaches a fresh row (rule 8).
			target.id = r.sess.id
		}
		key := r.key
		if r.verb.name == "cli-submit-api" {
			key = r.op.ClientKey
		}
		if r.verb.name == "cli-mail-notify" {
			// A notify's key is its message's: the CLI has no verb that
			// replays it, and P4 counts it apart.
			continue
		}
		r.replay = c.send(r.verb, target, key, r.payload, nil)
		if r.verb.async && r.replay.ok() {
			r.repOut = c.pollOutcome(r.replay.requestID, r.replay.cursor)
		}
		r.replayClass = replayClass(r)
	}
	time.Sleep(settleWindow)
	after := c.offsets()
	if after.deliveries != before.deliveries {
		fail("P4", "phase C delivered %d texts; a replay delivers nothing", after.deliveries-before.deliveries)
	}
	invs := c.invocations()
	for i := before.invocations; i < len(invs); i++ {
		inv := invs[i]
		if inv.Op != "start" && inv.Op != "stop" && slices.Contains(runtimeEffectOps, inv.Op) {
			fail("P4", "phase C ran %s on %v; a replay runs nothing", inv.Op, inv.Args)
		}
	}
}

// replayClass is the class of a replay's projection: the re-emitted event,
// the sync answer or the CLI's exit; "not-replayed" when it was not marked
// a replay.
func replayClass(r *x12LoadReq) string {
	a := r.replay
	switch {
	case !a.answered:
		return "no-answer"
	case r.verb.async:
		if !a.replayed {
			return "not-replayed"
		}
		return r.repOut.class
	case r.verb.surface == surfaceAPI:
		if a.ok() && a.replayed {
			return classSucceeded
		}
		if !a.ok() && strings.Contains(a.body, "replayed op "+r.opID+":") {
			return classFailed
		}
		return "not-replayed"
	default:
		if a.ok() && a.replayed && r.verb.message() {
			return cliMessageClass(a)
		}
		if a.ok() && a.replayed {
			return classSucceeded
		}
		if a.status == 1 && a.code == "op_already_failed" {
			return classFailed
		}
		return "not-replayed"
	}
}

// r1Class is R1's class of a request.
func r1Class(r *x12LoadReq) string {
	a := r.orig
	switch {
	case !a.answered:
		return "no-answer"
	case !a.ok() && !r.hasOp && r.verb.surface == surfaceAPI:
		return fmt.Sprintf("front-door %d %s", a.status, a.code)
	case !a.ok() && !r.hasOp:
		return "refused " + a.code
	case a.ok() && !r.hasOp && r.mode != "off":
		return "unrecorded " + causeOr(r.cause, "store")
	case r.hasOp && r.op.Outcome.Arm != "" && r.op.Outcome.Arm != opjournal.ArmSucceeded:
		_, reason := r.op.Outcome.Effective()
		return "journaled " + string(r.op.Outcome.Arm) + "{" + reason + "}"
	case r.hasOp:
		return "recorded"
	default:
		return "off"
	}
}

func causeOr(cause, def string) string {
	if cause == "" {
		return def
	}
	return cause
}

// checkLoadRequest is P1, P5 and R1's expected class for one request.
func (c *x12City) checkLoadRequest(r *x12LoadReq, baselines map[string]x12Shape, fail func(line, format string, args ...any)) {
	if r.mode == "require" && r.orig.ok() {
		x12Report.eval("P1", 1)
	}
	if r.mode == "require" && r.orig.ok() && r.verb.surface == surfaceAPI {
		if !r.hasOp || r.orig.opID == "" {
			fail("P1", "require answered %s for %s with no op", r.orig, r.verb.name)
		} else if r.verb.async && r.orig.requestID != r.orig.opID {
			fail("P1", "require answered request_id %q for op %q", r.orig.requestID, r.orig.opID)
		}
	}
	if r.mode == "require" && r.orig.ok() && r.verb.surface == surfaceCLI && !r.hasOp {
		fail("P1", "require: %s exited 0 with no op: %s", r.verb.name, r.orig.stderr)
	}
	// Each provoked cause lands in its class.
	class := r1Class(r)
	want := ""
	switch r.cause {
	case provokeUnknown:
		want = "front-door 404"
	case provokeEmpty, provokeKeyReuse:
		want = "front-door 422"
	case provokeTooLarge, provokeBadKey:
		want = map[string]string{"off": "off", "auto": "unrecorded " + r.cause, "require": "front-door 422"}[r.mode]
	case provokeNoRow:
		want = map[string]string{"off": "off", "auto": "unrecorded " + r.cause, "require": "recorded"}[r.mode]
	case provokeCLITooLong:
		want = map[string]string{"off": "off", "auto": "unrecorded " + r.cause, "require": "refused op_journal_unavailable"}[r.mode]
	case "":
		if r.mode != "off" && !r.hasOp {
			fail("R1", "%s %s: a well-formed request ran unrecorded: %s", r.mode, r.verb.name, r.orig)
		}
	}
	if want != "" && !strings.HasPrefix(class, want) {
		fail("R1", "%s %s (%s): class %q, want %q: %s", r.mode, r.verb.name, r.cause, class, want, r.orig)
	}
	// P5: every delivery of the request is off's text.
	if !r.verb.message() || r.cause != "" || r.verb.shape == "" {
		return
	}
	sh, ok := baselines[r.verb.shape]
	if !ok {
		return
	}
	for _, d := range c.deliveries() {
		if r.verb.shape == "cli-mail-notify" || !strings.Contains(d.Text, r.payload) {
			continue
		}
		x12Report.eval("P5", 1)
		if !strings.HasPrefix(r.verb.shape, "cli-nudge-wait") && !strings.HasPrefix(r.verb.shape, "cli-nudge-queue") {
			if want := sh.expected(r.payload); d.Text != want {
				fail("P5", "%s delivery %d = %q, want off's %q", r.verb.name, d.Seq, d.Text, want)
			}
			continue
		}
		// Queued reminders batch: compare each reminder line, and the
		// framing around them, with off's.
		frame, lines := reminderParts(d.Text)
		_, wantLines := reminderParts(sh.expected(r.payload))
		wantFrame := reminderFrame(baselines, sh, len(lines))
		if frame != wantFrame || len(wantLines) != 1 || !slices.Contains(lines, wantLines[0]) {
			fail("P5", "%s delivery %d = %q, want off's framing of %d reminders with the line %q", r.verb.name, d.Seq, d.Text, len(lines), wantLines)
		}
	}
}

// reminderParts splits a reminder into its framing and its "- [" lines.
func reminderParts(text string) (string, []string) {
	var frame []string
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "- [") {
			lines = append(lines, l)
			if len(lines) == 1 {
				frame = append(frame, "- [...]")
			}
			continue
		}
		frame = append(frame, l)
	}
	return strings.Join(frame, "\n"), lines
}

// latencyRow is one R2 row over ds.
func latencyRow(backend, mode, path string, ds []time.Duration) x12Latency {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(p float64) time.Duration {
		if len(s) == 0 {
			return 0
		}
		i := int(p*float64(len(s)) + 0.5)
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	row := x12Latency{Backend: backend, Mode: mode, Path: path, N: len(s), P50: q(0.50), P95: q(0.95), P99: q(0.99)}
	if len(s) > 0 {
		row.Max = s[len(s)-1]
	}
	return row
}

// bindingSize is the bytes on disk of the sessions binding.
func (c *x12City) bindingSize() int64 {
	root := filepath.Join(c.dir, ".gc", "lanes")
	if c.arm == armDolt {
		root = filepath.Join(c.workspaceRoot(), ".beads")
	}
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// growth is R3 over the load's recorded requests: newRows is how many op
// rows the store gained over the recorded rounds, measured, not derived from
// the requests; the physical figure per op is each recorded round's growth
// less the growth of the off round before it, over newRows.
func (c *x12City) growth(reqs []*x12LoadReq, newRows int, rounds []x12RoundGrowth) x12Growth {
	g := x12Growth{LogicalByKind: map[string]x12Size{}, NewRows: newRows, Rounds: rounds}
	seen := map[string]bool{}
	for _, r := range reqs {
		if r.mode == "off" || !r.hasOp {
			continue
		}
		g.Recorded++
		seen[r.opID] = true
	}
	var lastOff int64
	haveOff := false
	for _, rg := range rounds {
		if rg.Mode == "off" {
			lastOff, haveOff = rg.Bytes, true
			continue
		}
		g.PhysicalBytes += rg.Bytes
		g.PhysicalOverOff += rg.Bytes - lastOff
	}
	sizes := map[string][]int{}
	c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
		for id := range seen {
			b, err := raw.Get(id)
			if err != nil {
				return err
			}
			n := len(b.Title) + len(b.Description)
			for k, v := range b.Metadata {
				n += len(k) + len(v)
			}
			for _, l := range b.Labels {
				n += len(l)
			}
			kind := b.Metadata["gc.op.kind"]
			if kind == "" {
				kind = "op"
			}
			for _, r := range reqs {
				if r.opID == id {
					kind = string(r.verb.kind)
					if r.verb.message() && len(r.payload) > 256 {
						kind += " (payload > 256 B)"
					}
					break
				}
			}
			sizes[kind] = append(sizes[kind], n)
		}
		return nil
	})
	total, logicalRows := 0, 0
	for kind, ns := range sizes {
		sort.Ints(ns)
		sum := 0
		for _, n := range ns {
			sum += n
		}
		total += sum
		logicalRows += len(ns)
		g.LogicalByKind[kind] = x12Size{N: len(ns), Mean: float64(sum) / float64(len(ns)), P95: ns[min(len(ns)-1, int(0.95*float64(len(ns))))]}
	}
	physical := "no off round to set it against"
	if haveOff && g.NewRows > 0 {
		g.PhysicalPerOp = float64(g.PhysicalOverOff) / float64(g.NewRows)
		physical = fmt.Sprintf("%.0f physical bytes per op over the off round", g.PhysicalPerOp)
	}
	mean := 0.0
	if logicalRows > 0 {
		mean = float64(total) / float64(logicalRows)
	}
	g.DailyFormulaMD = fmt.Sprintf("bytes per day = ops per day × %.0f logical bytes per op (%s)", mean, physical)
	if v := os.Getenv(x12DailyOpsEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			g.DailyOps = n
			g.DailyBytes = float64(n) * mean
			g.DailyFormulaMD = fmt.Sprintf("%d ops per day × %.0f logical bytes = %.0f bytes per day (%s)", n, mean, g.DailyBytes, physical)
		}
	}
	return g
}
