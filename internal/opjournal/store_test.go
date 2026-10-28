package opjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

type harness struct {
	mem   *beads.MemStore
	rec   *beadstest.OpRecordingStore
	clk   *clock.Fake
	store *Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	return newHarnessOver(t, mem, mem)
}

// newHarnessOver records every operation on backing (a store wrapping mem, or
// mem itself) and checks, at cleanup, that the op store only ever listed by
// a key: gc.op.open, or gc.op.status on op rows.
func newHarnessOver(t *testing.T, mem *beads.MemStore, backing beads.Store) *harness {
	t.Helper()
	rec := beadstest.NewOpRecordingStore(backing)
	clk := &clock.Fake{Time: t0}
	s, err := NewStore(beads.SessionStore{Store: rec}, clk, WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { assertOnlyKeyedLists(t, rec) })
	return &harness{mem: mem, rec: rec, clk: clk, store: s}
}

func assertOnlyKeyedLists(t *testing.T, rec *beadstest.OpRecordingStore) {
	t.Helper()
	for _, op := range rec.Ops() {
		if op.Kind != beadstest.OpList {
			continue
		}
		q := op.Query
		openKeyed := q.Label == "" && len(q.Metadata) == 1 && q.Metadata[keyOpen] != ""
		legacyKeyed := q.Label == LabelOp && len(q.Metadata) == 1 && q.Metadata[keyStatus] == string(StatusSubmitted)
		laneScanKeyed := q.Label == LabelOp && len(q.Metadata) == 2 && q.Metadata[keyExecutor] == string(ExecutorLane) && q.Metadata[keyStatus] != ""
		openLaneKeyed := q.Label == "" && len(q.Metadata) == 2 && q.Metadata[keyOpen] != "" && q.Metadata[keyExecutor] == string(ExecutorLane)
		interruptsKeyed := strings.HasPrefix(q.Label, labelOpConvPrefix) && len(q.Metadata) == 1 && q.Metadata[keyKind] == string(KindInterrupt) && q.Limit > 0
		if !openKeyed && !legacyKeyed && !laneScanKeyed && !openLaneKeyed && !interruptsKeyed {
			t.Errorf("unkeyed list %+v from %v", q, op.Caller)
		}
		for _, req := range beads.NativeListPlan(op.Query) {
			if !beads.NativeListRequestKeyed(req) {
				t.Errorf("list %+v plans an unkeyed native request", op.Query)
			}
		}
	}
}

// msgReq is a lane message: the store tests drive the lane's status machine
// and leases, which only lane ops take.
func msgReq(key, payload string) Request {
	return Request{
		Conversation: "gc-1", Kind: KindMessage, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator}, Payload: payload,
	}
}

// lifecycleReq is a lifecycle-lane op: the store tests drive the lane's
// status machine, which only lane ops take.
func lifecycleReq(kind Kind, key string) Request {
	r := Request{
		Conversation: "gc-1", Kind: kind, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator},
	}
	if kind == KindKill {
		r.Scope = Scope{Kind: ScopeSession, Generation: 3}
	}
	if kind == KindCreate {
		r.Payload = `{"template":"x"}`
	}
	return r
}

func (h *harness) propose(t *testing.T, req Request) Op {
	t.Helper()
	op, created, err := h.store.Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose(%+v) = created %v, %v", req, created, err)
	}
	return op
}

func (h *harness) revision(t *testing.T, id string) int64 {
	t.Helper()
	b, err := h.mem.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Revision
}

func (h *harness) countOps(kind string) int {
	n := 0
	for _, op := range h.rec.Ops() {
		if op.Kind == kind {
			n++
		}
	}
	return n
}

func TestNewStoreRefusals(t *testing.T) {
	clk := &clock.Fake{Time: t0}
	t.Run("nil store", func(t *testing.T) {
		if _, err := NewStore(beads.SessionStore{}, clk, WithIDPrefix("gc")); err == nil {
			t.Fatal("accepted a nil store")
		}
		var nilMem *beads.MemStore
		if _, err := NewStore(beads.SessionStore{Store: nilMem}, clk, WithIDPrefix("gc")); err == nil {
			t.Fatal("accepted a typed nil store")
		}
	})
	t.Run("no clock", func(t *testing.T) {
		if _, err := NewStore(beads.SessionStore{Store: beads.NewMemStore()}, nil, WithIDPrefix("gc")); err == nil {
			t.Fatal("accepted a nil clock")
		}
	})
	t.Run("no conditional writer", func(t *testing.T) {
		rec := beadstest.NewRecordingStore(beads.NewMemStore())
		_, err := NewStore(beads.SessionStore{Store: rec}, clk, WithIDPrefix("gc"))
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("NewStore(no writer) = %v, want ErrConditionalWriteUnsupported", err)
		}
	})
	t.Run("conditional writes disabled", func(t *testing.T) {
		mem := &beads.MemStore{DisableConditionalWrites: true}
		_, err := NewStore(beads.SessionStore{Store: mem}, clk, WithIDPrefix("gc"))
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("NewStore(disabled) = %v, want ErrConditionalWriteUnsupported", err)
		}
	})
	t.Run("bd without conditional writes", func(t *testing.T) {
		calls := 0
		bd := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
			calls++
			return []byte("Flags:\n      --json   JSON output\n"), nil
		})
		_, err := NewStore(beads.SessionStore{Store: bd}, clk, WithIDPrefix("gc"))
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || !strings.Contains(err.Error(), "--if-revision") {
			t.Fatalf("NewStore(bd without --if-revision) = %v, want ErrConditionalWriteUnsupported naming the flag", err)
		}
		if calls == 0 {
			t.Fatal("NewStore accepted a bd store without probing it")
		}
	})
	t.Run("bd with conditional writes", func(t *testing.T) {
		bd := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
			if len(args) == 2 && args[1] == "--help" {
				return []byte("Flags:\n      --if-revision int   fence\n"), nil
			}
			return nil, fmt.Errorf("unexpected bd %v", args)
		})
		if _, err := NewStore(beads.SessionStore{Store: bd}, clk, WithIDPrefix("gc")); err != nil {
			t.Fatalf("NewStore(capable bd) = %v", err)
		}
	})
	t.Run("no prefix", func(t *testing.T) {
		_, err := NewStore(beads.SessionStore{Store: beads.NewMemStore()}, clk)
		if !errors.Is(err, ErrNoNamespace) {
			t.Fatalf("NewStore(no prefix) = %v, want ErrNoNamespace", err)
		}
	})
	t.Run("bad prefix", func(t *testing.T) {
		_, err := NewStore(beads.SessionStore{Store: beads.NewMemStore()}, clk, WithIDPrefix("gc-"))
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("NewStore(gc-) = %v, want ErrInvalidRequest", err)
		}
	})
}

func TestNewStoreDiscoversThePrefixAndWorksOverSQLite(t *testing.T) {
	sq, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sq.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	s, err := NewStore(beads.SessionStore{Store: sq}, &clock.Fake{Time: t0})
	if err != nil {
		t.Fatalf("NewStore(sqlite): %v", err)
	}
	if s.Prefix() != "gcs" {
		t.Fatalf("Prefix() = %q, want gcs", s.Prefix())
	}
	op, created, err := s.Propose(msgReq("k1", "hello"))
	if err != nil || !created || !strings.HasPrefix(op.ID, "gcs-op-") {
		t.Fatalf("Propose over sqlite = %s created %v, %v", op.ID, created, err)
	}
	if open, err := s.ListOpen("gc-1"); err != nil || len(open) != 1 || open[0].ID != op.ID {
		t.Fatalf("ListOpen over sqlite = %v, %v", ids(open), err)
	}
	if _, err := s.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatalf("Transition over sqlite: %v", err)
	}
	if _, err := s.Close(op.ID, Outcome{Arm: ArmSucceeded, Result: Result{Generation: 1}}, "lane"); err != nil {
		t.Fatalf("Close over sqlite: %v", err)
	}
	got, err := s.Get(op.ID)
	if err != nil || got.Status != StatusClosed || got.Outcome.Result.Generation != 1 {
		t.Fatalf("Get over sqlite = %+v, %v", got, err)
	}
	if open, err := s.ListOpen("gc-1"); err != nil || len(open) != 0 {
		t.Fatalf("ListOpen over sqlite after Close = %v, %v", ids(open), err)
	}
	b, _ := sq.Get(op.ID)
	if b.Status != "closed" {
		t.Fatalf("bead status %q after Close", b.Status)
	}
	if again, created, err := s.Propose(msgReq("k1", "hello")); err != nil || created || again.Status != StatusClosed {
		t.Fatalf("re-Propose of a closed op = %+v created %v, %v", again, created, err)
	}
}

func TestProposeCreatesASubmittedOp(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k1", "hello"))
	if op.ID != "gc-op-kdpftlql5feaypldytje3nypwy" {
		t.Fatalf("id %s", op.ID)
	}
	if op.Status != StatusSubmitted || len(op.Transitions) != 1 ||
		op.Transitions[0] != (Transition{Status: StatusSubmitted, At: t0, By: "operator:alice"}) {
		t.Fatalf("op %+v", op)
	}
	if !op.CreatedAt.Equal(t0) || !op.StartBy.Equal(t0.Add(24*time.Hour)) || !op.FinishBy.Equal(t0.Add(24*time.Hour+10*time.Minute)) {
		t.Fatalf("times created %s start %s finish %s", op.CreatedAt, op.StartBy, op.FinishBy)
	}
	b, err := h.mem.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != BeadType || b.Status != "open" || !b.NoHistory || b.Ephemeral || b.Assignee != "" || b.ParentID != "" {
		t.Fatalf("bead %+v", b)
	}
	got, err := h.store.Get(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != op.ID || got.PayloadDigest != op.PayloadDigest || got.Payload != "hello" {
		t.Fatalf("Get = %+v", got)
	}
	ready, err := h.mem.Ready()
	if err != nil || len(ready) != 0 {
		t.Fatalf("Ready() = %v, %v; ops must never be ready", ready, err)
	}
}

func TestProposeIsIdempotent(t *testing.T) {
	h := newHarness(t)
	first := h.propose(t, msgReq("k1", "hello"))
	rev := h.revision(t, first.ID)
	h.clk.Advance(time.Minute)
	retry := msgReq("k1", "hello")
	retry.Policy.Message.When = WhenBoundary // an explicit default
	retry.StartBy = t0.Add(2 * time.Hour)    // deadlines are not covered
	again, created, err := h.store.Propose(retry)
	if err != nil || created {
		t.Fatalf("re-Propose = created %v, %v", created, err)
	}
	if again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) || !again.StartBy.Equal(first.StartBy) {
		t.Fatalf("re-Propose returned %+v, want the stored op %+v", again, first)
	}
	if h.revision(t, first.ID) != rev {
		t.Fatal("re-Propose wrote the row")
	}
}

