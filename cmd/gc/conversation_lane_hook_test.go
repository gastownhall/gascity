package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// hookLaneHarness runs in-band passes over a lane fixture: the hook's output
// and the leases its ops were held under while the output was written are
// recorded, and its deadline checks answer as record says.
type hookLaneHarness struct {
	f        *laneFixture
	st       *opjournal.Store
	fence    runtime.InputFence
	leaseTTL time.Duration
	payload  []byte
	// record answers the pass's deadline checks in turn; once it runs out,
	// the deadline leaves time to record a delivery.
	record []error
	// deadline is the hook deadline the pass releases a refused batch by.
	deadline time.Time
	// room is the room the hook context has for the lane text.
	room int
	// stopReading is the pass's reading of the row's stop request; the zero
	// value reads it live, as an unknown controller mode does.
	stopReading session.StopRequestReading
	emitErr     error
	emitted     []string
	// leases are the leases of the ops open when each output was written.
	leases []opjournal.Lease
	stderr bytes.Buffer
}

func newHookLaneHarness(t *testing.T, opts ...laneFixtureOpt) *hookLaneHarness {
	t.Helper()
	return newHookLaneHarnessOver(t, newLaneTestStore(), opts...)
}

// newHookLaneHarnessOver is newHookLaneHarness over the sessions store store.
func newHookLaneHarnessOver(t *testing.T, store beads.Store, opts ...laneFixtureOpt) *hookLaneHarness {
	t.Helper()
	t.Setenv(hookDeadlineEnv, "")
	f := newLaneFixtureOver(t, store, opts...)
	h := &hookLaneHarness{f: f, st: f.laneStore(t), leaseTTL: laneHookLeaseTTLNoDeadline, room: hookLaneContextBudget}
	h.fence = runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	return h
}

func (h *hookLaneHarness) pass() hookLanePass {
	return hookLanePass{
		sessions: h.f.store,
		conv:     h.f.id,
		fence:    h.fence,
		leaseTTL: h.leaseTTL,
		deadline: h.deadline,
		room:     h.room,
		provider: "claude",
		payload:  h.payload,
		clk:      h.f.clk,
		stderr:   &h.stderr,
		emit: func(text string) error {
			h.emitted = append(h.emitted, text)
			h.recordLeases()
			return h.emitErr
		},
		recordTime: func() error {
			if len(h.record) == 0 {
				return nil
			}
			err := h.record[0]
			h.record = h.record[1:]
			return err
		},
		stampDelivered: true,
		stopReading: func() (session.StopRequestReading, error) {
			return h.stopReading, nil
		},
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
	if !out.emitted || out.emitErr != nil || !out.accepted {
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
	if got := laneLastNudgeStamp(t, f); got == "" {
		t.Fatal("an accepted in-band delivery left no last-nudge stamp")
	}
}

// laneLastNudgeStamp is the session's last-nudge-delivered stamp, empty when
// no delivery stamped it.
func laneLastNudgeStamp(t *testing.T, f *laneFixture) string {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	return b.Metadata[session.MetadataLastNudgeDeliveredAt]
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
	view, row := f.view(t)
	rec := &hookLaneRecordingExec{hookLaneExec: &hookLaneExec{pass: h.pass(), info: row.info, stop: row.stop}}
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

// TestHookLanePassDeadlineShortTakesNothing proves a pass whose hook deadline
// leaves no time to record a delivery takes nothing: no Take and no write
// into the hook context, the op untouched (no attempt, no failure, not maybe
// delivered) and the skip said on stderr. The next pass with time delivers
// it once, with no redelivery note.
func TestHookLanePassDeadlineShortTakesNothing(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	h.record = []error{errLaneHookDeadline}
	out := h.run(t)
	if out.emitted || out.accepted || out.done || len(h.emitted) != 0 {
		t.Fatalf("outcome = %+v, outputs %q; want nothing taken or written", out, h.emitted)
	}
	if len(h.record) != 0 {
		t.Fatal("the probe did not ask the deadline")
	}
	cur := f.get(t, h.st, op.ID)
	if cur.Status != op.Status || !cur.Lease.IsZero() || cur.Attempt != 0 || cur.Failures != 0 || cur.MaybeDelivered != 0 {
		t.Fatalf("op = %s lease %v attempt %d failures %d maybe_delivered %d; want it untouched", cur.Status, cur.Lease, cur.Attempt, cur.Failures, cur.MaybeDelivered)
	}
	if want := errLaneHookDeadline.Error() + "; its notices stay out of the hook context"; !strings.Contains(h.stderr.String(), want) {
		t.Fatalf("stderr = %q; want %q", h.stderr.String(), want)
	}
	if got := laneLastNudgeStamp(t, f); got != "" {
		t.Fatalf("a pass that took nothing stamped the session (%q)", got)
	}
	h.wantCleanDelivery(t, h.run(t), op, 1)
}

// TestHookLanePassDeadlineRaceReleases proves a batch the pass took while the
// hook deadline left time, but found none left at the write, is released
// unwritten: nothing joins the hook context, and the op returns to the lane
// after the transient backoff with no failure counted and not maybe
// delivered, so the next pass delivers it once with no redelivery note.
func TestHookLanePassDeadlineRaceReleases(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
	h.record = []error{nil, errLaneHookDeadline}
	out := h.run(t)
	if out.emitted || out.accepted || !out.done || len(h.emitted) != 0 {
		t.Fatalf("outcome = %+v, outputs %q; want a Done releasing the batch, nothing written", out, h.emitted)
	}
	if len(h.record) != 0 {
		t.Fatal("the write did not ask the deadline again")
	}
	cur := f.get(t, h.st, op.ID)
	if cur.Status != opjournal.StatusAccepted || !cur.Lease.IsZero() || cur.Attempt != 1 || cur.Failures != 0 || cur.MaybeDelivered != 0 {
		t.Fatalf("op = %s lease %v attempt %d failures %d maybe_delivered %d; want released after attempt 1, no failure, not maybe delivered",
			cur.Status, cur.Lease, cur.Attempt, cur.Failures, cur.MaybeDelivered)
	}
	if want := "transient: " + errLaneHookDeadline.Error(); cur.LastError != want {
		t.Fatalf("last error = %q, want %q", cur.LastError, want)
	}
	if want := f.clk.Now().Add(opjournal.TransientBackoffBase); !cur.RetryAt.Equal(want) {
		t.Fatalf("retry at %s, want %s, the first transient backoff", cur.RetryAt, want)
	}
	if !strings.Contains(h.stderr.String(), errLaneHookDeadline.Error()) {
		t.Fatalf("stderr = %q; want the release said", h.stderr.String())
	}
	if got := laneLastNudgeStamp(t, f); got != "" {
		t.Fatalf("a released batch stamped the session (%q)", got)
	}
	f.clk.Advance(time.Minute)
	h.wantCleanDelivery(t, h.run(t), op, 2)
}

// TestHookLanePassDeadlineShortAtMostOnce proves an at_most_once notice the
// hook deadline kept out of the hook context, before its Take or at its
// write, stays open and never maybe delivered, and the next pass with time
// delivers it exactly once.
func TestHookLanePassDeadlineShortAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record []error
		// attempt is the attempt that delivers it.
		attempt int
	}{
		{"before the take", []error{errLaneHookDeadline}, 1},
		{"at the write", []error{nil, errLaneHookDeadline}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookLaneHarness(t)
			f := h.f
			op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "once only", notice(opjournal.WhenBoundary), func(r *opjournal.Request) {
				r.Policy.Guarantee = opjournal.GuaranteeAtMostOnce
			}))
			h.record = tc.record
			h.run(t)
			if cur := f.get(t, h.st, op.ID); cur.Status == opjournal.StatusClosed || cur.MaybeDelivered != 0 || cur.Failures != 0 || len(h.emitted) != 0 {
				t.Fatalf("op = %s %+v maybe_delivered %d failures %d, outputs %q; want it open, unwritten and never maybe delivered",
					cur.Status, cur.Outcome, cur.MaybeDelivered, cur.Failures, h.emitted)
			}
			f.clk.Advance(time.Minute)
			h.wantCleanDelivery(t, h.run(t), op, tc.attempt)
		})
	}
}

