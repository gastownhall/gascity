package eventfact

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
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
		"empty session":      func(f *Fact) { f.Session = "" },
		"session space":      func(f *Fact) { f.Session = "gc 1" },
		"session comma":      func(f *Fact) { f.Session = "gc,1" },
		"session quote":      func(f *Fact) { f.Session = `gc"1` },
		"session newline":    func(f *Fact) { f.Session = "gc\n1" },
		"session too long":   func(f *Fact) { f.Session = strings.Repeat("s", 201) },
		"generation 0":       func(f *Fact) { f.Generation = 0 },
		"n 0":                func(f *Fact) { f.N = 0 },
		"origin kind":        func(f *Fact) { f.Origin.Kind = "box" },
		"origin nonce short": func(f *Fact) { f.Origin.Nonce = "abc" },
		"origin nonce upper": func(f *Fact) { f.Origin.Nonce = strings.ToUpper(testNonce) },
		"empty type":         func(f *Fact) { f.Event.Type = "" },
		"type space":         func(f *Fact) { f.Event.Type = "a b" },
		"type too long":      func(f *Fact) { f.Event.Type = strings.Repeat("t", 201) },
		"controller type":    func(f *Fact) { f.Event.Type = events.ExecutionStepCompleted },
		"ledger type":        func(f *Fact) { f.Event.Type = events.BeadCreated },
		"zero time":          func(f *Fact) { f.Event.Ts = time.Time{} },
		"time past 9999":     func(f *Fact) { f.Event.Ts = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		// The year is checked in UTC, the zone the row stores.
		"zoned time past 9999 in utc": func(f *Fact) {
			f.Event.Ts = time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("", -5*3600))
		},
		"zoned time before year 0 in utc": func(f *Fact) {
			f.Event.Ts = time.Date(0, 1, 1, 1, 0, 0, 0, time.FixedZone("", 5*3600))
		},
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
		// json.Compact keeps invalid UTF-8 inside a string, and the store's
		// JSON would rewrite it to U+FFFD, so the row could not come back as
		// written.
		"payload invalid utf8": func(f *Fact) { f.Event.Payload = json.RawMessage("{\"k\":\"" + string(bad) + "\"}") },
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
		"non-canonical n":       func(b *beads.Bead) { b.Metadata["gc.ev.n"] = "01" },
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

// rehash re-derives b's id and session label from its own identity metadata,
// so a refusal test reaches the check it targets instead of failing on the id.
func rehash(t *testing.T, b *beads.Bead) {
	t.Helper()
	m := b.Metadata
	id, err := opjournal.PinnedID("gc", idFamily, idTag, m[keySession], m[keyGen], m[keyOrigin], m[keyN])
	if err != nil {
		t.Fatalf("rehash: %v", err)
	}
	b.ID = id
	b.Labels = []string{Label, SessionLabel(m[keySession])}
}

func cloneRow(b beads.Bead) beads.Bead {
	b.Labels = slices.Clone(b.Labels)
	b.Metadata = maps.Clone(b.Metadata)
	return b
}

