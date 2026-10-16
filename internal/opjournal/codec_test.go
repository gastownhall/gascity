package opjournal

import (
	"errors"
	"maps"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestCodecRoundTripProperty(t *testing.T) {
	g := newOpGen(20261013)
	seenKinds := map[Kind]bool{}
	seenStatus := map[Status]bool{}
	seenArms := map[Arm]bool{}
	for i := range 500 {
		op := g.op()
		seenKinds[op.Kind] = true
		seenStatus[op.Status] = true
		seenArms[op.Outcome.Arm] = true
		b, err := EncodeOp(op)
		if err != nil {
			t.Fatalf("case %d: EncodeOp(%+v): %v", i, op, err)
		}
		checkOpBeadShape(t, b, op)
		got, err := DecodeOp(b)
		if err != nil {
			t.Fatalf("case %d: DecodeOp: %v\nbead: %+v", i, err, b)
		}
		if !reflect.DeepEqual(got, op) {
			t.Fatalf("case %d: round trip changed the op\n got: %+v\nwant: %+v", i, got, op)
		}
	}
	if len(seenKinds) != len(Kinds()) || len(seenStatus) != 5 || len(seenArms) != 5 {
		t.Fatalf("generator coverage: kinds %d statuses %d arms %d", len(seenKinds), len(seenStatus), len(seenArms))
	}
}

func checkOpBeadShape(t *testing.T, b beads.Bead, op Op) {
	t.Helper()
	if b.Type != BeadType || !b.NoHistory || b.Ephemeral || b.ParentID != "" || len(b.Needs) != 0 {
		t.Fatalf("bead shape %+v", b)
	}
	if strings.Contains(b.Title, op.Payload) && op.Payload != "" && len(op.Payload) > 8 {
		t.Fatalf("payload leaked into the title %q", b.Title)
	}
	wantStatus := "open"
	if op.Status == StatusClosed {
		wantStatus = "closed"
	}
	if b.Status != wantStatus {
		t.Fatalf("bead status %q for op status %s", b.Status, op.Status)
	}
	for k, v := range b.Metadata {
		if !beadmeta.ValidKey(k) || v == "" {
			t.Fatalf("metadata %q=%q", k, v)
		}
		if k == "gc.routed_to" || k == "gc.root_bead_id" || k == "gc.kind" {
			t.Fatalf("op row carries %s", k)
		}
	}
	for _, l := range b.Labels {
		if !beadmeta.ValidLabel(l) {
			t.Fatalf("label %q is not valid", l)
		}
	}
	if open := b.Metadata[keyOpen]; (op.Status == StatusClosed) != (open == "") || (open != "" && open != op.Conversation) {
		t.Fatalf("%s = %q for an op in %s of conversation %s", keyOpen, open, op.Status, op.Conversation)
	}
	if b.Metadata[keyExecutor] != string(op.Executor) {
		t.Fatalf("%s = %q, want %s", keyExecutor, b.Metadata[keyExecutor], op.Executor)
	}
	if got := coordclass.Classify(b); got != coordclass.ClassSessions {
		t.Fatalf("Classify(op %s) = %v, want sessions", op.Status, got)
	}
	if !beads.IsReadyExcludedBead(b) || !beads.HasReadyExcludedLabel(b) {
		t.Fatalf("op row is not ready-excluded by type and by label")
	}
}

func sampleOp(t *testing.T) Op {
	t.Helper()
	spec, _ := Lookup(KindMessage)
	op, err := normalizeRequest(baseMessage(), spec, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	op.ID, _ = DeriveID("gc", op.Conversation, op.Producer.Principal, op.ClientKey)
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: op.Producer.Principal}}
	return op
}

func encode(t *testing.T, op Op) beads.Bead {
	t.Helper()
	b, err := EncodeOp(op)
	if err != nil {
		t.Fatalf("EncodeOp: %v", err)
	}
	return b
}

func TestUnknownReasonRoundTripsAndReadsFailedOther(t *testing.T) {
	for _, arm := range []Arm{ArmFailed, ArmSkipped} {
		op := sampleOp(t)
		op.Status = StatusClosed
		op.Outcome = Outcome{Arm: arm, Reason: "brand_new_reason"}
		got, err := DecodeOp(encode(t, op))
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome.Reason != "brand_new_reason" || got.Outcome.Arm != arm {
			t.Fatalf("outcome %+v", got.Outcome)
		}
		if a, r := got.Outcome.Effective(); a != ArmFailed || r != ReasonOther {
			t.Fatalf("Effective() = (%s, %s), want (failed, other)", a, r)
		}
	}
	for _, o := range []Outcome{
		{Arm: ArmFailed, Reason: "refused:quota"},
		{Arm: ArmSkipped, Reason: ReasonStale},
		{Arm: ArmSucceeded},
	} {
		if a, r := o.Effective(); a != o.Arm || r != o.Reason {
			t.Errorf("Effective(%+v) = (%s, %s)", o, a, r)
		}
	}
}