func TestProposeKeyReuse(t *testing.T) {
	h := newHarness(t)
	first := h.propose(t, msgReq("k1", "hello"))
	rev := h.revision(t, first.ID)
	_, _, err := h.store.Propose(msgReq("k1", "goodbye"))
	var reuse *KeyReuseError
	if !errors.As(err, &reuse) || !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("Propose(other payload) = %v, want *KeyReuseError", err)
	}
	if reuse.Existing.ID != first.ID || reuse.Existing.Payload != "hello" {
		t.Fatalf("Existing = %+v", reuse.Existing)
	}
	if h.revision(t, first.ID) != rev {
		t.Fatal("key reuse wrote the row")
	}
}

// A close takes an optional payload (the API's close?delete=true carries
// {"delete":true}) and the digest covers it: under one key, a close with the
// payload and one without are different requests, each direction
// ErrKeyReuse, nothing written.
func TestProposeCloseOptionalPayload(t *testing.T) {
	h := newHarness(t)
	plain := h.propose(t, lifecycleReq(KindClose, "c1"))
	deleting := lifecycleReq(KindClose, "c2")
	deleting.Payload = `{"delete":true}`
	withPayload := h.propose(t, deleting)
	if plain.Payload != "" || withPayload.Payload != deleting.Payload || plain.PayloadDigest == withPayload.PayloadDigest {
		t.Fatalf("plain close %q digest %s, deleting close %q digest %s; want the payload kept and the digests apart",
			plain.Payload, plain.PayloadDigest, withPayload.Payload, withPayload.PayloadDigest)
	}
	revs := map[string]int64{plain.ID: h.revision(t, plain.ID), withPayload.ID: h.revision(t, withPayload.ID)}
	flipped := lifecycleReq(KindClose, "c1")
	flipped.Payload = deleting.Payload
	if _, _, err := h.store.Propose(flipped); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("Propose(c1 with the payload) = %v, want ErrKeyReuse", err)
	}
	if _, _, err := h.store.Propose(lifecycleReq(KindClose, "c2")); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("Propose(c2 without the payload) = %v, want ErrKeyReuse", err)
	}
	for id, rev := range revs {
		if h.revision(t, id) != rev {
			t.Fatalf("key reuse wrote op %s", id)
		}
	}
}

func TestProposePayloadCap(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.store.Propose(msgReq("max", strings.Repeat("a", MaxInlinePayload))); err != nil {
		t.Fatalf("16384-byte payload refused: %v", err)
	}
	creates := h.countOps(beadstest.OpCreate)
	if _, _, err := h.store.Propose(msgReq("big", strings.Repeat("a", MaxInlinePayload+1))); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("16385-byte payload = %v, want ErrPayloadTooLarge", err)
	}
	if h.countOps(beadstest.OpCreate) != creates {
		t.Fatal("an oversized payload reached the store")
	}
}

// TestValidatePayload pins the exported payload check's verdicts: it refuses
// what Propose refuses of a payload whatever the op's conversation, a payload
// over the cap as ErrPayloadTooLarge, one a kind cannot carry as
// ErrInvalidRequest, and an unknown kind as ErrUnknownKind. It is
// validatePayload, the check Propose applies, so the two cannot drift.
func TestValidatePayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    Kind
		payload string
		want    error
	}{
		{"message", KindMessage, "hello", nil},
		{"message at the cap", KindMessage, strings.Repeat("a", MaxInlinePayload), nil},
		{"message over the cap", KindMessage, strings.Repeat("a", MaxInlinePayload+1), ErrPayloadTooLarge},
		{"message not UTF-8", KindMessage, "bad \xff text", ErrInvalidRequest},
		{"message empty", KindMessage, "", ErrInvalidRequest},
		{"wake without payload", KindWake, "", nil},
		{"wake with payload", KindWake, "x", ErrInvalidRequest},
		{"unknown kind", "teleport", "x", ErrUnknownKind},
	} {
		err := ValidatePayload(tc.kind, tc.payload)
		if (tc.want == nil) != (err == nil) || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidatePayload = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestProposeValidation(t *testing.T) {
	h := newHarness(t)
	withKind := func(k Kind, f func(*Request)) Request {
		r := lifecycleReq(k, "v")
		f(&r)
		return r
	}
	msg := func(f func(*Request)) Request {
		r := msgReq("v", "hello")
		f(&r)
		return r
	}
	cases := map[string]struct {
		req  Request
		want error
	}{
		"unknown kind":              {withKind("teleport", func(*Request) {}), ErrUnknownKind},
		"message without payload":   {msg(func(r *Request) { r.Payload = "" }), ErrInvalidRequest},
		"wake with payload":         {withKind(KindWake, func(r *Request) { r.Payload = "x" }), ErrInvalidRequest},
		"bad when":                  {msg(func(r *Request) { r.Policy.Message.When = "later" }), ErrInvalidRequest},
		"foreign policy":            {msg(func(r *Request) { r.Policy.Restart.Mode = RestartFresh }), ErrInvalidRequest},
		"lifecycle guarantee":       {withKind(KindWake, func(r *Request) { r.Policy.Guarantee = GuaranteeAtMostOnce }), ErrInvalidRequest},
		"interrupt without session": {Request{Conversation: "gc-1", Kind: KindInterrupt, ClientKey: "s", Producer: Producer{Principal: "p", Actor: "agent"}}, ErrInvalidRequest},
		"respond without interaction": {Request{
			Conversation: "gc-1", Kind: KindRespond, ClientKey: "r", Payload: "yes",
			Producer: Producer{Principal: "p", Actor: "agent"}, Scope: Scope{Kind: ScopeSession, Generation: 2},
		}, ErrInvalidRequest},
		"message with turn":       {msg(func(r *Request) { r.Scope = Scope{Kind: ScopeSession, Generation: 2, Turn: "t"} }), ErrInvalidRequest},
		"context without epoch":   {msg(func(r *Request) { r.Scope = Scope{Kind: ScopeContext} }), ErrInvalidRequest},
		"kill conversation scope": {withKind(KindKill, func(r *Request) { r.Scope = Scope{Kind: ScopeConversation} }), ErrInvalidRequest},
		"interrupt with after": {Request{
			Conversation: "gc-1", Kind: KindInterrupt, ClientKey: "s", After: "gc-op-kdpftlql5feaypldytje3nypwy",
			Producer: Producer{Principal: "p", Actor: "agent"}, Scope: Scope{Kind: ScopeSession, Generation: 2},
		}, ErrInvalidRequest},
		"after not an op id":      {msg(func(r *Request) { r.After = "gc-12" }), ErrInvalidRequest},
		"after itself":            {msg(func(r *Request) { r.ClientKey = "k1"; r.After = "gc-op-kdpftlql5feaypldytje3nypwy" }), ErrInvalidRequest},
		"start before not_before": {msg(func(r *Request) { r.NotBefore = t0.Add(time.Hour); r.StartBy = t0 }), ErrInvalidRequest},
		"finish before start":     {msg(func(r *Request) { r.StartBy = t0.Add(time.Hour); r.FinishBy = t0 }), ErrInvalidRequest},
		"bad actor":               {msg(func(r *Request) { r.Producer.Actor = "Operator" }), ErrInvalidRequest},
		"no principal":            {msg(func(r *Request) { r.Producer.Principal = "" }), ErrInvalidRequest},
		"negative owner epoch":    {msg(func(r *Request) { r.Producer.OwnerEpoch = -1 }), ErrInvalidRequest},
		"bad conversation":        {msg(func(r *Request) { r.Conversation = "gc#1" }), ErrInvalidRequest},
		"bad payload utf8":        {msg(func(r *Request) { r.Payload = "\xff" }), ErrInvalidRequest},
		"negative kill grace":     {withKind(KindKill, func(r *Request) { r.Policy.Kill.Grace = -time.Second }), ErrInvalidRequest},
	}
	for name, c := range cases {
		if _, _, err := h.store.Propose(c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: Propose = %v, want %v", name, err, c.want)
		}
	}
	if n := h.countOps(beadstest.OpCreate); n != 0 {
		t.Fatalf("refused requests reached the store %d times", n)
	}
}

func TestProposeEveryKindWithDefaults(t *testing.T) {
	h := newHarness(t)
	reqs := []Request{
		msgReq("m", "hi"),
		{
			Conversation: "gc-1", Kind: KindInterrupt, ClientKey: "stop:3:t1", Producer: Producer{Principal: "p", Actor: "operator"},
			Scope: Scope{Kind: ScopeSession, Generation: 3, Turn: "t1"},
		},
		{
			Conversation: "gc-1", Kind: KindRespond, ClientKey: "r1", Payload: `{"action":"approve"}`, Producer: Producer{Principal: "p", Actor: "operator"},
			Scope: Scope{Kind: ScopeSession, Generation: 3, Interaction: "r1"},
		},
	}
	for _, k := range []Kind{KindCreate, KindWake, KindSuspend, KindResume, KindRestart, KindKill, KindClose} {
		reqs = append(reqs, lifecycleReq(k, string(k)))
	}
	for _, r := range reqs {
		op := h.propose(t, r)
		if op.Kind == KindRestart && op.Policy.Restart.Mode != RestartContinue {
			t.Errorf("restart mode default = %q", op.Policy.Restart.Mode)
		}
		if op.Kind == KindInterrupt && (op.Policy.Interrupt.OnFailure != OnFailureFail || !op.StartBy.Equal(t0.Add(time.Minute))) {
			t.Errorf("interrupt defaults %+v start %s", op.Policy, op.StartBy)
		}
	}
	open, err := h.store.ListOpen("gc-1")
	if err != nil || len(open) != len(reqs) {
		t.Fatalf("ListOpen = %d ops, %v; want %d", len(open), err, len(reqs))
	}
}

// A MemStore without HonorExplicitIDs mints its own id over the pinned one.
func TestProposePinnedIDNotHonored(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc"}
	h := newHarnessOver(t, mem, mem)
	_, _, err := h.store.Propose(msgReq("k1", "hello"))
	if !errors.Is(err, ErrPinnedIDNotHonored) {
		t.Fatalf("Propose = %v, want ErrPinnedIDNotHonored", err)
	}
	if all, _ := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); len(all) != 0 {
		t.Fatalf("stray row left behind: %+v", all)
	}
}

