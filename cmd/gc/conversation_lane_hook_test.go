package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// hookLaneHarness runs in-band passes over a lane fixture: the hook's output,
// its deadline answer and its doorbells are recorded.
type hookLaneHarness struct {
	f       *laneFixture
	st      *opjournal.Store
	fence   runtime.InputFence
	payload []byte
	ack     bool
	emitErr error
	emitted []string
	pokes   []string
	stderr  bytes.Buffer
}

func newHookLaneHarness(t *testing.T, opts ...laneFixtureOpt) *hookLaneHarness {
	t.Helper()
	t.Setenv(hookDeadlineEnv, "")
	f := newLaneFixture(t, opts...)
	h := &hookLaneHarness{f: f, st: f.laneStore(t), ack: true}
	if f.token != "" {
		h.fence = runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	}
	return h
}

func (h *hookLaneHarness) pass() hookLanePass {
	return hookLanePass{
		cityPath: h.f.dir,
		sessions: h.f.store,
		conv:     h.f.id,
		fence:    h.fence,
		provider: "claude",
		payload:  h.payload,
		clk:      h.f.clk,
		stderr:   &h.stderr,
		emit: func(text string) error {
			h.emitted = append(h.emitted, text)
			return h.emitErr
		},
		ackAllowed:     func(int) bool { return h.ack },
		poke:           func(_, conv string) { h.pokes = append(h.pokes, conv) },
		stampDelivered: true,
	}
}

func (h *hookLaneHarness) run(t *testing.T) hookLaneOutcome {
	t.Helper()
	return h.pass().run(context.Background())
}

// notice is a lane notice delivered when.
func notice(when opjournal.When) func(*opjournal.Request) {
	return func(r *opjournal.Request) {
		r.Policy.Message.Role = opjournal.RoleNotice
		r.Policy.Message.When = when
	}
}

