package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// hookLaneHarness runs in-band passes over a lane fixture: the hook's output,
// its deadline answer and the leases its ops were held under while the output
// was written are recorded.
type hookLaneHarness struct {
	f        *laneFixture
	st       *opjournal.Store
	fence    runtime.InputFence
	leaseTTL time.Duration
	payload  []byte
	ack      bool
	emitErr  error
	emitted  []string
	// leases are the leases of the ops open when each output was written.
	leases []opjournal.Lease
	stderr bytes.Buffer
}

func newHookLaneHarness(t *testing.T, opts ...laneFixtureOpt) *hookLaneHarness {
	t.Helper()
	t.Setenv(hookDeadlineEnv, "")
	f := newLaneFixture(t, opts...)
	h := &hookLaneHarness{f: f, st: f.laneStore(t), ack: true, leaseTTL: laneHookLeaseTTLNoDeadline}
	h.fence = runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	return h
}

func (h *hookLaneHarness) pass() hookLanePass {
	return hookLanePass{
		sessions: h.f.store,
		conv:     h.f.id,
		fence:    h.fence,
		leaseTTL: h.leaseTTL,
		provider: "claude",
		payload:  h.payload,
		clk:      h.f.clk,
		stderr:   &h.stderr,
		emit: func(text string) error {
			h.emitted = append(h.emitted, text)
			h.recordLeases()
			return h.emitErr
		},
		ackAllowed:     func(int) bool { return h.ack },
		stampDelivered: true,
	}
}

// recordLeases records the leases of the conversation's ops as they stand.
func (h *hookLaneHarness) recordLeases() {
	rows, err := h.f.store.List(beads.ListQuery{Metadata: map[string]string{"gc.op.conversation": h.f.id}, TierMode: beads.TierBoth})
	if err != nil {
		return
	}
	for _, row := range rows {
		if op, err := h.st.Get(row.ID); err == nil && !op.Lease.IsZero() {
			h.leases = append(h.leases, op.Lease)
		}
	}
}