// typeRefusingStore is a MemStore whose bd refuses the op type.
type typeRefusingStore struct{ *beads.MemStore }

func (s typeRefusingStore) Create(b beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, fmt.Errorf("bd create: invalid issue type: %s", b.Type)
}

func TestProposeUnregisteredType(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, typeRefusingStore{mem})
	_, _, err := h.store.Propose(msgReq("k1", "hello"))
	if !errors.Is(err, ErrBeadTypeUnregistered) || !strings.Contains(err.Error(), "gc doctor --fix") {
		t.Fatalf("Propose = %v, want ErrBeadTypeUnregistered naming the remedy", err)
	}
}

func TestProposeConcurrentOneKey(t *testing.T) {
	h := newHarness(t)
	const n = 32
	var wg sync.WaitGroup
	var createdCount atomic.Int32
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, created, err := h.store.Propose(msgReq("same", "hello"))
			ids[i], errs[i] = op.ID, err
			if created {
				createdCount.Add(1)
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("goroutine %d: id %s err %v (want %s)", i, ids[i], errs[i], ids[0])
		}
	}
	if createdCount.Load() != 1 {
		t.Fatalf("%d goroutines created the op, want exactly 1", createdCount.Load())
	}
	if all, _ := h.mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); len(all) != 1 {
		t.Fatalf("%d rows, want 1", len(all))
	}
}

func TestProposeConcurrentDistinctKeys(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, created, err := h.store.Propose(msgReq(fmt.Sprintf("k%d", i), "hello")); err != nil || !created {
				t.Errorf("key %d: created %v, %v", i, created, err)
			}
		}()
	}
	wg.Wait()
	open, err := h.store.ListOpen("gc-1")
	if err != nil || len(open) != 32 {
		t.Fatalf("ListOpen = %d, %v; want 32", len(open), err)
	}
}

