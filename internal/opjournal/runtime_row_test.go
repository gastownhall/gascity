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

// The vectors were computed independently of this package from the scheme:
// base32(sha256(lp("gascity/rt-id/v1") lp(conversation)))[:16 bytes].
func TestRuntimeRowIDGolden(t *testing.T) {
	cases := []struct {
		prefix, conversation, want string
	}{
		{"gc", "gc-1", "gc-rt-2jq5ygyjeimzmqfgjsndajtrmi"},
		{"gcs", "gc-1", "gcs-rt-2jq5ygyjeimzmqfgjsndajtrmi"},
		{"gc", "gc-2", "gc-rt-nyt75zymcwnps5nxgrzvkruuku"},
	}
	for _, c := range cases {
		got, err := RuntimeRowID(c.prefix, c.conversation)
		if err != nil || got != c.want {
			t.Errorf("RuntimeRowID(%q, %q) = %s, %v; want %s", c.prefix, c.conversation, got, err, c.want)
		}
	}
}

// Pairs whose text could be split at another separator still get distinct
// ids, and each decodes back to its own prefix and conversation.
func TestRuntimeRowIDSeparatorAmbiguity(t *testing.T) {
	pairs := [][2][2]string{
		{{"gc", "x-rt-y"}, {"gc-rt-x", "y"}},
		{{"gc-rt", "x"}, {"gc", "rt-x"}},
		{{"g", "c-1"}, {"gc", "1"}},
	}
	for _, pair := range pairs {
		var ids [2]string
		for i, pc := range pair {
			r := RuntimeRow{Conversation: pc[1]}
			b, err := EncodeRuntimeRow(pc[0], r)
			if err != nil {
				t.Fatalf("EncodeRuntimeRow(%q, %q): %v", pc[0], pc[1], err)
			}
			got, err := DecodeRuntimeRow(b)
			if err != nil || got.Conversation != pc[1] {
				t.Fatalf("DecodeRuntimeRow(%s) = %+v, %v", b.ID, got, err)
			}
			if p, ok := strings.CutSuffix(b.ID, runtimeIDInfix+runtimeIDSuffix(pc[1])); !ok || p != pc[0] {
				t.Fatalf("id %s does not split back to prefix %q", b.ID, pc[0])
			}
			ids[i] = b.ID
		}
		if ids[0] == ids[1] {
			t.Fatalf("pair %v shares id %s", pair, ids[0])
		}
	}
	// The scheme's tag keeps runtime ids apart from op and fact hashes of the
	// same text.
	rt := runtimeIDSuffix("gc-1")
	if rt == hash16(factIDTag, "gc-1") || rt == hash16(opIDTag, "gc-1") || rt == hash16(rtIDTag+"gc-1") {
		t.Fatal("runtime id hash collides with another scheme")
	}
}

func TestRuntimeRowIDRefusals(t *testing.T) {
	for _, c := range [][2]string{{"", "gc-1"}, {"gc-", "gc-1"}, {"g.c", "gc-1"}, {"gc", ""}, {"gc", "gc 1"}, {"gc", "gc#1"}} {
		if _, err := RuntimeRowID(c[0], c[1]); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("RuntimeRowID(%q, %q) = %v, want ErrInvalidRequest", c[0], c[1], err)
		}
	}
}