// identifiers are the strings no hook output may carry (I14): the lane
// fixture's, the hook lane's nonce prefix, and every lease the ops were held
// under while the output was written.
func (h *hookLaneHarness) identifiers(ops []opjournal.Op) []string {
	out := append(laneIdentifiers(h.st, ops), "#"+laneHookHostName+".")
	for _, l := range h.leases {
		out = append(out, l.String(), l.Nonce)
	}
	return out
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
	if len(h.leases) == 0 {
		t.Fatal("no lease was recorded while the hook output was written")
	}
	for _, id := range h.identifiers(delivered) {
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
	if !out.done {
		t.Fatalf("outcome = %+v; want the Done reported so the drain rings the lane host", out)
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
	if !out.done {
		t.Fatalf("outcome after the release = %+v; want the Done reported", out)
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
	for _, id := range h.identifiers([]opjournal.Op{cur}) {
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
			if out.emitted || out.accepted || out.done || len(h.emitted) != 0 {
				t.Fatalf("outcome = %+v, outputs %q; want nothing taken", out, h.emitted)
			}
			if cur := h.f.get(t, h.st, op.ID); cur.Attempt != 0 || cur.Status == opjournal.StatusClosed {
				t.Fatalf("op = %s attempt %d; want untouched", cur.Status, cur.Attempt)
			}
		})
	}
}

// TestHookLanePassTokenlessZombie proves a hook and a row that both name no
// token still compare generation and epoch: a tokenless hook of an earlier
// generation or epoch takes nothing.
func TestHookLanePassTokenlessZombie(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fence runtime.InputFence
	}{
		{"other generation", runtime.InputFence{Generation: 2, Epoch: 1}},
		{"other epoch", runtime.InputFence{Generation: 1, Epoch: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookLaneHarness(t, noLaneToken)
			h.fence = tc.fence
			op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
			if out := h.run(t); out.emitted || out.done {
				t.Fatalf("outcome = %+v; want nothing taken", out)
			}
			if cur := h.f.get(t, h.st, op.ID); cur.Attempt != 0 {
				t.Fatalf("op attempt %d; want untouched", cur.Attempt)
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
	if hookLaneDue(hookLaneFailingGuardedList{h.f.store}, h.f.id, &stderr) || !strings.Contains(stderr.String(), "conversation lanes") {
		t.Fatalf("a failed probe read as due, or went unreported: %q", stderr.String())
	}
}

type hookLaneFailingList struct{ beads.Store }

func (hookLaneFailingList) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("dolt down")
}

// hookLaneFailingGuardedList is a store with guarded batches whose Lists
// fail.
type hookLaneFailingGuardedList struct{ beads.Store }

func (hookLaneFailingGuardedList) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("dolt down")
}

func (s hookLaneFailingGuardedList) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return beads.GuardedBatcherFor(s.Store)
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
	if !strings.Contains(h.stderr.String(), "opening the lane store") || !strings.Contains(h.stderr.String(), beads.ErrGuardedBatchUnsupported.Error()) {
		t.Fatalf("stderr = %q; want the skip reported as a store without guarded batches", h.stderr.String())
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
		{runtime.InputFence{Generation: 2, Epoch: 3}, opjournal.ConversationView{Generation: 2, Epoch: 3}, true},
		{runtime.InputFence{Generation: 1, Epoch: 3}, opjournal.ConversationView{Generation: 2, Epoch: 3}, false},
		{runtime.InputFence{Generation: 2, Epoch: 1}, opjournal.ConversationView{Generation: 2, Epoch: 3}, false},
		{runtime.InputFence{}, opjournal.ConversationView{Generation: 2, Epoch: 3}, false},
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
	// pokes are the conversations the drain rang the lane host for.
	pokes []string
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
	// A claude-family hook: its plain-text output reaches the model, so the
	// in-band pass runs (hookLaneReachesModel).
	t.Setenv("GC_PROVIDER", "claude")
	prevPoke := hookLanePoke
	hookLanePoke = func(_, conv string, _ time.Duration) { f.pokes = append(f.pokes, conv) }
	t.Cleanup(func() { hookLanePoke = prevPoke })
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

// TestHookLaneReachesModel pins which prompt hooks put their output in front
// of the model: the JSON formats, and plain text only for the claude family
// and the opencode and mimocode plugins, the family read from the provider's
// name, so groq and cerebras (the opencode plugin) and claude spellings count.
func TestHookLaneReachesModel(t *testing.T) {
	for _, tc := range []struct {
		format, provider string
		want             bool
	}{
		{"", "claude", true},
		{"", "opencode", true},
		{"", "mimocode", true},
		{"", "groq", true},
		{"", "cerebras", true},
		{"", "claude-eco", true},
		{"", "claude/tmux-cli", true},
		{"", " Claude ", true},
		{hookOutputFormatCodex, "codex", true},
		{hookOutputFormatGemini, "gemini", true},
		{hookOutputFormatAntigravity, "antigravity", true},
		{" Codex ", "", true},
		{"", "cursor", false},
		{"", "copilot", false},
		{"", "kiro", false},
		{"", "kimi", false},
		{"", "pi", false},
		{"", "omp", false},
		{"", "codex", false},
		{"", "", false},
		{"unknown", "claude", false},
	} {
		if got := hookLaneReachesModel(tc.format, tc.provider); got != tc.want {
			t.Errorf("hookLaneReachesModel(%q, %q) = %v, want %v", tc.format, tc.provider, got, tc.want)
		}
	}
}

// laneListCounts are the Lists the drain sent the lane's store: all of them,
// and the open-lane probes (opjournal.HasOpenLaneOps) among them.
type laneListCounts struct{ all, probes int }

// laneListCountingStore counts the Lists the drain sends the lane's store.
type laneListCountingStore struct {
	laneTestStore
	lists *laneListCounts
}

func (s laneListCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists.all++
	if _, ok := q.Metadata["gc.op.open"]; ok && q.Limit == 1 {
		s.lists.probes++
	}
	return s.laneTestStore.List(q)
}

// countLaneLists routes the drain's lane store through a List counter.
func (f *hookLaneDrainFixture) countLaneLists(t *testing.T) *laneListCounts {
	t.Helper()
	lists := new(laneListCounts)
	prev := hookLaneSessionStore
	hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store {
		return laneListCountingStore{laneTestStore: f.sessions, lists: lists}
	}
	t.Cleanup(func() { hookLaneSessionStore = prev })
	return lists
}

// TestNudgeDrainInjectLaneSkipsFamiliesWithoutInBandOutput proves a family
// whose prompt hook does not put plain text in front of the model (cursor,
// copilot) takes no lane op and does not even probe for one: the notice waits
// for the lane host.
func TestNudgeDrainInjectLaneSkipsFamiliesWithoutInBandOutput(t *testing.T) {
	for _, family := range []string{"cursor", "copilot", ""} {
		t.Run("family "+family, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			t.Setenv("GC_PROVIDER", family)
			op := f.propose(t, "mail arrived")
			lists := f.countLaneLists(t)
			run := f.drain(t)
			if strings.Contains(run.stdout, "mail arrived") {
				t.Fatalf("a %q hook wrote the lane notice:\n%s", family, run.stdout)
			}
			if cur, err := f.st.Get(op.ID); err != nil || cur.Attempt != 0 || cur.Status == opjournal.StatusClosed {
				t.Fatalf("op = %+v, %v; want it left for the lane host", cur, err)
			}
			if lists.all != 0 || len(f.pokes) != 0 {
				t.Fatalf("lane store lists = %d, doorbells %v; want no probe and no doorbell", lists.all, f.pokes)
			}
		})
	}
}

// TestNudgeDrainInjectLaneSkipsAnExplicitTarget proves a drain that names its
// target runs no in-band pass: the fence it would check is the hook's own.
func TestNudgeDrainInjectLaneSkipsAnExplicitTarget(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	op := f.propose(t, "mail arrived")
	lists := f.countLaneLists(t)
	run := recordHookCommand(t, func(stdout, stderr io.Writer) int {
		return cmdNudgeDrainWithFormat([]string{f.sessionID}, true, "", stdout, stderr)
	})
	if strings.Contains(run.stdout, "mail arrived") || lists.probes != 0 {
		t.Fatalf("an explicit-target drain ran the lane (probes %d):\n%s", lists.probes, run.stdout)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Attempt != 0 {
		t.Fatalf("op = %+v, %v; want untouched", cur, err)
	}
}

// TestHookLaneDueReadsNothingWithoutGuardedBatches proves the probe sends no
// request to a store that can never host a lane: a bd CLI store spawns no bd
// process, and a store without guarded batches is not listed.
func TestHookLaneDueReadsNothingWithoutGuardedBatches(t *testing.T) {
	var calls [][]string
	runner := func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("[]"), nil
	}
	var stderr bytes.Buffer
	bd := beads.NewBdStoreWithPrefix(t.TempDir(), runner, "gc")
	if hookLaneDue(bd, "gc-1", &stderr) || len(calls) != 0 {
		t.Fatalf("probe on a bd store = due or ran bd: %v", calls)
	}
	lists := 0
	plain := hookLaneListCounter{Store: beads.NewMemStore(), lists: &lists}
	if hookLaneDue(plain, "gc-1", &stderr) || lists != 0 {
		t.Fatalf("probe on a store without guarded batches listed %d times", lists)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q; want a silent skip", stderr.String())
	}
}

type hookLaneListCounter struct {
	beads.Store
	lists *int
}

func (s hookLaneListCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	*s.lists++
	return s.Store.List(q)
}

// TestHookLaneSettleBy proves the pass records a delivery only by the
// inherited hook deadline less the archive margin the drain keeps to ack its
// legacy nudges and exit, and has no bound without a deadline.
func TestHookLaneSettleBy(t *testing.T) {
	prevStart := hookProcessStart
	t.Cleanup(func() { hookProcessStart = prevStart })
	deadline := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(deadline))
	hookProcessStart = deadline.Add(-100 * time.Hour)
	if got, want := hookLaneSettleBy(), deadline.Add(-hookDeadlineArchiveMarginMax); !got.Equal(want) {
		t.Fatalf("long-lived process: settle by %s, want the deadline less the full margin, %s", got, want)
	}
	hookProcessStart = deadline.Add(-3 * time.Second)
	if got, want := hookLaneSettleBy(), deadline.Add(-time.Second); !got.Equal(want) {
		t.Fatalf("3s process: settle by %s, want the deadline less a third of its time, %s", got, want)
	}
	t.Setenv(hookDeadlineEnv, "")
	if got := hookLaneSettleBy(); !got.IsZero() {
		t.Fatalf("no deadline: settle by %s, want no bound", got)
	}
	t.Setenv(hookDeadlineEnv, "soon")
	if got := hookLaneSettleBy(); !got.IsZero() {
		t.Fatalf("unparseable deadline: settle by %s, want no bound (the ack check refuses the delivery)", got)
	}
}

// TestHookLaneLeaseTTL proves the hook's lease outlives the inherited hook
// deadline by laneHookLeaseGrace (DES-O1 §4.6), never below the minimum.
func TestHookLaneLeaseTTL(t *testing.T) {
	now := time.Now()
	t.Setenv(hookDeadlineEnv, "")
	if got := hookLaneLeaseTTL(now); got != laneHookLeaseTTLNoDeadline {
		t.Fatalf("no deadline: ttl %s, want %s", got, laneHookLeaseTTLNoDeadline)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(now.Add(15*time.Second)))
	if got := hookLaneLeaseTTL(now); got < 24*time.Second || got > 25*time.Second {
		t.Fatalf("15s deadline: ttl %s, want 25s", got)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(now.Add(-time.Minute)))
	if got := hookLaneLeaseTTL(now); got != opjournal.MinLeaseTTL {
		t.Fatalf("passed deadline: ttl %s, want the minimum %s", got, opjournal.MinLeaseTTL)
	}
}

// TestHookLanePassNotesAnAbsentConversation proves a pass over a conversation
// with no session row says so on stderr, as the lane host logs it, since the
// conversation's open ops close for the absence.
func TestHookLanePassNotesAnAbsentConversation(t *testing.T) {
	h := newHookLaneHarness(t)
	ghost := "gc-ghost"
	r := h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary))
	r.Conversation = ghost
	op := h.f.propose(t, h.st, r)
	p := h.pass()
	p.conv = ghost
	p.run(context.Background())
	if got := h.f.get(t, h.st, op.ID); got.Outcome.Arm != opjournal.ArmSkipped {
		t.Fatalf("op = %+v, want it closed skipped", got.Outcome)
	}
	if want := "conversation " + ghost + " has no session row on two reads"; !strings.Contains(h.stderr.String(), want) {
		t.Fatalf("stderr = %q, want %q", h.stderr.String(), want)
	}
}