func TestGet(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.Get("gc-op-kdpftlql5feaypldytje3nypwy"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	corrupt, err := h.mem.Create(beads.Bead{
		Type: BeadType, Labels: []string{LabelOp, ConversationLabel("gc-1")},
		Metadata: map[string]string{keySchema: "1", keyKind: "message"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Get(corrupt.ID); !errors.Is(err, ErrCorruptOp) {
		t.Fatalf("Get(corrupt) = %v, want ErrCorruptOp", err)
	}
}

func TestListOpen(t *testing.T) {
	h := newHarness(t)
	a := h.propose(t, msgReq("a", "first"))
	h.clk.Advance(time.Second)
	deferred := msgReq("b", "deferred")
	deferred.NotBefore = t0.Add(time.Hour)
	b := h.propose(t, deferred)
	h.clk.Advance(time.Second)
	c := h.propose(t, msgReq("c", "third"))
	h.clk.Advance(time.Second)
	closedOp := h.propose(t, msgReq("d", "closed"))
	if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatal(err)
	}
	torn := h.propose(t, msgReq("e", "torn"))
	tornClose(t, h, torn.ID)
	other := msgReq("x", "elsewhere")
	other.Conversation = "gc-2"
	h.propose(t, other)

	h.rec.Reset()
	got, err := h.store.ListOpen("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{a.ID, c.ID, b.ID}; fmt.Sprint(ids(got)) != fmt.Sprint(want) {
		t.Fatalf("ListOpen = %v, want %v", ids(got), want)
	}
	if ops := h.rec.Ops(); len(ops) != 1 || ops[0].Kind != beadstest.OpList ||
		ops[0].Query.Label != "" || fmt.Sprint(ops[0].Query.Metadata) != fmt.Sprint(map[string]string{keyOpen: "gc-1"}) {
		t.Fatalf("ListOpen issued %+v, want exactly one List keyed by %s", ops, keyOpen)
	}
	h.rec.Reset()
	if ops, quarantined, err := h.store.ListOpenTolerant("gc-1"); err != nil || len(quarantined) != 0 || fmt.Sprint(ids(ops)) != fmt.Sprint(ids(got)) {
		t.Fatalf("ListOpenTolerant = %v, %v, %v; want ListOpen's answer", ids(ops), quarantined, err)
	}
	if n := h.countOps(beadstest.OpList); n != 1 {
		t.Fatalf("ListOpenTolerant issued %d lists, want 1", n)
	}

	if _, err := h.mem.Create(beads.Bead{
		ID: "gc-op-bogus", Type: BeadType, Labels: []string{LabelOp, ConversationLabel("gc-1")},
		Metadata: map[string]string{keySchema: "1", keyKind: "message", keyOpen: "gc-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ListOpen("gc-1"); !errors.Is(err, ErrCorruptOp) || !strings.Contains(err.Error(), "gc-op-bogus") {
		t.Fatalf("ListOpen over a corrupt row = %v, want ErrCorruptOp naming it", err)
	}
	if _, err := h.store.ListOpen("gc 1"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ListOpen(bad id) = %v", err)
	}
}

// tornClose writes a closed outcome to the metadata and leaves the bead open,
// as a crash between Close's two writes does.
func tornClose(t *testing.T, h *harness, id string) {
	t.Helper()
	b, _ := h.mem.Get(id)
	if err := h.mem.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: map[string]string{
		keyStatus: string(StatusClosed), keyOutcome: string(ArmSucceeded),
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestTransitionMachine(t *testing.T) {
	legal := [][]Status{
		{StatusSubmitted, StatusAccepted, StatusExecuting, StatusSettling},
		{StatusSubmitted, StatusExecuting, StatusSettling},
		{StatusSubmitted, StatusAccepted, StatusSettling},
	}
	for i, path := range legal {
		h := newHarness(t)
		op := h.propose(t, msgReq(fmt.Sprintf("p%d", i), "x"))
		for j := 1; j < len(path); j++ {
			h.clk.Advance(time.Second)
			next, err := h.store.Transition(op.ID, path[j-1], path[j], "lane")
			if err != nil {
				t.Fatalf("path %v step %d: %v", path, j, err)
			}
			if next.Status != path[j] || len(next.Transitions) != j+1 || next.Transitions[j].By != "lane" {
				t.Fatalf("after %s: %+v", path[j], next)
			}
		}
		got, _ := h.store.Get(op.ID)
		if got.Status != path[len(path)-1] || len(got.Transitions) != len(path) {
			t.Fatalf("stored %s with %d transitions", got.Status, len(got.Transitions))
		}
	}
}

func TestTransitionRefusals(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	wake := h.propose(t, lifecycleReq(KindWake, "w"))
	rev := h.revision(t, op.ID)
	cases := []struct {
		id         string
		expect, to Status
		want       error
	}{
		{op.ID, StatusSubmitted, StatusClosed, ErrIllegalTransition},
		{op.ID, StatusSubmitted, StatusSubmitted, ErrIllegalTransition},
		{op.ID, StatusSubmitted, StatusSettling, ErrIllegalTransition},
		{op.ID, StatusAccepted, StatusExecuting, ErrStatusMismatch},
		{op.ID, StatusSubmitted, "running", ErrIllegalTransition},
		{wake.ID, StatusSubmitted, StatusExecuting, ErrIllegalTransition},
	}
	for _, c := range cases {
		if _, err := h.store.Transition(c.id, c.expect, c.to, "lane"); !errors.Is(err, c.want) {
			t.Errorf("Transition(%s, %s->%s) = %v, want %v", c.id, c.expect, c.to, err, c.want)
		}
	}
	var se *StatusError
	if _, err := h.store.Transition(op.ID, StatusExecuting, StatusSettling, "lane"); !errors.As(err, &se) ||
		se.Current != StatusSubmitted || se.Expected != StatusExecuting {
		t.Fatalf("StatusError = %+v", se)
	}
	if h.revision(t, op.ID) != rev {
		t.Fatal("a refused transition wrote the row")
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Transition(op.ID, StatusClosed, StatusAccepted, "lane"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("out of closed = %v", err)
	}
	if _, err := h.store.Transition(wake.ID, StatusSubmitted, StatusAccepted, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty by = %v", err)
	}
	if _, err := h.store.Transition("gc-op-kdpftlql5feaypldytje3nypwz", StatusSubmitted, StatusAccepted, "lane"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("missing op = %v", err)
	}
}

// racingStore moves the revision under the op store's feet: every
// UpdateIfMatch first lets a concurrent writer touch the row, up to races
// times.
type racingStore struct {
	*beads.MemStore
	races atomic.Int32
}

func (s *racingStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.races.Add(-1) >= 0 {
		if err := s.SetMetadata(id, "x.concurrent", fmt.Sprint(s.races.Load())); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func TestTransitionRetriesARevisionRace(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	racer := &racingStore{MemStore: mem}
	h := newHarnessOver(t, mem, racer)
	op := h.propose(t, msgReq("k", "x"))
	racer.races.Store(1)
	if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusAccepted, "lane"); err != nil {
		t.Fatalf("Transition under one race: %v", err)
	}
	if n := h.countOps(beadstest.OpUpdateIfMatch); n != 2 {
		t.Fatalf("%d UpdateIfMatch calls, want 2 (one lost race, one win)", n)
	}
	got, _ := h.store.Get(op.ID)
	if got.Status != StatusAccepted || len(got.Transitions) != 2 {
		t.Fatalf("after the race: %+v", got)
	}

	racer.races.Store(1 << 20)
	_, err := h.store.Transition(op.ID, StatusAccepted, StatusExecuting, "lane")
	if !errors.Is(err, ErrContention) || !beads.IsPreconditionFailed(err) {
		t.Fatalf("Transition under constant interference = %v, want ErrContention wrapping the conflict", err)
	}
}

// Repairing a torn close is fenced like any other write, and losing every race
// reports the last conflict as mutate does.
func TestTornCloseRepairUnderContentionWrapsTheConflict(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	racer := &racingStore{MemStore: mem}
	h := newHarnessOver(t, mem, racer)
	op := h.propose(t, msgReq("k", "x"))
	tornClose(t, h, op.ID)
	racer.races.Store(1 << 20)
	_, _, err := h.store.Cancel(op.ID, "operator:bob", "")
	if !errors.Is(err, ErrContention) || !beads.IsPreconditionFailed(err) {
		t.Fatalf("Cancel(torn) under constant interference = %v, want ErrContention wrapping the conflict", err)
	}
}

func TestClose(t *testing.T) {
	for _, from := range []Status{StatusSubmitted, StatusAccepted, StatusExecuting, StatusSettling} {
		h := newHarness(t)
		op := h.propose(t, msgReq("k", "x"))
		path := map[Status][]Status{
			StatusAccepted:  {StatusAccepted},
			StatusExecuting: {StatusExecuting},
			StatusSettling:  {StatusExecuting, StatusSettling},
		}[from]
		cur := StatusSubmitted
		for _, s := range path {
			if _, err := h.store.Transition(op.ID, cur, s, "lane"); err != nil {
				t.Fatal(err)
			}
			cur = s
		}
		outcome := Outcome{Arm: ArmSucceeded, Degradation: "restarted_instead_of_interrupted", Result: Result{Generation: 4, Turn: "t9", Via: "hook"}}
		closedOp, err := h.store.Close(op.ID, outcome, "lane")
		if err != nil {
			t.Fatalf("Close from %s: %v", from, err)
		}
		if closedOp.Status != StatusClosed || closedOp.Outcome != outcome {
			t.Fatalf("Close returned %+v", closedOp)
		}
		b, _ := h.mem.Get(op.ID)
		got, _ := h.store.Get(op.ID)
		if b.Status != "closed" || got.Outcome != outcome || got.Transitions[len(got.Transitions)-1].Status != StatusClosed {
			t.Fatalf("stored bead %s op %+v", b.Status, got)
		}
		rev := b.Revision
		if _, err := h.store.Close(op.ID, outcome, "lane"); err != nil {
			t.Fatalf("idempotent Close: %v", err)
		}
		if _, err := h.store.Close(op.ID, Outcome{Arm: ArmFailed, Reason: ReasonPoison}, "lane"); !errors.Is(err, ErrAlreadyTerminal) {
			t.Fatalf("second outcome = %v, want ErrAlreadyTerminal", err)
		}
		if h.revision(t, op.ID) != rev {
			t.Fatal("a repeated Close wrote the row")
		}
	}
}

func TestCloseClearsTheLease(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	lease := Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "n", Deadline: t0.Add(time.Minute)}
	b, _ := h.mem.Get(op.ID)
	assignee := lease.String()
	if err := h.mem.UpdateIfMatch(op.ID, b.Revision, beads.UpdateOpts{
		Assignee: &assignee,
		Metadata: map[string]string{keyStatus: string(StatusExecuting)},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Get(op.ID); got.Lease != lease {
		t.Fatalf("lease read %+v", got.Lease)
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.mem.Get(op.ID); b.Assignee != "" {
		t.Fatalf("assignee %q survived Close", b.Assignee)
	}
}

func TestCloseValidatesTheOutcome(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	for name, o := range map[string]Outcome{
		"unknown arm":              {Arm: "won"},
		"succeeded with reason":    {Arm: ArmSucceeded, Reason: "x"},
		"failed without reason":    {Arm: ArmFailed},
		"reason not a token":       {Arm: ArmFailed, Reason: "Bad Reason"},
		"canceled without cancel":  {Arm: ArmCanceled},
		"degraded skip":            {Arm: ArmSkipped, Reason: ReasonStale, Degradation: "x"},
		"coalesced without target": {Arm: ArmSkipped, Reason: ReasonCoalescedInto},
		"target without coalesce":  {Arm: ArmSkipped, Reason: ReasonStale, Result: Result{CoalescedInto: op.ID}},
	} {
		if _, err := h.store.Close(op.ID, o, "lane"); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: Close = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSkipped, Reason: ReasonCoalescedInto, Result: Result{CoalescedInto: op.ID}}, "lane"); err != nil {
		t.Fatalf("coalesced_into: %v", err)
	}
}

func TestCloseRepairsATornClose(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	tornClose(t, h, op.ID)
	if b, _ := h.mem.Get(op.ID); b.Status != "open" {
		t.Fatal("setup: bead should still be open")
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatalf("repairing Close: %v", err)
	}
	if b, _ := h.mem.Get(op.ID); b.Status != "closed" {
		t.Fatalf("bead %s after the repairing Close", b.Status)
	}
}

func TestCancelMatrix(t *testing.T) {
	type row struct {
		name   string
		req    Request
		path   []Status
		result CancelResult
		status Status
		writes bool
	}
	rows := []row{
		{"conversation submitted", msgReq("a", "x"), nil, CancelClosed, StatusClosed, true},
		{"conversation accepted", msgReq("b", "x"), []Status{StatusAccepted}, CancelClosed, StatusClosed, true},
		{"conversation executing", msgReq("c", "x"), []Status{StatusExecuting}, CancelRequested, StatusExecuting, true},
		{"conversation settling", msgReq("d", "x"), []Status{StatusExecuting, StatusSettling}, CancelAlreadyDecided, StatusSettling, false},
		{"lifecycle submitted", lifecycleReq(KindKill, "e"), nil, CancelClosed, StatusClosed, true},
		{"lifecycle accepted", lifecycleReq(KindKill, "f"), []Status{StatusAccepted}, CancelRequested, StatusAccepted, true},
		{"lifecycle settling", lifecycleReq(KindKill, "g"), []Status{StatusAccepted, StatusSettling}, CancelAlreadyDecided, StatusSettling, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t)
			op := h.propose(t, r.req)
			cur := StatusSubmitted
			for _, s := range r.path {
				if _, err := h.store.Transition(op.ID, cur, s, "lane"); err != nil {
					t.Fatal(err)
				}
				cur = s
			}
			rev := h.revision(t, op.ID)
			h.clk.Advance(time.Minute)
			result, got, err := h.store.Cancel(op.ID, "operator:bob", "changed my mind")
			if err != nil || result != r.result || got.Status != r.status {
				t.Fatalf("Cancel = %s %s, %v; want %s %s", result, got.Status, err, r.result, r.status)
			}
			if wrote := h.revision(t, op.ID) != rev; wrote != r.writes {
				t.Fatalf("Cancel wrote=%v, want %v", wrote, r.writes)
			}
			stored, _ := h.store.Get(op.ID)
			if r.writes && (stored.Cancel != Cancel{By: "operator:bob", At: t0.Add(time.Minute), Reason: "changed my mind"}) {
				t.Fatalf("cancel record %+v", stored.Cancel)
			}
			if r.result == CancelClosed {
				b, _ := h.mem.Get(op.ID)
				if stored.Outcome != (Outcome{Arm: ArmCanceled}) || b.Status != "closed" {
					t.Fatalf("closed-canceled op %+v bead %s", stored.Outcome, b.Status)
				}
			}
			if r.result == CancelRequested {
				// The first cancel record wins; a second request writes nothing.
				rev := h.revision(t, op.ID)
				result, again, err := h.store.Cancel(op.ID, "operator:carol", "me too")
				if err != nil || result != CancelRequested || again.Cancel.By != "operator:bob" || h.revision(t, op.ID) != rev {
					t.Fatalf("second Cancel = %s %+v, %v", result, again.Cancel, err)
				}
				// The lane may then close the op canceled.
				if _, err := h.store.Close(op.ID, Outcome{Arm: ArmCanceled}, "lane"); err != nil {
					t.Fatalf("lane closing canceled: %v", err)
				}
			}
			closedRev := h.revision(t, op.ID)
			if stored, _ := h.store.Get(op.ID); stored.Status == StatusClosed {
				result, _, err := h.store.Cancel(op.ID, "operator:bob", "")
				if err != nil || result != CancelAlreadyTerminal || h.revision(t, op.ID) != closedRev {
					t.Fatalf("Cancel(closed) = %s, %v", result, err)
				}
			}
		})
	}
}

func TestCancelRefusals(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	if _, _, err := h.store.Cancel(op.ID, "", "r"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Cancel without by = %v", err)
	}
	if _, _, err := h.store.Cancel(op.ID, "p", "bad\x00reason"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Cancel with a control character = %v", err)
	}
}

func TestStoreNeverLists(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	if _, created, err := h.store.Propose(msgReq("k", "x")); err != nil || created {
		t.Fatalf("re-Propose = created %v, %v", created, err)
	}
	if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.Cancel(op.ID, "p", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmCanceled}, "lane"); err != nil {
		t.Fatal(err)
	}
	if n := h.countOps(beadstest.OpList); n != 0 {
		t.Fatalf("Propose/Transition/Cancel/Close issued %d lists", n)
	}
	if n := h.countOps(beadstest.OpDelete) + h.countOps(beadstest.OpDeleteIfMatch); n != 0 {
		t.Fatalf("the op store deleted %d rows", n)
	}
}

// A gc.op.* key a newer build added is not this build's to remove: decoding
// ignores it, and every write must leave it in place.
func TestWritesKeepUnknownOpKeys(t *testing.T) {
	const futureKey, futureVal = "gc.op.preempt_turn", "run-9"
	seed := func(t *testing.T, h *harness, req Request) Op {
		t.Helper()
		op := h.propose(t, req)
		if err := h.mem.SetMetadata(op.ID, futureKey, futureVal); err != nil {
			t.Fatal(err)
		}
		return op
	}
	assertKept := func(t *testing.T, h *harness, id, after string) {
		t.Helper()
		b, err := h.mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Metadata[futureKey] != futureVal {
			t.Fatalf("%s removed %s (metadata %v)", after, futureKey, b.Metadata)
		}
	}
	t.Run("transition", func(t *testing.T) {
		h := newHarness(t)
		op := seed(t, h, msgReq("k", "x"))
		if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusAccepted, "lane"); err != nil {
			t.Fatal(err)
		}
		assertKept(t, h, op.ID, "Transition")
	})
	t.Run("close", func(t *testing.T) {
		h := newHarness(t)
		op := seed(t, h, msgReq("k", "x"))
		if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
			t.Fatal(err)
		}
		assertKept(t, h, op.ID, "Close")
	})
	t.Run("cancel closes", func(t *testing.T) {
		h := newHarness(t)
		op := seed(t, h, msgReq("k", "x"))
		if r, _, err := h.store.Cancel(op.ID, "operator:bob", ""); err != nil || r != CancelClosed {
			t.Fatalf("Cancel = %s, %v", r, err)
		}
		assertKept(t, h, op.ID, "Cancel")
	})
	t.Run("cancel requested", func(t *testing.T) {
		h := newHarness(t)
		op := seed(t, h, msgReq("k", "x"))
		if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
			t.Fatal(err)
		}
		if r, _, err := h.store.Cancel(op.ID, "operator:bob", ""); err != nil || r != CancelRequested {
			t.Fatalf("Cancel = %s, %v", r, err)
		}
		assertKept(t, h, op.ID, "Cancel")
	})
}

// A key this build owns that the next encoding drops is still written empty.
func TestCloseBlanksTheLeaseAndOwnedKeysItDrops(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, msgReq("k", "x"))
	if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.Cancel(op.ID, "operator:bob", "stop"); err != nil {
		t.Fatal(err)
	}
	b, _ := h.mem.Get(op.ID)
	if b.Metadata[keyCancelReason] != "stop" {
		t.Fatalf("setup: cancel reason %q", b.Metadata[keyCancelReason])
	}
	cur, _ := h.store.Get(op.ID)
	next := cur
	next.Cancel = Cancel{}
	opts, err := patch(b, cur, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{keyCancelBy, keyCancelAt, keyCancelReason} {
		if v, ok := opts.Metadata[k]; !ok || v != "" {
			t.Errorf("patch did not blank %s: %v", k, opts.Metadata)
		}
	}
}

// upsertingStore is a store whose create of an existing pinned id overwrites
// the row, as bd before v1.3.0 does (INSERT ... ON DUPLICATE KEY UPDATE).
type upsertingStore struct{ *beads.MemStore }

func (s upsertingStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID != "" {
		if _, err := s.Get(b.ID); err == nil {
			status, assignee := "open", ""
			if err := s.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee, Metadata: b.Metadata}); err != nil {
				return beads.Bead{}, err
			}
			return s.Get(b.ID)
		}
	}
	return s.MemStore.Create(b)
}

// A retried Propose reads before it creates, so a store that upserts a
// duplicate pinned id never sees the second create.
func TestProposeRetryNeverRecreates(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	h := newHarnessOver(t, mem, upsertingStore{mem})
	op := h.propose(t, msgReq("k1", "hello"))
	if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	creates := h.countOps(beadstest.OpCreate)
	again, created, err := h.store.Propose(msgReq("k1", "hello"))
	if err != nil || created || again.Status != StatusExecuting {
		t.Fatalf("re-Propose = %s created %v, %v; want the stored executing op", again.Status, created, err)
	}
	if _, _, err := h.store.Propose(msgReq("k1", "other")); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("re-Propose with another payload = %v, want ErrKeyReuse", err)
	}
	if n := h.countOps(beadstest.OpCreate); n != creates {
		t.Fatalf("a retried Propose issued %d creates", n-creates)
	}
	if got, _ := h.store.Get(op.ID); got.Status != StatusExecuting || got.Payload != "hello" {
		t.Fatalf("stored op after retries: %s %q", got.Status, got.Payload)
	}
}

// closeRefusingStore fails every CloseIfMatch, as a transient bd error does.
type closeRefusingStore struct{ *beads.MemStore }

func (closeRefusingStore) CloseIfMatch(string, int64) error {
	return errors.New("bd close: transient failure")
}

// Close and Cancel commit the outcome and close the bead in one fenced write,
// so there is no second write to fail and leave the bead open.
func TestCloseIsOneFencedWrite(t *testing.T) {
	newH := func(t *testing.T) *harness {
		mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
		return newHarnessOver(t, mem, closeRefusingStore{mem})
	}
	assertClosed := func(t *testing.T, h *harness, id string) {
		t.Helper()
		if b, _ := h.mem.Get(id); b.Status != "closed" {
			t.Fatalf("bead %s is %s", id, b.Status)
		}
		if n := h.countOps(beadstest.OpCloseIfMatch); n != 0 {
			t.Fatalf("%d CloseIfMatch calls; the close must ride the metadata write", n)
		}
	}
	t.Run("close", func(t *testing.T) {
		h := newH(t)
		op := h.propose(t, msgReq("k", "x"))
		updates := h.countOps(beadstest.OpUpdateIfMatch)
		if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if n := h.countOps(beadstest.OpUpdateIfMatch) - updates; n != 1 {
			t.Fatalf("Close issued %d fenced writes, want 1", n)
		}
		assertClosed(t, h, op.ID)
	})
	t.Run("cancel", func(t *testing.T) {
		h := newH(t)
		op := h.propose(t, msgReq("k", "x"))
		if r, _, err := h.store.Cancel(op.ID, "operator:bob", ""); err != nil || r != CancelClosed {
			t.Fatalf("Cancel = %s, %v", r, err)
		}
		assertClosed(t, h, op.ID)
	})
}

// A torn close (metadata closed, bead open) left by an older build or an
// out-of-band write is repaired by any later Close or Cancel, not only by a
// Close with the identical outcome.
func TestTornCloseIsRepairedByAnyTerminalCall(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, msgReq("k", "x"))
		tornClose(t, h, op.ID)
		if r, _, err := h.store.Cancel(op.ID, "operator:bob", ""); err != nil || r != CancelAlreadyTerminal {
			t.Fatalf("Cancel(torn) = %s, %v", r, err)
		}
		if b, _ := h.mem.Get(op.ID); b.Status != "closed" {
			t.Fatalf("bead %s after Cancel", b.Status)
		}
	})
	t.Run("close with another outcome", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, msgReq("k", "x"))
		tornClose(t, h, op.ID)
		if _, err := h.store.Close(op.ID, Outcome{Arm: ArmFailed, Reason: ReasonPoison}, "lane"); !errors.Is(err, ErrAlreadyTerminal) {
			t.Fatalf("Close(torn, other outcome) = %v, want ErrAlreadyTerminal", err)
		}
		if b, _ := h.mem.Get(op.ID); b.Status != "closed" {
			t.Fatalf("bead %s after Close", b.Status)
		}
	})
}