// TestHookLanePassDeadlineRaceReleaseEndsAtTheDeadline proves the release of
// a batch the pass did not get in front of the model, the deadline refusing
// the write or the write failing, runs only to the hook deadline, not to the
// floor of a Done's budget: a ledger too slow to land it by then leaves the op
// executing with no failure counted, for the lane to take again once the
// hook's lease lapses.
func TestHookLanePassDeadlineRaceReleaseEndsAtTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		record  []error
		emitErr error
		// writes is how many writes the pass tries: none once the deadline
		// refuses, one that fails.
		writes int
	}{
		{"the deadline refused the write", []error{nil, errLaneHookDeadline}, nil, 0},
		{"the write failed", nil, errors.New("broken pipe"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadlines []time.Time
			h := newHookLaneHarnessOver(t, doneStallingStore{laneTestStore: newLaneTestStore(), deadlines: &deadlines})
			f := h.f
			op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
			h.record = tc.record
			h.emitErr = tc.emitErr
			h.deadline = f.clk.Now().Add(100 * time.Millisecond)
			start := time.Now()
			out := h.run(t)
			if out.emitted != (tc.writes > 0) || len(h.emitted) != tc.writes || out.done {
				t.Fatalf("outcome = %+v, outputs %q; want %d write(s) and no Done landed", out, h.emitted, tc.writes)
			}
			if len(deadlines) != 1 || !deadlines[0].Before(start.Add(time.Second)) {
				t.Fatalf("release batch deadlines = %v; want one, by the hook deadline 100ms after %s", deadlines, start)
			}
			cur := f.get(t, h.st, op.ID)
			if cur.Status != opjournal.StatusExecuting || cur.Failures != 0 || cur.MaybeDelivered != 0 {
				t.Fatalf("op = %s failures %d maybe_delivered %d; want it executing, no failure counted", cur.Status, cur.Failures, cur.MaybeDelivered)
			}
		})
	}
}

