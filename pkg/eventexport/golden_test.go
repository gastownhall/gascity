package eventexport

import (
	"encoding/json"
	"testing"
)

// TestGoldenWireBytes pins the exact JSON for representative envelopes so any
// change that alters the wire bytes is caught — and, per the SchemaVersion
// contract, must bump SchemaVersion. Crucially it proves that empty
// run_id/session_id are OMITTED (byte-identical to the pre-spine wire), which is
// what lets this restructure stay at SchemaVersion 1.
func TestGoldenWireBytes(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
		want string
	}{
		{
			name: "bead.closed with ref, no run/session (empty omitted)",
			env:  Envelope{Seq: 10, Type: "bead.closed", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "mc-wisp-i6vz0e"},
			want: `{"seq":10,"type":"bead.closed","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"mc-wisp-i6vz0e"}`,
		},
		{
			name: "session.woke actor-hash only; no ref/run/session keys",
			env:  Envelope{Seq: 1, Type: "session.woke", TS: "2026-06-21T10:03:27Z", ActorHash: "abcdef0123456789"},
			want: `{"seq":1,"type":"session.woke","ts":"2026-06-21T10:03:27Z","actor_hash":"abcdef0123456789"}`,
		},
		{
			name: "mail.sent reduced to {seq,type,ts}",
			env:  Envelope{Seq: 60, Type: "mail.sent", TS: "2026-06-21T10:03:27Z"},
			want: `{"seq":60,"type":"mail.sent","ts":"2026-06-21T10:03:27Z"}`,
		},
		{
			name: "populated run_id/session_id appear after actor_hash/ref",
			env:  Envelope{Seq: 2, Type: "bead.created", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "mc-2", RunID: "wf-root-abc", SessionID: "sess-9f2a"},
			want: `{"seq":2,"type":"bead.created","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"mc-2","run_id":"wf-root-abc","session_id":"sess-9f2a"}`,
		},
	}
	for _, tc := range cases {
		out, err := json.Marshal(tc.env)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		if string(out) != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s", tc.name, out, tc.want)
		}
	}
}

// TestBatchGoldenBytes pins the batch envelope shape.
func TestBatchGoldenBytes(t *testing.T) {
	b := Batch{CityID: "maintainer-city", SchemaVersion: SchemaVersion, Events: []Envelope{
		{Seq: 1, Type: "convoy.closed", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "gcg-4216"},
	}}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"city_id":"maintainer-city","schema_version":1,"events":[{"seq":1,"type":"convoy.closed","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"gcg-4216"}]}`
	if string(out) != want {
		t.Fatalf("batch golden:\n got %s\nwant %s", out, want)
	}
}