// A bead closed outside the journal while its op executes under a lease reads
// closed failed{closed_externally}. The next Close, Cancel or Transition stores
// that outcome under the revision fence, so reopening the bead cannot bring the
// op back.
func TestExternalCloseIsStoredByTheNextWrite(t *testing.T) {
	externally := Outcome{Arm: ArmFailed, Reason: ReasonClosedExternally}
	for _, c := range []struct {
		name string
		call func(h *harness, id string) error
	}{
		{"close with that outcome", func(h *harness, id string) error {
			_, err := h.store.Close(id, externally, "lane")
			return err
		}},
		{"close with another outcome", func(h *harness, id string) error {
			if _, err := h.store.Close(id, Outcome{Arm: ArmSucceeded}, "lane"); !errors.Is(err, ErrAlreadyTerminal) {
				return fmt.Errorf("Close(succeeded) = %w, want ErrAlreadyTerminal", err)
			}
			return nil
		}},
		{"cancel", func(h *harness, id string) error {
			if r, _, err := h.store.Cancel(id, "operator:bob", ""); err != nil || r != CancelAlreadyTerminal {
				return fmt.Errorf("Cancel = %s, %w, want %s", r, err, CancelAlreadyTerminal)
			}
			return nil
		}},
		{"transition", func(h *harness, id string) error {
			var se *StatusError
			if _, err := h.store.Transition(id, StatusExecuting, StatusSettling, "lane"); !errors.As(err, &se) || se.Current != StatusClosed {
				return fmt.Errorf("Transition = %w, want a StatusError at closed", err)
			}
			return nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			op := h.propose(t, msgReq("k", "x"))
			b, _ := h.mem.Get(op.ID)
			assignee := Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "n", Deadline: t0.Add(time.Minute)}.String()
			if err := h.mem.UpdateIfMatch(op.ID, b.Revision, beads.UpdateOpts{
				Assignee: &assignee,
				Metadata: map[string]string{keyStatus: string(StatusExecuting)},
			}); err != nil {
				t.Fatal(err)
			}
			if err := h.mem.Close(op.ID); err != nil {
				t.Fatal(err)
			}
			if err := c.call(h, op.ID); err != nil {
				t.Fatal(err)
			}
			b, _ = h.mem.Get(op.ID)
			if b.Metadata[keyStatus] != string(StatusClosed) || b.Metadata[keyOutcome] != string(ArmFailed) ||
				b.Metadata[keyOutcomeReason] != ReasonClosedExternally || b.Assignee != "" {
				t.Fatalf("stored status %q outcome %q{%q} assignee %q, want closed failed{%s} and no lease",
					b.Metadata[keyStatus], b.Metadata[keyOutcome], b.Metadata[keyOutcomeReason], b.Assignee, ReasonClosedExternally)
			}
			if err := h.mem.Reopen(op.ID); err != nil {
				t.Fatal(err)
			}
			got, err := h.store.Get(op.ID)
			if err != nil || got.Status != StatusClosed || got.Outcome != externally {
				t.Fatalf("Get after reopen = %s %+v, %v; want closed failed{%s}", got.Status, got.Outcome, err, ReasonClosedExternally)
			}
			if last := got.Transitions[len(got.Transitions)-1]; last.Status != StatusClosed {
				t.Fatalf("last transition %+v, want the stored close", last)
			}
			if open, err := h.store.ListOpen("gc-1"); err != nil || len(open) != 0 {
				t.Fatalf("ListOpen after reopen = %v, %v; want none", open, err)
			}
		})
	}
}

// One explicit deadline bounds the defaulted other: a finish_by inside the
// kind's start window pulls start_by in, and a start_by past the finish
// window pushes finish_by out.
func TestOneSidedDeadlines(t *testing.T) {
	h := newHarness(t)
	finishOnly := msgReq("finish", "x")
	finishOnly.FinishBy = t0.Add(time.Hour)
	op := h.propose(t, finishOnly)
	if !op.StartBy.Equal(t0.Add(time.Hour)) || !op.FinishBy.Equal(t0.Add(time.Hour)) {
		t.Fatalf("finish_by only: start %s finish %s, want both at +1h", op.StartBy, op.FinishBy)
	}

	deferred := msgReq("deferred", "x")
	deferred.NotBefore = t0.Add(2 * time.Hour)
	deferred.FinishBy = t0.Add(3 * time.Hour)
	op = h.propose(t, deferred)
	if !op.StartBy.Equal(t0.Add(3*time.Hour)) || !op.FinishBy.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("not_before + finish_by: start %s finish %s, want both at +3h", op.StartBy, op.FinishBy)
	}

	early := msgReq("early", "x")
	early.NotBefore = t0.Add(2 * time.Hour)
	early.FinishBy = t0.Add(time.Hour)
	if _, _, err := h.store.Propose(early); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("finish_by before not_before = %v, want ErrInvalidRequest", err)
	}

	startOnly := msgReq("start", "x")
	startOnly.StartBy = t0.Add(48 * time.Hour)
	op = h.propose(t, startOnly)
	if !op.StartBy.Equal(t0.Add(48*time.Hour)) || !op.FinishBy.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("start_by only: start %s finish %s, want both at +48h", op.StartBy, op.FinishBy)
	}
}