// TestHookLanePassDeliversNoticesInBand proves the in-band pass's golden
// output and record: it takes the boundary and after_turn notices, writes
// exactly RenderDelivery's block into the hook context with no identifier in
// it (I14), and closes each op succeeded via hook with an in_band ack and a
// none link (no_source); prompts, steers and if_idle notices wait for the
// lane host. It rings the lane host's doorbell after the Done.
func TestHookLanePassDeliversNoticesInBand(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	boundary := f.propose(t, h.st, f.msg("k-boundary", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	afterTurn := f.propose(t, h.st, f.msg("k-after", opjournal.RoleNotice, "work slung", notice(opjournal.WhenAfterTurn)))
	prompt := f.propose(t, h.st, f.msg("k-prompt", opjournal.RolePrompt, "fix the build"))
	ifIdle := f.propose(t, h.st, f.msg("k-idle", opjournal.RoleNotice, "only when idle", notice(opjournal.WhenIfIdle)))
	steer := f.propose(t, h.st, f.msg("k-steer", opjournal.RoleNotice, "steer now", notice(opjournal.WhenSteer)))

	out := h.run(t)
	if !out.emitted || out.emitErr != nil || !out.ackAllowed || !out.accepted {
		t.Fatalf("outcome = %+v; want one accepted in-band write; stderr=%s", out, h.stderr.String())
	}
	delivered := []opjournal.Op{f.get(t, h.st, boundary.ID), f.get(t, h.st, afterTurn.ID)}
	slices.SortFunc(delivered, func(a, b opjournal.Op) int { return int(a.LaneSeq) - int(b.LaneSeq) })
	want, err := opjournal.RenderDelivery(delivered, opjournal.TextReminder, false)
	if err != nil {
		t.Fatalf("RenderDelivery: %v", err)
	}
	if len(h.emitted) != 1 || h.emitted[0] != want {
		t.Fatalf("hook output = %q, want %q", h.emitted, want)
	}
	for _, id := range laneIdentifiers(h.st, delivered) {
		if id != "" && strings.Contains(h.emitted[0], id) {
			t.Fatalf("hook output %q carries identifier %q", h.emitted[0], id)
		}
	}
	for _, op := range delivered {
		o := op.Outcome
		if op.Status != opjournal.StatusClosed || o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaHook ||
			o.Result.Delivery.Ack != opjournal.AckInBand || o.Result.TurnLink != opjournal.LinkNone ||
			o.Result.TurnBasis != opjournal.BasisNoSource || o.Result.Turn != "" || o.Degradation != "" {
			t.Fatalf("op %s = %s %+v; want succeeded via hook, in_band, link none (no_source), no turn", op.ID, op.Status, o)
		}
	}
	for _, op := range []opjournal.Op{prompt, ifIdle, steer} {
		if got := f.get(t, h.st, op.ID); got.Status == opjournal.StatusClosed || got.Attempt != 0 {
			t.Fatalf("op %s (%s) = %s attempt %d; want it left for the lane host", op.ID, op.ClientKey, got.Status, got.Attempt)
		}
	}
	if !slices.Equal(h.pokes, []string{f.id}) {
		t.Fatalf("doorbells = %v, want one for %s", h.pokes, f.id)
	}
	if got := laneSessionMarker(t, f, session.MetadataLastNudgeDeliveredAt); got == "" {
		t.Fatal("an accepted in-band delivery left no last-nudge stamp")
	}
}

func laneSessionMarker(t *testing.T, f *laneFixture, key string) string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	return b.Metadata[key]
}

// TestHookLanePassJoinsTheTurn proves the in-band executor's probe and
// delivery shape: a boundary, caps InBand and not Live, no link wait, and a
// delivery in joins mode that does not steer.
func TestHookLanePassJoinsTheTurn(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	lane, err := opjournal.NewLane(h.st, laneHookHostName, f.clk)
	if err != nil {
		t.Fatal(err)
	}
	view, info := f.view(t)
	rec := &hookLaneRecordingExec{hookLaneExec: &hookLaneExec{pass: h.pass(), info: info}}
	res, err := lane.Pass(context.Background(), view, rec)
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	p := rec.probe
	if p.Boundary != opjournal.TriYes || !p.Running || !p.Available || !p.Caps.InBand || p.Caps.Live || p.Caps.LinkWait != 0 || p.Caps.Dedup {
		t.Fatalf("probe = %+v; want an available running boundary, in band, not live, no link wait, no dedup", p)
	}
	if len(rec.deliveries) != 1 || rec.deliveries[0].Mode != opjournal.LinkJoins || rec.deliveries[0].Steer {
		t.Fatalf("deliveries = %+v; want one joins delivery that does not steer", rec.deliveries)
	}
}

type hookLaneRecordingExec struct {
	*hookLaneExec
	probe      opjournal.Probe
	deliveries []opjournal.Delivery
}

func (e *hookLaneRecordingExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	p, err := e.hookLaneExec.Probe(ctx, conv)
	e.probe = p
	return p, err
}

func (e *hookLaneRecordingExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	e.deliveries = append(e.deliveries, d)
	return e.hookLaneExec.Deliver(ctx, d)
}

// TestHookLanePassDeadlineShortRelease proves a deadline-short write: the
// batch is released with maybe_delivered (the output may be in the turn),
// and the next delivery, on this non-deduplicating runtime, carries the
// plain redelivery note (D-NOTE) and nothing that names the op.
func TestHookLanePassDeadlineShortRelease(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	h.ack = false
	out := h.run(t)
	if !out.emitted || out.ackAllowed || out.accepted {
		t.Fatalf("outcome = %+v; want a write the deadline did not let it record", out)
	}
	first, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.emitted) != 1 || h.emitted[0] != first || strings.Contains(first, opjournal.RedeliveryNote) {
		t.Fatalf("first hook output = %q, want %q with no note", h.emitted, first)
	}
	cur := f.get(t, h.st, op.ID)
	if cur.Status == opjournal.StatusClosed || cur.MaybeDelivered != 1 || cur.Attempt != 1 {
		t.Fatalf("op after the short release = %s attempt %d maybe_delivered %d; want open, attempt 1, maybe_delivered 1", cur.Status, cur.Attempt, cur.MaybeDelivered)
	}
	if !slices.Equal(h.pokes, []string{f.id}) {
		t.Fatalf("doorbells after the release = %v, want one", h.pokes)
	}
	if got := laneSessionMarker(t, f, session.MetadataLastNudgeDeliveredAt); got != "" {
		t.Fatalf("a released delivery stamped the session (%q)", got)
	}

	h.ack = true
	f.clk.Advance(time.Hour)
	out = h.run(t)
	if !out.accepted || len(h.emitted) != 2 {
		t.Fatalf("redelivery outcome = %+v, outputs %d; want an accepted second write; stderr=%s", out, len(h.emitted), h.stderr.String())
	}
	cur = f.get(t, h.st, op.ID)
	want, err := opjournal.RenderDelivery([]opjournal.Op{cur}, opjournal.TextReminder, true)
	if err != nil {
		t.Fatal(err)
	}
	if h.emitted[1] != want || !strings.Contains(h.emitted[1], opjournal.RedeliveryNote) {
		t.Fatalf("redelivered hook output = %q, want %q carrying the note", h.emitted[1], want)
	}
	for _, id := range laneIdentifiers(h.st, []opjournal.Op{cur}) {
		if id != "" && strings.Contains(h.emitted[1], id) {
			t.Fatalf("redelivered hook output carries identifier %q", id)
		}
	}
	if cur.Outcome.Arm != opjournal.ArmSucceeded || cur.Attempt != 2 || cur.MaybeDelivered != 1 {
		t.Fatalf("op = %+v attempt %d maybe_delivered %d; want succeeded on attempt 2 with maybe_delivered 1", cur.Outcome, cur.Attempt, cur.MaybeDelivered)
	}
}

