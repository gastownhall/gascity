package eventfact

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const testNonce = "0123456789abcdef0123456789abcdef"

func testFact() Fact {
	deps := []string{"step-a", "step-b"}
	return Fact{
		Session:    "gc-a3f8e",
		Generation: 3,
		Origin:     Origin{Kind: OriginProcess, Nonce: testNonce},
		N:          1,
		Event: events.Event{
			Type:             "custom.box-signal",
			Ts:               time.Date(2026, 10, 15, 12, 30, 45, 123456789, time.UTC),
			Actor:            "rig/polecat-1",
			Subject:          "gc-42",
			Message:          "line one\nline two",
			Payload:          json.RawMessage(`{"k":"v","n":1}`),
			RunID:            "gc-run-1",
			SessionID:        "gc-other",
			StepID:           "step-c",
			DependsOnStepIDs: &deps,
		},
	}
}

// sameFact compares facts with time.Time compared by instant.
func sameFact(t *testing.T, got, want Fact) {
	t.Helper()
	if !got.Event.Ts.Equal(want.Event.Ts) {
		t.Fatalf("Ts = %v, want %v", got.Event.Ts, want.Event.Ts)
	}
	got.Event.Ts, want.Event.Ts = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fact mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func TestEncodeGoldenRow(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if b.ID != "gc-ev-e6o7rior5skvdx2d5w33iaddaq" {
		t.Errorf("ID = %s", b.ID)
	}
	if b.Title != "event custom.box-signal" || b.Status != "closed" || b.Type != "op" || !b.NoHistory || b.Ephemeral {
		t.Errorf("row shape = %+v", b)
	}
	if want := []string{"gc:event-fact", "gc:event-fact-conv:gc-a3f8e"}; !slices.Equal(b.Labels, want) {
		t.Errorf("Labels = %v, want %v", b.Labels, want)
	}
	want := map[string]string{
		"gc.ev.schema":              "1",
		"gc.ev.session":             "gc-a3f8e",
		"gc.ev.gen":                 "3",
		"gc.ev.origin":              "proc:" + testNonce,
		"gc.ev.n":                   "1",
		"gc.ev.type":                "custom.box-signal",
		"gc.ev.ts":                  "2026-10-15T12:30:45.123456789Z",
		"gc.ev.actor":               "rig/polecat-1",
		"gc.ev.subject":             "gc-42",
		"gc.ev.message":             "line one\nline two",
		"gc.ev.payload":             `{"k":"v","n":1}`,
		"gc.ev.run_id":              "gc-run-1",
		"gc.ev.session_id":          "gc-other",
		"gc.ev.step_id":             "step-c",
		"gc.ev.depends_on_step_ids": `["step-a","step-b"]`,
	}
	if !maps.Equal(map[string]string(b.Metadata), want) {
		t.Errorf("Metadata =\n %v\nwant\n %v", b.Metadata, want)
	}
	for k := range b.Metadata {
		if !beadmeta.ValidKey(k) {
			t.Errorf("metadata key %q is not a valid bead-metadata key", k)
		}
	}
	for _, l := range b.Labels {
		if !beadmeta.ValidLabel(l) {
			t.Errorf("label %q is not a valid label", l)
		}
	}
	if id, err := ID("gc", testFact()); err != nil || id != b.ID {
		t.Errorf("ID = %q, %v; Encode's id is %s", id, err, b.ID)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	empty := []string{}
	minimal := Fact{
		Session: "gc-1", Generation: 1, Origin: Origin{Kind: OriginDaemon, Nonce: "abcdefgh"}, N: 7,
		Event: events.Event{Type: "mail.sent", Ts: time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))},
	}
	knownRoot := minimal
	knownRoot.Event.DependsOnStepIDs = &empty
	withSeq := testFact()
	withSeq.Event.Seq = 99 // never stored: the projector's recorder assigns it
	for name, f := range map[string]Fact{"full": testFact(), "minimal": minimal, "known root": knownRoot, "seq": withSeq} {
		t.Run(name, func(t *testing.T) {
			b, err := Encode("gc", f)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(b)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			want := f
			want.ID = b.ID
			want.Event.Seq = 0
			sameFact(t, got, want)
		})
	}
}

