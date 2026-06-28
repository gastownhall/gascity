package eventexport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	fixedTS = time.Date(2026, 6, 21, 10, 3, 27, 0, time.UTC)
	// testSalt is >= minSaltLen so ProjectEvent does not fail closed on it.
	testSalt = []byte("sixteen-byte-salt-xx")
)

// proj is a test shorthand for ProjectEvent at the fixed test time.
func proj(seq uint64, typ, actor, subject, runID, sessionID string, opt Options) (Envelope, bool) {
	return ProjectEvent(TaggedEvent{Seq: seq, Type: typ, Ts: fixedTS, Actor: actor, Subject: subject, RunID: runID, SessionID: sessionID}, opt)
}

func TestProjectEvent_AllowlistAndExclusions(t *testing.T) {
	opt := Options{Salt: testSalt, ExportRef: true}

	if _, ok := proj(1, "bead.updated", "cache-reconcile", "mc-6c9w", "", "", opt); ok {
		t.Fatal("bead.updated must be excluded (not allowlisted)")
	}
	if _, ok := proj(2, "extmsg.inbound", "discord", "chan", "", "", opt); ok {
		t.Fatal("extmsg.inbound must be excluded")
	}

	got, ok := proj(10, "bead.closed", "cache-reconcile", "mc-wisp-i6vz0e", "", "", opt)
	if !ok {
		t.Fatal("bead.closed must be exportable")
	}
	if got.Ref != "mc-wisp-i6vz0e" {
		t.Fatalf("bead.closed ref = %q, want mc-wisp-i6vz0e", got.Ref)
	}
	if !isHex16(got.ActorHash) {
		t.Fatalf("actor_hash = %q, want 16-hex", got.ActorHash)
	}

	// session subject with a path separator -> ref dropped, event still exported.
	got, ok = proj(30, "session.woke", "gc", "gascity/codex-mini-1", "", "", opt)
	if !ok || got.Ref != "" {
		t.Fatalf("session.woke: ok=%v ref=%q (ref must drop on '/')", ok, got.Ref)
	}

	// order slug is author-defined (can embed customer/host names), not an opaque
	// id: ref must be dropped.
	got, _ = proj(20, "order.completed", "controller", "deploy-to-clientco-prod", "", "", opt)
	if got.Ref != "" {
		t.Fatalf("order.completed must not export a ref, got %q", got.Ref)
	}

	// project.identity.stamped subject is a scope-root directory name: dropped.
	got, _ = proj(25, "project.identity.stamped", "gc", "acme-client-repo", "", "", opt)
	if got.Ref != "" {
		t.Fatalf("project.identity.stamped must not export a ref, got %q", got.Ref)
	}

	// convoy id IS an opaque store id: ref kept.
	got, _ = proj(40, "convoy.closed", "human", "gcg-4216", "", "", opt)
	if got.Ref != "gcg-4216" {
		t.Fatalf("convoy.closed ref = %q, want gcg-4216", got.Ref)
	}

	// mail.sent reduced to {type, ts}.
	got, ok = proj(60, "mail.sent", "gc", "mc-x", "", "", opt)
	if !ok || got.ActorHash != "" || got.Ref != "" {
		t.Fatalf("mail.sent must be {type,ts} only, got %+v", got)
	}

	// seq==0 and zero time are dropped — load-bearing for the cursor/dedup contract.
	if _, ok := proj(0, "bead.closed", "gc", "mc-1", "", "", opt); ok {
		t.Fatal("seq==0 must be dropped")
	}
	if _, ok := ProjectEvent(TaggedEvent{Seq: 7, Type: "bead.closed", Ts: time.Time{}, Actor: "gc", Subject: "mc-1"}, opt); ok {
		t.Fatal("zero timestamp must be dropped")
	}
}

// TestProjectEvent_RejectsShortSalt proves the fail-closed salt guard: a salt
// shorter than 16 bytes drops the event rather than emit a brute-forceable hash.
func TestProjectEvent_RejectsShortSalt(t *testing.T) {
	for _, salt := range [][]byte{nil, {}, []byte("short"), []byte("fifteen-bytes!!")} {
		if _, ok := proj(1, "bead.closed", "gc", "mc-1", "", "", Options{Salt: salt, ExportRef: true}); ok {
			t.Fatalf("salt of %d bytes must fail closed (require >= %d)", len(salt), minSaltLen)
		}
	}
	// exactly 16 bytes is accepted.
	if _, ok := proj(1, "bead.closed", "gc", "mc-1", "", "", Options{Salt: []byte("0123456789abcdef"), ExportRef: true}); !ok {
		t.Fatal("16-byte salt must be accepted")
	}
}