// TestHookLanePassTakesItsBatchUnderTheLeaseTTL proves the pass holds its
// batch under the lease it was given, not the lane default.
func TestHookLanePassTakesItsBatchUnderTheLeaseTTL(t *testing.T) {
	h := newHookLaneHarness(t)
	h.leaseTTL = 47 * time.Second
	h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	start := h.f.clk.Now()
	if out := h.run(t); !out.accepted {
		t.Fatalf("outcome = %+v; stderr=%s", out, h.stderr.String())
	}
	if len(h.leases) != 1 {
		t.Fatalf("leases = %v; want the one the batch was held under", h.leases)
	}
	if got := h.leases[0].Deadline.Sub(start); got != h.leaseTTL {
		t.Fatalf("lease runs %s, want %s", got, h.leaseTTL)
	}
}

// TestHookLaneContextEndsAtTheHookDeadline proves the pass's context ends at
// the inherited hook deadline, and has none without one.
func TestHookLaneContextEndsAtTheHookDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(deadline))
	ctx, cancel := hookLaneContext()
	got, ok := ctx.Deadline()
	cancel()
	if !ok || !got.Equal(deadline) {
		t.Fatalf("context deadline = %v, %v; want %v", got, ok, deadline)
	}
	t.Setenv(hookDeadlineEnv, "")
	ctx, cancel = hookLaneContext()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("a hook with no deadline got a context deadline")
	}
}

