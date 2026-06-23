package eventexport

import (
	"reflect"
	"testing"
	"time"
)

func rfc(t *testing.T) string {
	t.Helper()
	return time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
}

func TestValidate_AcceptsRedactedEnvelope(t *testing.T) {
	opt := Options{Salt: []byte("s"), ExportRef: true}

	env, ok := ProjectFields(5, "bead.closed", fixedTS, "gc", "mc-1", "wf-root-abc", "sess-9f2a", opt)
	if !ok {
		t.Fatal("setup: bead.closed should project")
	}
	if err := Validate(env, opt); err != nil {
		t.Fatalf("valid bead.closed envelope rejected: %v", err)
	}

	m, _ := ProjectFields(6, "mail.sent", fixedTS, "gc", "mc-x", "", "", opt)
	if err := Validate(m, opt); err != nil {
		t.Fatalf("valid mail.sent envelope rejected: %v", err)
	}
}

func TestValidate_Rejects(t *testing.T) {
	opt := Options{ExportRef: true}
	cases := map[string]Envelope{
		"unknown type":        {Seq: 1, Type: "extmsg.inbound", TS: rfc(t)},
		"seq 0":               {Seq: 0, Type: "bead.closed", TS: rfc(t)},
		"bad ts":              {Seq: 1, Type: "bead.closed", TS: "not-a-time"},
		"non-hex actor_hash":  {Seq: 1, Type: "bead.closed", TS: rfc(t), ActorHash: "xyz"},
		"ref on non-ref type": {Seq: 1, Type: "order.completed", TS: rfc(t), Ref: "abc"},
		"non-opaque run_id":   {Seq: 1, Type: "bead.closed", TS: rfc(t), RunID: "a/b"},
		"non-opaque session":  {Seq: 1, Type: "bead.closed", TS: rfc(t), SessionID: "A@b"},
		"mail with extras":    {Seq: 1, Type: "mail.sent", TS: rfc(t), ActorHash: "0123456789abcdef"},
	}
	for name, env := range cases {
		if err := Validate(env, opt); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

func TestValidate_RejectsUnknownProfile(t *testing.T) {
	env, _ := ProjectFields(1, "bead.closed", fixedTS, "gc", "mc-1", "", "", Options{ExportRef: true})
	if err := Validate(env, Options{Profile: ProfileRedactedEnvelope + 1}); err == nil {
		t.Fatal("unknown profile must be rejected")
	}
}

// TestEnvelopeFieldCount fails when a field is added to Envelope, forcing the
// author to extend ProjectFields gating + Validate for the new field rather than
// letting it ship ungated.
func TestEnvelopeFieldCount(t *testing.T) {
	if n := reflect.TypeOf(Envelope{}).NumField(); n != 7 {
		t.Fatalf("Envelope has %d fields; a field changed — gate it in ProjectFields and Validate, then update this guard (and bump SchemaVersion if the wire changes)", n)
	}
}
