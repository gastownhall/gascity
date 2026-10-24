//go:build integration

package conversationlane_test

// X12 (COJ X12; DES-O0API §15 OJ-8): the O0 front door's crash evidence. A
// city on the legacy paths ([ops] authoritative empty) with its ops gates in
// auto or require, its API on a leased loopback port and its controller at
// production cadences, served by the same fake exec runtime and store arms
// as X2. GC_OPS_FAILPOINT kills the API's or a CLI's gc at one crash point
// of one recorded execution (K1 to K5), and the cell then checks what a
// same-key retry, the op sweeper or a rerun makes of what the crash left.
//
// Two tiers. The gating subset runs in conversationlane_x12_test (the
// integration build's X12 target, BUILD.bazel) on SQLite with no 15-minute
// waits. The evidence variants (BUILD.bazel x12_evidence_*_test,
// GC_X12_EVIDENCE=1) run, per backend, every cell at production timing,
// waiting out every op's finish_by (the matrix part), or the six-round
// fault-free load (the load part), and write the X12 report
// (x12_report_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/loopbackport"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const (
	// opsFailpointEnv is gc's op recording failpoint variable.
	opsFailpointEnv = "GC_OPS_FAILPOINT"
	// x12BackendEnv names the evidence tier's store arm (x12EvidenceEnv,
	// x12_evidence_guard_test.go, selects the tier); x12DailyOpsEnv is R3's
	// input volume.
	x12BackendEnv  = "GC_X12_BACKEND"
	x12DailyOpsEnv = "GC_X12_DAILY_OPS"
	// x12PartEnv names an evidence variant's part (matrix or load): which
	// store arms it may run on (x12PartArms) and the report's file name.
	x12PartEnv = "GC_X12_PART"
	// The closers of a legacy op (its closing transition's By).
	closerAPI   = "legacy:api"
	closerCLI   = "legacy:cli"
	closerSweep = "controller:op-sweep"
	// x12FinishGrace is P2's bound past finish_by; x12FinalWait is how long
	// past the last finish_by the evidence waits before it checks P2.
	x12FinishGrace = 60 * time.Second
	x12FinalWait   = 70 * time.Second
	// x12RequestTimeout bounds one API call: on embedded Dolt a recorded
	// wake that materializes a configured session took 75 s in the evidence
	// run, and a client that gives up cancels the request it is waiting for
	// and the materialized row closes. That product behavior is ga-30h7qt;
	// X12 waits it out rather than measuring it. x12OutcomeTimeout bounds
	// one outcome poll.
	x12RequestTimeout = 5 * time.Minute
	x12OutcomeTimeout = 60 * time.Second
	// x12EffectWait bounds how long a cell waits for the effects it expects
	// (a queued nudge is delivered by the controller's dispatcher).
	x12EffectWait = 60 * time.Second
)

// gatesOff runs a gc child with every ops gate off: fixtures record nothing,
// so every op in a city belongs to a measured request.
var gatesOff = []string{"GC_OPS_CONTENT=off", "GC_OPS_CONTROL=off", "GC_OPS_LIFECYCLE=off"}

// x12Evidence reports whether this binary runs X12's evidence tier.
func x12Evidence() bool { return os.Getenv(x12EvidenceEnv) == "1" }

// The evidence parts (x12PartEnv).
const (
	x12PartMatrix = "matrix"
	x12PartLoad   = "load"
)

// x12PartArms are the store arms each evidence part runs on. The crash arms
// (the matrix part, the incapable-store arm with it) run on SQLite and
// embedded Dolt, whose one-process store a crash leaves exactly as the
// killed gc wrote it; the maintainers' 2026-10-23 ruling on #8217 keeps
// embedded Dolt for them only. The load runs on SQLite and on the bd-dolt
// arm (harness_bddolt_test.go): a workspace bd provisioned on a dolt
// sql-server, served natively, the store a direct-server city gets. That arm
// stands in for the load arm the ruling asked for, a bd-served Dolt arm on
// the topology real cities run, until a maintainer accepts the substitute or
// ga-ve3tjw adds that arm: that topology serves sessions through BdStore,
// which records no ops with the pinned bd. On embedded Dolt the load's six
// rounds outlast any test timeout (ga-rqb904).
var x12PartArms = map[string][]storeArm{
	x12PartMatrix: {armSQLite, armDolt},
	x12PartLoad:   {armSQLite, armBdDolt},
}

// x12ArmFor is the store arm the evidence part runs on backend, or why it
// does not.
func x12ArmFor(part, backend string) (storeArm, error) {
	arms, ok := x12PartArms[part]
	if !ok {
		return "", fmt.Errorf("%s=%q: want %s or %s", x12PartEnv, part, x12PartMatrix, x12PartLoad)
	}
	if arm := storeArm(backend); slices.Contains(arms, arm) {
		return arm, nil
	}
	return "", fmt.Errorf("%s=%q: the %s part runs on %v", x12BackendEnv, backend, part, arms)
}