// TestHookLanePokeLeg proves the doorbell's legs fit in half the time left to
// the hook deadline, and that it is skipped when too little is left.
func TestHookLanePokeLeg(t *testing.T) {
	now := time.Now()
	t.Setenv(hookDeadlineEnv, "")
	if got := hookLanePokeLeg(now); got != laneHookPokeTimeout {
		t.Fatalf("no deadline: leg %s, want %s", got, laneHookPokeTimeout)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(now.Add(time.Second)))
	if got := hookLanePokeLeg(now); got <= 0 || 3*got > 500*time.Millisecond {
		t.Fatalf("1s left: leg %s; want three legs within half of it", got)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(now.Add(50*time.Millisecond)))
	if got := hookLanePokeLeg(now); got != 0 {
		t.Fatalf("50ms left: leg %s, want the doorbell skipped", got)
	}
	t.Setenv(hookDeadlineEnv, "not a time")
	if got := hookLanePokeLeg(now); got != 0 {
		t.Fatalf("unparsable deadline: leg %s, want the doorbell skipped", got)
	}
}

// TestNudgeDrainInjectLaneRingsLast proves the drain rings the lane host
// once, after a Done, and only once the legacy nudge it carried is acked and
// the session stamped: the doorbell never spends the time those need.
func TestNudgeDrainInjectLaneRingsLast(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.queueNudge(t)
	f.propose(t, "mail arrived")
	var atPoke nudgequeue.State
	var stampAtPoke string
	hookLanePoke = func(_, conv string, _ time.Duration) {
		f.pokes = append(f.pokes, conv)
		atPoke, _ = nudgequeue.LoadState(f.cityDir)
		if store, err := openCityStoreAt(f.cityDir); err == nil {
			if b, err := store.Get(f.sessionID); err == nil {
				stampAtPoke = b.Metadata[session.MetadataLastNudgeDeliveredAt]
			}
		}
	}
	run := f.drain(t)
	if !strings.Contains(run.stdout, "review queued work") || !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("combined output missing a part:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if !slices.Equal(f.pokes, []string{f.sessionID}) {
		t.Fatalf("doorbells = %v, want one for %s", f.pokes, f.sessionID)
	}
	if len(atPoke.Pending)+len(atPoke.InFlight) != 0 {
		t.Fatalf("the doorbell rang before the legacy ack: pending %d in flight %d", len(atPoke.Pending), len(atPoke.InFlight))
	}
	if stampAtPoke == "" {
		t.Fatal("the doorbell rang before the session's delivery stamp")
	}
}