// wantCleanDelivery asserts out is the harness's one write, op's delivery
// with no redelivery note, and that op closed succeeded on attempt with no
// failure counted, never maybe delivered.
func (h *hookLaneHarness) wantCleanDelivery(t *testing.T, out hookLaneOutcome, op opjournal.Op, attempt int) {
	t.Helper()
	if !out.accepted || len(h.emitted) != 1 {
		t.Fatalf("outcome = %+v, outputs %d; want one accepted write; stderr=%s", out, len(h.emitted), h.stderr.String())
	}
	want, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	if h.emitted[0] != want || strings.Contains(h.emitted[0], opjournal.RedeliveryNote) {
		t.Fatalf("hook output = %q, want %q with no redelivery note", h.emitted[0], want)
	}
	cur := h.f.get(t, h.st, op.ID)
	if cur.Outcome.Arm != opjournal.ArmSucceeded || cur.Attempt != attempt || cur.Failures != 0 || cur.MaybeDelivered != 0 {
		t.Fatalf("op = %+v attempt %d failures %d maybe_delivered %d; want succeeded on attempt %d, no failure, never maybe delivered",
			cur.Outcome, cur.Attempt, cur.Failures, cur.MaybeDelivered, attempt)
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
// takes nothing in band: a draining row, and a row v2 asked to stop at its
// generation, which the hook, holding no latch, reads by the controller's
// latched mode, live under v2 or when the mode is unknown (V2-18). Another
// generation's request blocks nothing, and neither does a request read as
// residue, as a controller that latched legacy has the hook read it.
func TestHookLanePassHonorsTheContentGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opt     laneFixtureOpt
		reading session.StopRequestReading
		emit    bool
	}{
		{"draining", func(meta map[string]string) { meta["state"] = string(session.StateDraining) }, session.StopRequestsLive, false},
		{"draining, stop requests read as residue", func(meta map[string]string) { meta["state"] = string(session.StateDraining) }, session.StopRequestsIgnored, false},
		{"stop requested at the row's generation", laneStopRequestedAt(1), session.StopRequestsLive, false},
		{"stop request of another generation", laneStopRequestedAt(2), session.StopRequestsLive, true},
		{"stop request read as residue", laneStopRequestedAt(1), session.StopRequestsIgnored, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookLaneHarness(t, tc.opt)
			h.stopReading = tc.reading
			op := h.f.propose(t, h.st, h.f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary)))
			out := h.run(t)
			if out.emitted != tc.emit {
				t.Fatalf("outcome = %+v; want emitted %v", out, tc.emit)
			}
			if cur := h.f.get(t, h.st, op.ID); !tc.emit && cur.Attempt != 0 {
				t.Fatalf("op attempt %d; want untouched", cur.Attempt)
			}
		})
	}
}

// TestHookLanePassHoldsTheDrainNotice (V2-18) pins the scope of the drain
// notice's exemption from draining and stop_requested: it is the lane
// executors' (the controller's lane host and the CLI's inline pass), which
// admit the notice's class. The hook admits no class, so a draining or
// stop-requested row holds the notice in band too, and the controller's
// lane host, which proposes it only when it can type into the session,
// delivers it.
func TestHookLanePassHoldsTheDrainNotice(t *testing.T) {
	drainNotice := func(r *opjournal.Request) {
		r.Producer = opjournal.Producer{Principal: laneDrainNoticeClass.Principal, Actor: opjournal.ActorController}
		r.Policy.Message = opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}
	}
	for _, tc := range []struct {
		name string
		opt  laneFixtureOpt
	}{
		{"draining", func(meta map[string]string) { meta["state"] = string(session.StateDraining) }},
		{"stop requested at the row's generation", laneStopRequestedAt(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHookLaneHarness(t, tc.opt)
			op := h.f.propose(t, h.st, h.f.msg("drain", opjournal.RoleNotice, "wrap up", drainNotice))
			if out := h.run(t); out.emitted {
				t.Fatalf("outcome = %+v; want the drain notice held", out)
			}
			if cur := h.f.get(t, h.st, op.ID); cur.Status != opjournal.StatusSubmitted || cur.Attempt != 0 {
				t.Fatalf("op %s attempt %d; want it submitted and untouched", cur.Status, cur.Attempt)
			}
		})
	}
}

// TestHookLanePassWriteFailure proves a hook context that could not be
// written releases the batch as a transient miss: the write error is
// reported, and the op returns to the lane after the transient backoff with
// no failure counted and not maybe delivered, so an at_most_once notice stays
// open too. The next pass delivers it once with no redelivery note.
func TestHookLanePassWriteFailure(t *testing.T) {
	for _, guarantee := range []opjournal.Guarantee{opjournal.GuaranteeAtLeastOnce, opjournal.GuaranteeAtMostOnce} {
		t.Run(string(guarantee), func(t *testing.T) {
			h := newHookLaneHarness(t)
			f := h.f
			op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, "mail arrived", notice(opjournal.WhenBoundary), func(r *opjournal.Request) {
				r.Policy.Guarantee = guarantee
			}))
			h.emitErr = errors.New("broken pipe")
			out := h.run(t)
			if !out.emitted || !errors.Is(out.emitErr, h.emitErr) || out.accepted || !out.done {
				t.Fatalf("outcome = %+v; want the write error reported and a Done releasing the batch", out)
			}
			cur := f.get(t, h.st, op.ID)
			if cur.Status != opjournal.StatusAccepted || !cur.Lease.IsZero() || cur.Attempt != 1 || cur.Failures != 0 || cur.MaybeDelivered != 0 {
				t.Fatalf("op = %s lease %v attempt %d failures %d maybe_delivered %d; want released after attempt 1, no failure, not maybe delivered",
					cur.Status, cur.Lease, cur.Attempt, cur.Failures, cur.MaybeDelivered)
			}
			if want := "transient: writing the hook context: broken pipe"; cur.LastError != want {
				t.Fatalf("last error = %q, want %q", cur.LastError, want)
			}
			if want := f.clk.Now().Add(opjournal.TransientBackoffBase); !cur.RetryAt.Equal(want) {
				t.Fatalf("retry at %s, want %s, the first transient backoff", cur.RetryAt, want)
			}
			if got := laneLastNudgeStamp(t, f); got != "" {
				t.Fatalf("a failed write stamped the session (%q)", got)
			}
			h.emitErr, h.emitted = nil, nil
			f.clk.Advance(time.Minute)
			h.wantCleanDelivery(t, h.run(t), op, 2)
		})
	}
}

