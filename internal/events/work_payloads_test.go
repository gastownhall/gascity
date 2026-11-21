package events

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// work.rate_limited is a known, controller-authored type with its typed
// payload registered: the SSE projection carries it typed.
func TestWorkRateLimitedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()
	if !slices.Contains(KnownEventTypes, WorkRateLimited) {
		t.Fatalf("%q is missing from KnownEventTypes", WorkRateLimited)
	}
	sample, ok := LookupPayload(WorkRateLimited)
	if !ok {
		t.Fatalf("%q has no registered payload", WorkRateLimited)
	}
	if _, ok := sample.(WorkRateLimitedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want WorkRateLimitedPayload", WorkRateLimited, sample)
	}
	if got := AuthorshipOf(WorkRateLimited); got != AuthorController {
		t.Fatalf("AuthorshipOf(%q) = %s, want controller", WorkRateLimited, got)
	}
}

// The payload round-trips, and its optional times are absent, not the zero
// time, when unset.
func TestWorkRateLimitedPayloadRoundTrips(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 31, 5, 1, 28, 0, time.UTC)
	refused := since.Add(2 * time.Minute)
	want := WorkRateLimitedPayload{
		SessionID: "gcg-1", SessionName: "seat-a", Template: "worker", Provider: "claude",
		Generation: 8, Conversation: 3, Transition: WorkRateLimitedRestarted, Since: since,
		Refused: 2, RefusedAt: &refused, ErrorCode: "rate_limit", HTTPStatus: 429,
		ErrorText: "API Error: Request rejected (429)", Claims: []string{"gc-1", "gc-2"},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	decoded, typed, err := DecodePayload(WorkRateLimited, raw)
	if err != nil || !typed {
		t.Fatalf("DecodePayload = %v, typed %v", err, typed)
	}
	got, ok := decoded.(WorkRateLimitedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T", decoded)
	}
	if string(mustJSON(t, got)) != string(raw) {
		t.Fatalf("round-trip = %s, want %s", mustJSON(t, got), raw)
	}
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"fresh_at", "cause", "alerted"} {
		if _, ok := shape[absent]; ok {
			t.Errorf("unset %q is on the wire: %s", absent, raw)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