// TestNudgeDrainInjectLaneEmptyPromptRings proves a lane-only delivery on an
// otherwise empty prompt rings the lane host, probing the lane once.
func TestNudgeDrainInjectLaneEmptyPromptRings(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.propose(t, "mail arrived")
	lists := f.countLaneLists(t)
	run := f.drain(t)
	if !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("not delivered:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if !slices.Equal(f.pokes, []string{f.sessionID}) {
		t.Fatalf("doorbells = %v, want one", f.pokes)
	}
	if lists.probes != 1 {
		t.Fatalf("the open-lane probe ran %d times, want once: the fast path's answer is passed through", lists.probes)
	}
}

// withShortHookDeadline runs fn with a hook deadline the ack margin refuses.
func withShortHookDeadline(t *testing.T, fn func()) {
	t.Helper()
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(2*time.Second)))
	prevStart := hookProcessStart
	hookProcessStart = time.Now().Add(-100 * time.Hour)
	defer func() {
		hookProcessStart = prevStart
		t.Setenv(hookDeadlineEnv, "")
	}()
	fn()
}

// TestNudgeDrainInjectLaneShortDeadlineReleasesLegacy proves one deadline
// answer covers a combined write: when it refuses, the legacy nudge the lane
// pass wrote alongside its notice is released for redelivery, never acked,
// and the lane op is maybe delivered. The answer counts both.
func TestNudgeDrainInjectLaneShortDeadlineReleasesLegacy(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.queueNudge(t)
	op := f.propose(t, "mail arrived")
	var run hookBudgetRun
	withShortHookDeadline(t, func() { run = f.drain(t) })
	if !strings.Contains(run.stdout, "review queued work") || !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("combined output missing a part:\n%s", run.stdout)
	}
	if !strings.Contains(run.stderr, "2 injected nudge(s)") {
		t.Fatalf("stderr = %q; want the deadline answer to count the legacy nudge and the notice", run.stderr)
	}
	state, err := nudgequeue.LoadState(f.cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("legacy nudge not released: pending %d in flight %d dead %d; stderr=%s", len(state.Pending), len(state.InFlight), len(state.Dead), run.stderr)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.MaybeDelivered != 1 || cur.Status == opjournal.StatusClosed {
		t.Fatalf("lane op = %+v, %v; want open with maybe_delivered 1", cur, err)
	}
}

// TestNudgeDrainInjectLaneOnlyDeliveryStampsSession proves an accepted
// lane-only delivery stamps the session's last nudge delivery.
func TestNudgeDrainInjectLaneOnlyDeliveryStampsSession(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.propose(t, "mail arrived")
	run := f.drain(t)
	if !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("not delivered:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	b, err := f.sessions.Get(f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata[session.MetadataLastNudgeDeliveredAt] == "" {
		t.Fatal("a lane-only delivery left no last-nudge stamp")
	}
}

