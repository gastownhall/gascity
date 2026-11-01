package events

import "time"

// What the claims-stall guard did about a stall (AgentClaimsPayload.Action).
const (
	// AgentClaimsHeld: the template's default routed-work demand is withheld,
	// one probe seat admitted each time the hold runs out (Until).
	AgentClaimsHeld = "held"
	// AgentClaimsStandby: the template was moved onto its standby provider
	// (Standby) until Until, when its own provider is probed again.
	AgentClaimsStandby = "standby"
)

// Why a stall began or ended, or a standby move ended
// (AgentClaimsPayload.Cause).
const (
	// AgentClaimsCauseSeats: Seats seats in a row failed to claim the work
	// routed to them while it waited (agent.claims_stalled).
	AgentClaimsCauseSeats = "seats"
	// AgentClaimsCauseProbe: the probe seat admitted after a hold, or after
	// a standby move ended, failed to claim too (agent.claims_stalled).
	AgentClaimsCauseProbe = "probe"
	// AgentClaimsCauseProviderHealth: the pack's provider-health record
	// says the provider is unhealthy (agent.claims_stalled), or healthy
	// again (agent.standby_released).
	AgentClaimsCauseProviderHealth = "provider_health"
	// AgentClaimsCauseClaimed: a seat on the stalled provider claimed its
	// work (agent.claims_resumed), or a seat of another template on the
	// provider the move replaced did (agent.standby_released).
	AgentClaimsCauseClaimed = "claimed"
	// AgentClaimsCauseConfigChanged: a config change moved the template off
	// the stalled provider, changed its standby, or removed it
	// (agent.claims_resumed, agent.standby_released).
	AgentClaimsCauseConfigChanged = "config_changed"
	// AgentClaimsCauseHoldEnded: a standby move's hold ran out and the
	// template returns to its own provider to probe it
	// (agent.standby_released).
	AgentClaimsCauseHoldEnded = "hold_ended"
)

// AgentClaimsPayload is the typed payload of the claims-stall guard's facts
// (agent.claims_stalled, agent.claims_resumed, agent.standby_engaged,
// agent.standby_released). Template is the agent template (its qualified
// name, also the envelope's Subject). Provider is the provider its seats ran
// on when they stalled; for a standby move, the template's own provider,
// which Standby stands in for. Episode names one stall, from the trip to what
// ends it, stable across the alerts and moves it causes: an operator's dedupe
// key. Since is when the episode began; Seats how many seats failed in the
// streak that tripped it, or in the episode so far; Until when the next probe
// seat may spawn while the template is held, or when a standby move ends.
// Engagements counts the standby moves in the episode.
type AgentClaimsPayload struct {
	Template    string     `json:"template"`
	Provider    string     `json:"provider"`
	Episode     string     `json:"episode"`
	Action      string     `json:"action,omitempty"`
	Cause       string     `json:"cause,omitempty"`
	Standby     string     `json:"standby,omitempty"`
	Seats       int        `json:"seats,omitempty"`
	Engagements int        `json:"engagements,omitempty"`
	Since       time.Time  `json:"since"`
	Until       *time.Time `json:"until,omitempty"`
}

// IsEventPayload marks AgentClaimsPayload as an events.Payload variant.
func (AgentClaimsPayload) IsEventPayload() {}

func init() {
	for _, t := range []string{AgentClaimsStalled, AgentClaimsResumed, AgentStandbyEngaged, AgentStandbyReleased} {
		RegisterPayload(t, AgentClaimsPayload{})
	}
}