func TestDecodeRefusals(t *testing.T) {
	good := encode(t, sampleOp(t))
	mutate := func(f func(b *beads.Bead)) beads.Bead {
		b := good
		b.Metadata = maps.Clone(good.Metadata)
		b.Labels = append([]string(nil), good.Labels...)
		f(&b)
		return b
	}
	cases := []struct {
		name string
		bead beads.Bead
		want error
	}{
		{"unknown kind", mutate(func(b *beads.Bead) { b.Metadata[keyKind] = "teleport" }), ErrUnsupportedKind},
		{"schema 2", mutate(func(b *beads.Bead) { b.Metadata[keySchema] = "2" }), ErrUnsupportedSchema},
		{"missing schema", mutate(func(b *beads.Bead) { delete(b.Metadata, keySchema) }), ErrCorruptOp},
		{"missing kind", mutate(func(b *beads.Bead) { delete(b.Metadata, keyKind) }), ErrCorruptOp},
		{"id mismatch", mutate(func(b *beads.Bead) { b.ID = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" }), ErrCorruptOp},
		{"reserved id prefix", mutate(func(b *beads.Bead) { b.ID = "g.c" + strings.TrimPrefix(b.ID, "gc") }), ErrCorruptOp},
		{"empty id prefix", mutate(func(b *beads.Bead) { b.ID = strings.TrimPrefix(b.ID, "gc") }), ErrCorruptOp},
		{"key edited", mutate(func(b *beads.Bead) { b.Metadata[keyClientKey] = "k2" }), ErrCorruptOp},
		{"non-lease assignee", mutate(func(b *beads.Bead) { b.Assignee = "alice" }), ErrCorruptOp},
		{"open with outcome", mutate(func(b *beads.Bead) { b.Metadata[keyOutcome] = "succeeded" }), ErrCorruptOp},
		{"closed without outcome", mutate(func(b *beads.Bead) { b.Metadata[keyStatus] = "closed" }), ErrCorruptOp},
		{"bad status", mutate(func(b *beads.Bead) { b.Metadata[keyStatus] = "running" }), ErrCorruptOp},
		{"bad integer", mutate(func(b *beads.Bead) { b.Metadata[keyLaneSeq] = "01" }), ErrCorruptOp},
		{"bad time", mutate(func(b *beads.Bead) { b.Metadata[keyCreatedAt] = "noon" }), ErrCorruptOp},
		{"bad transitions", mutate(func(b *beads.Bead) { b.Metadata[keyTransitions] = "[" }), ErrCorruptOp},
		{"bad digest", mutate(func(b *beads.Bead) { b.Metadata[keyPayloadDigest] = "abc" }), ErrCorruptOp},
		{"foreign policy", mutate(func(b *beads.Bead) { b.Metadata[keyRestartMode] = "fresh" }), ErrCorruptOp},
		{"wrong type", mutate(func(b *beads.Bead) { b.Type = "task" }), ErrCorruptOp},
		{"no op label", mutate(func(b *beads.Bead) { b.Labels = b.Labels[1:] }), ErrCorruptOp},
		{"no conversation label", mutate(func(b *beads.Bead) { b.Labels = b.Labels[:1] }), ErrCorruptOp},
		{"lease of another conversation", mutate(func(b *beads.Bead) {
			b.Assignee = Lease{Lane: "conversation", Conversation: "gc-2", Generation: 1, Nonce: "n", Deadline: t0}.String()
		}), ErrCorruptOp},
		{"lease of another lane", mutate(func(b *beads.Bead) {
			b.Assignee = Lease{Lane: "lifecycle", Conversation: "gc-1", Generation: 1, Nonce: "n", Deadline: t0}.String()
		}), ErrCorruptOp},
	}
	for _, c := range cases {
		if _, err := DecodeOp(c.bead); !errors.Is(err, c.want) {
			t.Errorf("%s: DecodeOp = %v, want %v", c.name, err, c.want)
		}
	}
}

// An attempt count that does not fit this platform's int is a corrupt row,
// never a silently wrapped count.
func TestDecodeAttemptOutsideIntIsRefused(t *testing.T) {
	good := encode(t, sampleOp(t))
	cases := []struct {
		attempt string
		fits    bool
	}{
		{strconv.Itoa(math.MaxInt), true},
		{"4294967297", strconv.IntSize == 64}, // 2^32+1 wraps to 1 in a 32-bit int
		{strconv.FormatUint(uint64(math.MaxInt)+1, 10), false},
	}
	for _, c := range cases {
		b := good
		b.Metadata = maps.Clone(good.Metadata)
		b.Metadata[keyAttempt] = c.attempt
		op, err := DecodeOp(b)
		if !c.fits {
			if !errors.Is(err, ErrCorruptOp) {
				t.Errorf("attempt %s: DecodeOp = (%d, %v), want ErrCorruptOp", c.attempt, op.Attempt, err)
			}
			continue
		}
		if err != nil || strconv.Itoa(op.Attempt) != c.attempt {
			t.Errorf("attempt %s: DecodeOp = (%d, %v), want the count back", c.attempt, op.Attempt, err)
		}
	}
}

// A vocabulary value this build does not know is a row written by a newer
// build, not a damaged one: it reads ErrUnsupportedValue, never ErrCorruptOp.
func TestDecodeUnknownVocabularyIsUnsupported(t *testing.T) {
	for _, key := range []string{keyWhen, keyWake, keyRole, keyOnUnsupported, keyGuarantee, keyScope} {
		b := encode(t, sampleOp(t))
		b.Metadata = maps.Clone(b.Metadata)
		b.Metadata[key] = "from_the_future"
		_, err := DecodeOp(b)
		if !errors.Is(err, ErrUnsupportedValue) || errors.Is(err, ErrCorruptOp) {
			t.Errorf("%s from_the_future: DecodeOp = %v, want ErrUnsupportedValue and not ErrCorruptOp", key, err)
		}
	}
	interrupt := encode(t, interruptSample(t))
	for key, v := range map[string]string{keyOnFailure: "from_the_future"} {
		b := interrupt
		b.Metadata = maps.Clone(b.Metadata)
		b.Metadata[key] = v
		if _, err := DecodeOp(b); !errors.Is(err, ErrUnsupportedValue) {
			t.Errorf("%s: DecodeOp = %v, want ErrUnsupportedValue", key, err)
		}
	}
	// A known value on the wrong kind is still damage.
	b := encode(t, sampleOp(t))
	b.Metadata = maps.Clone(b.Metadata)
	b.Metadata[keyOnFailure] = string(OnFailureRestart)
	if _, err := DecodeOp(b); !errors.Is(err, ErrCorruptOp) {
		t.Errorf("known on_failure on a message: DecodeOp = %v, want ErrCorruptOp", err)
	}
}

func interruptSample(t *testing.T) Op {
	t.Helper()
	spec, _ := Lookup(KindInterrupt)
	req := Request{
		Conversation: "gc-1", Kind: KindInterrupt, ClientKey: "stop:3:t1",
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator},
		Scope:    Scope{Kind: ScopeSession, Generation: 3, Turn: "t1"},
	}
	op, err := normalizeRequest(req, spec, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	op.ID, _ = DeriveID("gc", op.Conversation, op.Producer.Principal, op.ClientKey)
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: "operator:alice"}}
	return op
}

func TestDecodeIgnoresUnknownKeys(t *testing.T) {
	b := encode(t, sampleOp(t))
	b.Metadata["gc.op.future_field"] = "x"
	if _, err := DecodeOp(b); err != nil {
		t.Fatalf("DecodeOp with an unknown key: %v", err)
	}
}

func TestDecodeProjections(t *testing.T) {
	t.Run("external close", func(t *testing.T) {
		b := encode(t, sampleOp(t))
		b.Status = "closed"
		got, err := DecodeOp(b)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusClosed || got.Outcome != (Outcome{Arm: ArmFailed, Reason: ReasonClosedExternally}) {
			t.Fatalf("external close read %s %+v", got.Status, got.Outcome)
		}
	})
	t.Run("torn close", func(t *testing.T) {
		op := sampleOp(t)
		op.Status = StatusClosed
		op.Outcome = Outcome{Arm: ArmSucceeded}
		b := encode(t, op)
		b.Status = "open"
		got, err := DecodeOp(b)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusClosed || got.Outcome.Arm != ArmSucceeded {
			t.Fatalf("torn close read %s %+v", got.Status, got.Outcome)
		}
	})
}

func TestEncodeRefusals(t *testing.T) {
	cases := map[string]func(*Op){
		"unknown kind":        func(op *Op) { op.Kind = "teleport" },
		"schema":              func(op *Op) { op.SchemaRev = 2 },
		"id":                  func(op *Op) { op.ID = "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa" },
		"digest":              func(op *Op) { op.PayloadDigest = "" },
		"lifecycle executing": func(_ *Op) {},
		"canceled without cancel": func(op *Op) {
			op.Status = StatusClosed
			op.Outcome = Outcome{Arm: ArmCanceled}
		},
		"degraded failure": func(op *Op) {
			op.Status = StatusClosed
			op.Outcome = Outcome{Arm: ArmFailed, Reason: ReasonPoison, Degradation: "x"}
		},
		"coalesced without target": func(op *Op) {
			op.Status = StatusClosed
			op.Outcome = Outcome{Arm: ArmSkipped, Reason: ReasonCoalescedInto}
		},
		"lease of another conversation": func(op *Op) {
			op.Lease = Lease{Lane: "conversation", Conversation: "gc-2", Generation: 1, Nonce: "n", Deadline: t0}
		},
		"lease of another lane": func(op *Op) {
			op.Lease = Lease{Lane: "other", Conversation: op.Conversation, Generation: 1, Nonce: "n", Deadline: t0}
		},
		"leased lifecycle op": func(_ *Op) {},
		"too many transitions": func(op *Op) {
			for range maxTransitions {
				op.Transitions = append(op.Transitions, op.Transitions[0])
			}
		},
	}
	for name, change := range cases {
		op := sampleOp(t)
		if name == "lifecycle executing" {
			op = lifecycleSample(t)
			op.Status = StatusExecuting
		}
		if name == "leased lifecycle op" {
			op = lifecycleSample(t)
			op.Status = StatusAccepted
			op.Lease = Lease{Lane: string(LaneLifecycle), Conversation: op.Conversation, Generation: 1, Nonce: "n", Deadline: t0}
		}
		change(&op)
		if _, err := EncodeOp(op); err == nil {
			t.Errorf("%s: EncodeOp accepted %+v", name, op)
		}
	}
}

func lifecycleSample(t *testing.T) Op {
	t.Helper()
	spec, _ := Lookup(KindWake)
	req := Request{
		Conversation: "gc-1", Kind: KindWake, ClientKey: "w1",
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator},
	}
	op, err := normalizeRequest(req, spec, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	op.ID, _ = DeriveID("gc", op.Conversation, op.Producer.Principal, op.ClientKey)
	op.PayloadDigest = RequestDigest(op)
	op.Status = StatusSubmitted
	op.Transitions = []Transition{{Status: StatusSubmitted, At: op.CreatedAt, By: "operator:alice"}}
	return op
}

func TestAppendTransitionKeepsFirstAndLatest(t *testing.T) {
	var ts []Transition
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := range 40 {
		ts = appendTransition(ts, Transition{Status: StatusAccepted, At: t0.Add(time.Duration(i) * time.Second), By: "x"})
	}
	if len(ts) != maxTransitions || !ts[0].At.Equal(t0) || !ts[len(ts)-1].At.Equal(t0.Add(39*time.Second)) {
		t.Fatalf("transitions len %d first %s last %s", len(ts), ts[0].At, ts[len(ts)-1].At)
	}
}

// gc.op.executor round-trips every value, an absent key reads legacy, an
// unknown one is a newer build's (ErrUnsupportedValue), and an op with no
// valid executor is not encoded.
func TestExecutorCodec(t *testing.T) {
	for _, e := range executors {
		op := sampleOp(t)
		op.Executor = e
		got, err := DecodeOp(encode(t, op))
		if err != nil || got.Executor != e {
			t.Fatalf("%s: DecodeOp = %s, %v", e, got.Executor, err)
		}
	}
	b := encode(t, sampleOp(t))
	delete(b.Metadata, keyExecutor)
	if got, err := DecodeOp(b); err != nil || got.Executor != ExecutorLegacy {
		t.Fatalf("absent executor: DecodeOp = %q, %v; want legacy", got.Executor, err)
	}
	b.Metadata[keyExecutor] = "legacy:box"
	if _, err := DecodeOp(b); !errors.Is(err, ErrUnsupportedValue) {
		t.Fatalf("unknown executor: DecodeOp = %v, want ErrUnsupportedValue", err)
	}
	for _, e := range []ExecutorKind{"", "box"} {
		op := sampleOp(t)
		op.Executor = e
		if _, err := EncodeOp(op); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("EncodeOp(executor %q) = %v, want ErrInvalidRequest", e, err)
		}
	}
}