// TestNudgeDrainInjectLaneCarriesTheStepReminder proves the active formula
// step's reminder rides the hook context the lane pass writes.
func TestNudgeDrainInjectLaneCarriesTheStepReminder(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.activeStep(t)
	f.propose(t, "mail arrived")
	run := f.drain(t)
	if !strings.Contains(run.stdout, "mail arrived") || !strings.Contains(run.stdout, "inspect") {
		t.Fatalf("lane context lost a part:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
}

// TestNudgeDrainInjectLaneFenceReadsTheEnvEpochs proves the hook's fence
// reads its generation from GC_RUNTIME_EPOCH and its epoch from
// GC_CONTINUATION_EPOCH: swapped, the hook is another incarnation.
func TestNudgeDrainInjectLaneFenceReadsTheEnvEpochs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gen, epoch string
		delivered  bool
	}{
		{"matching", "3", "2", true},
		{"swapped", "2", "3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			if err := f.sessions.SetMetadata(f.sessionID, "generation", "3"); err != nil {
				t.Fatal(err)
			}
			if err := f.sessions.SetMetadata(f.sessionID, "continuation_epoch", "2"); err != nil {
				t.Fatal(err)
			}
			t.Setenv(runtime.RuntimeEpochEnv, tc.gen)
			t.Setenv(runtime.ContinuationEpochEnv, tc.epoch)
			op := f.propose(t, "mail arrived")
			run := f.drain(t)
			cur, err := f.st.Get(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := cur.Outcome.Arm == opjournal.ArmSucceeded; got != tc.delivered {
				t.Fatalf("delivered = %v, want %v; stdout=%s stderr=%s", got, tc.delivered, run.stdout, run.stderr)
			}
		})
	}
}

// hookLaneFailingWriter fails every write, as a closed hook pipe would.
type hookLaneFailingWriter struct{}

