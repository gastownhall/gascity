package events

// SupervisorFSPressureSkippedTickPayload is emitted when Linux PSI reports
// sustained filesystem pressure and the supervisor either skips an expensive
// daemon reconciliation phase or forces a bounded liveness tick.
type SupervisorFSPressureSkippedTickPayload struct {
	Avg60               float64 `json:"avg60" doc:"The Linux PSI some avg60 value observed for filesystem IO pressure."`
	Threshold           float64 `json:"threshold" doc:"The configured avg60 threshold that triggered the skip."`
	ConsecutiveSkips    int     `json:"consecutive_skips" doc:"Number of consecutive pressure skips including this tick."`
	MaxConsecutiveSkips int     `json:"max_consecutive_skips" doc:"Maximum consecutive skips before the supervisor forces one reconciliation tick."`
	Outcome             string  `json:"outcome" doc:"The pressure decision outcome: skipped for a shed tick or forced for the bounded liveness tick."`
	Trigger             string  `json:"trigger,omitempty" doc:"The daemon tick trigger, such as patrol or poke."`
}

// IsEventPayload marks SupervisorFSPressureSkippedTickPayload as an
// events.Payload variant.
func (SupervisorFSPressureSkippedTickPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SupervisorFSPressureSkippedTick, SupervisorFSPressureSkippedTickPayload{})
}