// x12BackendFrom is the store arm a test of part runs on, read from getenv:
// SQLite, production's sessions backend, outside the evidence tier; in it,
// the variant's backend, provided the variant is part's. A variant's env
// run on another part's test (its -test.run widened) is refused, not run on
// an arm that part does not cover.
func x12BackendFrom(getenv func(string) string, part string) (storeArm, error) {
	if getenv(x12EvidenceEnv) != "1" {
		return armSQLite, nil
	}
	if got := getenv(x12PartEnv); got != part {
		return "", fmt.Errorf("%s=%q, but this is a %s-part test: run it from its own evidence variant", x12PartEnv, got, part)
	}
	return x12ArmFor(part, getenv(x12BackendEnv))
}

// x12Backend is the store arm X12's test of part runs on (x12BackendFrom).
func x12Backend(t *testing.T, part string) storeArm {
	t.Helper()
	arm, err := x12BackendFrom(os.Getenv, part)
	if err != nil {
		t.Fatal(err)
	}
	return arm
}

// x12Modes are the recording modes X12 runs in. off is the baseline.
var x12Modes = []string{"auto", "require"}

// x12City is one city X12 runs requests in.
type x12City struct {
	*laneCity
	mode string
	port int
	name string
	// raw makes the API calls; typed reads the outcome and the event log.
	raw   *genclient.Client
	typed *genclient.ClientWithResponses
	// sessions are the on_demand named sessions the city declares, handed
	// out by nextSession in order.
	sessions []string
	used     int
}

// newX12City is a city in mode with an API and n on_demand named sessions.
func newX12City(t *testing.T, arm storeArm, mode string, n int, opTypeMissing bool) *x12City {
	t.Helper()
	port, err := loopbackport.Reserve()
	if err != nil {
		t.Fatalf("reserving the API port: %v", err)
	}
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("s%03d", i+1)
	}
	lc := newLaneCityWith(t, arm, activityNone, cityOptions{
		legacyOps: true, opsMode: mode, apiPort: port, onDemand: names,
		opTypeMissing: opTypeMissing, realCadences: true,
	})
	x12Report.setGC(lc.gc)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	hc := &http.Client{Timeout: x12RequestTimeout}
	raw, err := genclient.NewClient(base, genclient.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	typed, err := genclient.NewClientWithResponses(base, genclient.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return &x12City{laneCity: lc, mode: mode, port: port, name: filepath.Base(lc.root), raw: raw, typed: typed, sessions: names}
}

// setMode rewrites the city's [ops] gates; the controller reads them at its
// next start and every CLI at once.
func (c *x12City) setMode(mode string) {
	c.t.Helper()
	c.mode = mode
	c.opts.opsMode = mode
	c.writeFile(filepath.Join(c.dir, "city.toml"), c.cityTOML(filepath.Join(c.dir, ".gc", "fake-runtime.sh")), 0o644)
}

// start runs an unarmed controller, or one armed with an op failpoint.
func (c *x12City) start(failpoint string) *ctlProc {
	c.t.Helper()
	if c.ctl != nil && !c.ctl.exited() {
		c.t.Fatal("start: a controller is still running")
	}
	p, err := c.launchControllerWith(opsFailpointEnv, failpoint)
	if err != nil {
		c.t.Fatal(err)
	}
	c.ctl = p
	// The API builds the city's Server, whose recorders consult the
	// failpoint, for the city's first city-scoped request: make one, so an
	// armed controller has announced its failpoint before the cell runs.
	c.outcome("x12-probe", "0")
	if failpoint != "" {
		c.waitLogLine(p, opsFailpointEnv+"="+strconv.Quote(failpoint)+": "+testHookActive, 30*time.Second)
	}
	return p
}

// stop ends the running controller with SIGKILL, as X2 restarts one: gc
// stop would stop the city's sessions too, and a cell's fixture must still
// run under the next controller.
func (c *x12City) stop() {
	c.t.Helper()
	if c.ctl == nil || c.ctl.exited() {
		return
	}
	c.killController()
}

// restart stops the running controller and starts one armed as failpoint.
func (c *x12City) restart(failpoint string) *ctlProc {
	c.t.Helper()
	c.stop()
	return c.start(failpoint)
}

// waitLogLine waits until p's log carries line.
func (c *x12City) waitLogLine(p *ctlProc, line string, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(readFile(p.log), line) {
		if time.Now().After(deadline) {
			c.t.Fatalf("the controller log lacks %q after %s:\n%s", line, timeout, tail(readFile(p.log), 8<<10))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// --- sessions -------------------------------------------------------------

// sessionRow reads the session row id.
func (c *x12City) sessionRow(id string) beads.Bead {
	c.t.Helper()
	var row beads.Bead
	c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
		var err error
		row, err = raw.Get(id)
		return err
	})
	return row
}

// sessionByName is the open session row named name, if any.
func (c *x12City) sessionByName(name string) (beads.Bead, bool) {
	c.t.Helper()
	var row beads.Bead
	found := false
	c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
		rows, err := raw.List(beads.ListQuery{Label: "gc:session"})
		if err != nil {
			return err
		}
		for _, b := range rows {
			if b.Metadata["session_name"] == name && b.Status != "closed" {
				row, found = b, true
			}
		}
		return nil
	})
	return row, found
}

