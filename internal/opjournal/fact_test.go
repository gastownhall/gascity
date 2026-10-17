package opjournal

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestFactIDGolden(t *testing.T) {
	cases := []struct {
		fact Fact
		want string
	}{
		{Fact{Kind: FactSessionStarted, Conversation: "gc-1", Generation: 7}, "gc-fact-4uauva72vx34yzspqkdtej7rqm"},
		{Fact{Kind: FactTurnStarted, Conversation: "gc-1", Generation: 7, Turn: "run-9:2"}, "gc-fact-r7fafbiyugkxzfjexcorvd4w5e"},
	}
	for _, c := range cases {
		got, err := FactID("gc", c.fact)
		if err != nil || got != c.want {
			t.Errorf("FactID(%+v) = %s, %v; want %s", c.fact, got, err, c.want)
		}
	}
}

func TestFactKinds(t *testing.T) {
	if n := len(FactKinds()); n != 6 {
		t.Fatalf("%d fact kinds, want 6", n)
	}
}

func sampleFacts() []Fact {
	at := time.Date(2026, 10, 13, 12, 0, 0, 42, time.UTC)
	return []Fact{
		{Kind: FactSessionStarted, Conversation: "gc-1", Generation: 7, Basis: FactBasisLane, At: at},
		{
			Kind: FactSessionEnded, Conversation: "gc-1", Generation: 7, Reason: "drained", Basis: FactBasisLane, At: at,
			CauseOps: []string{"gc-op-kdpftlql5feaypldytje3nypwy", "gc-op-ygkwx2lvlsdrq7bnvlmgvytouu"},
		},
		{
			Kind: FactTurnStarted, Conversation: "gc-1", Generation: 7, Turn: "run-9:2", At: at,
			CauseOps: []string{"gc-op-kdpftlql5feaypldytje3nypwy"}, Basis: FactBasisRuntime,
		},
		{
			Kind: FactTurnEnded, Conversation: "gc-1", Generation: 7, Turn: "run-9:2", At: at,
			Reason: "completed", Basis: FactBasisRuntime, Detail: "end_turn",
		},
		{
			Kind: FactTurnEnded, Conversation: "gc-1", Generation: 7, Turn: "run-9:3", At: at,
			Reason: "ended", Basis: FactBasisObserved,
		},
		{Kind: FactInteractionRaised, Conversation: "gc-1", Generation: 7, Interaction: "req-1", At: at, Basis: FactBasisObserved},
		{Kind: FactInteractionResolved, Conversation: "gc-1", Generation: 7, Interaction: "req-1", At: at, Basis: FactBasisRuntime},
	}
}

