package events

import "time"

// Why a spawn-churn episode began, went on or ended
// (PoolSpawnChurnPayload.Cause).
const (
	// PoolSpawnChurnCauseTripped: Seats blind seats in a row woke and closed
	// without claiming work (session.pool_spawn_churn_cooling_down).
	PoolSpawnChurnCauseTripped = "tripped"
	// PoolSpawnChurnCauseProbeFailed: the one blind seat admitted after a
	// hold closed without claiming work too, and the hold doubled
	// (session.pool_spawn_churn_cooling_down).
	PoolSpawnChurnCauseProbeFailed = "probe_failed"
	// PoolSpawnChurnCauseClaimed: a blind seat of the template claimed work
	// (session.pool_spawn_churn_resumed).
	PoolSpawnChurnCauseClaimed = "claimed"
	// PoolSpawnChurnCauseConfigChanged: a config change removed the template
	// or changed how its demand or its seats work
	// (session.pool_spawn_churn_resumed).
	PoolSpawnChurnCauseConfigChanged = "config_changed"
)

// PoolSpawnChurnPayload is the typed payload of the controller's spawn-churn
// guard (session.pool_spawn_churn_cooling_down,
// session.pool_spawn_churn_resumed). Template is the pool template (also the
// envelope's Subject). Episode names one run of churn, from its trip to what
// ends it, stable across the events it records: an operator's dedupe key.
// Since is the first churned close of the streak that tripped it; Seats how
// many blind seats churned in that streak, or in the episode so far; Until
// when the hold ends and one blind seat may be spawned again.
type PoolSpawnChurnPayload struct {
	Template string     `json:"template"`
	Episode  string     `json:"episode"`
	Cause    string     `json:"cause"`
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