// bdTypingStore applies bd's --set-metadata typing to every fenced update, as
// BdStore's UpdateIfMatch is read back: "null" reads as "", and a number
// spelling reads back in its normalized form.
type bdTypingStore struct{ *beads.MemStore }

func (s bdTypingStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if opts.Metadata != nil {
		typed := make(map[string]string, len(opts.Metadata))
		for k, v := range opts.Metadata {
			typed[k] = bdTypedReadBack(v)
		}
		opts.Metadata = typed
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func bdTypedReadBack(v string) string {
	if v == "null" {
		return ""
	}
	if json.Valid([]byte(v)) {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return v
}

// Text that bd would type as a JSON scalar is refused before it is written,
// so an update can never turn a conversation unreadable; everything else
// round-trips through bd's typing.
func TestUpdateTextSurvivesBdTyping(t *testing.T) {
	newH := func(t *testing.T) *harness {
		mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
		return newHarnessOver(t, mem, bdTypingStore{mem})
	}
	t.Run("refused", func(t *testing.T) {
		h := newH(t)
		op := h.propose(t, msgReq("k", "x"))
		rev := h.revision(t, op.ID)
		for _, v := range []string{"null", "true", "false", "1.50", "1e3", "-0", "42"} {
			if _, _, err := h.store.Cancel(op.ID, v, ""); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Cancel(by %q) = %v, want ErrInvalidRequest", v, err)
			}
			if _, _, err := h.store.Cancel(op.ID, "operator:bob", v); !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("Cancel(reason %q) = %v, want ErrInvalidRequest", v, err)
			}
			for name, o := range map[string]Outcome{
				"reason":      {Arm: ArmFailed, Reason: v},
				"degradation": {Arm: ArmSucceeded, Degradation: v},
				"result turn": {Arm: ArmSucceeded, Result: Result{Turn: v}},
				"result via":  {Arm: ArmSucceeded, Result: Result{Via: v}},
			} {
				if _, err := h.store.Close(op.ID, o, "lane"); !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("Close(%s %q) = %v, want ErrInvalidRequest", name, v, err)
				}
			}
		}
		if h.revision(t, op.ID) != rev {
			t.Fatal("a refused update wrote the row")
		}
	})
	t.Run("round trip", func(t *testing.T) {
		h := newH(t)
		op := h.propose(t, msgReq("k", "x"))
		if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.store.Cancel(op.ID, "operator:bob", "changed my mind at 10"); err != nil {
			t.Fatal(err)
		}
		want := Outcome{Arm: ArmCanceled, Result: Result{Generation: 4, Turn: "t9", ContextEpoch: 2, Via: "hook"}}
		if _, err := h.store.Close(op.ID, want, "lane"); err != nil {
			t.Fatal(err)
		}
		got, err := h.store.Get(op.ID)
		if err != nil || got.Outcome != want || got.Cancel.Reason != "changed my mind at 10" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := h.store.ListOpen("gc-1"); err != nil {
			t.Fatalf("ListOpen: %v", err)
		}
	})
}

// legacyReq is a message an O0 legacy surface records: start_by and
// finish_by at the legacy execution bound, 15 minutes out.
func legacyReq(executor ExecutorKind, key string) Request {
	r := msgReq(key, "hello")
	r.Executor = executor
	r.Producer.Principal = "operator"
	r.StartBy = t0.Add(15 * time.Minute)
	r.FinishBy = r.StartBy
	return r
}

// Propose writes the executor and nothing changes it: a retry naming another
// executor (a gate flip between attempts) returns the stored op.
func TestProposeWritesTheExecutorOnce(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
	if op.Executor != ExecutorLegacyAPI {
		t.Fatalf("executor %s", op.Executor)
	}
	again, created, err := h.store.Propose(legacyReq(ExecutorLane, "k"))
	if err != nil || created || again.Executor != ExecutorLegacyAPI {
		t.Fatalf("re-Propose under another executor = %s created %v, %v", again.Executor, created, err)
	}
	if defaulted := h.propose(t, legacyReq("", "k2")); defaulted.Executor != ExecutorLegacy {
		t.Fatalf("defaulted executor %s, want legacy", defaulted.Executor)
	}
	if _, _, err := h.store.Propose(legacyReq("box", "k3")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Propose(executor box) = %v, want ErrInvalidRequest", err)
	}
}

// A minted key skips the read-first Get: one create and nothing else.
func TestProposeMintedKeySkipsTheRead(t *testing.T) {
	h := newHarness(t)
	req := legacyReq(ExecutorLegacyCLI, "k-minted")
	req.MintedKey = true
	h.rec.Reset()
	op := h.propose(t, req)
	ops := h.rec.Ops()
	if len(ops) != 1 || ops[0].Kind != beadstest.OpCreate {
		t.Fatalf("minted Propose issued %+v, want exactly one create", ops)
	}
	if op.Revision != h.revision(t, op.ID) {
		t.Fatalf("Revision %d, bead revision %d", op.Revision, h.revision(t, op.ID))
	}
	h.rec.Reset()
	req.MintedKey = false
	if _, created, err := h.store.Propose(req); err != nil || created {
		t.Fatalf("supplied-key retry = created %v, %v", created, err)
	}
	if n := h.countOps(beadstest.OpCreate); n != 0 {
		t.Fatalf("supplied-key retry issued %d creates", n)
	}
}

// Preview is Propose's first half: the same id and digest, no I/O.
func TestPreviewEqualsPropose(t *testing.T) {
	h := newHarness(t)
	req := legacyReq(ExecutorLegacyAPI, "k")
	h.rec.Reset()
	pre, err := h.store.Preview(req)
	if err != nil {
		t.Fatal(err)
	}
	if ops := h.rec.Ops(); len(ops) != 0 {
		t.Fatalf("Preview issued %+v", ops)
	}
	op := h.propose(t, req)
	if pre.ID != op.ID || pre.PayloadDigest != op.PayloadDigest || pre.Status != StatusSubmitted || pre.Executor != op.Executor {
		t.Fatalf("Preview = %s %s %s, Propose = %s %s %s", pre.ID, pre.PayloadDigest, pre.Status, op.ID, op.PayloadDigest, op.Status)
	}
	big := req
	big.Payload = strings.Repeat("x", MaxInlinePayload+1)
	if _, err := h.store.Preview(big); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("Preview(oversized) = %v", err)
	}
}

// Payloads spelled like JSON scalars round-trip on create, and their digest
// with them, on MemStore and SQLite.
func TestScalarSpelledPayloadsRoundTrip(t *testing.T) {
	sq, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sq.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	sqStore, err := NewStore(beads.SessionStore{Store: sq}, &clock.Fake{Time: t0})
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Store{"memstore": newHarness(t).store, "sqlite": sqStore} {
		for i, payload := range []string{"null", "042", "true", "1e3", "-0"} {
			op, created, err := s.Propose(msgReq(fmt.Sprint("k", i), payload))
			if err != nil || !created {
				t.Fatalf("%s: Propose(%q) = %v, %v", name, payload, created, err)
			}
			got, err := s.Get(op.ID)
			if err != nil || got.Payload != payload || got.PayloadDigest != op.PayloadDigest || RequestDigest(got) != op.PayloadDigest {
				t.Fatalf("%s: payload %q read back %q (digest %s), %v", name, payload, got.Payload, got.PayloadDigest, err)
			}
			if _, created, err := s.Propose(msgReq(fmt.Sprint("k", i), payload)); err != nil || created {
				t.Fatalf("%s: re-Propose(%q) = created %v, %v", name, payload, created, err)
			}
		}
	}
}

func TestAdopt(t *testing.T) {
	finish := t0.Add(30 * time.Minute)
	t.Run("adopts", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
		h.clk.Advance(time.Minute)
		got, err := h.store.Adopt(op.ID, 0, "legacy:api", finish)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := h.store.Get(op.ID)
		last := stored.Transitions[len(stored.Transitions)-1]
		switch {
		case got.Attempt != 1 || stored.Attempt != 1:
			t.Fatalf("attempt %d (stored %d), want 1", got.Attempt, stored.Attempt)
		case !stored.FinishBy.Equal(finish) || !got.FinishBy.Equal(finish):
			t.Fatalf("finish_by %s, want %s", stored.FinishBy, finish)
		case stored.Status != StatusSubmitted || len(stored.Transitions) != 2 ||
			last != (Transition{Status: StatusSubmitted, At: t0.Add(time.Minute), By: "legacy:api"}):
			t.Fatalf("status %s transitions %+v", stored.Status, stored.Transitions)
		}
		if _, err := h.store.Adopt(op.ID, 0, "legacy:api", finish); !errors.Is(err, ErrAttemptMoved) {
			t.Fatalf("second adoption of attempt 0 = %v, want ErrAttemptMoved", err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		h := newHarness(t)
		closedOp := h.propose(t, legacyReq(ExecutorLegacyAPI, "closed"))
		if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "legacy:api"); err != nil {
			t.Fatal(err)
		}
		lane := h.propose(t, legacyReq(ExecutorLane, "lane"))
		cli := h.propose(t, legacyReq(ExecutorLegacyCLI, "cli"))
		bare := h.propose(t, legacyReq(ExecutorLegacy, "bare"))
		api := h.propose(t, legacyReq(ExecutorLegacyAPI, "api"))
		for _, c := range []struct {
			name, id string
			finish   time.Time
			want     error
		}{
			{"closed", closedOp.ID, finish, ErrAlreadyTerminal},
			{"lane", lane.ID, finish, ErrIllegalTransition},
			{"legacy:cli", cli.ID, finish, ErrIllegalTransition},
			{"bare legacy", bare.ID, finish, ErrIllegalTransition},
			{"finish_by before start_by", api.ID, api.StartBy.Add(-time.Second), ErrInvalidRequest},
			{"no finish_by", api.ID, time.Time{}, ErrInvalidRequest},
		} {
			rev := h.revision(t, c.id)
			if _, err := h.store.Adopt(c.id, 0, "legacy:api", c.finish); !errors.Is(err, c.want) {
				t.Errorf("%s: Adopt = %v, want %v", c.name, err, c.want)
			}
			if h.revision(t, c.id) != rev {
				t.Errorf("%s: a refused adoption wrote the row", c.name)
			}
		}
	})
	t.Run("32 concurrent adopters, one winner", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
		var wins, moved atomic.Int32
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := h.store.Adopt(op.ID, 0, "legacy:api", finish)
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, ErrAttemptMoved):
					moved.Add(1)
				default:
					t.Errorf("Adopt = %v", err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 || moved.Load() != 31 {
			t.Fatalf("wins %d moved %d, want 1 and 31", wins.Load(), moved.Load())
		}
		if got, _ := h.store.Get(op.ID); got.Attempt != 1 {
			t.Fatalf("attempt %d after one adoption", got.Attempt)
		}
	})
}