// rowRunning reports whether a row is up: open, awake or active, and its
// runtime box exists.
func (c *x12City) rowRunning(b beads.Bead) bool {
	state := b.Metadata["state"]
	name := b.Metadata["session_name"]
	if b.Status == "closed" || (state != "awake" && state != "active") || name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(c.state, "box."+name))
	return err == nil
}

// generation is a row's session generation.
func generation(b beads.Bead) int {
	g, _ := strconv.Atoi(b.Metadata["generation"])
	return g
}

// waitRow polls session id until ok holds, and fails the test when it does
// not within timeout.
func (c *x12City) waitRow(id, what string, timeout time.Duration, ok func(beads.Bead) bool) beads.Bead {
	c.t.Helper()
	row, reached := c.pollRow(id, timeout, ok)
	if !reached {
		c.t.Fatalf("session %s not %s after %s: status %s state %s gen %s", id, what, timeout, row.Status, row.Metadata["state"], row.Metadata["generation"])
	}
	return row
}

// pollRow polls session id until ok holds or timeout passes; it returns the
// last row read and whether ok held.
func (c *x12City) pollRow(id string, timeout time.Duration, ok func(beads.Bead) bool) (beads.Bead, bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row := c.sessionRow(id)
		if ok(row) {
			return row, true
		}
		if time.Now().After(deadline) {
			return row, false
		}
		time.Sleep(c.pollGap())
	}
}

// x12Session is one session a cell targets.
type x12Session struct {
	name, id string
	gen      int
}

// worker is the city's always-on named session.
func (c *x12City) worker() x12Session {
	row := c.sessionRow(c.conv)
	return x12Session{name: sessionName, id: c.conv, gen: generation(row)}
}

// materialize wakes the next on_demand named session with every gate off
// and waits until it runs; suspended, when set, then suspends it.
func (c *x12City) materialize(suspended bool) x12Session {
	c.t.Helper()
	if c.used >= len(c.sessions) {
		c.t.Fatalf("the city declares %d on_demand sessions, all used", len(c.sessions))
	}
	name := c.sessions[c.used]
	c.used++
	if r := c.cli(gatesOff, "session", "wake", name); r.code() != 0 {
		c.t.Fatalf("materializing %s: %d %s %s", name, r.code(), r.stdout, r.stderr)
	}
	var row beads.Bead
	deadline := time.Now().Add(x12StateWait)
	for {
		if b, ok := c.sessionByName(name); ok && c.rowRunning(b) {
			row = b
			break
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("session %s did not start within %s", name, x12StateWait)
		}
		time.Sleep(c.pollGap())
	}
	s := x12Session{name: name, id: row.ID, gen: generation(row)}
	if suspended {
		if r := c.cli(gatesOff, "session", "suspend", s.id); r.code() != 0 {
			c.t.Fatalf("suspending %s: %d %s %s", name, r.code(), r.stdout, r.stderr)
		}
		c.waitRow(s.id, "suspended", time.Minute, func(b beads.Bead) bool {
			return c.rowStopped(b)
		})
	}
	return s
}

// rowStopped reports whether a suspend landed: the API's suspend leaves the
// row suspended, the CLI's asleep (a managed suspend), and neither a box.
func (c *x12City) rowStopped(b beads.Bead) bool {
	state := b.Metadata["state"]
	return (state == "suspended" || state == "asleep") && !c.boxOf(b.Metadata["session_name"])
}

func (c *x12City) boxOf(name string) bool {
	_, err := os.Stat(filepath.Join(c.state, "box."+name))
	return err == nil
}

// --- effects --------------------------------------------------------------

// invocation is one line of the fake runtime's ops.jsonl.
type invocation struct {
	At   string   `json:"at"`
	PID  int      `json:"pid"`
	PPID int      `json:"ppid"`
	Op   string   `json:"op"`
	Args []string `json:"args,omitempty"`
}

// runtimeEffectOps are the invocations that act on a session.
var runtimeEffectOps = []string{"start", "stop", "interrupt", "nudge", "send-keys", "clear-scrollback"}

// invocations reads the fake runtime's invocation log.
func (c *x12City) invocations() []invocation {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.state, "ops.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.t.Fatalf("reading the invocation log: %v", err)
	}
	var out []invocation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var inv invocation
		if err := json.Unmarshal([]byte(line), &inv); err != nil {
			// A line being appended while we read: the next read has it.
			continue
		}
		out = append(out, inv)
	}
	return out
}

