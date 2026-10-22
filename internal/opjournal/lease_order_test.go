package opjournal

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestLeaseRoundTrip(t *testing.T) {
	l := Lease{
		Lane: "conversation", Conversation: "gc-1", Generation: 7, Nonce: "n0nce",
		Deadline: time.Date(2026, 10, 13, 12, 0, 0, 123456789, time.UTC),
	}
	s := l.String()
	if s != "conversation@gc-1#7#n0nce@2026-10-13T12:00:00.123456789Z" {
		t.Fatalf("String() = %q", s)
	}
	got, err := ParseLease(s)
	if err != nil || got != l {
		t.Fatalf("ParseLease(%q) = %+v, %v; want %+v", s, got, err, l)
	}
	if (Lease{}).String() != "" || !(Lease{}).IsZero() {
		t.Fatal("zero lease must render empty")
	}
}

func TestParseLeaseRefusals(t *testing.T) {
	for _, s := range []string{
		"", "alice", "conversation@gc-1#7@2026-10-13T12:00:00Z",
		"conversation@gc-1#0#n@2026-10-13T12:00:00Z",
		"conversation@gc-1#x#n@2026-10-13T12:00:00Z",
		"conversation@gc-1#7#n@yesterday",
		"@gc-1#7#n@2026-10-13T12:00:00Z",
		"conversation@gc-1#7#@2026-10-13T12:00:00Z",
		"conversation@gc 1#7#n@2026-10-13T12:00:00Z",
		"conversation@gc-1#7#n#m@2026-10-13T12:00:00Z",
		"conversation@gc-1#07#n@2026-10-13T12:00:00Z",
		"conversation@gc-1#7#n@2026-10-13T14:00:00+02:00",
	} {
		if _, err := ParseLease(s); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ParseLease(%q) = %v, want ErrInvalidRequest", s, err)
		}
	}
}

// A lease is stored as the bead's assignee, which bd caps at 255 runes. The
// bound counts runes, not bytes; a longer lease is refused on encode and reads
// back as a corrupt row.
func TestLeaseIsBoundedByTheAssigneeLimit(t *testing.T) {
	conv := strings.Repeat("é", 80) // 80 runes, 160 bytes
	lease := func(nonce int) Lease {
		return Lease{Lane: "conversation", Conversation: conv, Generation: math.MaxInt64, Nonce: strings.Repeat("n", nonce), Deadline: t0}
	}
	atLimit, over := lease(120), lease(121)
	if n := utf8.RuneCountInString(atLimit.String()); n != 255 {
		t.Fatalf("setup: lease is %d runes, want 255", n)
	}
	if got, err := ParseLease(atLimit.String()); err != nil || got != atLimit {
		t.Fatalf("ParseLease(255 runes, %d bytes) = %+v, %v; want %+v", len(atLimit.String()), got, err, atLimit)
	}
	if _, err := ParseLease(over.String()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ParseLease(256 runes) = %v, want ErrInvalidRequest", err)
	}

	op := sampleOp(t)
	op.Executor = ExecutorLane
	op.Conversation = conv
	op.ID, _ = DeriveID("gc", conv, op.Producer.Principal, op.ClientKey)
	op.PayloadDigest = RequestDigest(op)
	op.Lease = atLimit
	b := encode(t, op)
	if got, err := DecodeOp(b); err != nil || got.Lease != atLimit {
		t.Fatalf("DecodeOp(255-rune lease) = %+v, %v; want %+v", got.Lease, err, atLimit)
	}
	op.Lease = over
	if _, err := EncodeOp(op); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("EncodeOp(256-rune lease) = %v, want ErrInvalidRequest", err)
	}
	b.Assignee = over.String()
	if _, err := DecodeOp(b); !errors.Is(err, ErrCorruptOp) {
		t.Fatalf("DecodeOp(256-rune assignee) = %v, want ErrCorruptOp", err)
	}
}

func TestCompareOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	early := Op{ID: "gc-op-b", CreatedAt: t0}
	later := Op{ID: "gc-op-a", CreatedAt: t0.Add(time.Second)}
	deferred := Op{ID: "gc-op-c", CreatedAt: t0.Add(-time.Hour), NotBefore: t0.Add(time.Minute)}
	pastNotBefore := Op{ID: "gc-op-d", CreatedAt: t0.Add(2 * time.Second), NotBefore: t0.Add(-time.Hour)}
	tieA := Op{ID: "gc-op-e", CreatedAt: t0.Add(3 * time.Second)}
	tieB := Op{ID: "gc-op-f", CreatedAt: t0.Add(3 * time.Second)}

	ops := []Op{tieB, deferred, pastNotBefore, later, tieA, early}
	sortOps(ops)
	want := []string{"gc-op-b", "gc-op-a", "gc-op-d", "gc-op-e", "gc-op-f", "gc-op-c"}
	for i, id := range want {
		if ops[i].ID != id {
			t.Fatalf("order = %v, want %v", ids(ops), want)
		}
	}
	if k := OrderKey(deferred); !k.Due.Equal(deferred.NotBefore) || !k.CreatedAt.Equal(deferred.CreatedAt) {
		t.Errorf("OrderKey(deferred) = %+v", k)
	}
	if CompareOrder(early, early) != 0 {
		t.Error("CompareOrder(x, x) != 0")
	}
}

func ids(ops []Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}

func sortOps(ops []Op) { slices.SortFunc(ops, CompareOrder) }