// inflightReq is a legacy:api legacyReq with a finish_by an hour past the
// rest: the deadline an adoption or an explicit request gives an older op.
func inflightReq(key string) Request {
	r := legacyReq(ExecutorLegacyAPI, key)
	r.FinishBy = t0.Add(time.Hour)
	return r
}

func TestListExpiredLegacy(t *testing.T) {
	h := newHarness(t)
	h.propose(t, inflightReq("inflight"))
	api := h.propose(t, legacyReq(ExecutorLegacyAPI, "a"))
	h.clk.Advance(time.Second)
	cli := h.propose(t, legacyReq(ExecutorLegacyCLI, "b"))
	h.clk.Advance(time.Second)
	bare := h.propose(t, legacyReq(ExecutorLegacy, "c"))
	h.clk.Advance(time.Second)
	h.propose(t, legacyReq(ExecutorLane, "lane"))
	closedOp := h.propose(t, legacyReq(ExecutorLegacyAPI, "closed"))
	if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "legacy:api"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mem.Create(beads.Bead{
		ID: "gc-op-bogus", Type: BeadType, Labels: []string{LabelOp, ConversationLabel("gc-1")},
		Metadata: map[string]string{keySchema: "1", keyKind: "message", keyStatus: string(StatusSubmitted)},
	}); err != nil {
		t.Fatal(err)
	}
	at := api.FinishBy.Add(time.Second)
	h.rec.Reset()
	scan, err := h.store.ListExpiredLegacy(at, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{api.ID, cli.ID, bare.ID}; fmt.Sprint(ids(scan.Expired)) != fmt.Sprint(want) {
		t.Fatalf("ListExpiredLegacy = %v, want %v", ids(scan.Expired), want)
	}
	if scan.Pending != 1 {
		t.Fatalf("pending = %d, want the in-flight op", scan.Pending)
	}
	if fmt.Sprint(scan.Undecodable) != "[gc-op-bogus]" {
		t.Fatalf("undecodable = %v", scan.Undecodable)
	}
	if scan.Full || scan.Truncated {
		t.Fatalf("a read whose matches ran out reported full %t, truncated %t", scan.Full, scan.Truncated)
	}
	ops := h.rec.Ops()
	if len(ops) != 1 || ops[0].Kind != beadstest.OpList || ops[0].Query.Label != LabelOp ||
		ops[0].Query.Metadata[keyStatus] != string(StatusSubmitted) || ops[0].Query.Limit != 100 {
		t.Fatalf("ListExpiredLegacy issued %+v, want one List keyed by %s", ops, keyStatus)
	}
	// The in-flight op ahead of api does not count toward limit.
	if scan, err := h.store.ListExpiredLegacy(at, 1); err != nil || fmt.Sprint(ids(scan.Expired)) != fmt.Sprint([]string{api.ID}) ||
		scan.Pending != 1 || !scan.Full || scan.Truncated {
		t.Fatalf("ListExpiredLegacy(1) = %v %+v, %v; want %s, full", ids(scan.Expired), scan, err, api.ID)
	}
	// finish_by itself is not past it.
	if scan, err := h.store.ListExpiredLegacy(api.FinishBy, 100); err != nil || len(scan.Expired) != 0 || scan.Pending != 4 {
		t.Fatalf("ListExpiredLegacy at finish_by = %v %+v, %v; want 4 pending", ids(scan.Expired), scan, err)
	}
}

// limit counts expired legacy ops, not rows read: a head of lane, undecodable
// and in-flight legacy rows at least limit long does not starve the sweeper,
// and the in-flight ops it passes count pending. Rows read stay bounded by
// legacyScanPages pages of limit, and a read the cap stops after a full page
// reports truncated: rows behind it may be unread. A read whose limit-th
// expired op ends a short page read every match, so it is not full.
func TestListExpiredLegacyPagesPastUnsweepableRows(t *testing.T) {
	h := newHarness(t)
	for i := range 3 {
		h.propose(t, legacyReq(ExecutorLane, fmt.Sprint("lane", i)))
	}
	if _, err := h.mem.Create(beads.Bead{
		ID: "gc-op-bogus", Type: BeadType, Labels: []string{LabelOp, ConversationLabel("gc-1")},
		Metadata: map[string]string{keySchema: "1", keyKind: "message", keyStatus: string(StatusSubmitted)},
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		h.propose(t, inflightReq(fmt.Sprint("inflight", i)))
	}
	a := h.propose(t, legacyReq(ExecutorLegacyAPI, "a"))
	b := h.propose(t, legacyReq(ExecutorLegacyCLI, "b"))
	h.propose(t, legacyReq(ExecutorLegacyCLI, "c"))
	at := a.FinishBy.Add(time.Second)
	scan, err := h.store.ListExpiredLegacy(at, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{a.ID, b.ID}; fmt.Sprint(ids(scan.Expired)) != fmt.Sprint(want) {
		t.Fatalf("ListExpiredLegacy(2) = %v, want %v", ids(scan.Expired), want)
	}
	if fmt.Sprint(scan.Undecodable) != "[gc-op-bogus]" {
		t.Fatalf("undecodable = %v", scan.Undecodable)
	}
	if scan.Pending != 3 || !scan.Full || scan.Truncated {
		t.Fatalf("scan = %+v, want the 3 in-flight ops pending, full, not truncated", scan)
	}

	// A head longer than the cap hides the expired op behind it, and says so.
	capped := newHarness(t)
	for i := range 2*legacyScanPages + 1 {
		capped.propose(t, inflightReq(fmt.Sprint("inflight", i)))
	}
	capped.propose(t, legacyReq(ExecutorLegacyCLI, "late"))
	capped.rec.Reset()
	if scan, err := capped.store.ListExpiredLegacy(at, 2); err != nil || len(scan.Expired) != 0 ||
		scan.Pending != 2*legacyScanPages || scan.Full || !scan.Truncated {
		t.Fatalf("ListExpiredLegacy(2) past the scan cap = %v %+v, %v", ids(scan.Expired), scan, err)
	}
	if n := capped.countOps(beadstest.OpList); n != legacyScanPages {
		t.Fatalf("read %d pages, want the cap %d", n, legacyScanPages)
	}

	// Matches that run out on the last page the cap allows are not truncated.
	short := newHarness(t)
	for i := range 2*legacyScanPages - 1 {
		short.propose(t, legacyReq(ExecutorLane, fmt.Sprint("lane", i)))
	}
	short.rec.Reset()
	if scan, err := short.store.ListExpiredLegacy(at, 2); err != nil || len(scan.Expired) != 0 || scan.Full || scan.Truncated {
		t.Fatalf("ListExpiredLegacy(2) ending on the cap's short page = %v %+v, %v", ids(scan.Expired), scan, err)
	}
	if n := short.countOps(beadstest.OpList); n != legacyScanPages {
		t.Fatalf("read %d pages, want %d", n, legacyScanPages)
	}

	// The limit-th expired op ends the short second page, so no row is left
	// unread.
	exact := newHarness(t)
	exact.propose(t, inflightReq("inflight"))
	exact.clk.Advance(time.Second)
	ea := exact.propose(t, legacyReq(ExecutorLegacyAPI, "a"))
	exact.clk.Advance(time.Second)
	eb := exact.propose(t, legacyReq(ExecutorLegacyCLI, "b"))
	exact.rec.Reset()
	scan, err = exact.store.ListExpiredLegacy(at, 2)
	if err != nil || fmt.Sprint(ids(scan.Expired)) != fmt.Sprint([]string{ea.ID, eb.ID}) || scan.Pending != 1 || scan.Full || scan.Truncated {
		t.Fatalf("ListExpiredLegacy(2) ending on its short last page = %v %+v, %v; want both expired ops, neither full nor truncated", ids(scan.Expired), scan, err)
	}
	if n := exact.countOps(beadstest.OpList); n != 2 {
		t.Fatalf("read %d pages, want 2", n)
	}
}

func TestCloseKnown(t *testing.T) {
	out := Outcome{Arm: ArmSucceeded, Result: Result{Via: "legacy.delivered"}}
	t.Run("fast path", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyCLI, "k"))
		h.rec.Reset()
		got, err := h.store.CloseKnown(op, out, "legacy:cli")
		if err != nil || got.Status != StatusClosed || got.Outcome != out {
			t.Fatalf("CloseKnown = %+v, %v", got, err)
		}
		ops := h.rec.Ops()
		if len(ops) != 1 || ops[0].Kind != beadstest.OpUpdateIfMatch {
			t.Fatalf("CloseKnown issued %+v, want one fenced update", ops)
		}
		b, _ := h.mem.Get(op.ID)
		if b.Status != "closed" || b.Metadata[keyOpen] != "" {
			t.Fatalf("bead %s open key %q after CloseKnown", b.Status, b.Metadata[keyOpen])
		}
		if stored, _ := h.store.Get(op.ID); stored.Outcome != out || stored.Status != StatusClosed {
			t.Fatalf("stored %+v", stored)
		}
	})
	t.Run("fallback when the row moved", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
		if _, err := h.store.Adopt(op.ID, 0, "legacy:api", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := h.store.CloseKnown(op, out, "legacy:api")
		if err != nil || got.Status != StatusClosed || got.Attempt != 1 {
			t.Fatalf("CloseKnown after a move = %+v, %v", got, err)
		}
		if _, err := h.store.CloseKnown(op, Outcome{Arm: ArmFailed, Reason: ReasonOther}, "legacy:api"); !errors.Is(err, ErrAlreadyTerminal) {
			t.Fatalf("CloseKnown with another outcome = %v, want ErrAlreadyTerminal", err)
		}
		if _, err := h.store.CloseKnown(op, out, "legacy:api"); err != nil {
			t.Fatalf("CloseKnown again with the same outcome = %v", err)
		}
	})
	t.Run("refuses a bad outcome", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
		if _, err := h.store.CloseKnown(op, Outcome{Arm: ArmFailed}, "legacy:api"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("CloseKnown(failed, no reason) = %v", err)
		}
	})
}