// Legacy ops carry no lease (DES-O0API I6): a lease on one is not encoded and
// reads as damage.
func TestLegacyOpsCarryNoLease(t *testing.T) {
	lease := Lease{Lane: "conversation", Conversation: "gc-1", Generation: 1, Nonce: "n", Deadline: time.Date(2026, 10, 13, 12, 1, 0, 0, time.UTC)}
	op := sampleOp(t)
	op.Executor = ExecutorLane
	op.Lease = lease
	b := encode(t, op)
	for _, e := range []ExecutorKind{ExecutorLegacy, ExecutorLegacyAPI, ExecutorLegacyCLI} {
		op.Executor = e
		if _, err := EncodeOp(op); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("EncodeOp(leased %s op) = %v, want ErrInvalidRequest", e, err)
		}
		b.Metadata = maps.Clone(b.Metadata)
		b.Metadata[keyExecutor] = string(e)
		if _, err := DecodeOp(b); !errors.Is(err, ErrCorruptOp) {
			t.Fatalf("DecodeOp(leased %s row) = %v, want ErrCorruptOp", e, err)
		}
	}
}

// A legacy op never leaves submitted until it closes (the sweeper's keyed
// read depends on it): one in another open status is not encoded and reads
// as damage.
func TestLegacyOpsStaySubmittedUntilClosed(t *testing.T) {
	op := sampleOp(t)
	op.Executor = ExecutorLane
	op.Status = StatusAccepted
	b := encode(t, op)
	for _, e := range []ExecutorKind{ExecutorLegacy, ExecutorLegacyAPI, ExecutorLegacyCLI} {
		for _, status := range []Status{StatusAccepted, StatusExecuting, StatusSettling} {
			op.Executor = e
			op.Status = status
			if _, err := EncodeOp(op); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("EncodeOp(%s op in %s) = %v, want ErrInvalidRequest", e, status, err)
			}
			b.Metadata = maps.Clone(b.Metadata)
			b.Metadata[keyExecutor] = string(e)
			b.Metadata[keyStatus] = string(status)
			if _, err := DecodeOp(b); !errors.Is(err, ErrCorruptOp) {
				t.Fatalf("DecodeOp(%s row in %s) = %v, want ErrCorruptOp", e, status, err)
			}
		}
	}
}