func fullRuntimeRow() RuntimeRow {
	at := time.Date(2026, 10, 13, 12, 0, 0, 7, time.UTC)
	return RuntimeRow{
		Conversation: "gc-1",
		Generation:   3,
		Epoch:        2,
		Seq:          41,
		Lease: Lease{
			Lane: "conversation", Conversation: "gc-1", Generation: 3,
			Nonce: "box.0123456789ab", Deadline: at.Add(time.Minute),
		},
		Turn: TurnCells{
			Generation: 3,
			Caps:       TurnCapsStamp{{"start", "recorded"}, {"end", "recorded"}, {"fence", "atomic"}, {"link", "exact"}, {"source", "protocol"}, {"interrupt", "turn"}, {"replace", "interrupt"}},
			ID:         "run-9:2", State: TurnStateRunning, At: at,
			LastID: "run-9:1", LastEnd: TurnEndCompleted, LastAt: at.Add(-time.Second), Seen: "run-9:1",
			ObservedAt: at.Add(time.Millisecond),
		},
		State: RuntimeReady, StateAt: at,
		Attached: AttachmentAttached, AttachedAt: at, ActivityAt: at,
		Observer: "box:b-17",
		Binding:  RuntimeBinding{Thread: "thread-a", Project: "proj-b", ProviderSession: "sess-c"},
		Host: HostStamp{
			Host: "box:b-17#n42", Generation: 3, Kinds: []Kind{KindMessage, KindInterrupt, KindRespond},
			Protocol: "rtsock/v1", Build: "gc 1.2.3 (abc123)", Since: at,
		},
		Extra: map[string]string{"gc.rt.future_cell": "x", "other.key": "y"},
	}
}

func TestRuntimeRowRoundTrip(t *testing.T) {
	for name, r := range map[string]RuntimeRow{
		"full":    fullRuntimeRow(),
		"minimal": {Conversation: "gc-1"},
		"closed":  {Conversation: "gc-1", Closed: true, Generation: 9},
		"ended": {Conversation: "gc-1", Generation: 1, Turn: TurnCells{
			Generation: 1, State: TurnStateEnded, At: t0, LastID: "t", LastEnd: "a_newer_reason", LastAt: t0, ObservedAt: t0,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := EncodeRuntimeRow("gc", r)
			if err != nil {
				t.Fatalf("EncodeRuntimeRow: %v", err)
			}
			wantStatus := "open"
			if r.Closed {
				wantStatus = "closed"
			}
			if b.Type != BeadType || !b.NoHistory || b.Ephemeral || b.Status != wantStatus || b.Assignee != "" {
				t.Fatalf("runtime row shape %+v", b)
			}
			if !reflect.DeepEqual(b.Labels, []string{LabelRuntime, RuntimeConversationLabel(r.Conversation)}) {
				t.Fatalf("labels %v", b.Labels)
			}
			for k := range b.Metadata {
				if !beadmeta.ValidKey(k) {
					t.Fatalf("invalid key %q", k)
				}
			}
			if coordclass.Classify(b) != coordclass.ClassSessions || !beads.IsReadyExcludedBead(b) {
				t.Fatalf("runtime row routing: class %v ready-excluded %v", coordclass.Classify(b), beads.IsReadyExcludedBead(b))
			}
			got, err := DecodeRuntimeRow(b)
			if err != nil {
				t.Fatalf("DecodeRuntimeRow: %v", err)
			}
			want := r
			want.ID = b.ID
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip\n got %+v\nwant %+v", got, want)
			}
			again, err := EncodeRuntimeRow("gc", got)
			if err != nil || !reflect.DeepEqual(again, b) {
				t.Fatalf("re-encode differs: %v\n got %+v\nwant %+v", err, again, b)
			}
		})
	}
}

