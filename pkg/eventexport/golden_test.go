package eventexport

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// TestGoldenWireBytes pins the exact JSON for representative envelopes so any
// change that alters the wire bytes is caught — and, per the SchemaVersion
// contract, must bump SchemaVersion. Crucially it proves that empty
// run_id/session_id are OMITTED (byte-identical to the pre-spine wire), so
// adding that correlation spine did not by itself change the envelope bytes.
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
			name: "populated run_id/session_id/step_id appear after actor_hash/ref",
			env:  Envelope{Seq: 2, Type: "bead.created", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "mc-2", RunID: "wf-root-abc", SessionID: "sess-9f2a", StepID: "mc-step-7"},
			want: `{"seq":2,"type":"bead.created","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"mc-2","run_id":"wf-root-abc","session_id":"sess-9f2a","step_id":"mc-step-7"}`,
		},
		{
			// The content opt-in path: free-form title/formula serialize verbatim
			// after step_id. Pinning this anchors the off-by-default exemption — the
			// empty-field cases above prove the DEFAULT wire is byte-identical to the
			// pre-content shape, while this case fixes the opt-in wire shape so the
			// receiver-ready contract cannot drift silently.
			name: "content opt-in: title/formula appear after step_id",
			env:  Envelope{Seq: 3, Type: "bead.closed", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "mc-3", Title: "ESCALATION: spike [HIGH]", Formula: "randy-triage-patrol"},
			want: `{"seq":3,"type":"bead.closed","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"mc-3","title":"ESCALATION: spike [HIGH]","formula":"randy-triage-patrol"}`,
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

// TestBatchGoldenBytes pins the batch envelope shape: an opaque city_hash (never
// a cleartext city name) and schema_version 2.
func TestBatchGoldenBytes(t *testing.T) {
	b := Batch{CityHash: "7f3a9c1e5b2d4068", SchemaVersion: SchemaVersion, Events: []Envelope{
		{Seq: 1, Type: "convoy.closed", TS: "2026-06-21T10:03:27Z", ActorHash: "0123456789abcdef", Ref: "gcg-4216"},
	}}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"city_hash":"7f3a9c1e5b2d4068","schema_version":2,"events":[{"seq":1,"type":"convoy.closed","ts":"2026-06-21T10:03:27Z","actor_hash":"0123456789abcdef","ref":"gcg-4216"}]}`
	if string(out) != want {
		t.Fatalf("batch golden:\n got %s\nwant %s", out, want)
	}
}

// TestAllowlistPolicyGolden pins the redaction POLICY (the allowed-type set, which
// types may carry a ref, which reduce to {type,ts}). Golden bytes + the field
// count pin the wire SHAPE, but widening the allowlist changes what may EGRESS
// without changing bytes or field count — no other guard fires. Changing this set
// is a redaction-policy change: update the golden AND bump SchemaVersion.
func TestAllowlistPolicyGolden(t *testing.T) {
	wantAllowed := []string{
		"bead.closed", "bead.created", "controller.started", "convoy.closed",
		"events.rotated", "gc.store.maintenance.done", "mail.sent",
		"order.completed", "order.failed", "order.fired",
		"project.identity.stamped", "session.drain_acked_with_assigned_work",
		"session.draining", "session.reset_stalled", "session.stopped",
		"session.stranded", "session.woke",
	}
	if got := AllowedTypeList(); !reflect.DeepEqual(got, wantAllowed) {
		t.Fatalf("allowlist policy changed:\n got  %v\n want %v\n-> update this golden AND bump SchemaVersion", got, wantAllowed)
	}
	if got := sortedKeys(refTypes); !reflect.DeepEqual(got, []string{"bead.closed", "bead.created", "convoy.closed"}) {
		t.Fatalf("refTypes policy changed: got %v -> bump SchemaVersion", got)
	}
	if got := sortedKeys(mailReduced); !reflect.DeepEqual(got, []string{"mail.sent"}) {
		t.Fatalf("mailReduced policy changed: got %v -> bump SchemaVersion", got)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