// TestHookLanePassTakesOnlyWhatFitsTheRoom proves the pass holds its batch to
// the room the hook context has for the lane text (Caps.MaxText): with room
// for one notice, it writes that notice alone and leaves the next untaken,
// and the next pass delivers it.
func TestHookLanePassTakesOnlyWhatFitsTheRoom(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	first := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, strings.Repeat("a", 400), notice(opjournal.WhenBoundary)))
	second := f.propose(t, h.st, f.msg("k2", opjournal.RoleNotice, strings.Repeat("b", 400), notice(opjournal.WhenBoundary)))
	alone, err := opjournal.RenderDelivery([]opjournal.Op{first}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	h.room = len(alone)
	if out := h.run(t); !out.accepted || len(h.emitted) != 1 || h.emitted[0] != alone {
		t.Fatalf("outcome = %+v, outputs %q; want the first notice alone, its %d bytes", out, h.emitted, len(alone))
	}
	if cur := f.get(t, h.st, second.ID); cur.Status == opjournal.StatusClosed || cur.Attempt != 0 {
		t.Fatalf("second notice = %s attempt %d; want it left untaken", cur.Status, cur.Attempt)
	}
	h.emitted = nil
	if out := h.run(t); !out.accepted || len(h.emitted) != 1 || !strings.Contains(h.emitted[0], strings.Repeat("b", 400)) {
		t.Fatalf("next pass = %+v, outputs %q; want the second notice", out, h.emitted)
	}
}

// TestHookLanePassLeavesANoticeTooLongForTheRoom proves a notice whose text
// alone is over the room is not taken: nothing is written, and the op is left
// untouched for a runtime with room.
func TestHookLanePassLeavesANoticeTooLongForTheRoom(t *testing.T) {
	h := newHookLaneHarness(t)
	f := h.f
	op := f.propose(t, h.st, f.msg("k1", opjournal.RoleNotice, strings.Repeat("a", 400), notice(opjournal.WhenBoundary)))
	alone, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
	if err != nil {
		t.Fatal(err)
	}
	h.room = len(alone) - 1
	if out := h.run(t); out.emitted || out.done || len(h.emitted) != 0 {
		t.Fatalf("outcome = %+v, outputs %q; want nothing taken or written", out, h.emitted)
	}
	if cur := f.get(t, h.st, op.ID); cur.Status == opjournal.StatusClosed || cur.Attempt != 0 || !cur.Lease.IsZero() {
		t.Fatalf("op = %s attempt %d lease %v; want it untouched", cur.Status, cur.Attempt, cur.Lease)
	}
}

// TestHookLaneDeliverRefusesATextOverTheRoom proves Deliver writes no text
// longer than the room, whatever batch it is handed: it releases the batch as
// a transient miss by the hook deadline and keeps the reason, and a text that
// fits is written.
func TestHookLaneDeliverRefusesATextOverTheRoom(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	var emitted []string
	exec := &hookLaneExec{pass: hookLanePass{
		room:       10,
		deadline:   deadline,
		clk:        clock.Real{},
		emit:       func(text string) error { emitted = append(emitted, text); return nil },
		recordTime: func() error { return nil },
	}}
	res := exec.Deliver(context.Background(), opjournal.Delivery{Text: strings.Repeat("x", 11)})
	if nd, ok := res.(opjournal.NotDelivered); !ok || !nd.Transient || !errors.Is(nd.Cause, errLaneHookNoRoom) || !nd.SettleBy.Equal(deadline) {
		t.Fatalf("Deliver(11 bytes) = %#v; want a transient release over the room, by the deadline", res)
	}
	if len(emitted) != 0 || exec.emitted || !errors.Is(exec.refused, errLaneHookNoRoom) {
		t.Fatalf("outputs %q (emitted %v), refused %v; want nothing written and the refusal kept", emitted, exec.emitted, exec.refused)
	}
	if res := exec.Deliver(context.Background(), opjournal.Delivery{Text: strings.Repeat("x", 10)}); len(emitted) != 1 {
		t.Fatalf("Deliver(10 bytes) = %#v, outputs %q; want the text that fits written", res, emitted)
	} else if _, ok := res.(opjournal.Delivered); !ok {
		t.Fatalf("Deliver(10 bytes) = %#v; want Delivered", res)
	}
}

