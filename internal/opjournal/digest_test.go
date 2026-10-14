package opjournal

import (
	"testing"
	"time"
)

func digestOf(t *testing.T, req Request) string {
	t.Helper()
	spec, err := Lookup(req.Kind)
	if err != nil {
		t.Fatal(err)
	}
	op, err := normalizeRequest(req, spec, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("normalizeRequest: %v", err)
	}
	return RequestDigest(op)
}

func baseMessage() Request {
	return Request{
		Conversation: "gc-1", Kind: KindMessage, ClientKey: "k1",
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator},
		Payload:  "hello",
	}
}

func TestRequestDigestGolden(t *testing.T) {
	const want = "73c94e607d7027bf7d12aac373d0c6f0e086a513aee86929e93a83d359ed24aa"
	if got := digestOf(t, baseMessage()); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}

func TestRequestDigestDefaultsHashAlike(t *testing.T) {
	explicit := baseMessage()
	explicit.Policy = Policy{
		Guarantee: GuaranteeAtLeastOnce,
		Message:   MessagePolicy{When: WhenBoundary, Wake: WakeIfNeeded, Role: RolePrompt, OnUnsupported: FallbackBoundary},
	}
	explicit.Scope = Scope{Kind: ScopeConversation}
	if digestOf(t, explicit) != digestOf(t, baseMessage()) {
		t.Fatal("an explicit default hashes differently from an omitted one")
	}
}

func TestRequestDigestCoverage(t *testing.T) {
	base := digestOf(t, baseMessage())
	changes := map[string]func(*Request){
		"payload":   func(r *Request) { r.Payload = "hello!" },
		"when":      func(r *Request) { r.Policy.Message.When = WhenAfterTurn },
		"wake":      func(r *Request) { r.Policy.Message.Wake = WakeLiveOnly },
		"guarantee": func(r *Request) { r.Policy.Guarantee = GuaranteeAtMostOnce },
		"scope":     func(r *Request) { r.Scope = Scope{Kind: ScopeSession, Generation: 3} },
		"after":     func(r *Request) { r.After = "gc-op-kdpftlql5feaypldytje3nypwy" },
	}
	for name, change := range changes {
		r := baseMessage()
		change(&r)
		if digestOf(t, r) == base {
			t.Errorf("changing %s did not change the digest", name)
		}
	}
	same := map[string]func(*Request){
		"start_by":    func(r *Request) { r.StartBy = time.Date(2026, 10, 13, 13, 0, 0, 0, time.UTC) },
		"finish_by":   func(r *Request) { r.FinishBy = time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC) },
		"not_before":  func(r *Request) { r.NotBefore = time.Date(2026, 10, 13, 12, 30, 0, 0, time.UTC) },
		"client_seq":  func(r *Request) { r.ClientSeq = 9 },
		"causation":   func(r *Request) { r.Causation = "gc-77" },
		"correlation": func(r *Request) { r.Correlation = "run-1" },
		"actor":       func(r *Request) { r.Producer.Actor = ActorAgent },
	}
	for name, change := range same {
		r := baseMessage()
		change(&r)
		if digestOf(t, r) != base {
			t.Errorf("changing %s changed the digest", name)
		}
	}
}