// effectCounts counts the runtime effects on session name since offset.
func effectCounts(invs []invocation, offset int, name string) map[string]int {
	out := map[string]int{}
	for i := offset; i < len(invs); i++ {
		inv := invs[i]
		if len(inv.Args) > 0 && inv.Args[0] == name && slices.Contains(runtimeEffectOps, inv.Op) {
			out[inv.Op]++
		}
	}
	return out
}

// offsets is where a cell's effects begin in the fake's logs.
type offsets struct{ invocations, deliveries int }

func (c *x12City) offsets() offsets {
	return offsets{invocations: len(c.invocations()), deliveries: len(c.deliveries())}
}

// --- requests -------------------------------------------------------------

// x12Answer is what one request's client got.
type x12Answer struct {
	// answered says the client got an answer: an HTTP response, or a CLI
	// that exited on its own.
	answered bool
	// status is the HTTP status or the CLI's exit code.
	status int
	body   string
	stderr string
	// requestID and cursor are an async 202's.
	requestID, cursor string
	// opID, opStatus, attempt and replayed are the answer's op fields.
	opID, opStatus string
	attempt        int
	replayed       bool
	// queued is a CLI message verb's JSON "queued" field, when it printed
	// one.
	queued *bool
	// outcome is a CLI verb's JSON "outcome" field.
	outcome string
	// code is an error answer's code.
	code    string
	latency time.Duration
	state   *os.ProcessState
	err     error
}

func (a x12Answer) ok() bool {
	if a.state != nil {
		return a.answered && a.status == 0
	}
	return a.answered && a.status >= 200 && a.status < 300
}

func (a x12Answer) String() string {
	if !a.answered {
		return fmt.Sprintf("no answer (%v)", a.err)
	}
	return fmt.Sprintf("%d %s %s", a.status, strings.TrimSpace(a.body), strings.TrimSpace(a.stderr))
}

// apiPost runs one API mutation of verb on target with key.
func (c *x12City) apiPost(verb x12Verb, target, key, payload string) x12Answer {
	ctx, cancel := context.WithTimeout(context.Background(), x12RequestTimeout)
	defer cancel()
	var k *string
	if key != "" {
		k = &key
	}
	start := time.Now()
	var resp *http.Response
	var err error
	switch verb.endpoint {
	case "submit":
		body := genclient.SubmitSessionJSONRequestBody{Message: payload}
		if verb.intent != "" {
			intent := genclient.SubmitIntent(verb.intent)
			body.Intent = &intent
		}
		resp, err = c.raw.SubmitSession(ctx, c.name, target, &genclient.SubmitSessionParams{XGCRequest: "true", IdempotencyKey: k}, body)
	case "messages":
		resp, err = c.raw.SendSessionMessage(ctx, c.name, target, &genclient.SendSessionMessageParams{XGCRequest: "true", IdempotencyKey: k}, genclient.SendSessionMessageJSONRequestBody{Message: payload})
	case "stop":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdStop(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdStopParams{XGCRequest: "true", IdempotencyKey: k}, genclient.PostV0CityByCityNameSessionByIdStopJSONRequestBody{})
	case "wake":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdWake(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdWakeParams{XGCRequest: "true", IdempotencyKey: k})
	case "suspend":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdSuspend(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdSuspendParams{XGCRequest: "true", IdempotencyKey: k})
	case "kill":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdKill(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdKillParams{XGCRequest: "true", IdempotencyKey: k})
	case "reset":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdReset(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdResetParams{XGCRequest: "true", IdempotencyKey: k})
	case "close":
		resp, err = c.raw.PostV0CityByCityNameSessionByIdClose(ctx, c.name, target, &genclient.PostV0CityByCityNameSessionByIdCloseParams{XGCRequest: "true", IdempotencyKey: k})
	default:
		c.t.Fatalf("apiPost: unknown endpoint %q", verb.endpoint)
	}
	a := x12Answer{latency: time.Since(start), err: err}
	if err != nil {
		return a
	}
	defer resp.Body.Close() //nolint:errcheck // read to the end below
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		a.err = err
		return a
	}
	a.answered, a.status, a.body = true, resp.StatusCode, string(data)
	decodeAPIAnswer(&a, data)
	return a
}

// apiAnswerBody is the union of the fields X12 reads from an API answer: an
// async 202, a sync command's 200, or an error.
type apiAnswerBody struct {
	RequestID   string                    `json:"request_id"`
	EventCursor string                    `json:"event_cursor"`
	Replayed    *bool                     `json:"replayed"`
	Op          *genclient.ConversationOp `json:"op"`
	Code        *string                   `json:"code"`
}

