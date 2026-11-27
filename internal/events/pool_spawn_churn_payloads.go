package events

import "time"

// Why a churn episode began or ended (PoolSpawnChurnPayload.Cause).
const (
	// PoolSpawnChurnCauseTripped: poolSpawnChurnTrip churned closes of
	// counted blind seats opened the episode
	// (session.pool_spawn_churn_cooling_down).
	PoolSpawnChurnCauseTripped = "tripped"
	// PoolSpawnChurnCauseProbeFailed: the one probe seat admitted after a
	// hold churned too, and the hold doubled
	// (session.pool_spawn_churn_cooling_down).
	PoolSpawnChurnCauseProbeFailed = "probe_failed"
	// PoolSpawnChurnCauseClaimed: a counted seat claimed work, so blind
	// seats can be served again (session.pool_spawn_churn_resumed).
	PoolSpawnChurnCauseClaimed = "claimed"
	// PoolSpawnChurnCauseConfigChanged: a config change moved the template's
	// scale_check, work_query, authoritative flag or provider, or removed it
	// (session.pool_spawn_churn_resumed).
	PoolSpawnChurnCauseConfigChanged = "config_changed"
)

// PoolSpawnChurnPayload is the typed payload of the spawn-churn guard's
// facts (session.pool_spawn_churn_cooling_down,
// session.pool_spawn_churn_resumed). Template is the agent template (its
// qualified name, also the envelope's Subject). Episode names one churn
// episode, from its trip to what ends it, stable across the alerts it
// raises: an operator's dedupe key. Since is when the episode began (the
// first churned close of the streak that tripped it); Seats how many seats
// churned in that streak, or in the episode so far; Until when the next
// probe seat may spawn while the template is held.
type PoolSpawnChurnPayload struct {
	Template string     `json:"template"`
	Episode  string     `json:"episode"`
	Cause    string     `json:"cause,omitempty"`
	Seats    int        `json:"seats,omitempty"`
	Since    time.Time  `json:"since"`
	Until    *time.Time `json:"until,omitempty"`
}

// IsEventPayload marks PoolSpawnChurnPayload as an events.Payload variant.
func (PoolSpawnChurnPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionPoolSpawnChurnCoolingDown, PoolSpawnChurnPayload{})
	RegisterPayload(SessionPoolSpawnChurnResumed, PoolSpawnChurnPayload{})
}
