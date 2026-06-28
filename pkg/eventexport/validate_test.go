package eventexport

import (
	"errors"
	"reflect"
	"strings"
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
		"non-opaque step_id":  {Seq: 1, Type: "bead.closed", TS: rfc(t), StepID: "a/b"},
		"mail with extras":    {Seq: 1, Type: "mail.sent", TS: rfc(t), ActorHash: "0123456789abcdef"},
		"mail with step_id":   {Seq: 1, Type: "mail.sent", TS: rfc(t), StepID: "mc-step-1"},
		// Receiver-side content trust boundary: the length cap and the
		// mail-reduced "only {seq,type,ts}" rule must hold against a hostile or
		// buggy producer, with no producer Options in scope.
		"over-cap title":    {Seq: 1, Type: "bead.closed", TS: rfc(t), Title: strings.Repeat("a", maxContentLen+1)},
		"over-cap formula":  {Seq: 1, Type: "bead.closed", TS: rfc(t), Formula: strings.Repeat("b", maxContentLen+1)},
		"mail with title":   {Seq: 1, Type: "mail.sent", TS: rfc(t), Title: "secret subject"},
		"mail with formula": {Seq: 1, Type: "mail.sent", TS: rfc(t), Formula: "f"},
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
	// Producer policy (content symmetry with the ExportRef check above): an
	// envelope carrying free-form Title/Formula while the content opt-in is disabled
	// is a producer-side inconsistency the self-check must catch. ValidateEnvelope
	// (the receiver) length-bounds content but cannot see the opt-in, which is a
	// producer-only knob not on the wire — so this guard lives in Validate.
	titleEnv := Envelope{Seq: 1, Type: "bead.closed", TS: rfc(t), Title: "deploy prod"}
	if err := Validate(titleEnv, Options{emitContent: false}); err == nil {
		t.Fatal("Validate must flag title present while content opt-in disabled")
	}
	if err := Validate(titleEnv, Options{emitContent: true}); err != nil {
		t.Fatalf("title envelope with content opt-in enabled must pass producer self-check: %v", err)
	}
	formulaEnv := Envelope{Seq: 1, Type: "bead.closed", TS: rfc(t), Formula: "randy-triage-patrol"}
	if err := Validate(formulaEnv, Options{emitContent: false}); err == nil {
		t.Fatal("Validate must flag formula present while content opt-in disabled")
	}
	if err := Validate(formulaEnv, Options{emitContent: true}); err != nil {
		t.Fatalf("formula envelope with content opt-in enabled must pass producer self-check: %v", err)
	}
	// Unknown profile rejected.
	if err := Validate(refEnv(t), Options{ExportRef: true, Profile: ProfileRedactedEnvelope + 1}); err == nil {
		t.Fatal("unknown profile must be rejected")
	}
}