func decodeAPIAnswer(a *x12Answer, data []byte) {
	var b apiAnswerBody
	if json.Unmarshal(data, &b) != nil {
		return
	}
	a.requestID, a.cursor = b.RequestID, b.EventCursor
	if b.Replayed != nil {
		a.replayed = *b.Replayed
	}
	if b.Op != nil {
		a.opID, a.opStatus = b.Op.Id, string(b.Op.Status)
		if b.Op.Attempt != nil {
			a.attempt = int(*b.Op.Attempt)
		}
	}
	if b.Code != nil {
		a.code = *b.Code
	}
}

// cliRun runs one gc CLI verb, with extra env.
func (c *x12City) cliRun(extra []string, args ...string) x12Answer {
	start := time.Now()
	r := c.cli(extra, args...)
	a := x12Answer{latency: time.Since(start), state: r.state, body: r.stdout, stderr: r.stderr, err: r.err, status: r.code()}
	a.answered = r.state != nil && r.state.Exited()
	// --json output: a success line or the failure envelope.
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		var out struct {
			OpID     string `json:"op_id"`
			Key      string `json:"key"`
			Replayed bool   `json:"replayed"`
			Queued   *bool  `json:"queued"`
			Outcome  string `json:"outcome"`
			Error    *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &out) != nil {
			continue
		}
		if out.OpID != "" {
			a.opID = out.OpID
		}
		a.replayed = a.replayed || out.Replayed
		if out.Queued != nil {
			a.queued = out.Queued
		}
		if out.Outcome != "" {
			a.outcome = out.Outcome
		}
		if out.Error != nil {
			a.code = out.Error.Code
		}
	}
	if strings.Contains(r.stderr, ": replayed op ") {
		a.replayed = true
	}
	return a
}