func (hookLaneFailingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestNudgeDrainInjectLaneWriteFailureAcksNothing proves a combined hook
// context the drain could not write acks neither part: the legacy nudge is
// recorded failed, not acked, and the lane op is not delivered.
func TestNudgeDrainInjectLaneWriteFailureAcksNothing(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.queueNudge(t)
	op := f.propose(t, "mail arrived")
	run := recordHookCommand(t, func(_, stderr io.Writer) int {
		return cmdNudgeDrainWithFormat(nil, true, "", hookLaneFailingWriter{}, stderr)
	})
	if run.code != 0 {
		t.Fatalf("exit %d; the hook never fails the prompt; stderr=%s", run.code, run.stderr)
	}
	state, err := nudgequeue.LoadState(f.cityDir)
	if err != nil {
		t.Fatal(err)
	}
	var item nudgequeue.Item
	for _, it := range slices.Concat(state.Pending, state.InFlight, state.Dead) {
		item = it
	}
	if item.ID == "" || item.LastError == "" {
		t.Fatalf("legacy nudge after a failed write = %+v (pending %d in flight %d dead %d); want it kept with the write failure recorded", item, len(state.Pending), len(state.InFlight), len(state.Dead))
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status == opjournal.StatusClosed || cur.Failures != 1 {
		t.Fatalf("lane op = %+v, %v; want open with one failure", cur, err)
	}
}

// TestNudgeDrainInjectLaneSurvivesAClaimFailure proves a legacy queue that
// cannot be claimed still lets the in-band pass deliver the lane notice, and
// says why the legacy nudges were skipped.
func TestNudgeDrainInjectLaneSurvivesAClaimFailure(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	op := f.propose(t, "mail arrived")
	path := nudgequeue.StatePath(f.cityDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := f.drain(t)
	if !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("lane notice not delivered past a claim failure:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if !strings.Contains(run.stderr, "gc nudge drain: claiming queued nudges") {
		t.Fatalf("stderr = %q; want the claim failure reported", run.stderr)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("lane op = %+v, %v; want succeeded", cur, err)
	}
}

// leaseRecordingStore records the leases the guarded batches it applies
// write.
type leaseRecordingStore struct {
	laneTestStore
	leases *[]opjournal.Lease
}

func (s leaseRecordingStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	inner, ok := beads.GuardedBatcherFor(s.laneTestStore)
	if !ok {
		return nil, false
	}
	return leaseRecordingBatcher{inner: inner, leases: s.leases}, true
}

type leaseRecordingBatcher struct {
	inner  beads.GuardedBatcher
	leases *[]opjournal.Lease
}

func (b leaseRecordingBatcher) ApplyGuarded(ctx context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	for _, it := range batch.Items {
		if it.Update == nil || it.Update.Opts.Assignee == nil || *it.Update.Opts.Assignee == "" {
			continue
		}
		if l, err := opjournal.ParseLease(*it.Update.Opts.Assignee); err == nil {
			*b.leases = append(*b.leases, l)
		}
	}
	return b.inner.ApplyGuarded(ctx, batch)
}

// TestNudgeDrainInjectLaneLeaseOutlivesTheHookDeadline proves the drain takes
// its batch under a lease that runs to the hook deadline plus
// laneHookLeaseGrace (DES-O1 §4.6).
func TestNudgeDrainInjectLaneLeaseOutlivesTheHookDeadline(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.propose(t, "mail arrived")
	var leases []opjournal.Lease
	prev := hookLaneSessionStore
	hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store {
		return leaseRecordingStore{laneTestStore: f.sessions, leases: &leases}
	}
	t.Cleanup(func() { hookLaneSessionStore = prev })
	deadline := time.Now().Add(time.Minute)
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(deadline))
	prevStart := hookProcessStart
	hookProcessStart = time.Now()
	t.Cleanup(func() { hookProcessStart = prevStart })
	run := f.drain(t)
	if !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("not delivered:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if len(leases) == 0 {
		t.Fatal("the pass wrote no lease")
	}
	want := deadline.Add(laneHookLeaseGrace)
	if got := leases[0].Deadline; got.Before(want.Add(-2*time.Second)) || got.After(want.Add(time.Second)) {
		t.Fatalf("lease deadline = %s, want about %s (hook deadline + %s)", got, want, laneHookLeaseGrace)
	}
}

// doneStallingStore holds the guarded batch of a pass's Done until its
// context ends, as a ledger too slow to settle in the hook's time would, and
// records the deadline each held batch ran under.
type doneStallingStore struct {
	laneTestStore
	deadlines *[]time.Time
}

func (s doneStallingStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	inner, ok := beads.GuardedBatcherFor(s.laneTestStore)
	if !ok {
		return nil, false
	}
	return doneStallingBatcher{inner: inner, deadlines: s.deadlines}, true
}

type doneStallingBatcher struct {
	inner     beads.GuardedBatcher
	deadlines *[]time.Time
}

// ApplyGuarded holds the batch the lane labels as its Done and applies every
// other one.
func (b doneStallingBatcher) ApplyGuarded(ctx context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	if batch.Reason != "opjournal: lane done" {
		return b.inner.ApplyGuarded(ctx, batch)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return beads.GuardedResult{}, errors.New("the Done's batch ran with no deadline")
	}
	*b.deadlines = append(*b.deadlines, deadline)
	<-ctx.Done()
	return beads.GuardedResult{}, ctx.Err()
}

// TestNudgeDrainInjectLaneSlowDoneReleasesLegacy proves a pass whose deadline
// answer was yes, but whose Done outlasts the archive margin, retires
// nothing: the Done ends at the pass's settle-by time and leaves the lane op
// executing, to be taken again as maybe delivered once its lease lapses, and
// the drain asks the deadline again before its ack, so it releases the
// legacy nudge it wrote beside the notice.
func TestNudgeDrainInjectLaneSlowDoneReleasesLegacy(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.queueNudge(t)
	op := f.propose(t, "mail arrived")
	var deadlines []time.Time
	prev := hookLaneSessionStore
	hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store {
		return doneStallingStore{laneTestStore: f.sessions, deadlines: &deadlines}
	}
	t.Cleanup(func() { hookLaneSessionStore = prev })
	prevStart := hookProcessStart
	hookProcessStart = time.Now().Add(-100 * time.Hour)
	t.Cleanup(func() { hookProcessStart = prevStart })
	// The full archive margin and 2s more: the pass's deadline answer comes
	// well inside the 2s, and its Done is held past them.
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(hookDeadlineArchiveMarginMax+2*time.Second)))
	settleBy := hookLaneSettleBy()
	run := f.drain(t)
	if !strings.Contains(run.stdout, "review queued work") || !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("combined output missing a part:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if strings.Contains(run.stderr, "2 injected nudge(s)") {
		t.Fatalf("the pass's own deadline answer was no; it must come before %s:\n%s", settleBy, run.stderr)
	}
	if len(deadlines) != 1 || deadlines[0].After(settleBy.Add(100*time.Millisecond)) {
		t.Fatalf("Done batch deadlines = %v; want one, by the settle-by time %s", deadlines, settleBy)
	}
	if !strings.Contains(run.stderr, "too close to ack 1 injected nudge(s)") {
		t.Fatalf("stderr = %q; want the deadline asked again after the Done, refusing the legacy ack", run.stderr)
	}
	state, err := nudgequeue.LoadState(f.cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("legacy nudge not released: pending %d in flight %d dead %d; stderr=%s", len(state.Pending), len(state.InFlight), len(state.Dead), run.stderr)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status != opjournal.StatusExecuting {
		t.Fatalf("lane op = %+v, %v; want it executing, its delivery unrecorded", cur, err)
	}
}

// TestNudgeDrainInjectNoDoorbellWithoutADone: a pass that records no Done (an
// open prompt the hook cannot serve, as when the controller is down) rings no
// doorbell.
func TestNudgeDrainInjectNoDoorbellWithoutADone(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.propose(t, "fix the build", func(r *opjournal.Request) { r.Policy.Message.Role = opjournal.RolePrompt })
	run := f.drain(t)
	if run.code != 0 {
		t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
	}
	if len(f.pokes) != 0 {
		t.Fatalf("doorbells = %v; want none, the pass recorded no Done", f.pokes)
	}
}

// TestNudgeDrainInjectDoorbellRingsTheDrainsCity: the doorbell goes to the
// city the drain resolved.
func TestNudgeDrainInjectDoorbellRingsTheDrainsCity(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	f.propose(t, "mail arrived")
	var cities []string
	hookLanePoke = func(city, _ string, _ time.Duration) { cities = append(cities, city) }
	run := f.drain(t)
	if len(cities) != 1 || !samePath(cities[0], f.cityDir) {
		t.Fatalf("doorbell cities = %q, want [%s]; stderr=%s", cities, f.cityDir, run.stderr)
	}
}

// TestNudgeDrainInjectTrimsTheSessionID: a GC_SESSION_ID padded with
// whitespace still names the hook's conversation.
func TestNudgeDrainInjectTrimsTheSessionID(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	t.Setenv("GC_SESSION_ID", " "+f.sessionID+"\n")
	op := f.propose(t, "mail arrived")
	run := f.drain(t)
	cur, err := f.st.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v; want succeeded; stdout=%s stderr=%s", cur.Outcome, run.stdout, run.stderr)
	}
}