// TestHookLanePassDeadlineShortAtMostOnce proves an at_most_once notice whose
// write the deadline cut short closes failed{ambiguous_delivery}: it is never
// delivered again.
func TestHookLanePassDeadlineShortAtMostOnce(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "once only", notice(opjournal.WhenBoundary), func(r *opjournal.Request) {
		r.Policy.Guarantee = opjournal.GuaranteeAtMostOnce
	}))
	h.ack = false
	h.run(t)
	cur := f.get(t, h.st, op.ID)
	if cur.Status != opjournal.StatusClosed || cur.Outcome.Arm != opjournal.ArmFailed || cur.Outcome.Reason != opjournal.ReasonAmbiguousDelivery {
		t.Fatalf("op = %s %+v; want failed{ambiguous_delivery}", cur.Status, cur.Outcome)
	}
}

// TestHookLanePassLinksOnlyAVerifiedPayload proves the link: none when the
// family's hook field is unverified, whatever the payload carries; exact
// (hook_payload) when a verified family's payload names the turn.
func TestHookLanePassLinksOnlyAVerifiedPayload(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verified  bool
		wantTurn  string
		wantLink  opjournal.LinkConfidence
		wantBasis opjournal.LinkBasis
	}{
		{"unverified family", false, "", opjournal.LinkNone, opjournal.BasisNoSource},
		{"verified family", true, "turn-7", opjournal.LinkExact, opjournal.BasisHookPayload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.verified {
				prev := hookLanePayloadTurnID
				hookLanePayloadTurnID = func(_ string, payload []byte) string {
					if bytes.Contains(payload, []byte(`"turn_id":"turn-7"`)) {
						return "turn-7"
					}
					return ""
				}
				t.Cleanup(func() { hookLanePayloadTurnID = prev })
			}
			h := newHookLaneHarness(t)
			h.payload = []byte(`{"hook_event_name":"UserPromptSubmit","turn_id":"turn-7","prompt_id":"turn-7"}`)
			op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
			if out := h.run(t); !out.accepted {
				t.Fatalf("outcome = %+v; stderr=%s", out, h.stderr.String())
			}
			r := h.f.get(t, h.st, op.ID).Outcome.Result
			if r.Turn != tc.wantTurn || r.TurnLink != tc.wantLink || r.TurnBasis != tc.wantBasis {
				t.Fatalf("link = turn %q %s (%s); want turn %q %s (%s)", r.Turn, r.TurnLink, r.TurnBasis, tc.wantTurn, tc.wantLink, tc.wantBasis)
			}
		})
	}
}

