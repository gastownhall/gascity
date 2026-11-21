package events

import "testing"

// AttributionActor is the actor for non-gating reads (completion attribution,
// self-wake filter, execution backstop). A CLI write carries no verified identity,
// so Actor is "unknown" and the claimed identity lives in ClaimedActor (gascity#9180).
func TestAttributionActor(t *testing.T) {
	tests := []struct {
		name string
		evt  Event
		want string
	}{
		{"trusted actor is returned as-is", Event{Actor: "controller"}, "controller"},
		{"unknown with a claim returns the claim", Event{Actor: UnknownActor, ClaimedActor: "mayor"}, "mayor"},
		{"unknown with no claim stays unknown", Event{Actor: UnknownActor}, UnknownActor},
		{"claim is ignored when actor is trusted", Event{Actor: "controller", ClaimedActor: "mayor"}, "controller"},
	}
	for _, tt := range tests {
		if got := tt.evt.AttributionActor(); got != tt.want {
			t.Errorf("%s: AttributionActor() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
