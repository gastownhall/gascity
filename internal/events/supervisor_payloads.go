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

// SupervisorCPUPressureHeldStartsPayload is emitted when Linux PSI reports
// host CPU pressure above the threshold and the controller holds starts and
// creates: at the episode's start, at each start forced through the hold, and
// when the pressure returns to or below the threshold.
type SupervisorCPUPressureHeldStartsPayload struct {
	Avg10            float64 `json:"avg10" doc:"The Linux PSI some avg10 value observed for CPU pressure."`
	Threshold        float64 `json:"threshold" doc:"The configured avg10 threshold above which starts are held."`
	MaxHoldIntervals int     `json:"max_hold_intervals" doc:"Patrol intervals a hold lasts before one start or create is forced through."`
	Outcome          string  `json:"outcome" doc:"held when the hold begins, forced when a start or create is forced through it, released when the pressure ends."`
}

// IsEventPayload marks SupervisorCPUPressureHeldStartsPayload as an
// events.Payload variant.
func (SupervisorCPUPressureHeldStartsPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SupervisorFSPressureSkippedTick, SupervisorFSPressureSkippedTickPayload{})
	RegisterPayload(SupervisorCPUPressureHeldStarts, SupervisorCPUPressureHeldStartsPayload{})
}