// Decode is the projector's trust boundary: a box can write a fact row
// without Encode, so Decode refuses every row outside schema 1's limits,
// with the id re-derived so that only the targeted check can refuse it.
func TestDecodeRefusesWhatEncodeRefuses(t *testing.T) {
	good, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	set := func(key, value string) func(map[string]string) {
		return func(m map[string]string) { m[key] = value }
	}
	del := func(key string) func(map[string]string) {
		return func(m map[string]string) { delete(m, key) }
	}
	depsJSON := func(ids []string) string {
		data, _ := json.Marshal(ids)
		return string(data)
	}
	cases := map[string]func(map[string]string){
		"payload over MaxPayload":   set(keyPayload, `"`+strings.Repeat("x", MaxPayload-1)+`"`),
		"payload not compacted":     set(keyPayload, `{ "k": "v" }`),
		"payload not utf8":          set(keyPayload, "{\"k\":\"\xff\"}"),
		"gen absent":                del(keyGen),
		"gen 0":                     set(keyGen, "0"),
		"gen negative":              set(keyGen, "-1"),
		"n absent":                  del(keyN),
		"n 0":                       set(keyN, "0"),
		"session over limit":        set(keySession, strings.Repeat("s", maxSessionLen+1)),
		"type over limit":           set(keyType, strings.Repeat("t", maxTypeLen+1)),
		"type with a space":         set(keyType, "a b"),
		"type with a tab":           set(keyType, "a\tb"),
		"controller type":           set(keyType, events.ExecutionStepCompleted),
		"ledger type":               set(keyType, events.BeadCreated),
		"actor over limit":          set(keyActor, strings.Repeat("a", maxActorLen+1)),
		"subject over limit":        set(keySubject, strings.Repeat("s", maxSubjectLen+1)),
		"message over limit":        set(keyMessage, strings.Repeat("m", MaxMessage+1)),
		"run_id over limit":         set(keyRunID, strings.Repeat("r", maxIDFieldLen+1)),
		"session_id over limit":     set(keySessionID, strings.Repeat("s", maxIDFieldLen+1)),
		"step_id over limit":        set(keyStepID, strings.Repeat("s", maxIDFieldLen+1)),
		"depends-on over 8 KiB":     set(keyDependsOnStepIDs, depsJSON(slices.Repeat([]string{strings.Repeat("s", maxIDFieldLen)}, 40))),
		"depends-on entry too long": set(keyDependsOnStepIDs, depsJSON([]string{strings.Repeat("s", maxIDFieldLen+1)})),
		"depends-on not compacted":  set(keyDependsOnStepIDs, `[ "step-a" ]`),
		"depends-on not utf8":       set(keyDependsOnStepIDs, "[\"\xff\"]"),
		"ts with an offset":         set(keyTs, "2026-10-15T14:30:45.123456789+02:00"),
		"ts with trailing zeros":    set(keyTs, "2026-10-15T12:30:45.100Z"),
		"ts the zero time":          set(keyTs, "0001-01-01T00:00:00Z"),
		"schema 0":                  set(keySchema, "0"),
		"schema non-canonical":      set(keySchema, "01"),
		"dropped non-canonical":     func(m map[string]string) { delete(m, keyPayload); m[keyPayloadDropped] = "016385" },
		"dropped 0":                 func(m map[string]string) { delete(m, keyPayload); m[keyPayloadDropped] = "0" },
		"dropped negative":          func(m map[string]string) { delete(m, keyPayload); m[keyPayloadDropped] = "-20000" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := cloneRow(good)
			mutate(b.Metadata)
			rehash(t, &b)
			if _, err := Decode(b); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Decode = %v, want ErrCorrupt", err)
			}
		})
	}
}

