package api

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// bead.recovery_release_failed carries the failed bead, the agent identity,
// the hook that reported it and bd's error text, and decodes through the
// registry like every other typed event.
func TestBeadRecoveryReleaseFailedPayloadRoundTrip(t *testing.T) {
	raw := BeadRecoveryReleaseFailedPayloadJSON("ga-7", "rig/dog-1", "on_death", "dolt: connection refused")

	got, registered, err := events.DecodePayload(events.BeadRecoveryReleaseFailed, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("bead.recovery_release_failed has no registered payload")
	}
	p, ok := got.(BeadRecoveryReleaseFailedPayload)
	if !ok {
		t.Fatalf("payload = %T, want BeadRecoveryReleaseFailedPayload", got)
	}
	want := BeadRecoveryReleaseFailedPayload{BeadID: "ga-7", Agent: "rig/dog-1", Hook: "on_death", Error: "dolt: connection refused"}
	if p != want {
		t.Fatalf("payload = %+v, want %+v", p, want)
	}

	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"bead_id", "agent", "hook", "error"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("wire form %s is missing %q", raw, key)
		}
	}
}
