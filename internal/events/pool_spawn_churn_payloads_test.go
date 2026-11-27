package events

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// The spawn-churn guard's two facts are known event types with a typed
// payload, written only by the controller, and the payload round-trips.
func TestPoolSpawnChurnEventsAreKnownTypedAndControllerAuthored(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{SessionPoolSpawnChurnCoolingDown, SessionPoolSpawnChurnResumed} {
		if !slices.Contains(KnownEventTypes, typ) {
			t.Fatalf("%q is missing from KnownEventTypes", typ)
		}
		sample, ok := LookupPayload(typ)
		if !ok {
			t.Fatalf("%q has no registered payload", typ)
		}
		if _, ok := sample.(PoolSpawnChurnPayload); !ok {
			t.Fatalf("%q registered payload is %T, want PoolSpawnChurnPayload", typ, sample)
		}
		if got, ok := RegisteredAuthorship(typ); !ok || got != AuthorController {
			t.Fatalf("%q authorship = %q (registered %v), want %q", typ, got, ok, AuthorController)
		}
	}
	if SessionPoolSpawnChurnCoolingDown != "session.pool_spawn_churn_cooling_down" || SessionPoolSpawnChurnResumed != "session.pool_spawn_churn_resumed" {
		t.Fatalf("event names = %q, %q", SessionPoolSpawnChurnCoolingDown, SessionPoolSpawnChurnResumed)
	}

	until := time.Date(2026, 11, 27, 12, 5, 0, 0, time.UTC)
	want := PoolSpawnChurnPayload{
		Template: "gascity/worker",
		Episode:  "gascity/worker@2026-11-27T12:00:00Z",
		Cause:    PoolSpawnChurnCauseTripped,
		Seats:    3,
		Since:    time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
		Until:    &until,
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	typed, registered, err := DecodePayload(SessionPoolSpawnChurnCoolingDown, raw)
	if err != nil || !registered {
		t.Fatalf("DecodePayload: registered=%v err=%v", registered, err)
	}
	got, ok := typed.(PoolSpawnChurnPayload)
	if !ok || got.Template != want.Template || got.Episode != want.Episode || got.Cause != want.Cause ||
		got.Seats != want.Seats || !got.Since.Equal(want.Since) || got.Until == nil || !got.Until.Equal(until) {
		t.Fatalf("decoded = %#v, want %#v", typed, want)
	}
}