// gc.op.open carries the conversation exactly while the op is open; one
// naming another conversation is damage. Revision is decoded from the bead
// and never encoded.
func TestOpenKeyAndRevision(t *testing.T) {
	op := sampleOp(t)
	b := encode(t, op)
	if b.Metadata[keyOpen] != op.Conversation {
		t.Fatalf("open op: %s = %q", keyOpen, b.Metadata[keyOpen])
	}
	op.Revision = 41
	if again := encode(t, op); !reflect.DeepEqual(again, b) {
		t.Fatalf("Revision changed the encoding: %+v", again)
	}
	b.Revision = 7
	if got, err := DecodeOp(b); err != nil || got.Revision != 7 {
		t.Fatalf("DecodeOp revision = %d, %v; want 7", got.Revision, err)
	}
	b.Metadata[keyOpen] = "gc-2"
	if _, err := DecodeOp(b); !errors.Is(err, ErrCorruptOp) {
		t.Fatalf("DecodeOp(open for another conversation) = %v, want ErrCorruptOp", err)
	}
	op.Status = StatusClosed
	op.Outcome = Outcome{Arm: ArmSucceeded}
	if v, ok := encode(t, op).Metadata[keyOpen]; ok {
		t.Fatalf("closed op carries %s = %q", keyOpen, v)
	}
}

// arrival_seq is platform-owned: bd assigns it as a row column, and gc reads
// it from there (OJ-10a), never from gc.op.* metadata a producer could forge.
// The codec neither writes nor reads gc.op.arrival_seq, so Op.ArrivalSeq
// stays zero until OJ-10a maps the column.
func TestArrivalSeqIsNeverOpMetadata(t *testing.T) {
	const forged = "gc.op.arrival_seq"
	op := sampleOp(t)
	op.ArrivalSeq = 42
	b := encode(t, op)
	if v, ok := b.Metadata[forged]; ok {
		t.Fatalf("EncodeOp wrote %s = %q", forged, v)
	}
	b.Metadata[forged] = "7"
	got, err := DecodeOp(b)
	if err != nil {
		t.Fatalf("DecodeOp with a forged %s: %v", forged, err)
	}
	if got.ArrivalSeq != 0 {
		t.Fatalf("DecodeOp read ArrivalSeq = %d from %s, want 0", got.ArrivalSeq, forged)
	}
	b.Metadata[forged] = "not-a-number"
	if _, err := DecodeOp(b); err != nil {
		t.Fatalf("DecodeOp parsed %s: %v", forged, err)
	}
}