func TestDependsOnStepIDsKeepsUnknownApartFromRoot(t *testing.T) {
	f := testFact()
	f.Event.DependsOnStepIDs = nil
	b, _ := Encode("gc", f)
	if _, ok := b.Metadata["gc.ev.depends_on_step_ids"]; ok {
		t.Fatal("unknown topology stored a key")
	}
	if got, _ := Decode(b); got.Event.DependsOnStepIDs != nil {
		t.Fatalf("unknown topology decoded as %v", *got.Event.DependsOnStepIDs)
	}
	empty := []string{}
	f.Event.DependsOnStepIDs = &empty
	b, _ = Encode("gc", f)
	got, _ := Decode(b)
	if got.Event.DependsOnStepIDs == nil || len(*got.Event.DependsOnStepIDs) != 0 {
		t.Fatalf("known root decoded as %v", got.Event.DependsOnStepIDs)
	}
}

// The payload rule: stored compacted; over MaxPayload it is omitted and its
// size recorded, so the event still reaches the log.
func TestPayloadRule(t *testing.T) {
	body := func(n int) json.RawMessage { // a JSON string of exactly n bytes
		return json.RawMessage(`"` + strings.Repeat("x", n-2) + `"`)
	}
	f := testFact()
	f.Event.Payload = json.RawMessage("{ \"a\" : [1, 2] }")
	b, err := Encode("gc", f)
	if err != nil || b.Metadata["gc.ev.payload"] != `{"a":[1,2]}` {
		t.Fatalf("compaction: %q, %v", b.Metadata["gc.ev.payload"], err)
	}

	f.Event.Payload = body(MaxPayload)
	b, _ = Encode("gc", f)
	if got, _ := Decode(b); string(got.Event.Payload) != string(body(MaxPayload)) || got.PayloadDropped != 0 {
		t.Fatalf("a payload of exactly MaxPayload bytes was not kept")
	}

	f.Event.Payload = body(MaxPayload + 1)
	b, err = Encode("gc", f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, ok := b.Metadata["gc.ev.payload"]; ok || b.Metadata["gc.ev.payload_dropped"] != "16385" {
		t.Fatalf("oversized payload stored as %v", b.Metadata)
	}
	got, err := Decode(b)
	if err != nil || got.Event.Payload != nil || got.PayloadDropped != MaxPayload+1 {
		t.Fatalf("Decode = %+v, %v", got, err)
	}

	f.Event.Payload = json.RawMessage(`{"a":`)
	if _, err := Encode("gc", f); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a payload that is not JSON: %v", err)
	}
}

func TestEncodeRefuses(t *testing.T) {
	bad := []byte{0xff}
	cases := map[string]func(*Fact){
		"empty session":       func(f *Fact) { f.Session = "" },
		"session space":       func(f *Fact) { f.Session = "gc 1" },
		"session comma":       func(f *Fact) { f.Session = "gc,1" },
		"session quote":       func(f *Fact) { f.Session = `gc"1` },
		"session newline":     func(f *Fact) { f.Session = "gc\n1" },
		"session too long":    func(f *Fact) { f.Session = strings.Repeat("s", 201) },
		"generation 0":        func(f *Fact) { f.Generation = 0 },
		"n 0":                 func(f *Fact) { f.N = 0 },
		"origin kind":         func(f *Fact) { f.Origin.Kind = "box" },
		"origin nonce short":  func(f *Fact) { f.Origin.Nonce = "abc" },
		"origin nonce upper":  func(f *Fact) { f.Origin.Nonce = strings.ToUpper(testNonce) },
		"empty type":          func(f *Fact) { f.Event.Type = "" },
		"type space":          func(f *Fact) { f.Event.Type = "a b" },
		"type too long":       func(f *Fact) { f.Event.Type = strings.Repeat("t", 201) },
		"zero time":           func(f *Fact) { f.Event.Ts = time.Time{} },
		"time past 9999":      func(f *Fact) { f.Event.Ts = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"actor not utf8":      func(f *Fact) { f.Event.Actor = string(bad) },
		"message too long":    func(f *Fact) { f.Event.Message = strings.Repeat("m", MaxMessage+1) },
		"step id too long":    func(f *Fact) { f.Event.StepID = strings.Repeat("s", 257) },
		"depends-on not utf8": func(f *Fact) { f.Event.DependsOnStepIDs = &[]string{string(bad)} },
		"payload dropped set": func(f *Fact) { f.PayloadDropped = 1 },
		"id does not match":   func(f *Fact) { f.ID = "gc-ev-aaaaaaaaaaaaaaaaaaaaaaaaaa" },
		"depends-on over 8 KiB": func(f *Fact) {
			f.Event.DependsOnStepIDs = &[]string{strings.Repeat("s", 256)}
			*f.Event.DependsOnStepIDs = slices.Repeat(*f.Event.DependsOnStepIDs, 40)
		},
		"message invalid utf8":    func(f *Fact) { f.Event.Message = "ok" + string(bad) },
		"subject invalid utf8":    func(f *Fact) { f.Event.Subject = string(bad) },
		"run id invalid utf8":     func(f *Fact) { f.Event.RunID = string(bad) },
		"session id invalid utf8": func(f *Fact) { f.Event.SessionID = string(bad) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := testFact()
			mutate(&f)
			if _, err := Encode("gc", f); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Encode = %v, want ErrInvalid", err)
			}
		})
	}
	for _, prefix := range []string{"", "gc-", "g.c", "g c"} {
		if _, err := Encode(prefix, testFact()); !errors.Is(err, ErrInvalid) {
			t.Errorf("prefix %q: %v, want ErrInvalid", prefix, err)
		}
	}
}