// A fact carries only a type a worker site may author. A box can write a row
// without the recorder (D-TRUST), so the codec, not only the recorder, keeps a
// forged controller observation such as a step completion, or a copy of a
// ledger-derived bead event, out of the city's log. For every type this build
// declares, Encode and Decode accept it exactly when events.AuthorshipOf calls
// it Worker.
func TestCodecAcceptsOnlyWorkerAuthoredTypes(t *testing.T) {
	good, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	var accepted, refused int
	for _, eventType := range events.KnownEventTypes {
		f := testFact()
		f.Event.Type = eventType
		_, encodeErr := Encode("gc", f)
		b := cloneRow(good)
		b.Metadata[keyType] = eventType
		_, decodeErr := Decode(b)
		if a := events.AuthorshipOf(eventType); a != events.AuthorWorker {
			refused++
			if !errors.Is(encodeErr, ErrInvalid) || !errors.Is(decodeErr, ErrCorrupt) {
				t.Errorf("%s is %s-authored: Encode = %v, Decode = %v; want ErrInvalid and ErrCorrupt", eventType, a, encodeErr, decodeErr)
			}
			continue
		}
		accepted++
		if encodeErr != nil || decodeErr != nil {
			t.Errorf("%s is worker-authored: Encode = %v, Decode = %v; want both to accept it", eventType, encodeErr, decodeErr)
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("%d types accepted and %d refused; the registry must yield both", accepted, refused)
	}
}

// A dropped size that does not fit this platform's int is a corrupt row,
// never a silently wrapped size.
func TestDecodePayloadDroppedOutsideIntIsRefused(t *testing.T) {
	big := testFact()
	big.Event.Payload = json.RawMessage(`"` + strings.Repeat("x", MaxPayload) + `"`)
	good, err := Encode("gc", big)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		dropped string
		fits    bool
	}{
		{strconv.Itoa(math.MaxInt), true},
		{"4294987296", strconv.IntSize == 64}, // 2^32+20000 wraps to 20000, a size over MaxPayload, in a 32-bit int
		{strconv.FormatUint(uint64(math.MaxInt)+1, 10), false},
	}
	for _, c := range cases {
		b := cloneRow(good)
		b.Metadata[keyPayloadDropped] = c.dropped
		f, err := Decode(b)
		if !c.fits {
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("dropped %s: Decode = (%d, %v), want ErrCorrupt", c.dropped, f.PayloadDropped, err)
			}
			continue
		}
		if err != nil || strconv.Itoa(f.PayloadDropped) != c.dropped {
			t.Errorf("dropped %s: Decode = (%d, %v), want the size back", c.dropped, f.PayloadDropped, err)
		}
	}
}

// Fact rows are schema 1. A later revision is ErrUnsupportedSchema, so a
// projector older than its writers refuses the row, never skipping it
// silently.
func TestDecodeSchemaRevisions(t *testing.T) {
	good, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	if good.Metadata[keySchema] != "1" || FactSchemaRev != 1 {
		t.Fatalf("schema = %q, FactSchemaRev = %d", good.Metadata[keySchema], FactSchemaRev)
	}
	newer := cloneRow(good)
	newer.Metadata[keySchema] = "2"
	if _, err := Decode(newer); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("schema 2: %v, want ErrUnsupportedSchema", err)
	}
}

// A gc.ev.* key this build does not know is ignored: a new optional field is
// additive within schema 1, so a projector older than its writers still
// records the event, without the field (doc.go, "Schema evolution").
func TestDecodeIgnoresUnknownKeys(t *testing.T) {
	b, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	b.Metadata["gc.ev.a_later_field"] = "x"
	b.Metadata["gc.other"] = "y"
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := testFact()
	want.ID = b.ID
	sameFact(t, got, want)
}

// Every events.Event field except Seq has a gc.ev.<json tag> key, and a fact
// that sets every field round-trips. A field added to events.Event fails here
// until testFact sets it and the codec stores it.
func TestCodecCoversEveryEventField(t *testing.T) {
	f := testFact()
	ev := reflect.ValueOf(f.Event)
	b, err := Encode("gc", f)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ev.NumField() {
		field := ev.Type().Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Fatalf("events.Event.%s has no JSON tag; decide its gc.ev key", field.Name)
		}
		if tag == "seq" {
			continue
		}
		if ev.Field(i).IsZero() {
			t.Errorf("testFact leaves events.Event.%s unset; set it so the codec is checked to carry it", field.Name)
			continue
		}
		if _, ok := b.Metadata["gc.ev."+tag]; !ok {
			t.Errorf("events.Event.%s is not stored as gc.ev.%s", field.Name, tag)
		}
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	f.ID = b.ID
	sameFact(t, got, f)
}

// factKeys is every key a fact row carries under schema 1.
var factKeys = []string{
	keySchema, keySession, keyGen, keyOrigin, keyN,
	keyType, keyTs, keyActor, keySubject, keyMessage, keyPayload, keyPayloadDropped,
	keyRunID, keySessionID, keyStepID, keyDependsOnStepIDs,
}

