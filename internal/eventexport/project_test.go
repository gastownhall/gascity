package eventexport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func ev(seq uint64, typ, actor, subject, message, payload string) events.Event {
	e := events.Event{
		Seq:     seq,
		Type:    typ,
		Ts:      time.Date(2026, 6, 21, 10, 3, 27, 0, time.UTC),
		Actor:   actor,
		Subject: subject,
		Message: message,
	}
	if payload != "" {
		e.Payload = json.RawMessage(payload)
	}
	return e
}

func TestProject_AllowlistAndExclusions(t *testing.T) {
	opt := Options{Salt: []byte("salt"), ExportRef: true}

	if _, ok := Project(ev(1, "bead.updated", "cache-reconcile", "mc-6c9w", "", `{"bead":{"title":"secret"}}`), opt); ok {
		t.Fatal("bead.updated must be excluded (not allowlisted)")
	}
	if _, ok := Project(ev(2, "extmsg.inbound", "discord", "chan", "", `{"actor":"Stephanie Jarmak"}`), opt); ok {
		t.Fatal("extmsg.inbound must be excluded")
	}

	got, ok := Project(ev(10, "bead.closed", "cache-reconcile", "mc-wisp-i6vz0e", "", `{"bead":{"title":"private"}}`), opt)
	if !ok {
		t.Fatal("bead.closed must be exportable")
	}
	if got.Ref != "mc-wisp-i6vz0e" {
		t.Fatalf("bead.closed ref = %q, want mc-wisp-i6vz0e", got.Ref)
	}
	if len(got.ActorHash) != 16 || !isHex(got.ActorHash) {
		t.Fatalf("actor_hash = %q, want 16-hex", got.ActorHash)
	}

	// session subject with a path separator -> ref dropped, event still exported.
	got, ok = Project(ev(30, "session.woke", "gc", "gascity/codex-mini-1", "", ""), opt)
	if !ok || got.Ref != "" {
		t.Fatalf("session.woke: ok=%v ref=%q (ref must drop on '/')", ok, got.Ref)
	}

	// order slug is author-defined (can embed customer/host names), not an
	// opaque id: ref must be dropped.
	got, _ = Project(ev(20, "order.completed", "controller", "deploy-to-clientco-prod", "", ""), opt)
	if got.Ref != "" {
		t.Fatalf("order.completed must not export a ref, got %q", got.Ref)
	}

	// project.identity.stamped subject is a scope-root directory name: dropped.
	got, _ = Project(ev(25, "project.identity.stamped", "gc", "acme-client-repo", "", ""), opt)
	if got.Ref != "" {
		t.Fatalf("project.identity.stamped must not export a ref, got %q", got.Ref)
	}

	// convoy id IS an opaque store id: ref kept.
	got, _ = Project(ev(40, "convoy.closed", "human", "gcg-4216", "", ""), opt)
	if got.Ref != "gcg-4216" {
		t.Fatalf("convoy.closed ref = %q, want gcg-4216", got.Ref)
	}

	// mail.sent reduced to {type, ts}.
	got, ok = Project(ev(60, "mail.sent", "gc", "mc-x", "body", `{"to":"x"}`), opt)
	if !ok || got.ActorHash != "" || got.Ref != "" {
		t.Fatalf("mail.sent must be {type,ts} only, got %+v", got)
	}
}

// TestProject_NoLeak projects a corpus carrying the exact sensitive content the
// raw stream holds, and proves none of it survives into the marshaled batch.
func TestProject_NoLeak(t *testing.T) {
	opt := Options{Salt: []byte("org-salt"), ExportRef: true}
	corpus := []events.Event{
		ev(1, "bead.updated", "cache-reconcile", "mc-6c9w", "", `{"bead":{"title":"Finalize scope","dependencies":[{"depends_on_id":"mc-0ugy"}],"metadata":{"gc.execution_routed_to":"wendy.wendy"}}}`),
		ev(2, "bead.closed", "cache-reconcile", "mc-wisp-i6vz0e", "", `{"bead":{"title":"some private title"}}`),
		ev(3, "order.failed", "controller", "orphan-sweep", "some failure detail that must not leak", ""),
		ev(4, "session.stopped", "gc", "gascity-packs/gc.design-test-risk-reviewer", "", `{"reason":"/data/projects/maintainer-city exited"}`),
		ev(5, "mail.sent", "gascity/codex-mini-1", "mc-wisp-wcvwm2", "private body", `{"to":"someone"}`),
		ev(6, "extmsg.inbound", "discord", "chan", "", `{"actor":"Stephanie Jarmak","conversation_id":"998877"}`),
		ev(7, "convoy.closed", "human", "gcg-4216", "", ""),
		ev(8, "project.identity.stamped", "gc", "acme-client-repo", "", ""),       // scope-root dir name
		ev(9, "order.completed", "controller", "deploy-to-clientco-prod", "", ""), // author slug
	}
	var batch Batch
	batch.CityID = "maintainer-city"
	batch.SchemaVersion = SchemaVersion
	for _, e := range corpus {
		if env, ok := Project(e, opt); ok {
			batch.Events = append(batch.Events, env)
		}
	}
	out, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(out)
	forbidden := []string{
		"/data/projects", "gascity/", "gascity-packs/", "github.com",
		"Stephanie", "Jarmak", "title", "Finalize", "depends_on", "dependencies",
		"metadata", "private body", "private title", "failure detail", "exited",
		"conversation_id", "998877", "@", "payload", "message", "subject",
		"acme-client-repo", "clientco", "deploy-to-clientco-prod",
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
	if len(a) != 16 || !isHex(a) {
		t.Fatalf("hash %q not 16-hex", a)
	}
	if ActorHash([]byte("s"), "") != "" {
		t.Fatal("empty actor -> empty hash")
	}
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		hexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !hexDigit {
			return false
		}
	}
	return true
}