// TestNudgeDrainInjectTokenlessHook: a hook and a row that both name no token
// deliver unfenced when the env epochs name the row's incarnation, and take
// nothing when they name another (DES-O1 sec 8).
func TestNudgeDrainInjectTokenlessHook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		epoch     string
		delivered bool
	}{
		{"same incarnation", "", true},
		{"other epoch", "7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			if err := f.sessions.SetMetadata(f.sessionID, "instance_token", ""); err != nil {
				t.Fatal(err)
			}
			t.Setenv(runtime.InstanceTokenEnv, "")
			if tc.epoch != "" {
				t.Setenv(runtime.ContinuationEpochEnv, tc.epoch)
			}
			op := f.propose(t, "mail arrived")
			run := f.drain(t)
			cur, err := f.st.Get(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := cur.Outcome.Arm == opjournal.ArmSucceeded; got != tc.delivered {
				t.Fatalf("delivered = %v, want %v; stdout=%s stderr=%s", got, tc.delivered, run.stdout, run.stderr)
			}
			if tc.delivered && cur.Outcome.Degradation != opjournal.DegradationUnfencedDelivery {
				t.Fatalf("degradation = %q, want %q", cur.Outcome.Degradation, opjournal.DegradationUnfencedDelivery)
			}
		})
	}
}

// TestNudgeDrainInjectJSONFormatsRunTheLanePass: the JSON hook formats run the
// in-band pass whatever GC_PROVIDER names, and write one JSON document that
// carries the notice.
func TestNudgeDrainInjectJSONFormatsRunTheLanePass(t *testing.T) {
	for _, format := range []string{hookOutputFormatCodex, hookOutputFormatGemini, hookOutputFormatAntigravity} {
		t.Run(format, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			t.Setenv("GC_PROVIDER", format)
			op := f.propose(t, "mail arrived")
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int {
				return cmdNudgeDrainWithFormat(nil, true, format, stdout, stderr)
			})
			if !strings.Contains(run.stdout, "mail arrived") {
				t.Fatalf("lane notice not in the %s hook output:\n%s\nstderr=%s", format, run.stdout, run.stderr)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(run.stdout)), &doc); err != nil {
				t.Fatalf("%s hook output is not one JSON document: %v\n%s", format, err, run.stdout)
			}
			if cur, err := f.st.Get(op.ID); err != nil || cur.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("op = %+v, %v; want succeeded", cur, err)
			}
		})
	}
}