// TestHookLanePassZombieTakesNothing proves the fence: a hook whose
// environment names another incarnation than the row (a zombie runtime)
// takes nothing and writes nothing; the op waits on content_block.
func TestHookLanePassZombieTakesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fence runtime.InputFence
	}{
		{"other token", runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok-old"}},
		{"other generation", runtime.InputFence{Generation: 2, Epoch: 1, Token: "tok-1"}},
		{"other epoch", runtime.InputFence{Generation: 1, Epoch: 2, Token: "tok-1"}},
		{"no token", runtime.InputFence{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookLaneHarness(t)
			h.fence = tc.fence
			op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
			out := h.run(t)
			if out.emitted || out.accepted || len(h.emitted) != 0 || len(h.pokes) != 0 {
				t.Fatalf("outcome = %+v, outputs %q, doorbells %v; want nothing taken", out, h.emitted, h.pokes)
			}
			if cur := h.f.get(t, h.st, op.ID); cur.Attempt != 0 || cur.Status == opjournal.StatusClosed {
				t.Fatalf("op = %s attempt %d; want untouched", cur.Status, cur.Attempt)
			}
		})
	}
}

// TestHookLanePassTokenlessRow proves a row and a hook that both name no
// token deliver, recorded as unfenced.
func TestHookLanePassTokenlessRow(t *testing.T) {
	h := newHookLaneHarness(t, noLaneToken)
	op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	if out := h.run(t); !out.accepted {
		t.Fatalf("outcome = %+v; stderr=%s", out, h.stderr.String())
	}
	if got := h.f.get(t, h.st, op.ID).Outcome.Degradation; got != opjournal.DegradationUnfencedDelivery {
		t.Fatalf("degradation = %q, want %q", got, opjournal.DegradationUnfencedDelivery)
	}
}

// TestHookLanePassHonorsTheContentGate proves a row the content gate blocks
// (draining) takes nothing in band.
func TestHookLanePassHonorsTheContentGate(t *testing.T) {
	h := newHookLaneHarness(t, func(meta map[string]string) { meta["state"] = string(session.StateDraining) })
	op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	if out := h.run(t); out.emitted {
		t.Fatalf("outcome = %+v; want nothing written into a draining session", out)
	}
	if cur := h.f.get(t, h.st, op.ID); cur.Attempt != 0 {
		t.Fatalf("op attempt %d; want untouched", cur.Attempt)
	}
}

// TestHookLanePassWriteFailure proves a hook context that could not be
// written releases the batch as not delivered, with no maybe_delivered.
func TestHookLanePassWriteFailure(t *testing.T) {
	h := newHookLaneHarness(t)
	op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	h.emitErr = errors.New("broken pipe")
	out := h.run(t)
	if !out.emitted || out.emitErr == nil || out.accepted {
		t.Fatalf("outcome = %+v; want the write error reported", out)
	}
	cur := h.f.get(t, h.st, op.ID)
	if cur.Status == opjournal.StatusClosed || cur.MaybeDelivered != 0 || cur.Failures != 1 {
		t.Fatalf("op = %s maybe_delivered %d failures %d; want open, not maybe delivered, one failure", cur.Status, cur.MaybeDelivered, cur.Failures)
	}
}

// TestHookLaneDue proves the probe: false with no open lane op, true with
// one, and false (reported) when the read fails.
func TestHookLaneDue(t *testing.T) {
	h := newHookLaneHarness(t)
	var stderr bytes.Buffer
	if hookLaneDue(h.f.store, h.f.id, &stderr) {
		t.Fatal("a conversation with no ops is due")
	}
	h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	if !hookLaneDue(h.f.store, h.f.id, &stderr) {
		t.Fatal("a conversation with an open notice is not due")
	}
	if hookLaneDue(h.f.store, "", &stderr) {
		t.Fatal("no conversation is due")
	}
	if hookLaneDue(hookLaneFailingList{h.f.store}, h.f.id, &stderr) || !strings.Contains(stderr.String(), "conversation lanes") {
		t.Fatalf("a failed probe read as due, or went unreported: %q", stderr.String())
	}
}