// TestHookLaneTextRoom proves the room the hook context leaves the lane text:
// the budget less the drain's own text, and none once that text fills it.
func TestHookLaneTextRoom(t *testing.T) {
	if got := hookLaneTextRoom(); got != hookLaneContextBudget {
		t.Fatalf("room beside nothing = %d, want the budget %d", got, hookLaneContextBudget)
	}
	if got := hookLaneTextRoom("clock\n", "", "step"); got != hookLaneContextBudget-10 {
		t.Fatalf("room beside 10 bytes = %d, want %d", got, hookLaneContextBudget-10)
	}
	if got := hookLaneTextRoom(strings.Repeat("x", hookLaneContextBudget)); got > 0 {
		t.Fatalf("room beside a full context = %d, want none", got)
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
	// No controller answers by default: the mode is unknown.
	setLaneControllerMode(t, reconcilerLegacy, false)
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
// lane host, and after a delivery that ended unknown (the plain note). The
// output is one hook context, and it carries no identifier (I14).
func TestNudgeDrainInjectLaneGolden(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "hook_lane"))
	if err != nil {
		t.Fatal(err)
	}
	update := os.Getenv(hookBudgetGoldenUpdateEnv) != ""
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op
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
		{name: "redelivery note", seed: func(t *testing.T, f *hookLaneDrainFixture) []opjournal.Op {
			return []opjournal.Op{f.maybeDeliver(t, f.propose(t, "mail arrived"))}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			ops := tc.seed(t, f)
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

// maybeDeliver runs a lane pass whose delivery of op ends unknown, as a
// runtime gone mid-delivery leaves it, and ages its retry gate: op is back on
// the lane maybe delivered, for the next drain to deliver with the note.
func (f *hookLaneDrainFixture) maybeDeliver(t *testing.T, op opjournal.Op) opjournal.Op {
	t.Helper()
	lane, err := opjournal.NewLane(f.st, laneHostName, clock.Real{})
	if err != nil {
		t.Fatal(err)
	}
	view, _, _, err := readConversationLaneView(sessionFrontDoor(f.sessions), f.sessionID, session.StopRequestsLive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lane.Pass(context.Background(), view, laneUnknownExec{laneBoundaryExec{boundary: opjournal.TriYes}}); err != nil {
		t.Fatalf("lane pass: %v", err)
	}
	cur, err := f.st.Get(op.ID)
	if err != nil || cur.Status == opjournal.StatusClosed || cur.MaybeDelivered != 1 {
		t.Fatalf("op after the unknown delivery = %+v, %v; want it open, maybe delivered on attempt 1", cur, err)
	}
	f.ageRetry(t, cur)
	return cur
}

// laneUnknownExec is laneBoundaryExec with every delivery ending unknown.
type laneUnknownExec struct{ laneBoundaryExec }

func (laneUnknownExec) Deliver(context.Context, opjournal.Delivery) opjournal.DeliveryResult {
	return opjournal.Unknown{Cause: errors.New("the runtime went away mid-delivery")}
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
// of the model as the turn starts: the codex and gemini JSON formats, and
// plain text only for the claude family, the family read from the provider's
// name, so claude spellings count. The opencode and mimocode plugins, which
// groq and cerebras run too, drain for every session in their process, and
// antigravity drains before every model call into an ephemeral message, so
// their notices wait for the lane host.
func TestHookLaneReachesModel(t *testing.T) {
	for _, tc := range []struct {
		format, provider string
		want             bool
	}{
		{"", "claude", true},
		{"", "claude-eco", true},
		{"", "claude/tmux-cli", true},
		{"", " Claude ", true},
		{hookOutputFormatCodex, "codex", true},
		{hookOutputFormatGemini, "gemini", true},
		{hookOutputFormatAntigravity, "antigravity", false},
		{" Codex ", "", true},
		{"", "opencode", false},
		{"", "mimocode", false},
		{"", "groq", false},
		{"", "cerebras", false},
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

// TestNudgeDrainInjectLaneSkipsFamiliesWithoutInBandOutput proves a hook
// whose output does not reach the model at the start of the turn takes no
// lane op and does not even probe for one: plain text for a family that does
// not show it (cursor, copilot), and antigravity's ephemeral message, which
// it writes before every model call. The notice waits for the lane host.
func TestNudgeDrainInjectLaneSkipsFamiliesWithoutInBandOutput(t *testing.T) {
	for _, tc := range []struct{ format, family string }{
		{"", "cursor"}, {"", "copilot"}, {"", ""}, {hookOutputFormatAntigravity, "antigravity"},
	} {
		t.Run(fmt.Sprintf("format %q family %q", tc.format, tc.family), func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			t.Setenv("GC_PROVIDER", tc.family)
			op := f.propose(t, "mail arrived")
			lists := f.countLaneLists(t)
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int {
				return cmdNudgeDrainWithFormat(nil, true, tc.format, stdout, stderr)
			})
			if strings.Contains(run.stdout, "mail arrived") {
				t.Fatalf("a %q %q hook wrote the lane notice:\n%s", tc.format, tc.family, run.stdout)
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
		t.Fatalf("unparseable deadline: settle by %s, want no bound (the deadline check refuses before the Take)", got)
	}
}

// TestHookLaneRecordTime proves the pass's deadline check: there is time to
// record a delivery without a deadline or with the archive margin still
// ahead, none once the margin is gone, and a deadline that does not parse
// refuses with its parse error.
func TestHookLaneRecordTime(t *testing.T) {
	prevStart := hookProcessStart
	t.Cleanup(func() { hookProcessStart = prevStart })
	hookProcessStart = time.Now().Add(-100 * time.Hour)
	t.Setenv(hookDeadlineEnv, "")
	if err := hookLaneRecordTime(); err != nil {
		t.Fatalf("no deadline: %v, want time to record", err)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Minute)))
	if err := hookLaneRecordTime(); err != nil {
		t.Fatalf("a minute left: %v, want time to record", err)
	}
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(time.Second)))
	if err := hookLaneRecordTime(); !errors.Is(err, errLaneHookDeadline) {
		t.Fatalf("a second left: %v, want %v", err, errLaneHookDeadline)
	}
	t.Setenv(hookDeadlineEnv, "soon")
	if err := hookLaneRecordTime(); err == nil || errors.Is(err, errLaneHookDeadline) || !strings.Contains(err.Error(), hookDeadlineEnv) {
		t.Fatalf("unparseable deadline: %v, want the parse error", err)
	}
}

// TestHookLaneDeadline proves a refused batch's release is bounded by the
// inherited hook deadline itself, and by nothing without one or with one that
// does not parse.
func TestHookLaneDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Truncate(time.Millisecond)
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(deadline))
	if got := hookLaneDeadline(); !got.Equal(deadline) {
		t.Fatalf("deadline = %s, want %s", got, deadline)
	}
	for _, raw := range []string{"", "soon"} {
		t.Setenv(hookDeadlineEnv, raw)
		if got := hookLaneDeadline(); !got.IsZero() {
			t.Fatalf("%s=%q: deadline = %s, want none", hookDeadlineEnv, raw, got)
		}
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

// TestNudgeDrainInjectLaneReadsStopRequestsByTheLatchedMode (V2-18) drives
// the injecting drain over a row carrying v2's stop request at its
// generation. The hook holds no latch, so it reads the request by the mode
// the city's controller latched: a legacy controller, as after a rollback,
// has it read the request as residue at once, so the notice reaches the
// model; under v2, or with no controller answering, it honors the request
// and leaves the notice for the controller's lane host.
func TestNudgeDrainInjectLaneReadsStopRequestsByTheLatchedMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      reconcilerMode
		known     bool
		delivered bool
	}{
		{"latched legacy", reconcilerLegacy, true, true},
		{"latched v2", reconcilerV2, true, false},
		{"mode unknown", reconcilerLegacy, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			row, err := f.sessions.Get(f.sessionID)
			if err != nil {
				t.Fatal(err)
			}
			gen, err := strconv.ParseInt(row.Metadata["generation"], 10, 64)
			if err != nil {
				t.Fatalf("row generation %q: %v", row.Metadata["generation"], err)
			}
			for k, v := range stopBeginPatch(censusRow{Incarnation: gen}, drainIdle, laneT0) {
				if err := f.sessions.SetMetadata(f.sessionID, k, v); err != nil {
					t.Fatal(err)
				}
			}
			asks := setLaneControllerMode(t, tc.mode, tc.known)
			op := f.propose(t, "mail arrived")
			run := f.drain(t)
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			if !slices.Equal(asks.cities, []string{f.cityDir}) || !slices.Equal(asks.legs, []time.Duration{laneHookPokeTimeout}) {
				t.Fatalf("mode asks %+v; want one for the city %s with the doorbell's leg %s", asks, f.cityDir, laneHookPokeTimeout)
			}
			if noted := strings.Contains(run.stderr, "stop request is honored because the controller's session reconciler is unknown"); noted == tc.known {
				t.Fatalf("stderr %q: unknown-mode note %v, want %v", run.stderr, noted, !tc.known)
			}
			cur, err := f.st.Get(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(run.stdout, "mail arrived"); got != tc.delivered {
				t.Fatalf("hook output carries the notice: %v, want %v:\n%s", got, tc.delivered, run.stdout)
			}
			switch {
			case tc.delivered && (cur.Outcome.Arm != opjournal.ArmSucceeded || cur.Outcome.Result.Via != laneViaHook):
				t.Fatalf("op %s = %s %+v; want succeeded via hook", cur.ID, cur.Status, cur.Outcome)
			case !tc.delivered && (cur.Status == opjournal.StatusClosed || cur.Attempt != 0):
				t.Fatalf("op %s = %s attempt %d; want it left for the lane host", cur.ID, cur.Status, cur.Attempt)
			}
		})
	}
}

// TestNudgeDrainInjectLaneAsksTheModeOnlyWhenDue (V2-18) proves the drain
// asks the controller for its latched mode only once the in-band pass runs:
// with nothing due it asks nothing.
func TestNudgeDrainInjectLaneAsksTheModeOnlyWhenDue(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	asks := setLaneControllerMode(t, reconcilerLegacy, false)
	if run := f.drain(t); run.code != 0 {
		t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
	}
	if len(asks.cities) != 0 {
		t.Fatalf("a drain with nothing due asked the controller's mode %d times, want none", len(asks.cities))
	}
	f.propose(t, "mail arrived")
	if run := f.drain(t); run.code != 0 || !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("exit %d, stdout %q; stderr=%s", run.code, run.stdout, run.stderr)
	}
	if len(asks.cities) != 1 {
		t.Fatalf("a drain with a notice due asked the controller's mode %d times, want once", len(asks.cities))
	}
}

// TestNudgeDrainInjectLaneAsksTheModeWithinTheHookDeadline (V2-18) proves
// the drain bounds its ask for the controller's latched mode by the hook's
// own deadline: with little time left, the leg is shorter than the
// doorbell's full timeout, never the fixed constant.
func TestNudgeDrainInjectLaneAsksTheModeWithinTheHookDeadline(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	asks := setLaneControllerMode(t, reconcilerLegacy, true)
	f.propose(t, "mail arrived")
	withShortHookDeadline(t, func() { f.drain(t) })
	if len(asks.legs) != 1 || asks.legs[0] <= 0 || asks.legs[0] >= laneHookPokeTimeout {
		t.Fatalf("mode asks %+v; want one leg in (0, %s), bounded by the hook deadline", asks, laneHookPokeTimeout)
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

// TestNudgeDrainInjectLaneShortDeadlineTakesNothing proves a drain whose hook
// deadline leaves no time to record a delivery takes no lane op: the notice
// stays out of the hook context, untouched (no attempt, no failure, not maybe
// delivered) for the next drain or the lane host, and the skip is said on
// stderr. A legacy nudge the drain wrote alone is released for redelivery,
// never acked.
func TestNudgeDrainInjectLaneShortDeadlineTakesNothing(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy nudge %v", legacy), func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			if legacy {
				f.queueNudge(t)
			}
			op := f.propose(t, "mail arrived")
			var run hookBudgetRun
			withShortHookDeadline(t, func() { run = f.drain(t) })
			if run.code != 0 || strings.Contains(run.stdout, "mail arrived") || strings.Contains(run.stdout, "review queued work") != legacy {
				t.Fatalf("exit %d, output:\n%s\nwant the notice kept out and the legacy nudge (%v) written", run.code, run.stdout, legacy)
			}
			if !strings.Contains(run.stderr, errLaneHookDeadline.Error()) {
				t.Fatalf("stderr = %q; want the pass's skip said", run.stderr)
			}
			if cur, err := f.st.Get(op.ID); err != nil || cur.Status == opjournal.StatusClosed || cur.Attempt != 0 || cur.Failures != 0 || cur.MaybeDelivered != 0 {
				t.Fatalf("lane op = %+v, %v; want it untouched", cur, err)
			}
			if len(f.pokes) != 0 {
				t.Fatalf("doorbells = %v; want none, the pass recorded no Done", f.pokes)
			}
			if !legacy {
				return
			}
			if !strings.Contains(run.stderr, "too close to ack 1 injected nudge(s)") {
				t.Fatalf("stderr = %q; want the legacy ack refused", run.stderr)
			}
			state, err := nudgequeue.LoadState(f.cityDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Pending) != 1 {
				t.Fatalf("legacy nudge not released: pending %d in flight %d dead %d; stderr=%s", len(state.Pending), len(state.InFlight), len(state.Dead), run.stderr)
			}
		})
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

// codexHookContextTokenCap is the cap Codex holds a hook handler's
// additionalContext to when the handler sets no additionalContextLimit, as
// none in gc's .codex/hooks.json does (codex-rs hooks/src/output_spill.rs).
const codexHookContextTokenCap = 2500

// codexApproxTokens is the count Codex measures an additionalContext by
// against its cap: a quarter of its bytes, rounded up (codex-rs
// utils/string approx_token_count), not the tokens a tokenizer would make.
func codexApproxTokens(text string) int { return (len(text) + 3) / 4 }

// hookModelContext is the hook context a provider puts in front of the model
// from a drain's stdout in format: the text itself for the plain format, and
// the decoded additionalContext of the one document a JSON format writes.
func hookModelContext(t *testing.T, format, stdout string) string {
	t.Helper()
	if format == "" {
		return stdout
	}
	var doc struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%s hook output is not one JSON document: %v\n%.300s", format, err, stdout)
	}
	return doc.HookSpecificOutput.AdditionalContext
}