func TestProjectEvent_RunSessionGating(t *testing.T) {
	// EmitCorrelation must be set for run_id/session_id to appear at all (the
	// fail-closed default; the production export sets it true).
	off := Options{Salt: testSalt, ExportRef: true}
	got, _ := proj(1, "bead.closed", "gc", "mc-1", "wf-root-abc", "sess-9f2a", off)
	if got.RunID != "" || got.SessionID != "" {
		t.Fatalf("EmitCorrelation=false must drop run/session, got run=%q session=%q", got.RunID, got.SessionID)
	}

	on := Options{Salt: testSalt, ExportRef: true, EmitCorrelation: true}
	got, ok := proj(1, "bead.closed", "gc", "mc-1", "wf-root-abc", "sess-9f2a", on)
	if !ok || got.RunID != "wf-root-abc" || got.SessionID != "sess-9f2a" {
		t.Fatalf("opaque run/session must round-trip when emitted, got run=%q session=%q", got.RunID, got.SessionID)
	}

	// non-opaque values dropped to "" by safeRef even with emission enabled —
	// exercise a path, an address, an uppercase, and a space through BOTH slots.
	for _, bad := range []string{"gascity/codex", "user@host", "My Run", "UPPER", "a b"} {
		g, _ := proj(2, "bead.closed", "gc", "mc-2", bad, bad, on)
		if g.RunID != "" || g.SessionID != "" {
			t.Fatalf("non-opaque %q must drop to empty, got run=%q session=%q", bad, g.RunID, g.SessionID)
		}
	}

	// mail.sent stays {type,ts} only — never carries run/session.
	got, _ = proj(3, "mail.sent", "gc", "mc-3", "wf-root-abc", "sess-9f2a", on)
	if got.RunID != "" || got.SessionID != "" {
		t.Fatalf("mail.sent must not carry run/session, got %+v", got)
	}
}

// step_id (the acting work bead) is gated exactly like run/session: EmitCorrelation
// fail-closed, safeRef-opaque-only, never on mail-reduced types, empty when the
// subject bead carries no gc.step_id.
func TestProjectEvent_StepIDGating(t *testing.T) {
	te := func(step string) TaggedEvent {
		return TaggedEvent{Seq: 1, Type: "bead.closed", Ts: fixedTS, Actor: "gc", Subject: "mc-1", RunID: "wf-root-abc", SessionID: "sess-9f2a", StepID: step}
	}
	off := Options{Salt: testSalt, ExportRef: true}
	if g, _ := ProjectEvent(te("mc-step-7"), off); g.StepID != "" {
		t.Fatalf("EmitCorrelation=false must drop step_id, got %q", g.StepID)
	}
	on := Options{Salt: testSalt, ExportRef: true, EmitCorrelation: true}
	if g, ok := ProjectEvent(te("mc-step-7"), on); !ok || g.StepID != "mc-step-7" {
		t.Fatalf("opaque step_id must round-trip when emitted, got %q ok=%v", g.StepID, ok)
	}
	for _, bad := range []string{"gascity/codex", "user@host", "Up Per", "a b"} {
		if g, _ := ProjectEvent(te(bad), on); g.StepID != "" {
			t.Fatalf("non-opaque step_id %q must drop to empty, got %q", bad, g.StepID)
		}
	}
	mail := te("mc-step-7")
	mail.Type = "mail.sent"
	if g, _ := ProjectEvent(mail, on); g.StepID != "" {
		t.Fatalf("mail.sent must not carry step_id, got %q", g.StepID)
	}
	// A non-work bead carries no gc.step_id → empty, omitted cleanly (no error).
	if g, ok := ProjectEvent(te(""), on); !ok || g.StepID != "" {
		t.Fatalf("empty step_id must be omitted cleanly, got %q ok=%v", g.StepID, ok)
	}
}