func TestDecodeRefuses(t *testing.T) {
	good, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*beads.Bead){
		"wrong type":            func(b *beads.Bead) { b.Type = "task" },
		"no label":              func(b *beads.Bead) { b.Labels = []string{"gc:event-fact-conv:gc-a3f8e"} },
		"no session label":      func(b *beads.Bead) { b.Labels = []string{"gc:event-fact"} },
		"no schema":             func(b *beads.Bead) { delete(b.Metadata, "gc.ev.schema") },
		"schema not a number":   func(b *beads.Bead) { b.Metadata["gc.ev.schema"] = "one" },
		"no type":               func(b *beads.Bead) { delete(b.Metadata, "gc.ev.type") },
		"no time":               func(b *beads.Bead) { delete(b.Metadata, "gc.ev.ts") },
		"bad time":              func(b *beads.Bead) { b.Metadata["gc.ev.ts"] = "yesterday" },
		"no origin":             func(b *beads.Bead) { delete(b.Metadata, "gc.ev.origin") },
		"bad origin":            func(b *beads.Bead) { b.Metadata["gc.ev.origin"] = "box:" + testNonce },
		"non-canonical gen":     func(b *beads.Bead) { b.Metadata["gc.ev.gen"] = "03" },
		"another n":             func(b *beads.Bead) { b.Metadata["gc.ev.n"] = "2" },
		"another session":       func(b *beads.Bead) { b.Metadata["gc.ev.session"] = "gc-other" },
		"bad prefix":            func(b *beads.Bead) { b.ID = "g.c" + strings.TrimPrefix(b.ID, "gc") },
		"not an ev id":          func(b *beads.Bead) { b.ID = "gc-fact-e6o7rior5skvdx2d5w33iaddaq" },
		"payload not json":      func(b *beads.Bead) { b.Metadata["gc.ev.payload"] = "{" },
		"payload and dropped":   func(b *beads.Bead) { b.Metadata["gc.ev.payload_dropped"] = "20000" },
		"dropped under the cap": func(b *beads.Bead) { delete(b.Metadata, "gc.ev.payload"); b.Metadata["gc.ev.payload_dropped"] = "10" },
		"depends-on not array":  func(b *beads.Bead) { b.Metadata["gc.ev.depends_on_step_ids"] = `{"a":1}` },
		"depends-on null":       func(b *beads.Bead) { b.Metadata["gc.ev.depends_on_step_ids"] = "null" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := good
			b.Labels = slices.Clone(good.Labels)
			b.Metadata = maps.Clone(good.Metadata)
			mutate(&b)
			if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Decode = %v, want ErrCorrupt", err)
			}
		})
	}
	newer := good
	newer.Metadata = maps.Clone(good.Metadata)
	newer.Metadata["gc.ev.schema"] = "2"
	if _, err := Decode(newer); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("schema 2: %v, want ErrUnsupportedSchema", err)
	}
	// The status is not read: a store that creates rows open leaves a fact
	// open until its writer closes it.
	open := good
	open.Status = "open"
	if _, err := Decode(open); err != nil {
		t.Fatalf("an open fact row: %v", err)
	}
}

