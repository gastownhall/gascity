package events

// UnknownActor is the actor for an event whose writer has no verified identity
// (gascity#9180). It is never a trusted identity.
const UnknownActor = "unknown"

// AttributionActor returns the identity to use for non-gating reads: completion
// attribution, the self-wake filter, and the execution backstop session match.
// It returns Actor, except that an UnknownActor event with a claim returns the
// claim. The claim is informational, not verified, and it never feeds a gate.
func (e Event) AttributionActor() string {
	if e.Actor == UnknownActor && e.ClaimedActor != "" {
		return e.ClaimedActor
	}
	return e.Actor
}