func TestFactRoundTrip(t *testing.T) {
	for _, f := range sampleFacts() {
		b, err := EncodeFact("gc", f)
		if err != nil {
			t.Fatalf("EncodeFact(%+v): %v", f, err)
		}
		if b.Status != "closed" || b.Type != BeadType || !b.NoHistory || b.Ephemeral {
			t.Fatalf("fact bead shape %+v", b)
		}
		for k := range b.Metadata {
			if !beadmeta.ValidKey(k) {
				t.Fatalf("invalid key %q", k)
			}
		}
		if coordclass.Classify(b) != coordclass.ClassSessions || !beads.IsReadyExcludedBead(b) || !beads.HasReadyExcludedLabel(b) {
			t.Fatalf("fact row routing: class %v ready-excluded %v by label %v", coordclass.Classify(b), beads.IsReadyExcludedBead(b), beads.HasReadyExcludedLabel(b))
		}
		got, err := DecodeFact(b)
		if err != nil {
			t.Fatalf("DecodeFact: %v", err)
		}
		want := f
		want.ID = b.ID
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestFactIdentityRefusals(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cases := map[string]Fact{
		"gen 0":                 {Kind: FactSessionStarted, Conversation: "gc-1", At: at},
		"turn fact without":     {Kind: FactTurnEnded, Conversation: "gc-1", Generation: 1, At: at},
		"session fact w/ turn":  {Kind: FactSessionEnded, Conversation: "gc-1", Generation: 1, Turn: "t", At: at},
		"interaction w/o id":    {Kind: FactInteractionRaised, Conversation: "gc-1", Generation: 1, At: at},
		"turn w/ interaction":   {Kind: FactTurnStarted, Conversation: "gc-1", Generation: 1, Turn: "t", Interaction: "r", At: at},
		"no time":               {Kind: FactSessionStarted, Conversation: "gc-1", Generation: 1},
		"bad cause":             {Kind: FactSessionStarted, Conversation: "gc-1", Generation: 1, At: at, CauseOps: []string{"gc-1"}},
		"bad conversation":      {Kind: FactSessionStarted, Conversation: "gc 1", Generation: 1, At: at},
		"wrong id for occasion": {ID: "gc-fact-aaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: FactSessionStarted, Conversation: "gc-1", Generation: 1, At: at},
		"unknown basis":         {Kind: FactSessionStarted, Conversation: "gc-1", Generation: 1, At: at, Basis: "guessed"},
		"no basis":              {Kind: FactSessionStarted, Conversation: "gc-1", Generation: 1, At: at},
		"detail not a token":    {Kind: FactTurnEnded, Conversation: "gc-1", Generation: 1, Turn: "t", At: at, Basis: FactBasisRuntime, Detail: "End Turn"},
	}
	for name, f := range cases {
		if _, err := EncodeFact("gc", f); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: EncodeFact = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := EncodeFact("gc", Fact{Kind: "turn.paused", Conversation: "gc-1", Generation: 1, At: at}); !errors.Is(err, ErrUnsupportedKind) {
		t.Errorf("unknown fact kind: %v", err)
	}
}

func TestDecodeFactRefusals(t *testing.T) {
	good, err := EncodeFact("gc", sampleFacts()[2])
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(b *beads.Bead)) beads.Bead {
		b := good
		b.Metadata = maps.Clone(good.Metadata)
		f(&b)
		return b
	}
	cases := []struct {
		name string
		bead beads.Bead
		want error
	}{
		{"unknown kind", mutate(func(b *beads.Bead) { b.Metadata[keyFactKind] = "turn.paused" }), ErrUnsupportedKind},
		{"schema 2", mutate(func(b *beads.Bead) { b.Metadata[keyFactSchema] = "2" }), ErrUnsupportedSchema},
		{"edited turn", mutate(func(b *beads.Bead) { b.Metadata[keyFactTurn] = "run-9:3" }), ErrCorruptOp},
		{"bad generation", mutate(func(b *beads.Bead) { b.Metadata[keyFactGeneration] = "x" }), ErrCorruptOp},
		{"basis not a token", mutate(func(b *beads.Bead) { b.Metadata[keyFactBasis] = "Guessed It" }), ErrCorruptOp},
		{"detail not a token", mutate(func(b *beads.Bead) { b.Metadata[keyFactDetail] = "End Turn" }), ErrCorruptOp},
		{"reserved id prefix", mutate(func(b *beads.Bead) { b.ID = "g.c" + strings.TrimPrefix(b.ID, "gc") }), ErrCorruptOp},
		{"empty id prefix", mutate(func(b *beads.Bead) { b.ID = strings.TrimPrefix(b.ID, "gc") }), ErrCorruptOp},
		{"an op row", encode(t, sampleOp(t)), ErrCorruptOp},
	}
	for _, c := range cases {
		if _, err := DecodeFact(c.bead); !errors.Is(err, c.want) {
			t.Errorf("%s: DecodeFact = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := DecodeOp(good); !errors.Is(err, ErrCorruptOp) {
		t.Errorf("DecodeOp(fact row) = %v, want ErrCorruptOp", err)
	}
}

// Rows written before the basis and detail keys existed still decode, with
// both empty, and keep their pinned ids.
func TestFactRowsWithoutBasisOrDetailDecode(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	old := beads.Bead{
		ID:     "gc-fact-r7fafbiyugkxzfjexcorvd4w5e",
		Status: "closed",
		Type:   BeadType,
		Labels: []string{LabelFact, FactConversationLabel("gc-1")},
		Metadata: map[string]string{
			keyFactSchema:       "1",
			keyFactKind:         string(FactTurnStarted),
			keyFactConversation: "gc-1",
			keyFactGeneration:   "7",
			keyFactTurn:         "run-9:2",
			keyFactAt:           formatTime(at),
		},
		NoHistory: true,
	}
	got, err := DecodeFact(old)
	if err != nil {
		t.Fatalf("DecodeFact(pre-basis row): %v", err)
	}
	want := Fact{ID: old.ID, Kind: FactTurnStarted, Conversation: "gc-1", Generation: 7, Turn: "run-9:2", At: at}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pre-basis row\n got %+v\nwant %+v", got, want)
	}
}

// Basis and detail are provenance, not identity: they never move the id.
func TestFactBasisAndDetailKeepTheID(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	plain := Fact{Kind: FactTurnEnded, Conversation: "gc-1", Generation: 7, Turn: "run-9:2", Basis: FactBasisRuntime, At: at}
	marked := plain
	marked.Basis, marked.Detail, marked.Reason = FactBasisObserved, "max_tokens", "ended"
	a, errA := EncodeFact("gc", plain)
	b, errB := EncodeFact("gc", marked)
	if errA != nil || errB != nil || a.ID != b.ID {
		t.Fatalf("ids %s (%v) and %s (%v) differ", a.ID, errA, b.ID, errB)
	}
	if b.Metadata[keyFactBasis] != "observed" || b.Metadata[keyFactDetail] != "max_tokens" {
		t.Fatalf("basis and detail keys: %v", b.Metadata)
	}
	if a.Metadata[keyFactBasis] != "runtime" {
		t.Fatalf("basis key: %v", a.Metadata)
	}
	for _, basis := range FactBases() {
		f := plain
		f.Basis = basis
		if _, err := EncodeFact("gc", f); err != nil {
			t.Errorf("basis %s: %v", basis, err)
		}
	}
}

// A basis a newer build added is provenance, not identity: the row decodes
// with the basis as read.
func TestDecodeFactKeepsANewerBasis(t *testing.T) {
	good, err := EncodeFact("gc", sampleFacts()[3])
	if err != nil {
		t.Fatal(err)
	}
	good.Metadata = maps.Clone(good.Metadata)
	good.Metadata[keyFactBasis] = "replayed"
	got, err := DecodeFact(good)
	if err != nil || got.Basis != "replayed" {
		t.Fatalf("DecodeFact(newer basis) = %+v, %v", got, err)
	}
}