type hookLaneFailingList struct{ beads.Store }

func (hookLaneFailingList) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("dolt down")
}

// TestHookLanePassWithoutGuardedBatches proves a store that cannot apply
// guarded batches hosts no in-band lane: nothing is written, and the skip is
// reported.
func TestHookLanePassWithoutGuardedBatches(t *testing.T) {
	h := newHookLaneHarness(t)
	h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	p := h.pass()
	p.sessions = hookLaneFailingList{Store: beads.NewMemStore()}
	if out := p.run(context.Background()); out.emitted {
		t.Fatalf("outcome = %+v; want nothing written", out)
	}
	if !strings.Contains(h.stderr.String(), "opening the lane store") {
		t.Fatalf("stderr = %q; want the skip reported", h.stderr.String())
	}
}

// TestHookLaneFenceMatches pins the fence comparison.
func TestHookLaneFenceMatches(t *testing.T) {
	view := opjournal.ConversationView{Generation: 2, Epoch: 3, Token: "tok"}
	for _, tc := range []struct {
		hook runtime.InputFence
		conv opjournal.ConversationView
		want bool
	}{
		{runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}, view, true},
		{runtime.InputFence{Generation: 2, Epoch: 3, Token: "other"}, view, false},
		{runtime.InputFence{Generation: 1, Epoch: 3, Token: "tok"}, view, false},
		{runtime.InputFence{Generation: 2, Epoch: 1, Token: "tok"}, view, false},
		{runtime.InputFence{}, view, false},
		{runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}, opjournal.ConversationView{Generation: 2, Epoch: 3}, false},
		{runtime.InputFence{}, opjournal.ConversationView{Generation: 2, Epoch: 3}, true},
	} {
		if got := hookLaneFenceMatches(tc.hook, tc.conv); got != tc.want {
			t.Errorf("hookLaneFenceMatches(%v, %+v) = %v, want %v", tc.hook, tc.conv, got, tc.want)
		}
	}
}

// hookLaneDrainFixture is a hook budget city whose sessions store, for the
// in-band pass, is a store with guarded batches holding a copy of the
// session row: the file store a test city opens has no id prefix, so it
// hosts no lane.
type hookLaneDrainFixture struct {
	hookBudgetFixture
	sessions laneTestStore
	st       *opjournal.Store
}

func newHookLaneDrainFixture(t *testing.T) *hookLaneDrainFixture {
	t.Helper()
	f := &hookLaneDrainFixture{hookBudgetFixture: newHookBudgetFixture(t), sessions: newLaneTestStore()}
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	row, err := store.Get(f.sessionID)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	resetHookBudgetProcessState(t)
	if _, err := f.sessions.Create(row); err != nil {
		t.Fatalf("copying the session row: %v", err)
	}
	t.Setenv(runtime.InstanceTokenEnv, row.Metadata["instance_token"])
	t.Setenv(runtime.RuntimeEpochEnv, row.Metadata["generation"])
	t.Setenv(runtime.ContinuationEpochEnv, row.Metadata["continuation_epoch"])
	t.Setenv(hookDeadlineEnv, "")
	prevStore := hookLaneSessionStore
	hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store { return f.sessions }
	t.Cleanup(func() { hookLaneSessionStore = prevStore })
	f.st, err = opjournal.NewLaneStore(beads.SessionStore{Store: f.sessions}, clock.Real{})
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	return f
}

// propose writes a lane notice of payload, keyed by its payload.
func (f *hookLaneDrainFixture) propose(t *testing.T, payload string, mods ...func(*opjournal.Request)) opjournal.Op {
	t.Helper()
	key := "k:" + payload
	req := opjournal.Request{
		Conversation: f.sessionID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "gc:mail", Actor: opjournal.ActorBackground}, Payload: payload,
		Policy: opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice}},
	}
	for _, m := range mods {
		m(&req)
	}
	op, created, err := f.st.Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose(%s) = created %v, %v", key, created, err)
	}
	return op
}