// The key names are the wire contract other slices write against.
func TestRuntimeRowKeyNames(t *testing.T) {
	b, err := EncodeRuntimeRow("gc", fullRuntimeRow())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"gc.rt.schema":                   "1",
		"gc.rt.conversation":             "gc-1",
		"gc.rt.gen":                      "3",
		"gc.rt.epoch":                    "2",
		"gc.rt.seq":                      "41",
		"gc.rt.lease":                    "conversation@gc-1#3#box.0123456789ab@2026-10-13T12:01:00.000000007Z",
		"gc.rt.turn.caps":                "start=recorded,end=recorded,fence=atomic,link=exact,source=protocol,interrupt=turn,replace=interrupt",
		"gc.rt.turn.gen":                 "3",
		"gc.rt.turn.id":                  "run-9:2",
		"gc.rt.turn.state":               "running",
		"gc.rt.turn.at":                  "2026-10-13T12:00:00.000000007Z",
		"gc.rt.turn.last_id":             "run-9:1",
		"gc.rt.turn.last_end":            "completed",
		"gc.rt.turn.last_at":             "2026-10-13T11:59:59.000000007Z",
		"gc.rt.turn.seen":                "run-9:1",
		"gc.rt.turn.observed_at":         "2026-10-13T12:00:00.001000007Z",
		"gc.rt.state":                    "ready",
		"gc.rt.state_at":                 "2026-10-13T12:00:00.000000007Z",
		"gc.rt.attached":                 "attached",
		"gc.rt.attached_at":              "2026-10-13T12:00:00.000000007Z",
		"gc.rt.activity_at":              "2026-10-13T12:00:00.000000007Z",
		"gc.rt.observer":                 "box:b-17",
		"gc.rt.binding.thread":           "thread-a",
		"gc.rt.binding.project":          "proj-b",
		"gc.rt.binding.provider_session": "sess-c",
		"gc.rt.host":                     "box:b-17#n42",
		"gc.rt.host_gen":                 "3",
		"gc.rt.host_kinds":               "message,interrupt,respond",
		"gc.rt.host_protocol":            "rtsock/v1",
		"gc.rt.host_build":               "gc 1.2.3 (abc123)",
		"gc.rt.host_since":               "2026-10-13T12:00:00.000000007Z",
		"gc.rt.future_cell":              "x",
		"other.key":                      "y",
	}
	if !reflect.DeepEqual(b.Metadata, want) {
		for k, v := range b.Metadata {
			if want[k] != v {
				t.Errorf("key %s = %q, want %q", k, v, want[k])
			}
		}
		for k := range want {
			if _, ok := b.Metadata[k]; !ok {
				t.Errorf("key %s missing", k)
			}
		}
	}
}

func TestEncodeRuntimeRowRefusals(t *testing.T) {
	mut := func(f func(r *RuntimeRow)) RuntimeRow {
		r := fullRuntimeRow()
		r.Extra = maps.Clone(r.Extra)
		f(&r)
		return r
	}
	cases := map[string]RuntimeRow{
		"bad conversation":       mut(func(r *RuntimeRow) { r.Conversation = "gc 1" }),
		"wrong id":               mut(func(r *RuntimeRow) { r.ID = "gc-rt-aaaaaaaaaaaaaaaaaaaaaaaaaa" }),
		"negative generation":    mut(func(r *RuntimeRow) { r.Generation = -1 }),
		"lease of another conv":  mut(func(r *RuntimeRow) { r.Lease.Conversation = "gc-2" }),
		"invalid lease":          mut(func(r *RuntimeRow) { r.Lease.Nonce = "" }),
		"running with no id":     mut(func(r *RuntimeRow) { r.Turn.ID = "" }),
		"ended with an id":       mut(func(r *RuntimeRow) { r.Turn.State = TurnStateEnded }),
		"unknown turn state":     mut(func(r *RuntimeRow) { r.Turn.State = "paused" }),
		"reason not a token":     mut(func(r *RuntimeRow) { r.Turn.LastEnd = "Completed" }),
		"seen turn not text":     mut(func(r *RuntimeRow) { r.Turn.Seen = " run-9:1" }),
		"caps key not a token":   mut(func(r *RuntimeRow) { r.Turn.Caps = TurnCapsStamp{{"Start", "recorded"}} }),
		"caps value empty":       mut(func(r *RuntimeRow) { r.Turn.Caps = TurnCapsStamp{{"start", ""}} }),
		"caps key twice":         mut(func(r *RuntimeRow) { r.Turn.Caps = TurnCapsStamp{{"start", "a"}, {"start", "b"}} }),
		"unknown runtime state":  mut(func(r *RuntimeRow) { r.State = "sleeping" }),
		"unknown attachment":     mut(func(r *RuntimeRow) { r.Attached = "maybe" }),
		"observer with newline":  mut(func(r *RuntimeRow) { r.Observer = "box:\n" }),
		"binding with space pad": mut(func(r *RuntimeRow) { r.Binding.Thread = " t" }),
		"host kind not a lane's": mut(func(r *RuntimeRow) { r.Host.Kinds = []Kind{KindWake} }),
		"host kind twice":        mut(func(r *RuntimeRow) { r.Host.Kinds = []Kind{KindMessage, KindMessage} }),
		"host protocol":          mut(func(r *RuntimeRow) { r.Host.Protocol = "RT Sock" }),
		"extra owns a key":       mut(func(r *RuntimeRow) { r.Extra["gc.rt.seq"] = "9" }),
		"extra empty value":      mut(func(r *RuntimeRow) { r.Extra["gc.rt.future_cell"] = "" }),
	}
	for name, r := range cases {
		if _, err := EncodeRuntimeRow("gc", r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: EncodeRuntimeRow = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := EncodeRuntimeRow("gc", mut(func(r *RuntimeRow) { r.Host.Kinds = []Kind{"poke"} })); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown host kind: %v", err)
	}
	if _, err := EncodeRuntimeRow("g.c", fullRuntimeRow()); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("bad prefix: %v", err)
	}
}