// TestNudgeDrainInjectLaneFitsTheHookContextBudget proves the hook context a
// drain writes with lane notices in it stays within hookLaneContextBudget, the
// clock line and the legacy nudge beside them counted, in every format whose
// output reaches the model, measured as the model gets it (a JSON format's
// additionalContext, decoded). It stays within Codex's cap as Codex counts
// it too, for identifier and markup text that a tokenizer would split finer
// than prose. A backlog over the room goes out over several drains, each
// notice once.
func TestNudgeDrainInjectLaneFitsTheHookContextBudget(t *testing.T) {
	dense := strings.Repeat("gc-7f3a.<op:k/9>&_x2;", 95)
	for _, tc := range []struct{ name, format string }{
		{"plain", ""},
		{"codex", hookOutputFormatCodex},
		{"gemini", hookOutputFormatGemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookLaneDrainFixture(t)
			if tc.format != "" {
				t.Setenv("GC_PROVIDER", tc.format)
			}
			f.queueNudge(t)
			var ops []opjournal.Op
			for i := range 6 {
				key := func(r *opjournal.Request) { r.ClientKey = fmt.Sprintf("k:big-%d", i) }
				ops = append(ops, f.propose(t, fmt.Sprintf("notice %d %s", i, dense), key))
			}
			delivered := func() int {
				n := 0
				for _, op := range ops {
					if cur, err := f.st.Get(op.ID); err == nil && cur.Outcome.Arm == opjournal.ArmSucceeded {
						n++
					}
				}
				return n
			}
			drain := func() string {
				t.Helper()
				run := recordHookCommand(t, func(stdout, stderr io.Writer) int {
					return cmdNudgeDrainWithFormat(nil, true, tc.format, stdout, stderr)
				})
				text := hookModelContext(t, tc.format, run.stdout)
				if len(text) > hookLaneContextBudget || codexApproxTokens(text) > codexHookContextTokenCap {
					t.Fatalf("hook context is %d bytes, %d tokens as Codex counts them; want at most %d bytes and %d tokens; stderr=%s",
						len(text), codexApproxTokens(text), hookLaneContextBudget, codexHookContextTokenCap, run.stderr)
				}
				return text
			}
			if text := drain(); !strings.Contains(text, "review queued work") || !strings.Contains(text, "notice 0 ") {
				t.Fatalf("first hook context lost the legacy nudge or the first notice:\n%.300s", text)
			}
			if n := delivered(); n == 0 || n == len(ops) {
				t.Fatalf("first drain delivered %d of %d notices; want the ones that fit, not all", n, len(ops))
			}
			for range ops {
				if delivered() == len(ops) {
					break
				}
				if text := drain(); !strings.Contains(text, "notice ") {
					t.Fatalf("a later hook context carries no notice:\n%.300s", text)
				}
			}
			for _, op := range ops {
				if cur, err := f.st.Get(op.ID); err != nil || cur.Outcome.Arm != opjournal.ArmSucceeded || cur.Attempt != 1 {
					t.Fatalf("op %s = %+v attempt %d, %v; want each notice delivered once", op.ID, cur.Outcome, cur.Attempt, err)
				}
			}
		})
	}
}