// Event-fact rows and the operation journal's rows never decode as each other.
func TestJournalCodecsRefuseEachOther(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opjournal.DecodeOp(b); !errors.Is(err, opjournal.ErrCorruptOp) {
		t.Errorf("DecodeOp(event fact) = %v", err)
	}
	if _, err := opjournal.DecodeFact(b); !errors.Is(err, opjournal.ErrCorruptOp) {
		t.Errorf("DecodeFact(event fact) = %v", err)
	}
	if _, err := opjournal.DecodeRuntimeRow(b); !errors.Is(err, opjournal.ErrCorruptOp) {
		t.Errorf("DecodeRuntimeRow(event fact) = %v", err)
	}
	fact, err := opjournal.EncodeFact("gc", opjournal.Fact{
		Kind: opjournal.FactSessionStarted, Conversation: "gc-a3f8e", Generation: 3,
		Basis: opjournal.FactBasisLane, At: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if IsEventFact(fact) {
		t.Error("an opjournal fact row reads as an event fact")
	}
	if _, err := Decode(fact); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Decode(opjournal fact) = %v", err)
	}
}

// A fact row is a journal row in every routing and visibility rule that
// keys on type "op": the sessions class, controller-owned, never Ready.
func TestFactRowIsAJournalRow(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	if got := coordclass.Classify(b); got != coordclass.ClassSessions {
		t.Errorf("Classify = %v, want the sessions class", got)
	}
	if !coordclass.ControllerOwned(b) {
		t.Error("an event-fact row is not ControllerOwned: the generic bead API could write one")
	}
	if !beads.IsReadyExcludedBead(b) {
		t.Error("an event-fact row is not ready-excluded")
	}
}

func TestIDsSeparateWriters(t *testing.T) {
	seen := map[string]string{}
	for name, mutate := range map[string]func(*Fact){
		"base":    func(*Fact) {},
		"n":       func(f *Fact) { f.N = 2 },
		"gen":     func(f *Fact) { f.Generation = 4 },
		"session": func(f *Fact) { f.Session = "gc-a3f8f" },
		"daemon":  func(f *Fact) { f.Origin.Kind = OriginDaemon },
		"nonce":   func(f *Fact) { f.Origin.Nonce = strings.Repeat("1", 32) },
	} {
		f := testFact()
		mutate(&f)
		id, err := ID("gc", f)
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("%s and %s share id %s", name, other, id)
		}
		seen[id] = name
	}
	// The event's content is not part of the id: a retried batch re-creates
	// the same ids whatever it carries.
	a, b := testFact(), testFact()
	b.Event.Message = "another message"
	idA, _ := ID("gc", a)
	idB, _ := ID("gc", b)
	if idA != idB {
		t.Fatalf("the id moved with the event's content: %s vs %s", idA, idB)
	}
}

func TestOrigins(t *testing.T) {
	for _, kind := range []OriginKind{OriginProcess, OriginDaemon} {
		o, err := NewOrigin(kind)
		if err != nil {
			t.Fatalf("NewOrigin(%s): %v", kind, err)
		}
		back, err := ParseOrigin(o.String())
		if err != nil || back != o {
			t.Fatalf("ParseOrigin(%s) = %+v, %v", o, back, err)
		}
		other, _ := NewOrigin(kind)
		if other == o {
			t.Fatalf("two NewOrigin calls drew %s", o)
		}
	}
	if _, err := NewOrigin("box"); !errors.Is(err, ErrInvalid) {
		t.Errorf("NewOrigin(box) = %v", err)
	}
	for _, s := range []string{"", "proc", "proc:", "box:abcdefgh", "proc:ABCDEFGH", "proc:abc", "proc:abcdefgh:x"} {
		if _, err := ParseOrigin(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseOrigin(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

// A refused prefix keeps opjournal's error in the chain.
func TestEncodeKeepsThePinnedIDError(t *testing.T) {
	_, err := Encode("gc-", testFact())
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, opjournal.ErrInvalidRequest) {
		t.Fatalf("Encode(bad prefix) = %v, want ErrInvalid wrapping opjournal.ErrInvalidRequest", err)
	}
}