// TestProject_NoLeak feeds the projection a corpus carrying the sensitive markers
// the raw stream holds — in the primitive fields the projection actually receives
// — and proves none survive into the marshaled batch. The adapter-level
// counterpart (internal/eventfeed) proves the events.Event payload/message are
// never copied INTO these fields in the first place.
func TestProject_NoLeak(t *testing.T) {
	opt := Options{Salt: testSalt, ExportRef: true}
	type in struct {
		seq                            uint64
		typ, actor, subject, run, sess string
	}
	corpus := []in{
		{1, "bead.updated", "cache-reconcile", "mc-6c9w", "", ""}, // dropped (not allowlisted)
		{2, "bead.closed", "cache-reconcile", "mc-wisp-i6vz0e", "", ""},
		{3, "order.failed", "controller", "orphan-sweep", "", ""},
		{4, "session.stopped", "gc", "gascity-packs/gc.design-test-risk-reviewer", "", ""},
		{5, "mail.sent", "gascity/codex-mini-1", "mc-wisp-wcvwm2", "", ""},
		{6, "extmsg.inbound", "discord", "chan", "", ""}, // dropped
		{7, "convoy.closed", "human", "gcg-4216", "", ""},
		{8, "project.identity.stamped", "gc", "acme-client-repo", "", ""},       // scope-root dir name
		{9, "order.completed", "controller", "deploy-to-clientco-prod", "", ""}, // author slug
	}
	var batch Batch
	// A customer-identifying city name (claude's "acme-prod" example) must be
	// reduced to an opaque hash; the cleartext must never reach the wire.
	const cityName = "acme-prod"
	batch.CityHash = CityHash(opt.Salt, cityName)
	batch.SchemaVersion = SchemaVersion
	for _, e := range corpus {
		if env, ok := proj(e.seq, e.typ, e.actor, e.subject, e.run, e.sess, opt); ok {
			batch.Events = append(batch.Events, env)
		}
	}
	out, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(out)
	forbidden := []string{
		"/data/projects", "gascity/", "gascity-packs/",
		"acme-client-repo", "clientco", "deploy-to-clientco-prod",
		"orphan-sweep", "@", "payload", "message",
		cityName, // the cleartext city name must never appear; only its hash ships
	}
	for _, f := range forbidden {
		if strings.Contains(blob, f) {
			t.Fatalf("LEAK: projected batch contains %q\n%s", f, blob)
		}
	}
	// Structural oracle: only opaque-store-id types may ever carry a ref.
	for _, en := range batch.Events {
		if en.Ref != "" && !refTypes[en.Type] {
			t.Fatalf("type %q must not carry a ref, got %q", en.Type, en.Ref)
		}
	}
	if len(batch.Events) < 4 {
		t.Fatalf("expected allowlisted events to survive, got %d", len(batch.Events))
	}
	t.Logf("projected %d/%d events, %d bytes, zero leaks", len(batch.Events), len(corpus), len(out))
}

func TestActorHash(t *testing.T) {
	a := ActorHash([]byte("s1"), "wendy.wendy")
	if a != ActorHash([]byte("s1"), "wendy.wendy") {
		t.Fatal("same salt+actor must be deterministic")
	}
	if a == ActorHash([]byte("s2"), "wendy.wendy") {
		t.Fatal("different salt must change the hash")
	}
	if !isHex16(a) {
		t.Fatalf("hash %q not 16-hex", a)
	}
	if ActorHash([]byte("s"), "") != "" {
		t.Fatal("empty actor -> empty hash")
	}
}

func TestCityHash(t *testing.T) {
	c := CityHash([]byte("s1"), "acme-prod")
	if c != CityHash([]byte("s1"), "acme-prod") {
		t.Fatal("same salt+city must be deterministic")
	}
	if c == CityHash([]byte("s2"), "acme-prod") {
		t.Fatal("different salt must change the hash")
	}
	if !isHex16(c) {
		t.Fatalf("hash %q not 16-hex", c)
	}
	if CityHash([]byte("s"), "") != "" {
		t.Fatal("empty city -> empty hash")
	}
	// Domain separation: a city and an actor that share a name must not collide.
	if CityHash([]byte("s"), "shared-name") == ActorHash([]byte("s"), "shared-name") {
		t.Fatal("CityHash must be domain-separated from ActorHash")
	}
}

func TestIsOpaqueRef(t *testing.T) {
	for _, ok := range []string{"mc-wisp-i6vz0e", "gcg-4216", "wf-root-abc", "a.b_c-1"} {
		if !IsOpaqueRef(ok) {
			t.Errorf("IsOpaqueRef(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "gascity/codex", "user@host", "My Run", "UPPER", "a b", strings.Repeat("a", maxRefLen+1)} {
		if IsOpaqueRef(bad) {
			t.Errorf("IsOpaqueRef(%q) = true, want false", bad)
		}
	}
}
