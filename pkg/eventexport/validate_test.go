package eventexport

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func rfc(t *testing.T) string {
	t.Helper()
	return time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
}

// refEnv is a ref-bearing bead.closed envelope as a producer with ExportRef=true
// emits it.
func refEnv(t *testing.T) Envelope {
	t.Helper()
	env, ok := ProjectEvent(TaggedEvent{Seq: 5, Type: "bead.closed", Ts: fixedTS, Actor: "gc", Subject: "mc-1"}, Options{Salt: testSalt, ExportRef: true})
	if !ok {
		t.Fatal("setup: bead.closed should project")
	}
	if env.Ref == "" {
		t.Fatal("setup: expected a ref")
	}
	return env
}

// TestValidateEnvelope_AcceptsRefWithoutOptions is the regression for the defect:
// a ref is wire-valid iff its type may carry one and it is opaque — NOT gated on
// the producer's ExportRef, which is not on the wire. A receiver re-validating a
// ref-bearing bead.closed with no producer config MUST accept it (else silent
// total data-loss).
func TestValidateEnvelope_AcceptsRefWithoutOptions(t *testing.T) {
	if err := ValidateEnvelope(refEnv(t)); err != nil {
		t.Fatalf("receiver-side ValidateEnvelope rejected a valid ref-bearing envelope: %v", err)
	}
}

func TestValidateEnvelope_Rejects(t *testing.T) {
	cases := map[string]Envelope{
		"unknown type":        {Seq: 1, Type: "extmsg.inbound", TS: rfc(t)},
		"seq 0":               {Seq: 0, Type: "bead.closed", TS: rfc(t)},
		"bad ts":              {Seq: 1, Type: "bead.closed", TS: "not-a-time"},
		"non-hex actor_hash":  {Seq: 1, Type: "bead.closed", TS: rfc(t), ActorHash: "xyz"},
		"ref on non-ref type": {Seq: 1, Type: "order.completed", TS: rfc(t), Ref: "abc"},
		"non-opaque ref":      {Seq: 1, Type: "bead.closed", TS: rfc(t), Ref: "a/b"},
		"non-opaque run_id":   {Seq: 1, Type: "bead.closed", TS: rfc(t), RunID: "a/b"},
		"non-opaque session":  {Seq: 1, Type: "bead.closed", TS: rfc(t), SessionID: "A@b"},
		"mail with extras":    {Seq: 1, Type: "mail.sent", TS: rfc(t), ActorHash: "0123456789abcdef"},
	}
	for name, env := range cases {
		if err := ValidateEnvelope(env); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

func TestValidate_ProducerPolicy(t *testing.T) {
	prod := Options{ExportRef: true}
	if err := Validate(refEnv(t), prod); err != nil {
		t.Fatalf("producer self-check rejected its own ref-bearing envelope: %v", err)
	}
	// Producer policy: a ref present with ExportRef disabled is an inconsistency
	// the producer's own self-check catches (the receiver, by contrast, accepts
	// it — see ValidateEnvelope).
	if err := Validate(refEnv(t), Options{ExportRef: false}); err == nil {
		t.Fatal("Validate must flag ref present while ExportRef disabled")
	}
	// Unknown profile rejected.
	if err := Validate(refEnv(t), Options{ExportRef: true, Profile: ProfileRedactedEnvelope + 1}); err == nil {
		t.Fatal("unknown profile must be rejected")
	}
}

func TestValidateBatch(t *testing.T) {
	good := Batch{CityID: "c", SchemaVersion: SchemaVersion, Events: []Envelope{refEnv(t)}}
	if err := ValidateBatch(good); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}

	// schema skew -> typed ErrSchemaMismatch (so ingest can errors.Is it).
	skew := Batch{CityID: "c", SchemaVersion: SchemaVersion + 1, Events: nil}
	err := ValidateBatch(skew)
	if err == nil || !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema skew must wrap ErrSchemaMismatch, got %v", err)
	}

	// a bad row fails with its index.
	bad := Batch{CityID: "c", SchemaVersion: SchemaVersion, Events: []Envelope{
		refEnv(t),
		{Seq: 0, Type: "bead.closed", TS: rfc(t)}, // row 1: seq 0
	}}
	if err := ValidateBatch(bad); err == nil {
		t.Fatal("batch with a bad row must fail")
	} else if got := err.Error(); !contains(got, "row 1") {
		t.Fatalf("batch error should name the failing row index, got %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestProfileZeroValue locks the safe default as the zero value forever — append
// new profiles, never insert (the zero-value Options must always be redacted).
func TestProfileZeroValue(t *testing.T) {
	if ProfileRedactedEnvelope != 0 {
		t.Fatalf("ProfileRedactedEnvelope must be the zero value, got %d", ProfileRedactedEnvelope)
	}
	var zero Options
	if zero.Profile != ProfileRedactedEnvelope {
		t.Fatal("zero-value Options.Profile must be ProfileRedactedEnvelope")
	}
}

// TestEnvelopeFieldCount fails when a field is added to Envelope, forcing the
// author to gate it in ProjectEvent + ValidateEnvelope (and bump SchemaVersion if
// the wire changes) rather than letting it ship ungated.
func TestEnvelopeFieldCount(t *testing.T) {
	if n := reflect.TypeOf(Envelope{}).NumField(); n != 7 {
		t.Fatalf("Envelope has %d fields; a field changed — gate it in ProjectEvent and ValidateEnvelope, then update this guard (and bump SchemaVersion if the wire changes)", n)
	}
}