// assertReencodes is the structural guard on Decode: a row it accepts is a
// row Encode writes. Its fact re-encodes, and the re-encoded row carries the
// same value under every key this build knows.
func assertReencodes(t *testing.T, b beads.Bead, f Fact) {
	t.Helper()
	prefix := b.ID[:strings.LastIndex(b.ID, "-"+idFamily+"-")]
	dropped := f.PayloadDropped
	f.PayloadDropped = 0
	again, err := Encode(prefix, f)
	if err != nil {
		t.Fatalf("Decode accepted a row whose fact Encode refuses: %v\nrow: %v", err, b.Metadata)
	}
	if again.ID != b.ID {
		t.Fatalf("re-encoded id %s, row id %s", again.ID, b.ID)
	}
	for _, key := range factKeys {
		want := b.Metadata[key]
		if key == keyPayloadDropped && dropped > 0 {
			continue // the re-encode has no payload to drop
		}
		if got := again.Metadata[key]; got != want {
			t.Errorf("%s: re-encoded %q, row %q", key, got, want)
		}
	}
}

// Every row Decode accepts, from every accepting case in this file and from
// the seed corpus of FuzzDecode, re-encodes.
func TestDecodeAcceptsOnlyEncodableRows(t *testing.T) {
	for name, b := range acceptedRows(t) {
		t.Run(name, func(t *testing.T) {
			f, err := Decode(b)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			assertReencodes(t, b, f)
		})
	}
}

func acceptedRows(t *testing.T) map[string]beads.Bead {
	t.Helper()
	out := map[string]beads.Bead{}
	full, err := Encode("gc", testFact())
	if err != nil {
		t.Fatal(err)
	}
	out["full"] = full
	minimal := Fact{
		Session: "gc-1", Generation: 1, Origin: Origin{Kind: OriginDaemon, Nonce: "abcdefgh"}, N: 7,
		Event: events.Event{Type: "mail.sent", Ts: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	if out["minimal"], err = Encode("my-city", minimal); err != nil {
		t.Fatal(err)
	}
	big := testFact()
	big.Event.Payload = json.RawMessage(`"` + strings.Repeat("x", MaxPayload) + `"`)
	if out["payload dropped"], err = Encode("gc", big); err != nil {
		t.Fatal(err)
	}
	unknown := cloneRow(full)
	unknown.Metadata["gc.ev.a_later_field"] = "x"
	out["unknown key"] = unknown
	scalar := cloneRow(full)
	scalar.Metadata[keyPayload] = `1`
	out["scalar payload"] = scalar
	return out
}

// FuzzDecode sets one metadata key of a valid row to an arbitrary value, with
// the id re-derived; whatever Decode accepts must re-encode.
func FuzzDecode(f *testing.F) {
	for _, seed := range [][2]string{
		{keyPayload, `{ "k": "v" }`},
		{keyPayload, "{\"k\":\"\xff\"}"},
		{keyGen, "0"},
		{keyN, ""},
		{keyTs, "2026-10-15T14:30:45+02:00"},
		{keyDependsOnStepIDs, `[ "a" ]`},
		{keyType, "a b"},
		{keyType, events.ExecutionStepCompleted},
		{keyActor, strings.Repeat("a", maxActorLen+1)},
		{keyMessage, "\xff"},
		{keySchema, "01"},
		{keyPayloadDropped, "16385"},
		{keyPayloadDropped, "0"},
		{"gc.ev.a_later_field", "x"},
	} {
		f.Add(seed[0], seed[1])
	}
	good, err := Encode("gc", testFact())
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, key, value string) {
		b := cloneRow(good)
		if value == "" {
			delete(b.Metadata, key)
		} else {
			b.Metadata[key] = value
		}
		if _, err := opjournal.PinnedID("gc", idFamily, idTag, b.Metadata[keySession], b.Metadata[keyGen], b.Metadata[keyOrigin], b.Metadata[keyN]); err != nil {
			t.Skip()
		}
		rehash(t, &b)
		fact, err := Decode(b)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf("Decode error %v is neither ErrCorrupt nor ErrUnsupportedSchema", err)
			}
			return
		}
		assertReencodes(t, b, fact)
	})
}