// Every close blanks gc.op.open, a bead closed outside the journal included,
// so ListOpen never reads a closed row.
func TestClosesBlankTheOpenKey(t *testing.T) {
	h := newHarness(t)
	closedOp := h.propose(t, msgReq("a", "x"))
	if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatal(err)
	}
	canceled := h.propose(t, msgReq("b", "x"))
	if r, _, err := h.store.Cancel(canceled.ID, "operator:bob", ""); err != nil || r != CancelClosed {
		t.Fatalf("Cancel = %s, %v", r, err)
	}
	external := h.propose(t, msgReq("c", "x"))
	if err := h.mem.Close(external.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.mem.Get(external.ID); b.Metadata[keyOpen] != "gc-1" {
		t.Fatalf("setup: external close dropped %s", keyOpen)
	}
	if _, err := h.store.Close(external.ID, Outcome{Arm: ArmFailed, Reason: ReasonClosedExternally}, "lane"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{closedOp.ID, canceled.ID, external.ID} {
		if b, _ := h.mem.Get(id); b.Metadata[keyOpen] != "" {
			t.Errorf("%s still carries %s = %q", id, keyOpen, b.Metadata[keyOpen])
		}
	}
	if open, err := h.store.ListOpen("gc-1"); err != nil || len(open) != 0 {
		t.Fatalf("ListOpen = %v, %v", ids(open), err)
	}
}

// ListOpenTolerant serves the conversation's decodable rows and quarantines
// the rest, where ListOpen fails.
func TestListOpenTolerantQuarantines(t *testing.T) {
	h := newHarness(t)
	a := h.propose(t, msgReq("a", "x"))
	if _, err := h.mem.Create(beads.Bead{
		ID: "gc-op-newer", Type: BeadType, Labels: []string{LabelOp, ConversationLabel("gc-1")},
		Metadata: map[string]string{keySchema: "1", keyKind: "teleport", keyOpen: "gc-1"},
	}); err != nil {
		t.Fatal(err)
	}
	b := h.propose(t, msgReq("b", "y"))
	h.rec.Reset()
	ops, quarantined, err := h.store.ListOpenTolerant("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(ops)) != fmt.Sprint([]string{a.ID, b.ID}) {
		t.Fatalf("served %v", ids(ops))
	}
	if len(quarantined) != 1 || quarantined[0].ID != "gc-op-newer" || !errors.Is(quarantined[0].Err, ErrUnsupportedKind) {
		t.Fatalf("quarantined %+v", quarantined)
	}
	if n := h.countOps(beadstest.OpList); n != 1 || len(h.rec.Ops()) != 1 {
		t.Fatalf("ListOpenTolerant issued %+v, want one list", h.rec.Ops())
	}
	if _, err := h.store.ListOpen("gc-1"); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("ListOpen = %v, want ErrUnsupportedKind", err)
	}
}

// CloseExpired closes an open legacy op past its finish_by, deciding on the
// row it reads: an op adopted (finish_by moved), a lane op, an op not yet
// expired and a closed op are not written.
func TestCloseExpired(t *testing.T) {
	expired := Outcome{Arm: ArmFailed, Reason: ReasonDeadlineInFlight}
	past := t0.Add(16 * time.Minute)
	t.Run("closes an expired legacy op", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyCLI, "k"))
		h.rec.Reset()
		got, closedNow, err := h.store.CloseExpired(op, past, "controller:op-sweep")
		if err != nil || !closedNow || got.Status != StatusClosed || got.Outcome != expired {
			t.Fatalf("CloseExpired = %s %+v closed %v, %v", got.Status, got.Outcome, closedNow, err)
		}
		if ops := h.rec.Ops(); len(ops) != 1 || ops[0].Kind != beadstest.OpUpdateIfMatch {
			t.Fatalf("CloseExpired of a known expired op issued %+v, want one fenced update", ops)
		}
		if b, _ := h.mem.Get(op.ID); b.Status != "closed" {
			t.Fatalf("bead %s", b.Status)
		}
		if last := got.Transitions[len(got.Transitions)-1]; last.By != "controller:op-sweep" {
			t.Fatalf("closed by %q", last.By)
		}
	})
	t.Run("leaves an adopted op open", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyAPI, "k"))
		if _, err := h.store.Adopt(op.ID, 0, "legacy:api", past.Add(15*time.Minute)); err != nil {
			t.Fatal(err)
		}
		rev := h.revision(t, op.ID)
		// op is the row as read before the adoption: the fenced write misses
		// and the re-read sees the adopter's finish_by.
		got, closedNow, err := h.store.CloseExpired(op, past, "controller:op-sweep")
		if err != nil || closedNow || got.Status != StatusSubmitted || got.Attempt != 1 {
			t.Fatalf("CloseExpired(adopted) = %s attempt %d closed %v, %v", got.Status, got.Attempt, closedNow, err)
		}
		if h.revision(t, op.ID) != rev {
			t.Fatal("CloseExpired wrote an adopted op")
		}
	})
	t.Run("leaves an op at or before its finish_by open", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyCLI, "k"))
		for _, now := range []time.Time{t0, op.FinishBy} {
			if got, closedNow, err := h.store.CloseExpired(op, now, "controller:op-sweep"); err != nil || closedNow || got.Status != StatusSubmitted {
				t.Fatalf("CloseExpired(at %s) = %s closed %v, %v", now, got.Status, closedNow, err)
			}
		}
	})
	t.Run("never closes a lane op", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLane, "k"))
		if got, closedNow, err := h.store.CloseExpired(op, past, "controller:op-sweep"); err != nil || closedNow || got.Status != StatusSubmitted {
			t.Fatalf("CloseExpired(lane) = %s closed %v, %v", got.Status, closedNow, err)
		}
	})
	t.Run("returns a closed op unchanged", func(t *testing.T) {
		h := newHarness(t)
		op := h.propose(t, legacyReq(ExecutorLegacyCLI, "k"))
		done := Outcome{Arm: ArmSucceeded, Result: Result{Via: "legacy"}}
		if _, err := h.store.Close(op.ID, done, "legacy:cli"); err != nil {
			t.Fatal(err)
		}
		got, closedNow, err := h.store.CloseExpired(op, past, "controller:op-sweep")
		if err != nil || closedNow || got.Status != StatusClosed || got.Outcome != done {
			t.Fatalf("CloseExpired(closed) = %s %+v closed %v, %v", got.Status, got.Outcome, closedNow, err)
		}
	})
}

// Only its legacy path, an adopter or the sweeper closes a legacy op, and it
// never leaves submitted before: Transition and Cancel refuse an open one.
func TestLegacyOpsRefuseTransitionAndCancel(t *testing.T) {
	h := newHarness(t)
	op := h.propose(t, legacyReq(ExecutorLegacyCLI, "k"))
	for _, to := range []Status{StatusAccepted, StatusExecuting} {
		if _, err := h.store.Transition(op.ID, StatusSubmitted, to, "lane"); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("Transition(legacy op -> %s) = %v, want ErrIllegalTransition", to, err)
		}
	}
	if _, _, err := h.store.Cancel(op.ID, "operator:alice", "no"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Cancel(open legacy op) = %v, want ErrIllegalTransition", err)
	}
	if got, _ := h.store.Get(op.ID); got.Status != StatusSubmitted || got.Revision != op.Revision {
		t.Fatalf("refused moves wrote the op: %s revision %d", got.Status, got.Revision)
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded, Result: Result{Via: "legacy"}}, "legacy:cli"); err != nil {
		t.Fatal(err)
	}
	if res, _, err := h.store.Cancel(op.ID, "operator:alice", "no"); err != nil || res != CancelAlreadyTerminal {
		t.Fatalf("Cancel(closed legacy op) = %s, %v; want already_terminal", res, err)
	}
}

// A gc.op.arrival_seq a producer wrote through bd is not this build's key:
// the store neither reads it nor removes it.
func TestStoreLeavesAForgedArrivalSeqUntouched(t *testing.T) {
	const forged = "gc.op.arrival_seq"
	h := newHarness(t)
	op := h.propose(t, msgReq("a", "x"))
	if err := h.mem.SetMetadata(op.ID, forged, "9"); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.Get(op.ID)
	if err != nil || got.ArrivalSeq != 0 {
		t.Fatalf("Get = ArrivalSeq %d, %v; want 0", got.ArrivalSeq, err)
	}
	if _, err := h.store.Close(op.ID, Outcome{Arm: ArmSucceeded}, "lane"); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.mem.Get(op.ID); b.Metadata[forged] != "9" {
		t.Fatalf("Close changed %s to %q", forged, b.Metadata[forged])
	}
}

// TestHasSubmittedOps proves the capability-free open-op probe: false on a
// store with no op rows or only closed ones, true once any op (lane or
// legacy) is submitted, and one keyed read of at most one row.
func TestHasSubmittedOps(t *testing.T) {
	h := newHarness(t)
	probe := func() bool {
		t.Helper()
		got, err := HasSubmittedOps(beads.SessionStore{Store: h.rec})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if probe() {
		t.Fatal("an empty store reports a submitted op")
	}
	closedOp := h.propose(t, legacyReq(ExecutorLegacyAPI, "closed"))
	if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "legacy:api"); err != nil {
		t.Fatal(err)
	}
	if probe() {
		t.Fatal("a closed op reads as submitted")
	}
	h.propose(t, legacyReq(ExecutorLane, "lane"))
	h.rec.Reset()
	if !probe() {
		t.Fatal("a submitted op was not seen")
	}
	ops := h.rec.Ops()
	if len(ops) != 1 || ops[0].Kind != beadstest.OpList || ops[0].Query.Limit != 1 {
		t.Fatalf("probe ops = %+v, want one keyed list of limit 1", ops)
	}
}

// TestHasSubmittedOpsReadError proves a failed read is returned, never read
// as "no ops".
func TestHasSubmittedOpsReadError(t *testing.T) {
	if _, err := HasSubmittedOps(beads.SessionStore{Store: failingListStore{beads.NewMemStore()}}); err == nil {
		t.Fatal("a failed read reported no error")
	}
}

// TestHasSubmittedOpsNilStore proves a nil or typed-nil store is an error,
// never a panic or "no ops".
func TestHasSubmittedOpsNilStore(t *testing.T) {
	if _, err := HasSubmittedOps(beads.SessionStore{}); err == nil {
		t.Fatal("a nil store reported no error")
	}
	var nilMem *beads.MemStore
	if _, err := HasSubmittedOps(beads.SessionStore{Store: nilMem}); err == nil {
		t.Fatal("a typed nil store reported no error")
	}
}

type failingListStore struct{ beads.Store }

func (failingListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("dolt down")
}