func TestDecodeRuntimeRowRefusals(t *testing.T) {
	good, err := EncodeRuntimeRow("gc", fullRuntimeRow())
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(b *beads.Bead)) beads.Bead {
		b := good
		b.Metadata = maps.Clone(good.Metadata)
		b.Labels = append([]string(nil), good.Labels...)
		f(&b)
		return b
	}
	fact, err := EncodeFact("gc", sampleFacts()[0])
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		bead beads.Bead
		want error
	}{
		{"schema 2", mutate(func(b *beads.Bead) { b.Metadata[keyRTSchema] = "2" }), ErrUnsupportedSchema},
		{"no schema", mutate(func(b *beads.Bead) { delete(b.Metadata, keyRTSchema) }), ErrCorruptOp},
		{"turn state not a token", mutate(func(b *beads.Bead) { b.Metadata[keyRTTurnState] = "Paused now" }), ErrCorruptOp},
		{"runtime state not a token", mutate(func(b *beads.Bead) { b.Metadata[keyRTState] = "Sleeping" }), ErrCorruptOp},
		{"attachment not a token", mutate(func(b *beads.Bead) { b.Metadata[keyRTAttached] = "may be" }), ErrCorruptOp},
		{"host kind not a token", mutate(func(b *beads.Bead) { b.Metadata[keyRTHostKinds] = "message,Poke" }), ErrCorruptOp},
		{"host kind of another lane", mutate(func(b *beads.Bead) { b.Metadata[keyRTHostKinds] = "message,wake" }), ErrCorruptOp},
		{"edited conversation", mutate(func(b *beads.Bead) { b.Metadata[keyRTConversation] = "gc-2" }), ErrCorruptOp},
		{"missing conversation label", mutate(func(b *beads.Bead) { b.Labels = []string{LabelRuntime} }), ErrCorruptOp},
		{"bad seq", mutate(func(b *beads.Bead) { b.Metadata[keyRTSeq] = "-1" }), ErrCorruptOp},
		{"bad lease", mutate(func(b *beads.Bead) { b.Metadata[keyRTLease] = "nope" }), ErrCorruptOp},
		{"bad caps", mutate(func(b *beads.Bead) { b.Metadata[keyRTTurnCaps] = "start" }), ErrCorruptOp},
		{"bad time", mutate(func(b *beads.Bead) { b.Metadata[keyRTStateAt] = "noon" }), ErrCorruptOp},
		{"reserved id prefix", mutate(func(b *beads.Bead) { b.ID = "g.c" + strings.TrimPrefix(b.ID, "gc") }), ErrCorruptOp},
		{"a fact row", fact, ErrCorruptOp},
		{"an op row", encode(t, sampleOp(t)), ErrCorruptOp},
	}
	for _, c := range cases {
		if _, err := DecodeRuntimeRow(c.bead); !errors.Is(err, c.want) {
			t.Errorf("%s: DecodeRuntimeRow = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := DecodeOp(good); !errors.Is(err, ErrCorruptOp) {
		t.Errorf("DecodeOp(runtime row) = %v, want ErrCorruptOp", err)
	}
	if _, err := DecodeFact(good); !errors.Is(err, ErrCorruptOp) {
		t.Errorf("DecodeFact(runtime row) = %v, want ErrCorruptOp", err)
	}
}

// A patch writes only the owned keys that changed, blanks the ones next
// drops, and never touches a key this build does not own.
func TestRuntimeRowPatchPreservesUnknownKeys(t *testing.T) {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	row, err := EncodeRuntimeRow("gc", fullRuntimeRow())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Create(row); err != nil {
		t.Fatal(err)
	}
	stored, _ := mem.Get(row.ID)
	cur, err := DecodeRuntimeRow(stored)
	if err != nil {
		t.Fatal(err)
	}

	if opts, err := RuntimeRowPatch(stored, cur); err != nil || len(opts.Metadata) != 0 || opts.Status != nil {
		t.Fatalf("no-change patch = %+v, %v", opts, err)
	}

	// A next built from scratch, not decoded from the row, writes empty over
	// every owned cell it leaves zero, a newer build's value included.
	newer := stored
	newer.Metadata = maps.Clone(stored.Metadata)
	newer.Metadata[keyRTState] = "sleeping"
	fresh, err := RuntimeRowPatch(newer, RuntimeRow{Conversation: cur.Conversation, Seq: cur.Seq + 1})
	if err != nil {
		t.Fatalf("fresh-next patch: %v", err)
	}
	for _, k := range []string{keyRTState, keyRTTurnState, keyRTObserver, keyRTLease, keyRTHostKinds} {
		if v, ok := fresh.Metadata[k]; !ok || v != "" {
			t.Fatalf("fresh-next patch leaves %s = %q (written %v), want it written empty", k, v, ok)
		}
	}
	if v := fresh.Metadata[keyRTSeq]; v != formatUint(cur.Seq+1) {
		t.Fatalf("fresh-next patch writes seq %q", v)
	}
	for k := range fresh.Metadata {
		if !strings.HasPrefix(k, "gc.rt.") || k == "gc.rt.future_cell" {
			t.Fatalf("fresh-next patch writes key %s it does not own", k)
		}
	}

	next := cur
	next.Extra = nil // a writer's view of the extras never reaches the store
	next.Lease = Lease{}
	next.Seq = 42
	next.Turn, _, _ = FoldTurn(cur.Turn, TurnObservation{
		Conversation: "gc-1", Generation: 3, ObservedAt: t0.Add(time.Hour),
		Recent: []ObservedTurnEnd{{ID: "run-9:2", EndedAt: t0.Add(time.Hour), Status: TurnSourceCompleted}},
	}, InterruptRef{})
	opts, err := RuntimeRowPatch(stored, next)
	if err != nil {
		t.Fatalf("RuntimeRowPatch: %v", err)
	}
	for k := range opts.Metadata {
		if !strings.HasPrefix(k, "gc.rt.") || k == "gc.rt.future_cell" {
			t.Fatalf("patch writes key %s it does not own", k)
		}
	}
	if v, ok := opts.Metadata[keyRTLease]; !ok || v != "" {
		t.Fatalf("a dropped lease is not blanked: %v", opts.Metadata)
	}
	if v, ok := opts.Metadata[keyRTTurnID]; !ok || v != "" {
		t.Fatalf("an ended turn's id is not blanked: %v", opts.Metadata)
	}
	if _, ok := opts.Metadata[keyRTObserver]; ok {
		t.Fatalf("an unchanged key is written: %v", opts.Metadata)
	}
	if err := mem.UpdateIfMatch(row.ID, stored.Revision, opts); err != nil {
		t.Fatal(err)
	}
	after, _ := mem.Get(row.ID)
	got, err := DecodeRuntimeRow(after)
	if err != nil {
		t.Fatalf("decode after patch: %v", err)
	}
	want := next
	want.Extra = cur.Extra
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after patch\n got %+v\nwant %+v", got, want)
	}

	closing := got
	closing.Closed = true
	opts, err = RuntimeRowPatch(after, closing)
	if err != nil || opts.Status == nil || *opts.Status != "closed" || len(opts.Metadata) != 0 {
		t.Fatalf("closing patch = %+v, %v", opts, err)
	}
}