func TestValidateBatch(t *testing.T) {
	const cityHash = "0123456789abcdef" // a valid opaque 16-hex partition key

	good := Batch{CityHash: cityHash, SchemaVersion: SchemaVersion, Events: []Envelope{refEnv(t)}}
	if err := ValidateBatch(good); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}

	// schema skew -> typed ErrSchemaMismatch (so ingest can errors.Is it).
	skew := Batch{CityHash: cityHash, SchemaVersion: SchemaVersion + 1, Events: nil}
	err := ValidateBatch(skew)
	if err == nil || !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema skew must wrap ErrSchemaMismatch, got %v", err)
	}

	// Receiver trust boundary: city_hash must be the opaque 16-hex partition-key
	// shape schema v2 promises. An empty, too-short, cleartext-shaped, uppercase,
	// or over-length value is rejected before any row is processed — the receiver
	// cannot assume the producer redacted the operator-chosen city name.
	for name, ch := range map[string]string{
		"empty":          "",
		"too short":      "c",
		"cleartext city": "acme-prod",
		"uppercase hex":  "0123456789ABCDEF",
		"too long":       "0123456789abcdef0",
	} {
		b := Batch{CityHash: ch, SchemaVersion: SchemaVersion, Events: []Envelope{refEnv(t)}}
		if err := ValidateBatch(b); err == nil {
			t.Errorf("%s city_hash %q must be rejected", name, ch)
		}
	}

	// a producer-computed city_hash is accepted (the positive end of the gate).
	if err := ValidateBatch(Batch{CityHash: CityHash(testSalt, "acme-prod"), SchemaVersion: SchemaVersion, Events: []Envelope{refEnv(t)}}); err != nil {
		t.Fatalf("producer-computed city_hash rejected: %v", err)
	}

	// a bad row fails with its index.
	bad := Batch{CityHash: cityHash, SchemaVersion: SchemaVersion, Events: []Envelope{
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
	// 11 = the original 7 + StepID (a version-NEUTRAL opaque correlation field,
	// gated in ProjectEvent + ValidateEnvelope exactly like run_id/session_id) +
	// Title + Formula (free-form content under the content opt-in — the deliberate
	// exception to envelope-only, gated separately and length-capped, never
	// opaque-gated) + the trailing blank `_ struct{}` keyed-literal guard, which is
	// NOT a wire field (json ignores it; it only forces keyed Envelope literals).
	if n := reflect.TypeOf(Envelope{}).NumField(); n != 11 {
		t.Fatalf("Envelope has %d fields; a field changed — gate it in ProjectEvent and ValidateEnvelope, then update this guard (and bump SchemaVersion if the wire changes)", n)
	}
}

// TestOptionsContentOptInUnexported locks the content opt-in as package-private.
// If emitContent were exported, any importer of pkg/eventexport could call
// ProjectEvent with content enabled and emit Title/Formula on a SchemaVersion==2
// batch — exactly the reachable wire change the off-by-default exemption forbids.
// When a producer makes content reachable (ga-mt1e99) it owns the SchemaVersion
// decision; exporting this gate without that coordination must fail here rather
// than ship a silent wire change.
func TestOptionsContentOptInUnexported(t *testing.T) {
	f, ok := reflect.TypeOf(Options{}).FieldByName("emitContent")
	if !ok {
		t.Fatal("Options.emitContent missing: the content opt-in gate must exist as an unexported field")
	}
	if f.PkgPath == "" {
		t.Fatal("Options.emitContent must stay UNEXPORTED: an exported content opt-in lets importers emit title/formula on schema v2 without a SchemaVersion bump (see ga-mt1e99)")
	}
}

// TestProjectEvent_ContentGating proves the content opt-in exception: with the
// flag OFF (default) free-form title/formula are dropped even when the source
// carries them; with it ON they round-trip verbatim (spaces/punctuation intact,
// NOT opaque-gated); over-cap values drop; and mail-reduced types never carry content.
func TestProjectEvent_ContentGating(t *testing.T) {
	salt := []byte("0123456789abcdef")
	src := TaggedEvent{
		Seq: 1, Type: "bead.closed", Ts: time.Unix(1700000000, 0),
		Actor: "alice", Subject: "mc-wisp-i6vz0e",
		StepID:  "review-pipeline.synthesize",
		Title:   "ESCALATION: JSONL spike detected [HIGH]",
		Formula: "randy-triage-patrol",
	}
	// OFF: content + step_id dropped.
	off, ok := ProjectEvent(src, Options{Salt: salt})
	if !ok {
		t.Fatal("projection unexpectedly dropped the event")
	}
	if off.Title != "" || off.Formula != "" || off.StepID != "" {
		t.Fatalf("content/correlation opt-ins off must drop content+step_id, got title=%q formula=%q step_id=%q", off.Title, off.Formula, off.StepID)
	}
	// ON: content round-trips free-form, step_id under EmitCorrelation.
	on, ok := ProjectEvent(src, Options{Salt: salt, emitContent: true, EmitCorrelation: true})
	if !ok {
		t.Fatal("projection unexpectedly dropped the event")
	}
	if on.Title != src.Title {
		t.Fatalf("title must round-trip verbatim, got %q want %q", on.Title, src.Title)
	}
	if on.Formula != src.Formula {
		t.Fatalf("formula must round-trip verbatim, got %q want %q", on.Formula, src.Formula)
	}
	if on.StepID != src.StepID {
		t.Fatalf("step_id must round-trip (opaque), got %q want %q", on.StepID, src.StepID)
	}
	if err := ValidateEnvelope(on); err != nil {
		t.Fatalf("populated content envelope must validate: %v", err)
	}
	// Over-cap title is DROPPED, not truncated.
	big := src
	big.Title = strings.Repeat("a", maxContentLen+1)
	over, _ := ProjectEvent(big, Options{Salt: salt, emitContent: true})
	if over.Title != "" {
		t.Fatalf("over-cap title must be dropped, got %d bytes", len(over.Title))
	}
	// Exactly maxContentLen is ACCEPTED and round-trips verbatim: the cap is an
	// inclusive bound (only len > maxContentLen drops), so a 256-byte title/formula
	// is the largest legal value. Pinning the boundary on BOTH the projection and
	// the receiver (ValidateEnvelope) sides makes an off-by-one regression (> -> >=
	// in capContent or ValidateEnvelope) fail loudly instead of silently dropping or
	// rejecting legitimate max-length content.
	edge := src
	edge.Title = strings.Repeat("a", maxContentLen)
	edge.Formula = strings.Repeat("b", maxContentLen)
	atCap, ok := ProjectEvent(edge, Options{Salt: salt, emitContent: true})
	if !ok {
		t.Fatal("projection unexpectedly dropped the exactly-maxContentLen event")
	}
	if len(atCap.Title) != maxContentLen || atCap.Title != edge.Title {
		t.Fatalf("exactly-maxContentLen title must survive verbatim, got %d bytes", len(atCap.Title))
	}
	if len(atCap.Formula) != maxContentLen || atCap.Formula != edge.Formula {
		t.Fatalf("exactly-maxContentLen formula must survive verbatim, got %d bytes", len(atCap.Formula))
	}
	if err := ValidateEnvelope(atCap); err != nil {
		t.Fatalf("exactly-maxContentLen content envelope must validate: %v", err)
	}
	// mail.sent never carries content even with the content opt-in on.
	mail := TaggedEvent{Seq: 2, Type: "mail.sent", Ts: time.Unix(1700000000, 0), Actor: "alice", Title: "secret subject", Formula: "f"}
	menv, _ := ProjectEvent(mail, Options{Salt: salt, emitContent: true})
	if menv.Title != "" || menv.Formula != "" || menv.ActorHash != "" {
		t.Fatalf("mail.sent must stay {seq,type,ts}, got %+v", menv)
	}
	if err := ValidateEnvelope(menv); err != nil {
		t.Fatalf("mail-reduced envelope must validate: %v", err)
	}
}