func (f *hookLaneDrainFixture) drain(t *testing.T) hookBudgetRun {
	t.Helper()
	return recordHookCommand(t, func(stdout, stderr io.Writer) int {
		return cmdNudgeDrainWithFormat(nil, true, "", stdout, stderr)
	})
}

// TestNudgeDrainInjectLaneGolden pins the injecting drain's hook output with
// lane notices due: alone, beside a legacy nudge, with a prompt left for the
// lane host, and after a deadline-short release (the plain note). The output
// is one hook context, and it carries no identifier (I14).
func TestNudgeDrainInjectLaneGolden(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "hook_lane"))
	if err != nil {
		t.Fatal(err)
	}
	update := os.Getenv(hookBudgetGoldenUpdateEnv) != ""
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op
		// short runs a first drain the deadline cuts short.
		short bool
		// waits says the ops stay for the lane host.
		waits bool
	}{
		{name: "one notice", seed: func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op {
			return []opjournal.Op{f.propose(t, "mail arrived")}
		}},
		{name: "notice and legacy nudge", seed: func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op {
			f.queueNudge(t)
			return []opjournal.Op{f.propose(t, "mail arrived")}
		}},
		{name: "prompt waits", waits: true, seed: func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op {
			return []opjournal.Op{f.propose(t, "fix the build", func(r *opjournal.Request) { r.Policy.Message.Role = opjournal.RolePrompt })}
		}},
		{name: "redelivery note", short: true, seed: func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op {
			return []opjournal.Op{f.propose(t, "mail arrived")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			ops := tc.seed(t, f)
			if tc.short {
				t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(2*time.Second)))
				prevStart := hookProcessStart
				hookProcessStart = time.Now().Add(-100 * time.Hour)
				first := f.drain(t)
				hookProcessStart = prevStart
				t.Setenv(hookDeadlineEnv, "")
				if !strings.Contains(first.stderr, "hook deadline too close") {
					t.Fatalf("first drain was not cut short: stderr=%s", first.stderr)
				}
				cur, err := f.st.Get(ops[0].ID)
				if err != nil || cur.MaybeDelivered != 1 {
					t.Fatalf("op after the short drain = %+v, %v; want maybe_delivered 1", cur, err)
				}
				f.ageRetry(t, cur)
			}
			run := f.drain(t)
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			got := normalizeHookBudgetOutput(t, run.stdout)
			if run.stderr != "" {
				got += "--- stderr ---\n" + strings.ReplaceAll(run.stderr, f.cityDir, "<city>")
			}
			for _, op := range ops {
				cur, err := f.st.Get(op.ID)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case tc.waits && (cur.Status == opjournal.StatusClosed || cur.Attempt != 0):
					t.Fatalf("op %s = %s attempt %d; want it left for the lane host", cur.ID, cur.Status, cur.Attempt)
				case !tc.waits && (cur.Outcome.Arm != opjournal.ArmSucceeded || cur.Outcome.Result.Via != laneViaHook):
					t.Fatalf("op %s = %s %+v; want succeeded via hook", cur.ID, cur.Status, cur.Outcome)
				}
				for _, id := range laneIdentifiers(f.st, []opjournal.Op{cur}) {
					if id != "" && strings.Contains(run.stdout, id) {
						t.Fatalf("hook output carries identifier %q:\n%s", id, run.stdout)
					}
				}
			}
			path := filepath.Join(goldenDir, strings.ReplaceAll(tc.name, " ", "_")+".golden")
			if update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden %s (set %s=1 to write it): %v", path, hookBudgetGoldenUpdateEnv, err)
			}
			if got != string(want) {
				t.Errorf("hook output differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

// ageRetry moves op's retry gate into the past, as the release's backoff
// running out would.
func (f *hookLaneDrainFixture) ageRetry(t *testing.T, op opjournal.Op) {
	t.Helper()
	b, err := f.sessions.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for k := range b.Metadata {
		if strings.HasSuffix(k, "retry_at") {
			if err := f.sessions.SetMetadata(op.ID, k, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
		}
	}
}