func TestRuntimeRowPatchRefusals(t *testing.T) {
	row, err := EncodeRuntimeRow("gc", fullRuntimeRow())
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := DecodeRuntimeRow(row)
	other := cur
	other.Conversation = "gc-2"
	if _, err := RuntimeRowPatch(row, other); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("patch to another conversation = %v, want ErrInvalidRequest", err)
	}
	reopen := cur
	closed := row
	closed.Status = "closed"
	if _, err := RuntimeRowPatch(closed, reopen); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("patch reopening a closed row = %v, want ErrInvalidRequest", err)
	}
	// Text an update would not store as a string is refused before any write.
	for _, v := range []string{"42", "null", "true", "1e3"} {
		n := cur
		n.Turn.ID = v
		if _, err := RuntimeRowPatch(row, n); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("patch with turn id %q = %v, want ErrInvalidRequest", v, err)
		}
	}
	bad := row
	bad.Metadata = maps.Clone(row.Metadata)
	bad.Metadata[keyRTSchema] = "2"
	if _, err := RuntimeRowPatch(bad, cur); !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("patch over a newer row = %v, want ErrUnsupportedSchema", err)
	}
}

// An observation value a newer build writes (a turn or runtime state, an
// attachment, a host kind) decodes as read, so the lane cells on the same row
// stay writable by this build; a patch never writes such a value itself.
func TestRuntimeRowKeepsNewerObservationValues(t *testing.T) {
	good, err := EncodeRuntimeRow("gc", fullRuntimeRow())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, key, value string
		read             func(RuntimeRow) string
		write            func(*RuntimeRow, string)
	}{
		{
			"turn state", keyRTTurnState, "paused",
			func(r RuntimeRow) string { return string(r.Turn.State) },
			func(r *RuntimeRow, v string) { r.Turn.State = TurnState(v) },
		},
		{
			"runtime state", keyRTState, "sleeping",
			func(r RuntimeRow) string { return string(r.State) },
			func(r *RuntimeRow, v string) { r.State = RuntimeState(v) },
		},
		{
			"attachment", keyRTAttached, "shared",
			func(r RuntimeRow) string { return string(r.Attached) },
			func(r *RuntimeRow, v string) { r.Attached = Attachment(v) },
		},
		{
			"host kind", keyRTHostKinds, "message,poke",
			func(r RuntimeRow) string { return joinKinds(r.Host.Kinds) },
			func(r *RuntimeRow, v string) {
				r.Host.Kinds = nil
				for _, k := range strings.Split(v, ",") {
					r.Host.Kinds = append(r.Host.Kinds, Kind(k))
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := good
			b.Metadata = maps.Clone(good.Metadata)
			b.Metadata[c.key] = c.value
			cur, err := DecodeRuntimeRow(b)
			if err != nil {
				t.Fatalf("DecodeRuntimeRow: %v", err)
			}
			if got := c.read(cur); got != c.value {
				t.Fatalf("%s = %q, want %q kept as read", c.key, got, c.value)
			}

			next := cur
			next.Seq++
			opts, err := RuntimeRowPatch(b, next)
			if err != nil {
				t.Fatalf("a lane-cell patch over a newer value: %v", err)
			}
			if len(opts.Metadata) != 1 || opts.Metadata[keyRTSeq] != formatUint(next.Seq) {
				t.Fatalf("patch = %v, want only %s", opts.Metadata, keyRTSeq)
			}

			other := cur
			c.write(&other, "newer_still")
			if _, err := RuntimeRowPatch(b, other); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("a patch writing a value this build does not know = %v, want a refusal", err)
			}
			if _, err := EncodeRuntimeRow("gc", cur); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrUnknownKind) {
				t.Fatalf("encoding a value this build does not know = %v, want a refusal", err)
			}
		})
	}
}
