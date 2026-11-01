package events

import "time"

// What the claims-stall guard did about a stall (AgentClaimsPayload.Action).
const (
	// AgentClaimsHeld: the template's default routed-work demand is withheld,
	// one probe seat admitted each time the hold runs out (Until).
	AgentClaimsHeld = "held"
)

// Why a stall began or ended (AgentClaimsPayload.Cause).
const (
	// AgentClaimsCauseSeats: Seats seats in a row failed to claim the work
	// routed to them while it waited (agent.claims_stalled).
	AgentClaimsCauseSeats = "seats"
	// AgentClaimsCauseProbe: the probe seat admitted after a hold failed to
	// claim too, and the hold was extended (agent.claims_stalled).
	AgentClaimsCauseProbe = "probe"
	// AgentClaimsCauseClaimed: a seat on the stalled provider claimed its
	// work (agent.claims_resumed).
	AgentClaimsCauseClaimed = "claimed"
	// AgentClaimsCauseConfigChanged: a config change moved the template off
	// the stalled provider, or removed it (agent.claims_resumed).
	AgentClaimsCauseConfigChanged = "config_changed"
)

// AgentClaimsPayload is the typed payload of the claims-stall guard's facts
// (agent.claims_stalled, agent.claims_resumed). Template is the agent template
// (its qualified name, also the envelope's Subject) and Provider the provider
// its seats ran on. Episode names one stall, from the trip to what ends it,
// stable across the alerts it raises: an operator's dedupe key. Since is when
// the episode began; Seats how many seats failed in the streak that tripped
// it, or in the episode so far; Until when the next probe seat may spawn while
// the template is held.
type AgentClaimsPayload struct {
	Template string     `json:"template"`
	Provider string     `json:"provider"`
	Episode  string     `json:"episode"`
	Action   string     `json:"action,omitempty"`
	Cause    string     `json:"cause,omitempty"`
	Seats    int        `json:"seats,omitempty"`
	Since    time.Time  `json:"since"`
	Until    *time.Time `json:"until,omitempty"`
}

// IsEventPayload marks AgentClaimsPayload as an events.Payload variant.
func (AgentClaimsPayload) IsEventPayload() {}

func init() {
	RegisterPayload(AgentClaimsStalled, AgentClaimsPayload{})
	RegisterPayload(AgentClaimsResumed, AgentClaimsPayload{})
}