// TestNudgeDrainInjectLaneRunsNoPassWithoutRoom proves a drain whose own hook
// context already fills the budget takes no lane op: its legacy nudge is
// written alone, the notice stays untouched for a later drain or the lane
// host, and the skip is said on stderr.
func TestNudgeDrainInjectLaneRunsNoPassWithoutRoom(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	big := strings.Repeat("y", hookLaneContextBudget)
	if err := enqueueQueuedNudge(f.cityDir, newQueuedNudge(hookBudgetIdentity, big, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	resetHookBudgetProcessState(t)
	asks := setLaneControllerMode(t, reconcilerLegacy, true)
	op := f.propose(t, "mail arrived")
	run := f.drain(t)
	if len(asks.cities) != 0 {
		t.Fatalf("a drain with no room asked the controller's mode %d times, want none: it asks only once the pass runs", len(asks.cities))
	}
	if strings.Contains(run.stdout, "mail arrived") || !strings.Contains(run.stdout, big) {
		t.Fatalf("output carries the notice, or lost the legacy nudge:\n%.200s", run.stdout)
	}
	if !strings.Contains(run.stderr, "leaves no room for lane notices") {
		t.Fatalf("stderr = %q; want the skip said", run.stderr)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status == opjournal.StatusClosed || cur.Attempt != 0 {
		t.Fatalf("lane op = %+v, %v; want it untouched", cur, err)
	}
}

// TestNudgeDrainInjectLaneControllerUpEmptyPromptOpensNoStore proves an empty
// prompt whose step reminder the controller answered opens no store for
// lanes, even with a lane op open: the controller's lane host serves the
// conversation, and the notice stays for it.
func TestNudgeDrainInjectLaneControllerUpEmptyPromptOpensNoStore(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	op := f.propose(t, "mail arrived")
	withActiveWorkStepClient(t, readyWorkTestServer(t, hookCityController(t, f.hookBudgetFixture)), "")
	laneStores := 0
	prev := hookLaneSessionStore
	hookLaneSessionStore = func(string, *config.City, io.Writer) beads.Store {
		laneStores++
		return f.sessions
	}
	t.Cleanup(func() { hookLaneSessionStore = prev })
	run := f.drain(t)
	if run.opens != 0 || laneStores != 0 {
		t.Fatalf("the drain opened %d stores and asked for the lane store %d times; want none with the controller up", run.opens, laneStores)
	}
	if strings.Contains(run.stdout, "mail arrived") || len(f.pokes) != 0 {
		t.Fatalf("output carries the notice, or rang the lane host %v:\n%s", f.pokes, run.stdout)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status == opjournal.StatusClosed || cur.Attempt != 0 {
		t.Fatalf("lane op = %+v, %v; want it left for the lane host", cur, err)
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
// recorded failed, not acked, and the lane op is released, not delivered: no
// failure counted, not maybe delivered.
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
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status != opjournal.StatusAccepted || cur.Failures != 0 || cur.MaybeDelivered != 0 {
		t.Fatalf("lane op = %+v, %v; want it released, no failure counted, not maybe delivered", cur, err)
	}
}

// TestNudgeDrainInjectLaneOnlyWriteFailureIsReported proves a lane-only hook
// context the drain could not write is said on stderr, where the combined
// write's failure is recorded on its legacy nudges, and the lane op is
// released, not delivered: no failure counted, not maybe delivered.
func TestNudgeDrainInjectLaneOnlyWriteFailureIsReported(t *testing.T) {
	f := newHookLaneDrainFixture(t)
	op := f.propose(t, "mail arrived")
	run := recordHookCommand(t, func(_, stderr io.Writer) int {
		return cmdNudgeDrainWithFormat(nil, true, "", hookLaneFailingWriter{}, stderr)
	})
	if run.code != 0 {
		t.Fatalf("exit %d; the hook never fails the prompt; stderr=%s", run.code, run.stderr)
	}
	if !strings.Contains(run.stderr, "writing the hook context") || !strings.Contains(run.stderr, "broken pipe") {
		t.Fatalf("stderr = %q; want the write failure said", run.stderr)
	}
	if cur, err := f.st.Get(op.ID); err != nil || cur.Status != opjournal.StatusAccepted || cur.Failures != 0 || cur.MaybeDelivered != 0 {
		t.Fatalf("lane op = %+v, %v; want it released, no failure counted, not maybe delivered", cur, err)
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
// checks found time, but whose Done outlasts the archive margin, retires
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
	// The full archive margin and 2s more: the pass's deadline checks come
	// well inside the 2s, and its Done is held past them.
	t.Setenv(hookDeadlineEnv, hookDeadlineEnvValue(time.Now().Add(hookDeadlineArchiveMarginMax+2*time.Second)))
	settleBy := hookLaneSettleBy()
	run := f.drain(t)
	if !strings.Contains(run.stdout, "review queued work") || !strings.Contains(run.stdout, "mail arrived") {
		t.Fatalf("combined output missing a part:\n%s\nstderr=%s", run.stdout, run.stderr)
	}
	if strings.Contains(run.stderr, errLaneHookDeadline.Error()) {
		t.Fatalf("the pass's own deadline checks found no time; they must come before %s:\n%s", settleBy, run.stderr)
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

// TestNudgeDrainInjectJSONFormatsRunTheLanePass: the codex and gemini JSON
// hook formats run the in-band pass whatever GC_PROVIDER names, and write one
// JSON document that carries the notice.
func TestNudgeDrainInjectJSONFormatsRunTheLanePass(t *testing.T) {
	for _, format := range []string{hookOutputFormatCodex, hookOutputFormatGemini} {
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