// pollOutcome polls an async request's outcome until it is not pending.
func (c *x12City) pollOutcome(requestID, cursor string) x12Outcome {
	c.t.Helper()
	deadline := time.Now().Add(x12OutcomeTimeout)
	for {
		o := c.outcome(requestID, cursor)
		if o.state != "pending" && o.state != "" {
			return o
		}
		if time.Now().After(deadline) {
			return o
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// x12Outcome is GET .../request/{id}'s answer, reduced to what X12 reads.
type x12Outcome struct {
	state string
	// class is the terminal event's outcome class (x12Class).
	class string
	// opStatus is the op status the terminal event carries.
	opStatus string
	replayed bool
	code     string
	err      error
}

// outcome reads an async request's outcome once.
func (c *x12City) outcome(requestID, cursor string) x12Outcome {
	ctx, cancel := context.WithTimeout(context.Background(), x12RequestTimeout)
	defer cancel()
	resp, err := c.typed.GetRequestOutcomeWithResponse(ctx, c.name, requestID, &genclient.GetRequestOutcomeParams{AfterSeq: cursor})
	if err != nil {
		return x12Outcome{err: err}
	}
	if resp.JSON200 == nil {
		return x12Outcome{err: fmt.Errorf("outcome %s: %d %s", requestID, resp.StatusCode(), resp.Body)}
	}
	o := x12Outcome{state: string(resp.JSON200.State)}
	if ev := resp.JSON200.Event; ev != nil {
		o.class, o.opStatus, o.replayed, o.code = terminalClass(*ev)
	}
	return o
}

// terminalClass is a terminal event's outcome class, the op status it
// carries, whether it is a replay's, and its error code.
func terminalClass(ev genclient.TypedEventStreamEnvelope) (class, opStatus string, replayed bool, code string) {
	kind, _ := ev.Discriminator()
	opOf := func(op *genclient.ConversationOp) string {
		if op == nil {
			return ""
		}
		return string(op.Status)
	}
	switch kind {
	case "request.result.session.submit":
		e, err := ev.AsTypedEventStreamEnvelopeRequestResultSessionSubmit()
		if err != nil {
			return "", "", false, ""
		}
		return deliveredClass(e.Payload.Queued), opOf(e.Payload.Op), false, ""
	case "request.result.session.message":
		e, err := ev.AsTypedEventStreamEnvelopeRequestResultSessionMessage()
		if err != nil {
			return "", "", false, ""
		}
		return deliveredClass(e.Payload.Queued), opOf(e.Payload.Op), false, ""
	case "request.failed":
		e, err := ev.AsTypedEventStreamEnvelopeRequestFailed()
		if err != nil {
			return "", "", false, ""
		}
		return classFailed, opOf(e.Payload.Op), false, e.Payload.ErrorCode
	}
	return "", "", false, ""
}

// Outcome classes P4 compares (rule 4: the same outcome class).
const (
	classDelivered = "succeeded-delivered"
	classQueued    = "succeeded-queued"
	classSucceeded = "succeeded"
	classFailed    = "failed"
)

func deliveredClass(queued bool) string {
	if queued {
		return classQueued
	}
	return classDelivered
}

// opClass is an op row's outcome class, for a message op (whose class
// carries delivered or queued, rule 4 and the design's F5) or not.
func opClass(op opjournal.Op, message bool) string {
	arm, _ := op.Outcome.Effective()
	switch arm {
	case opjournal.ArmSucceeded:
		if !message {
			return classSucceeded
		}
		if op.Outcome.Result.Via == "legacy.queued" {
			return classQueued
		}
		return classDelivered
	case opjournal.ArmSkipped:
		// A kill's or stop's skipped{not_needed} answers 200: succeeded.
		if !message {
			return classSucceeded
		}
		return classFailed
	default:
		return classFailed
	}
}

// degradeEvents counts the city's ops.recording_degraded events of stage on
// surface.
func (c *x12City) degradeEvents(stage, reason string) int {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), x12RequestTimeout)
	defer cancel()
	typ := "ops.recording_degraded"
	limit := int64(1000)
	resp, err := c.typed.GetV0CityByCityNameEventsWithResponse(ctx, c.name, &genclient.GetV0CityByCityNameEventsParams{Type: &typ, Limit: &limit})
	if err != nil || resp.JSON200 == nil || resp.JSON200.Items == nil {
		c.t.Fatalf("reading the event log: %v %s", err, respBody(resp))
	}
	n := 0
	for _, ev := range *resp.JSON200.Items {
		e, err := ev.AsTypedEventStreamEnvelopeOpsRecordingDegraded()
		if err != nil || e.Type != typ {
			continue
		}
		if string(e.Payload.Surface) == "api" && string(e.Payload.Stage) == stage && (reason == "" || e.Payload.Reason == reason) {
			n++
		}
	}
	return n
}

func respBody(r *genclient.GetV0CityByCityNameEventsResponse) string {
	if r == nil {
		return ""
	}
	return string(r.Body)
}

// --- ops ------------------------------------------------------------------

// allOps reads every op row of the city, closed ones included.
func (c *x12City) allOps() []opjournal.Op {
	c.t.Helper()
	var ops []opjournal.Op
	c.mustStore(func(raw beads.Store, st *opjournal.Store) error {
		rows, err := raw.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
		if err != nil {
			return err
		}
		for _, b := range rows {
			if b.Type != opjournal.BeadType {
				continue
			}
			op, err := st.Get(b.ID)
			if err != nil {
				return fmt.Errorf("op %s: %w", b.ID, err)
			}
			ops = append(ops, op)
		}
		return nil
	})
	return ops
}

// opsByID reads every op row of the city in one store handle.
func (c *x12City) opsByID() map[string]opjournal.Op {
	out := map[string]opjournal.Op{}
	for _, op := range c.allOps() {
		out[op.ID] = op
	}
	return out
}

// pollGap is how often a wait re-reads the store: a fresh embedded-Dolt
// handle is slow to open, so the embedded-Dolt arm polls less often.
func (c *x12City) pollGap() time.Duration {
	if c.arm == armDolt {
		return time.Second
	}
	return 250 * time.Millisecond
}

// opByID reads op id; ok is false when there is none.
func (c *x12City) opByID(id string) (opjournal.Op, bool) {
	c.t.Helper()
	var op opjournal.Op
	found := true
	c.mustStore(func(_ beads.Store, st *opjournal.Store) error {
		var err error
		op, err = st.Get(id)
		if errors.Is(err, beads.ErrNotFound) {
			found = false
			return nil
		}
		return err
	})
	return op, found
}

// operatorOpID is the id of the operator's op on conv under key.
func operatorOpID(t *testing.T, conv, key string) string {
	t.Helper()
	id, err := opjournal.DeriveID(laneIDPrefix, conv, "operator", key)
	if err != nil {
		t.Fatalf("deriving the op id of %s/%s: %v", conv, key, err)
	}
	return id
}

// closer is the By of op's closing transition and how many it has.
func closer(op opjournal.Op) (string, int, time.Time) {
	by, n, at := "", 0, time.Time{}
	for _, tr := range op.Transitions {
		if tr.Status == opjournal.StatusClosed {
			by, at = tr.By, tr.At
			n++
		}
	}
	return by, n, at
}

// outcomeText is op's outcome as arm{reason} (via).
func outcomeText(op opjournal.Op) string {
	if op.Status != opjournal.StatusClosed {
		return string(op.Status)
	}
	arm, reason := op.Outcome.Effective()
	s := string(arm)
	if reason != "" {
		s += "{" + reason + "}"
	}
	if op.Outcome.Result.Via != "" {
		s += " via " + op.Outcome.Result.Via
	}
	return s
}

// --- identifiers ----------------------------------------------------------

// assertNoOpIdentifier fails when a delivered text of the city carries an
// identifier (DES-O0API I11): an op id, a client or minted key, a request
// id, a delivery key, a mail cause key, a session bead id or an instance
// token. The bare "operator" label is allowed.
func (c *x12City) assertNoOpIdentifier(rec func(line, format string, args ...any), extra ...string) {
	c.t.Helper()
	ids := slices.Clone(extra)
	for _, op := range c.allOps() {
		ids = append(ids, op.ID, op.ClientKey, "gc:"+op.ID+":1")
	}
	c.mustStore(func(raw beads.Store, _ *opjournal.Store) error {
		rows, err := raw.List(beads.ListQuery{Label: "gc:session", IncludeClosed: true})
		if err != nil {
			return err
		}
		for _, b := range rows {
			ids = append(ids, b.ID, b.Metadata["instance_token"])
		}
		return nil
	})
	ds := c.deliveries()
	x12Report.eval(x12P5Identifiers, len(ds))
	for _, d := range ds {
		for _, id := range ids {
			if id != "" && len(id) > 3 && strings.Contains(d.Text, id) {
				rec("P5", "delivery %d carries identifier %q: %q", d.Seq, id, d.Text)
			}
		}
	}
}

// --- baselines (P5) -------------------------------------------------------

// x12Shape is one request shape whose delivered text and effects off
// defines.
type x12Shape struct {
	// template is the delivered text with the payload as %[1]s; a shape
	// whose text carries no payload (a mail notify) is exact.
	template string
	// exact says the text carries no payload.
	exact bool
	// deliveries and effects are one execution's signature.
	deliveries int
	effects    map[string]int
}

// expected is the text shape delivers for payload.
func (s x12Shape) expected(payload string) string {
	if s.exact {
		return s.template
	}
	return strings.ReplaceAll(s.template, "\x00payload\x00", payload)
}

var (
	x12BaselineOnce sync.Once
	x12BaselineSet  map[string]x12Shape
	x12BaselineErr  string
)

// x12Baselines learns, once per test binary, what off delivers for each
// request shape: one request of each, payload "x12 baseline <shape>
// sentinel", in an off SQLite city.
func x12Baselines(t *testing.T) map[string]x12Shape {
	t.Helper()
	x12BaselineOnce.Do(func() {
		x12BaselineErr = "learning the off baselines failed in " + t.Name()
		x12BaselineSet = learnBaselines(t)
	})
	if x12BaselineSet == nil {
		t.Fatal(x12BaselineErr)
	}
	return x12BaselineSet
}

func learnBaselines(t *testing.T) map[string]x12Shape {
	// Two on_demand sessions for the mail notify and spare, and two per
	// surface for the lifecycle signatures.
	c := newX12City(t, armSQLite, "off", 6, false)
	c.start("")
	w := c.worker()
	out := map[string]x12Shape{}
	sentinel := func(shape string) string { return "x12 baseline " + shape + " sentinel" }
	learn := func(shape string, s x12Session, exact bool, send func(payload string) x12Answer) {
		t.Helper()
		before := c.offsets()
		payload := sentinel(shape)
		a := send(payload)
		if !a.ok() {
			t.Fatalf("baseline %s: %s", shape, a)
		}
		if a.requestID != "" {
			if o := c.pollOutcome(a.requestID, a.cursor); o.state != "succeeded" {
				t.Fatalf("baseline %s: outcome %+v", shape, o)
			}
		}
		var text string
		var n int
		deadline := time.Now().Add(x12EffectWait)
		for {
			ds := c.deliveries()[before.deliveries:]
			n = 0
			for _, d := range ds {
				if d.Session != s.name {
					continue
				}
				if exact || strings.Contains(d.Text, payload) {
					text = d.Text
					n++
				}
			}
			if n > 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		time.Sleep(settleWindow)
		if n != 1 {
			t.Fatalf("baseline %s: %d deliveries, want 1", shape, n)
		}
		sh := x12Shape{exact: exact, deliveries: 1, effects: effectCounts(c.invocations(), before.invocations, s.name)}
		if exact {
			sh.template = text
		} else {
			if strings.Count(text, payload) != 1 {
				t.Fatalf("baseline %s: the payload occurs %d times in %q", shape, strings.Count(text, payload), text)
			}
			sh.template = strings.Replace(text, payload, "\x00payload\x00", 1)
		}
		out[shape] = sh
	}
	for _, v := range x12Verbs {
		if v.shape == "" || v.viaAPI || v.shape == "cli-submit-local" || v.shape == "cli-mail-notify" {
			continue
		}
		v := v
		learn(v.shape, w, false, func(payload string) x12Answer {
			return c.send(v, w, "", payload, nil)
		})
	}
	// Shapes outside the matrix's verbs, the load's.
	for _, v := range x12LoadOnlyVerbs {
		v := v
		learn(v.shape, w, false, func(payload string) x12Answer { return c.send(v, w, "", payload, nil) })
	}
	rcpt := c.materialize(false)
	learn("cli-mail-notify", rcpt, true, func(payload string) x12Answer {
		return c.send(verbByName("cli-mail-notify"), rcpt, "", payload, nil)
	})
	// A stop's effect signature, for the stop cells.
	before := c.offsets()
	if a := c.send(verbByName("stop"), w, "", "", nil); !a.ok() {
		t.Fatalf("baseline api-stop: %s", a)
	}
	time.Sleep(settleWindow)
	out["api-stop"] = x12Shape{effects: effectCounts(c.invocations(), before.invocations, w.name)}
	for _, surface := range []string{surfaceAPI, surfaceCLI} {
		learnLifecycle(t, c, surface, out)
	}
	c.stop()
	learn("cli-submit-local", w, false, func(payload string) x12Answer {
		return c.send(verbByName("cli-submit-local"), w, "", payload, nil)
	})
	out[shapeReminderBatch] = learnReminderBatch(t, c, w)
	return out
}

// learnLifecycle learns off's runtime-effect signature of each lifecycle
// verb on surface, keyed lifecycleShape: on one running session suspend,
// wake, reset and kill in turn, each waited into its transition, and close
// on a second running one.
func learnLifecycle(t *testing.T, c *x12City, surface string, out map[string]x12Shape) {
	t.Helper()
	verb := func(name string) x12Verb {
		if surface == surfaceCLI {
			return verbByName("cli-" + name)
		}
		return verbByName(name)
	}
	learn := func(s x12Session, name string) {
		t.Helper()
		v := verb(name)
		before := c.offsets()
		if a := c.send(v, s, "", "", nil); !a.ok() {
			t.Fatalf("baseline %s: %s", lifecycleShape(v), a)
		}
		what, reached := c.lifecycleTransition(v, s)
		c.waitRow(s.id, what, x12StateWait, reached)
		time.Sleep(settleWindow)
		out[lifecycleShape(v)] = x12Shape{effects: effectCounts(c.invocations(), before.invocations, s.name)}
	}
	s := c.materialize(false)
	for _, name := range []string{"suspend", "wake", "reset", "kill"} {
		learn(s, name)
		s.gen = generation(c.sessionRow(s.id))
	}
	learn(c.materialize(false), "close")
}

// shapeReminderBatch is the framing of a batch of queued reminders, its
// count as \x00n\x00: queued nudges the dispatcher delivers together.
const shapeReminderBatch = "reminder-batch"

// learnReminderBatch learns off's framing of a batch of n queued reminders
// from batches of two and three, queued while the fake runtime reports the
// session busy and delivered together once it reports it idle. The two must
// differ only in the count.
func learnReminderBatch(t *testing.T, c *x12City, w x12Session) x12Shape {
	t.Helper()
	if c.ctl == nil || c.ctl.exited() {
		c.start("")
	}
	c.setActivityMode(activityLastNudge)
	defer c.setActivityMode(activityNone)
	frames := map[int]string{}
	for _, n := range []int{2, 3} {
		c.setActivity(time.Now().Add(time.Hour))
		before := c.offsets()
		var payloads []string
		for i := range n {
			p := fmt.Sprintf("x12 baseline batch %d of %d sentinel", i+1, n)
			payloads = append(payloads, p)
			if a := c.send(verbByName("cli-nudge-queue"), w, "", p, nil); !a.ok() {
				t.Fatalf("baseline batch: %s", a)
			}
		}
		c.setActivity(time.Now().Add(-time.Minute))
		deadline := time.Now().Add(x12EffectWait)
		var got []delivery
		for len(got) == 0 && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			got = deliveriesOf(c.deliveries()[before.deliveries:], payloads[0])
		}
		if len(got) != 1 {
			t.Fatalf("baseline batch of %d: %d deliveries carry its first line", n, len(got))
		}
		frame, lines := reminderParts(got[0].Text)
		if len(lines) != n {
			t.Fatalf("baseline batch of %d delivered %d lines together: %q", n, len(lines), got[0].Text)
		}
		frames[n] = frame
	}
	tmpl := strings.Replace(frames[2], " 2 ", " \x00n\x00 ", 1)
	if tmpl == frames[2] || strings.Replace(tmpl, "\x00n\x00", "3", 1) != frames[3] {
		t.Fatalf("off's batch framings differ beyond the count:\n%q\n%q", frames[2], frames[3])
	}
	return x12Shape{template: tmpl}
}

// reminderFrame is the framing off gives a batch of n reminders.
func reminderFrame(baselines map[string]x12Shape, single x12Shape, n int) string {
	if n == 1 {
		frame, _ := reminderParts(single.expected("x"))
		return frame
	}
	return strings.Replace(baselines[shapeReminderBatch].template, "\x00n\x00", strconv.Itoa(n), 1)
}

// --- the shared send ------------------------------------------------------

// send runs one request of verb on s with key and payload: through the API
// or as a gc child, with extra env on a child.
func (c *x12City) send(v x12Verb, s x12Session, key, payload string, extra []string) x12Answer {
	if v.surface == surfaceAPI {
		return c.apiPost(v, s.id, key, payload)
	}
	return c.cliRun(extra, v.cliArgs(s, key, payload)...)
}
